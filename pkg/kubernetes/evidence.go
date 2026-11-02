package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

type inventoryReadContextKey struct{}

type evidenceReadStage string

const (
	evidenceIdentityProof       evidenceReadStage = "identity_proof"
	evidenceBaseKindProof       evidenceReadStage = "base_kind_proof"
	evidenceNodeProof           evidenceReadStage = "node_proof"
	evidenceStoreSelection      evidenceReadStage = "store_selection"
	evidenceStoreQuery          evidenceReadStage = "store_query"
	evidenceCheckpointKindMap   evidenceReadStage = "checkpoint_kind_mapping"
	evidenceCheckpointKindProof evidenceReadStage = "checkpoint_kind_proof"
	evidenceFinalCredential     evidenceReadStage = "final_credential"
	evidencePayloadDecode       evidenceReadStage = "payload_decode"
)

// Preserve the typed cause without exposing provider errors, query values, or
// cluster objects in diagnostics. One failure emits one fixed-vocabulary stage.
type evidenceReadError struct {
	stage evidenceReadStage
	cause error
}

func (err *evidenceReadError) Error() string { return "kubernetes: historical evidence read failed" }
func (err *evidenceReadError) Unwrap() error { return err.cause }

func evidenceReadFailure(stage evidenceReadStage, err error) error {
	if err == nil {
		return nil
	}
	var previous *evidenceReadError
	if errors.As(err, &previous) {
		return err
	}
	class := errorClass(err)
	var envelope *inventoryListValidationError
	var syntax *json.SyntaxError
	var fieldType *json.UnmarshalTypeError
	switch {
	case errors.Is(err, storage.ErrUnsupported):
		class = "storage_unsupported"
	case errors.Is(err, storage.ErrNotFound):
		class = "storage_not_found"
	case errors.Is(err, storage.ErrEvidenceLimit):
		class = "storage_limit"
	case errors.Is(err, storage.ErrInvalidEvidence):
		class = "invalid_evidence"
	case errors.Is(err, storage.ErrEvidenceConflict):
		class = "evidence_conflict"
	case errors.As(err, &envelope):
		class = "invalid_list_envelope"
	case errors.As(err, &syntax), errors.As(err, &fieldType):
		class = "invalid_json"
	}
	cause := err
	for depth := 0; depth < 8; depth++ {
		next := errors.Unwrap(cause)
		if next == nil {
			break
		}
		cause = next
	}
	log.Printf("kubernetes: historical evidence failed stage=%s class=%s cause_type=%T", stage, class, cause)
	return &evidenceReadError{stage: stage, cause: err}
}

func pinInventory(ctx context.Context, generation inventoryGeneration) context.Context {
	return context.WithValue(ctx, inventoryReadContextKey{}, generation)
}

func (service *Service) evidenceStore() storage.EvidenceStore {
	if service == nil {
		return nil
	}
	service.changes.mu.Lock()
	enabled := service.changes.evidenceEnabled
	service.changes.mu.Unlock()
	if !enabled {
		return nil
	}
	store, _ := service.usageProvider().(storage.EvidenceStore)
	if store != nil && store.EvidencePolicy().Mode == "unsupported" {
		return nil
	}
	return store
}

func evidenceIdentity(generation inventoryGeneration) string {
	state := generation.View.Kinds["Namespace"]
	if state.State != "ready" || state.Partial {
		return ""
	}
	for _, record := range generation.View.Records {
		if record.Kind == "Namespace" && record.Name == "kube-system" {
			return record.UID
		}
	}
	return ""
}

func issueObservationBounds(view kubeindex.Snapshot) (time.Time, time.Time, bool) {
	var oldest, latest time.Time
	complete := true
	for _, kind := range []string{"Pod", "ReplicaSet", "Deployment", "StatefulSet", "DaemonSet", "Job", "Node", "Event"} {
		state := view.Kinds[kind]
		if state.State != "ready" || state.Partial || state.Absent || state.ObservedAt.IsZero() {
			complete = false
			continue
		}
		if oldest.IsZero() || state.ObservedAt.Before(oldest) {
			oldest = state.ObservedAt
		}
		if state.ObservedAt.After(latest) {
			latest = state.ObservedAt
		}
	}
	return oldest, latest, complete
}

func (service *Service) prepareEvidence(ctx context.Context, generation *inventoryGeneration, previous inventoryGeneration) error {
	store := service.evidenceStore()
	if store == nil {
		return nil
	}
	if generation.liveAuthorityOnly {
		return unverifiedInventoryAuthority(ErrInventoryNotReady)
	}
	identity := evidenceIdentity(*generation)
	if identity == "" {
		return ErrIncompleteInventory
	}
	scope := storage.EvidenceScope{OrgID: service.scope.OrgID, ClusterID: service.scope.ClusterID, ClusterUID: identity}
	checkpoint, err := store.EvidenceCheckpoint(ctx, scope)
	if err != nil {
		return err
	}
	oldest, latest, complete := issueObservationBounds(generation.View)
	if latest.IsZero() {
		return ErrIncompleteInventory
	}
	viewIdentity, _ := json.Marshal(generation.View)
	batchID := "capture:" + logIdentity(identity+"\x00"+string(viewIdentity)+"\x00"+generation.PodSampledAt.UTC().Format(time.RFC3339Nano))
	batch := storage.EvidenceBatch{ID: batchID, Expected: checkpoint.ID, Generation: checkpoint.Generation, At: latest, Epoch: service.scope.CredentialID}
	batch.Coverage = map[string]storage.EvidenceInterval{}
	for kind, state := range generation.View.Kinds {
		if state.State != "ready" || state.Partial || state.Absent || state.ObservedAt.IsZero() {
			continue
		}
		batch.At = maxTime(batch.At, state.ObservedAt)
		// LIST starts at ObservedAt, but returned objects can change while the
		// request is in flight. Their evidence was observed by completion.
		batch.At = maxTime(batch.At, state.CompletedAt)
		from := checkpoint.Coverage[kind].From
		if from.IsZero() {
			from = state.ObservedAt
		}
		batch.Coverage[kind] = storage.EvidenceInterval{From: from, To: state.ObservedAt}
	}

	if complete {
		batch.CoveredThrough = oldest
	}
	page, err := service.issuesSnapshot(pinInventory(ctx, *generation), IssueOptions{unpaged: true, captureOnly: true})
	if err != nil {
		return err
	}
	if page.Sync.Partial {
		batch.CoveredThrough = time.Time{}
	}
	for _, issue := range page.Items {
		if issue.Partial {
			batch.CoveredThrough = time.Time{}
		}
		key := logIdentity(issue.Root.Namespace + "\x00" + issue.Root.Kind + "\x00" + issue.Root.UID + "\x00" + issue.Rule)
		raw, err := json.Marshal(issue)
		if err != nil {
			return err
		}
		authorizationKind := issue.Root.Kind
		if strings.HasPrefix(issue.Rule, "event.") {
			authorizationKind = "Event"
		} else if strings.HasPrefix(issue.Rule, "pod.") {
			authorizationKind = "Pod"
		}
		batch.Issues = append(batch.Issues, storage.EvidenceIssue{AuthorizationKind: authorizationKind, Broken: issue.Broken, LastSeen: latest, Key: key, Namespace: issue.Root.Namespace, Kind: issue.Root.Kind, UID: issue.Root.UID, Rule: issue.Rule, Payload: raw})
	}
	byUID := map[string]kubeindex.Record{}
	for _, record := range previous.View.Records {
		byUID[record.UID] = record
	}
	for _, record := range generation.View.Records {
		byUID[record.UID] = record
	}
	appendChange := func(change kubechanges.Change) error {
		if record, ok := byUID[change.UID]; ok {
			change.RootUID = kubebreaks.Root(record, byUID).UID
		}
		raw, err := json.Marshal(change)
		if err != nil {
			return err
		}
		authorizationKind := change.Kind
		if change.Type == kubechanges.WarningBurst {
			authorizationKind = "Event"
		}
		priority := 1
		if change.Source == "observed" {
			priority = 2
		}
		batch.Records = append(batch.Records, storage.EvidenceRecord{AuthorizationKind: authorizationKind, Priority: priority, ID: "change:" + change.ID, Type: "change", At: change.At, ObservedAt: batch.At, Namespace: change.Namespace, Name: change.Name, Kind: change.Kind, UID: change.UID, RootUID: change.RootUID, Rule: string(change.Type), Epoch: batch.Epoch, Payload: raw})
		return nil
	}
	before := map[string]kubeindex.Record{}
	for _, record := range previous.View.Records {
		before[record.UID] = record
	}
	after := map[string]kubeindex.Record{}
	for _, record := range generation.View.Records {
		after[record.UID] = record
		state := generation.View.Kinds[record.Kind]
		if state.State != "ready" || state.Partial {
			continue
		}
		old, known := before[record.UID]
		delta := kubeindex.Delta{New: &record}
		if known {
			delta.Old = &old
		}
		for _, change := range kubechanges.DetectAll(service.scope.ClusterID, delta, state.ObservedAt) {
			if err := appendChange(change); err != nil {
				return err
			}
		}
	}
	for uid, record := range before {
		if _, exists := after[uid]; exists {
			continue
		}
		state := generation.View.Kinds[record.Kind]
		if state.State != "ready" || state.Partial {
			continue
		}
		for _, change := range kubechanges.DetectAll(service.scope.ClusterID, kubeindex.Delta{Old: &record}, state.ObservedAt) {
			if err := appendChange(change); err != nil {
				return err
			}
		}
	}
	for _, change := range kubechanges.Reconstruct(service.scope.ClusterID, generation.View.Records, latest) {
		if err := appendChange(change); err != nil {
			return err
		}
	}
	for _, crash := range generation.Crashes {
		raw, err := json.Marshal(crash)
		if err != nil {
			return err
		}
		observed := maxTime(latest, crash.ObservedAt)
		batch.At = maxTime(batch.At, observed)
		batch.Records = append(batch.Records, storage.EvidenceRecord{AuthorizationKind: "Pod", ID: "crash:" + crash.ID, Type: "crash", At: crash.At, ObservedAt: crash.ObservedAt, Namespace: crash.Namespace, Kind: crash.RootKind, UID: crash.PodUID, RootUID: crash.RootUID, PodUID: crash.PodUID, Epoch: batch.Epoch, Payload: raw})
	}
	if !checkpoint.At.IsZero() && oldest.Sub(checkpoint.At) > 2*time.Minute {
		raw, _ := json.Marshal(kubechanges.Gap{From: checkpoint.At, To: oldest})
		batch.Records = append(batch.Records, storage.EvidenceRecord{ID: "gap:" + batch.ID, Type: "coverage_gap", GapFrom: checkpoint.At, GapTo: oldest, At: oldest, ObservedAt: batch.At, Epoch: batch.Epoch, Payload: raw})
	}
	if err = batch.Validate(); err != nil {
		return err
	}
	for kind, interval := range batch.Coverage {
		prior := checkpoint.Coverage[kind]
		state := previous.View.Kinds[kind]
		knownFailure := previous.EvidenceFailure != "" || state.State == "error" || state.Partial
		if prior.To.IsZero() || !interval.To.After(prior.To) || !knownFailure && interval.To.Sub(prior.To) <= 2*inventoryCadence(kind) {
			continue
		}
		raw, _ := json.Marshal(kubechanges.Gap{From: prior.To, To: interval.To})
		batch.Records = append(batch.Records, storage.EvidenceRecord{ID: "gap:" + batch.ID + ":" + kind, Type: "coverage_gap", GapFrom: prior.To, GapTo: interval.To, Kind: kind, At: interval.To, ObservedAt: batch.At, Epoch: batch.Epoch, Payload: raw})
	}
	if err = batch.Validate(); err != nil {
		return err
	}
	generation.EvidenceScope = scope
	generation.EvidenceBatch = &batch
	return nil
}

func (service *Service) commitEvidence(ctx context.Context, generation inventoryGeneration) error {
	if generation.EvidenceBatch == nil || !strings.HasPrefix(generation.EvidenceBatch.ID, "capture:") {
		return nil
	}
	store := service.evidenceStore()
	if store == nil {
		return storage.ErrUnsupported
	}
	return service.commitCredential(ctx, func() error {
		batch := *generation.EvidenceBatch
		err := store.CommitEvidence(ctx, generation.EvidenceScope, batch)
		if !errors.Is(err, storage.ErrEvidenceConflict) {
			return err
		}
		checkpoint, readErr := store.EvidenceCheckpoint(ctx, generation.EvidenceScope)
		if readErr != nil {
			return readErr
		}
		// Explicit journal deletion invalidates queued old evidence. The next live
		// capture starts normally; do not repopulate it by replaying a stale batch.
		if checkpoint.Generation != batch.Generation {
			return nil
		}
		batch.Expected = checkpoint.ID
		if !batch.At.After(checkpoint.At) {
			// A concurrent authorized producer advanced coverage. Preserve this older
			// observation without changing current lifecycle or inventing a recovery.
			batch.At, batch.HistoricalOnly = checkpoint.At, true
			batch.CoveredThrough, batch.Coverage = time.Time{}, nil
		}
		return store.CommitEvidence(ctx, generation.EvidenceScope, batch)
	})
}

func (service *Service) evidenceClusterUID(ctx context.Context) (string, error) {
	if service == nil || service.client == nil {
		return "", ErrCredentialUnavailable
	}
	var identity struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
	}
	if err := service.client.GetJSON(ctx, "/api/v1/namespaces/kube-system", &identity); err != nil {
		return "", err
	}
	if identity.Metadata.UID == "" {
		return "", ErrIncompleteInventory
	}
	return identity.Metadata.UID, nil
}

// authorizeEvidence proves physical cluster identity and present read authority
// without consulting live inventory or using credential-scoped cached objects.
func (service *Service) authorizeEvidence(ctx context.Context, namespace string) (storage.EvidenceScope, error) {
	// Authorization is a bounded control read, not bulk inventory collection.
	ctx = context.WithValue(ctx, captureReadContextKey{}, nil)
	if service == nil || service.client == nil {
		return storage.EvidenceScope{}, ErrCredentialUnavailable
	}
	if err := service.validateCredential(ctx); err != nil {
		return storage.EvidenceScope{}, err
	}
	ctx, cancel := service.credentialContext(ctx)
	defer cancel()
	ctx, cancelBudget := ensureOperationBudgetRequests(ctx, 32)
	defer cancelBudget()
	uid, err := service.evidenceClusterUID(ctx)
	if err != nil {
		return storage.EvidenceScope{}, evidenceReadFailure(evidenceIdentityProof, err)
	}
	for _, id := range []string{"core~v1~pods", "apps~v1~replicasets", "apps~v1~deployments", "apps~v1~statefulsets", "apps~v1~daemonsets", "batch~v1~jobs", "core~v1~events"} {
		if err := service.authorizeEvidenceKind(ctx, namespace, id); err != nil {
			return storage.EvidenceScope{}, evidenceReadFailure(evidenceBaseKindProof, err)
		}
	}

	// Node-derived issues are cluster-scoped; namespace-filtered reads exclude them.
	if namespace == "" {
		var response struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := service.client.GetJSON(ctx, "/api/v1/nodes?limit=1", &response); err != nil {
			return storage.EvidenceScope{}, evidenceReadFailure(evidenceNodeProof, err)
		}
	}
	return storage.EvidenceScope{OrgID: service.scope.OrgID, ClusterID: service.scope.ClusterID, ClusterUID: uid}, nil
}

func (service *Service) authorizeEvidenceKind(ctx context.Context, namespace, id string) error {
	ctx = context.WithValue(ctx, captureReadContextKey{}, nil)
	parts := strings.Split(id, "~")
	if len(parts) != 3 {
		return ErrInvalidArguments
	}
	path := "/apis/" + parts[0] + "/" + parts[1]
	if parts[0] == "core" {
		path = "/api/" + parts[1]
	}
	if namespace != "" && id != indexedResourceIDs["Node"] && id != indexedResourceIDs["Namespace"] {
		path += "/namespaces/" + url.PathEscape(namespace)
	}
	path += "/" + parts[2] + "?limit=1"
	var response struct {
		Items []json.RawMessage `json:"items"`
	}
	return service.client.GetMetadataJSON(ctx, path, &response)
}

// A retained display owner is not a permission source. Unknown or malformed
// observations remain visible as gaps, never as authorized application data.
func (service *Service) projectEvidence(record storage.EvidenceRecord) storage.EvidenceRecord {
	sourceKind, err := service.evidenceRecordAuthorizationKind(record)
	if err == nil && (sourceKind == "Node" || sourceKind == "Namespace") && record.Namespace != "" {
		err = ErrIncompleteInventory
	}
	if err == nil {
		switch record.Type {
		case "crash":
			var value kubechanges.CrashEvent
			err = json.Unmarshal(record.Payload, &value)
			if err == nil && (!value.Valid() || value.Cluster != service.scope.ClusterID || record.ID != "crash:"+value.ID || record.Namespace != value.Namespace || record.Kind != value.RootKind || record.UID != value.PodUID || record.PodUID != value.PodUID || record.RootUID != value.RootUID || record.AuthorizationKind != "" && record.AuthorizationKind != "Pod") {
				err = ErrIncompleteInventory
			}
		case "change":
			// Typed change identity and source were checked before projection.
		case "issue_opened", "issue_observed", "issue_resolved":
			var value storage.EvidenceIssue
			err = json.Unmarshal(record.Payload, &value)
			if err == nil {
				var issue Issue
				err = json.Unmarshal(value.Payload, &issue)
				if err == nil && (issue.Root.UID != record.RootUID || issue.Root.Namespace != record.Namespace || issue.Root.Kind != record.Kind) {
					err = ErrIncompleteInventory
				}
			}
		case "coverage_gap":
			var value kubechanges.Gap
			err = json.Unmarshal(record.Payload, &value)
		default:
			err = ErrIncompleteInventory
		}
	}
	if err == nil {
		return record
	}
	return unavailableEvidence(record)
}

func unavailableEvidence(record storage.EvidenceRecord) storage.EvidenceRecord {
	from, to := record.At, record.At
	if record.ObservedAt.Before(from) && !record.ObservedAt.IsZero() {
		from = record.ObservedAt
	}
	if record.ObservedAt.After(to) {
		to = record.ObservedAt
	}
	raw, _ := json.Marshal(kubechanges.Gap{From: from, To: to})
	record.Type, record.Payload, record.AuthorizationKind = "coverage_gap", raw, ""
	record.Kind, record.GapFrom, record.GapTo = "", from, to
	return record
}

// A crash is observed from Pod status. Its display root can be an arbitrary
// controller owner and is not the API whose read permission authorizes it.
func (service *Service) evidenceRecordAuthorizationKind(record storage.EvidenceRecord) (string, error) {
	if record.Type == "change" {
		var change Change
		if err := json.Unmarshal(record.Payload, &change); err != nil {
			return "", err
		}
		if !change.Valid() || change.UID == "" || change.Cluster != service.scope.ClusterID ||
			record.ID != "change:"+change.ID || record.Namespace != change.Namespace || record.Name != change.Name ||
			record.Kind != change.Kind || record.UID != change.UID || record.RootUID != change.RootUID ||
			record.Rule != string(change.Type) || !record.At.Equal(change.At) {
			return "", ErrIncompleteInventory
		}
		kind := change.Kind
		if change.Type == kubechanges.WarningBurst {
			kind = "Event"
		}
		if record.AuthorizationKind != "" && record.AuthorizationKind != kind {
			return "", ErrIncompleteInventory
		}
		if indexedResourceIDs[kind] == "" {
			return "", storage.ErrUnsupported
		}
		return kind, nil
	}
	kind := record.AuthorizationKind
	if kind == "" {
		kind = record.Kind
		if record.Type == "crash" {
			var crash kubechanges.CrashEvent
			if err := json.Unmarshal(record.Payload, &crash); err != nil {
				return "", evidenceReadFailure(evidencePayloadDecode, err)
			}
			if !crash.Valid() || crash.Cluster != service.scope.ClusterID || record.ID != "crash:"+crash.ID || record.Namespace != crash.Namespace || record.Kind != crash.RootKind || record.UID != crash.PodUID || record.PodUID != crash.PodUID || record.RootUID != crash.RootUID {
				return "", evidenceReadFailure(evidencePayloadDecode, ErrIncompleteInventory)
			}
			kind = "Pod"
		}
	}
	if kind == "" && record.Type == "coverage_gap" {
		return "", nil
	}
	if indexedResourceIDs[kind] == "" {
		return "", evidenceReadFailure(evidenceCheckpointKindMap, storage.ErrUnsupported)
	}
	return kind, nil
}

// Present authorization denials must survive later transient source failures,
// regardless of provider traversal order or repeated memoized proof errors.
func mergeEvidenceProofError(previous, next error) error {
	priority := func(err error) int {
		switch {
		case err == nil:
			return 0
		case errors.Is(err, ErrUnauthorized):
			return 3
		case errors.Is(err, ErrForbidden):
			return 2
		default:
			return 1
		}
	}
	if priority(next) > priority(previous) {
		return next
	}
	return previous
}

func (service *Service) queryEvidence(ctx context.Context, query storage.EvidenceQuery) (storage.EvidencePage, error) {
	store := service.evidenceStore()
	if store == nil {
		return storage.EvidencePage{}, evidenceReadFailure(evidenceStoreSelection, storage.ErrUnsupported)
	}
	for _, value := range []string{query.Namespace, query.Kind, query.UID, query.RootUID, query.PodUID, query.Rule} {
		if value != "" && (!safeSegment(value) || len(value) > 256) {
			return storage.EvidencePage{}, ErrInvalidArguments
		}
	}
	if !query.Since.IsZero() && !query.Until.IsZero() && query.Since.After(query.Until) {
		return storage.EvidencePage{}, ErrInvalidArguments
	}
	ctx, cancel := ensureOperationBudgetRequests(ctx, 64)
	defer cancel()

	scope, err := service.authorizeEvidence(ctx, query.Namespace)
	if err != nil {
		return storage.EvidencePage{}, err
	}
	scope.ReadOrgs = append([]string(nil), service.readOrgs...)
	// Bind the projection to opaque cursors so unavailable rows occupy stable
	// slots while counts include only decodable, supported evidence.
	proofs := map[string]error{}
	var proofErr error
	query.ProjectionID = "source-authorized-v2"
	query.Projection = func(record storage.EvidenceRecord) storage.EvidenceRecord {
		record = service.projectEvidence(record)
		if record.Type == "coverage_gap" {
			return record
		}
		kind, kindErr := service.evidenceRecordAuthorizationKind(record)
		if kindErr != nil {
			return unavailableEvidence(record)
		}
		authorityErr, found := proofs[kind]
		if !found {
			authorityErr = service.authorizeEvidenceKind(ctx, query.Namespace, indexedResourceIDs[kind])
			proofs[kind] = authorityErr
		}
		if authorityErr == nil {
			return record
		}
		if !errors.Is(authorityErr, ErrNotFound) {
			proofErr = mergeEvidenceProofError(proofErr, authorityErr)
		}
		return unavailableEvidence(record)
	}
	if query.Kind != "" {
		id := indexedResourceIDs[query.Kind]
		if id == "" {
			return storage.EvidencePage{}, storage.ErrUnsupported
		}
		if err := service.authorizeEvidenceKind(ctx, query.Namespace, id); err != nil {
			return storage.EvidencePage{}, evidenceReadFailure(evidenceCheckpointKindProof, err)
		}
	}
	page, err := store.QueryEvidence(ctx, scope, query)
	if proofErr != nil {
		return storage.EvidencePage{}, evidenceReadFailure(evidenceCheckpointKindProof, proofErr)
	}
	if errors.Is(err, storage.ErrInvalidEvidence) || errors.Is(err, storage.ErrEvidenceConflict) {
		return storage.EvidencePage{}, evidenceReadFailure(evidenceStoreQuery, errors.Join(ErrInvalidArguments, err))
	}
	if err != nil {
		return page, evidenceReadFailure(evidenceStoreQuery, err)
	}
	if err = service.validateCredential(ctx); err != nil {
		return storage.EvidencePage{}, evidenceReadFailure(evidenceFinalCredential, err)
	}
	return page, nil
}

func (service *Service) evidenceTimeline(ctx context.Context, query TimelineQuery) (TimelinePage, error) {
	kind := "timeline"
	if query.BreaksOnly {
		kind = "break"
	}
	source, err := service.queryEvidence(ctx, storage.EvidenceQuery{Since: query.Since, Until: query.Until, Namespace: query.Namespace, Kind: query.Kind, RootUID: query.RootUID, PodUID: query.PodUID, ResourceUID: query.ResourceUID, Type: kind, Limit: normalizePageLimit(query.Limit), Cursor: query.Cursor})
	if err != nil {
		return TimelinePage{}, err
	}
	page := TimelinePage{Items: []TimelineItem{}, Gaps: []kubechanges.Gap{}, Counts: map[string]int{"change": 0, "crash": 0, "break": 0}, Next: source.Next, Since: source.Since, Until: source.Until, BreaksOnly: query.BreaksOnly, Sync: SyncStatus{State: "ready"}}
	if !source.Checkpoint.PrunedThrough.IsZero() {
		page.Gaps = append(page.Gaps, kubechanges.Gap{From: query.Since, To: source.Checkpoint.PrunedThrough})
	}
	for _, record := range source.Items {
		item := TimelineItem{ID: record.ID, At: record.At}
		switch record.Type {
		case "change":
			var change Change
			if err = json.Unmarshal(record.Payload, &change); err != nil {
				return page, evidenceReadFailure(evidencePayloadDecode, err)
			}
			item.Type, item.Change = "change", &change
		case "crash":
			var crash kubechanges.CrashEvent
			if err = json.Unmarshal(record.Payload, &crash); err != nil {
				return page, evidenceReadFailure(evidencePayloadDecode, err)
			}
			item.Type, item.Crash = "crash", &crash
		case "issue_opened", "issue_resolved":
			var observation storage.EvidenceIssue
			if err = json.Unmarshal(record.Payload, &observation); err != nil {
				return page, evidenceReadFailure(evidencePayloadDecode, err)
			}
			var issue Issue
			if err = json.Unmarshal(observation.Payload, &issue); err != nil {
				return page, evidenceReadFailure(evidencePayloadDecode, err)
			}
			issue.FirstSeen, issue.LastSeen, issue.OccurrenceID, issue.ClosedAt = observation.FirstSeen, observation.LastSeen, observation.OccurrenceID, observation.ClosedAt
			issue.State = "open"
			if observation.ObservationOnly {
				issue.State = "observed"
			}
			if observation.ClosedAt != nil {
				issue.State = "resolved"
			}
			item.Type, item.Issue = "break", &issue
		case "coverage_gap":
			var gap kubechanges.Gap
			if err = json.Unmarshal(record.Payload, &gap); err != nil {
				return page, evidenceReadFailure(evidencePayloadDecode, err)
			}
			page.Gaps = append(page.Gaps, gap)
			continue
		default:
			continue
		}
		page.Items = append(page.Items, item)
		page.Counts[item.Type]++
	}
	page.Counts = map[string]int{"change": source.Counts["change"], "crash": source.Counts["crash"], "break": source.Counts["issue_opened"] + source.Counts["issue_resolved"]}
	page.CrashCount, page.Total = page.Counts["crash"], page.Counts["change"]+page.Counts["crash"]+page.Counts["break"]
	if !source.Checkpoint.CoveredThrough.IsZero() {
		page.Sync.AgeSeconds = max(0, time.Since(source.Checkpoint.CoveredThrough).Seconds())
	}
	page.Gaps = mergeEvidenceGaps(page.Gaps, source.Since, source.Until)
	return page, nil
}

func (service *Service) historicalIssues(ctx context.Context, options IssueOptions) (IssuePage, error) {
	page := IssuePage{Items: []Issue{}, Totals: map[string]int{}, Broken: BrokenTotals{BySeverity: map[string]int{}}, Sync: SyncStatus{State: "ready"}}
	variant := options.Severity
	if options.Broken {
		variant += ":broken"
	}
	source, err := service.queryEvidence(ctx, storage.EvidenceQuery{Variant: variant, Since: options.Since, Until: options.Until, Namespace: options.Namespace, Kind: options.Kind, RootUID: options.RootUID, Type: "occurrence", Limit: normalizePageLimit(options.Limit), Cursor: options.Cursor})
	if err != nil {
		return page, evidenceReadFailure(evidencePayloadDecode, err)
	}
	for _, record := range source.Items {
		if record.Type == "coverage_gap" {
			page.Sync.Partial = true
			page.Partial = append(page.Partial, PartialFailure{Class: "historical_evidence_unavailable"})
			continue
		}
		var observation storage.EvidenceIssue
		if err = json.Unmarshal(record.Payload, &observation); err != nil {
			return page, evidenceReadFailure(evidencePayloadDecode, err)
		}
		var issue Issue
		if err = json.Unmarshal(observation.Payload, &issue); err != nil {
			return page, evidenceReadFailure(evidencePayloadDecode, err)
		}
		if options.Broken && !issue.Broken || options.Severity != "" && issue.Severity != options.Severity {
			continue
		}
		issue.FirstSeen, issue.LastSeen, issue.OccurrenceID, issue.ClosedAt = observation.FirstSeen, observation.LastSeen, observation.OccurrenceID, observation.ClosedAt
		issue.State = "open"
		if observation.ObservationOnly {
			issue.State = "observed"
		}
		if observation.ClosedAt != nil {
			issue.State = "resolved"
		}
		page.Items = append(page.Items, issue)
		page.Totals[issue.Severity]++
		if issue.Broken {
			page.Broken.Total++
			page.Broken.BySeverity[issue.Severity]++
		}
	}
	page.Next = source.Next
	sort.SliceStable(page.Items, func(i, j int) bool { return page.Items[i].LastSeen.After(page.Items[j].LastSeen) })
	return page, nil
}

func (service *Service) persistedRootContext(ctx context.Context, options IssueOptions) (IssuePage, error) {
	page := IssuePage{Items: []Issue{}, Totals: map[string]int{}, Broken: BrokenTotals{BySeverity: map[string]int{}}, Sync: SyncStatus{State: "ready"}, SelectionStatus: "not_requested"}
	selecting := options.OccurrenceID != "" || !options.Center.IsZero()
	if selecting {
		page.SelectionStatus = "not_found"
	}
	// Search retained occurrences, not the selected evidence interval. A missing
	// live object, recovered issue, or empty selected interval cannot erase this
	// identity. Each candidate still passes the normal current authority gate.
	options.Since, options.Until, options.Limit = time.Unix(1, 0).UTC(), time.Now().UTC(), 100
	for scanned := 0; scanned < 1000; scanned += 100 {
		history, err := service.historicalIssues(ctx, options)
		if err != nil {
			return page, err
		}
		page.Sync.Partial = page.Sync.Partial || history.Sync.Partial
		page.Partial = append(page.Partial, history.Partial...)
		for _, issue := range history.Items {
			if issue.Root.UID != options.RootUID || options.Namespace != "" && issue.Root.Namespace != options.Namespace {
				return page, ErrIncompleteInventory
			}
			if page.RootContext == nil {
				page.RootContext = &PersistedRootContext{Root: issue.Root, Occurrence: issue, Retained: true}
			}
			if !selecting {
				return page, nil
			}
			if options.OccurrenceID != "" {
				// ID is authoritative: center never permits a different occurrence.
				if issue.OccurrenceID == options.OccurrenceID {
					page.SelectedOccurrence, page.SelectionStatus = &issue, "resolved"
					return page, nil
				}
			} else if issue.BrokeAt != nil && issue.BrokeAt.Equal(options.Center) {
				if page.SelectionStatus == "resolved" {
					page.SelectedOccurrence, page.SelectionStatus = nil, "ambiguous"
				} else if page.SelectionStatus != "ambiguous" {
					page.SelectedOccurrence, page.SelectionStatus = &issue, "resolved"
				}
			}
		}
		if history.Next == "" {
			if selecting && page.Sync.Partial && page.SelectionStatus != "ambiguous" {
				page.SelectedOccurrence, page.SelectionStatus = nil, "partial"
			}
			return page, nil
		}
		options.Cursor = history.Next
	}
	if selecting {
		// A bounded scan cannot prove absence or uniqueness beyond its horizon.
		page.SelectedOccurrence, page.SelectionStatus = nil, "partial"
		page.Sync.Partial = true
		page.Partial = append(page.Partial, PartialFailure{Class: "selection_scan_limit"})
		return page, nil
	}
	return page, storage.ErrEvidenceLimit
}

func (service *Service) evidenceChanges(ctx context.Context, query ChangeQuery) (ChangePage, error) {
	now := time.Now().UTC()
	if query.Cursor == "" {
		if query.Until.IsZero() {
			query.Until = now
		}
		if query.Since.IsZero() {
			query.Since = query.Until.Add(-24 * time.Hour)
		}
	}
	source, err := service.queryEvidence(ctx, storage.EvidenceQuery{Since: query.Since, Until: query.Until, Namespace: query.Namespace, Kind: query.Kind, RootUID: query.RootUID, Name: query.Name, Rule: string(query.Type), Type: "change", WithGaps: true, Limit: normalizePageLimit(query.Limit), Cursor: query.Cursor})
	page := ChangePage{Items: []Change{}, Gaps: []kubechanges.Gap{}, Next: source.Next, Sync: SyncStatus{State: "ready"}}
	if err != nil {
		return page, evidenceReadFailure(evidencePayloadDecode, err)
	}
	for _, record := range source.Items {
		if record.Type == "coverage_gap" {
			var gap kubechanges.Gap
			if err = json.Unmarshal(record.Payload, &gap); err != nil {
				return page, evidenceReadFailure(evidencePayloadDecode, err)
			}
			page.Gaps = append(page.Gaps, gap)
			continue
		}
		var change Change
		if err = json.Unmarshal(record.Payload, &change); err != nil {
			return page, evidenceReadFailure(evidencePayloadDecode, err)
		}
		page.Items = append(page.Items, change)
	}
	if !source.Checkpoint.PrunedThrough.IsZero() && query.Since.Before(source.Checkpoint.PrunedThrough) {
		page.Gaps = append(page.Gaps, kubechanges.Gap{From: query.Since, To: source.Checkpoint.PrunedThrough})
	}
	page.Gaps = mergeEvidenceGaps(page.Gaps, source.Since, source.Until)
	return page, nil
}

func (service *Service) evidenceCrashes(ctx context.Context, query kubechanges.CrashQuery) (CrashPage, error) {
	now := time.Now().UTC()
	if query.Cursor == "" {
		if query.Until.IsZero() {
			query.Until = now
		}
		if query.Since.IsZero() {
			query.Since = query.Until.Add(-24 * time.Hour)
		}
	}
	source, err := service.queryEvidence(ctx, storage.EvidenceQuery{Since: query.Since, Until: query.Until, Namespace: query.Namespace, RootUID: query.RootUID, PodUID: query.PodUID, ByObservation: true, Type: "crash", WithGaps: true, Limit: normalizePageLimit(query.Limit), Cursor: query.Cursor})
	page := CrashPage{CrashPage: kubechanges.CrashPage{Items: []kubechanges.CrashEvent{}, Gaps: []kubechanges.Gap{}, CaptureGaps: []kubechanges.Gap{}, Coverage: map[string]kubechanges.Gap{}, Since: query.Since, Until: query.Until, Next: source.Next, RetentionBasis: source.Policy.Mode}, Sync: SyncStatus{State: "ready"}, Capture: service.CaptureStatus()}
	if err != nil {
		return page, evidenceReadFailure(evidencePayloadDecode, err)
	}
	for _, record := range source.Items {
		if record.Type == "coverage_gap" {
			var gap kubechanges.Gap
			if err = json.Unmarshal(record.Payload, &gap); err != nil {
				return page, evidenceReadFailure(evidencePayloadDecode, err)
			}
			page.CaptureGaps = append(page.CaptureGaps, gap)
			continue
		}
		var crash kubechanges.CrashEvent
		if err = json.Unmarshal(record.Payload, &crash); err != nil {
			return page, evidenceReadFailure(evidencePayloadDecode, err)
		}
		page.Items = append(page.Items, crash)
	}
	page.CrashCount = source.Counts["crash"]
	page.Since, page.Until = source.Since, source.Until
	page.StartedAt = source.Checkpoint.StartedAt
	page.RetentionFrom = source.Checkpoint.PrunedThrough
	if !source.Checkpoint.PrunedThrough.IsZero() && query.Since.Before(source.Checkpoint.PrunedThrough) {
		page.Gaps = append(page.Gaps, kubechanges.Gap{From: query.Since, To: source.Checkpoint.PrunedThrough})
	}
	page.Gaps = mergeEvidenceGaps(page.Gaps, source.Since, source.Until)
	page.CaptureGaps = mergeEvidenceGaps(page.CaptureGaps, source.Since, source.Until)
	return page, nil
}

func (service *Service) evidenceChangeWindow(ctx context.Context, since, until time.Time, namespace string) (kubechanges.Page, error) {
	source, err := service.queryEvidence(ctx, storage.EvidenceQuery{Since: since, Until: until, Namespace: namespace, Type: "change", WithGaps: true, Limit: 500})
	page := kubechanges.Page{Items: []Change{}, Next: source.Next, Coverage: map[string]kubechanges.Gap{}}
	if err != nil {
		return page, evidenceReadFailure(evidencePayloadDecode, err)
	}
	for _, record := range source.Items {
		if record.Type == "coverage_gap" {
			var gap kubechanges.Gap
			if err = json.Unmarshal(record.Payload, &gap); err != nil {
				return page, evidenceReadFailure(evidencePayloadDecode, err)
			}
			page.Gaps = append(page.Gaps, gap)
			continue
		}
		var change Change
		if err = json.Unmarshal(record.Payload, &change); err != nil {
			return page, evidenceReadFailure(evidencePayloadDecode, err)
		}
		page.Items = append(page.Items, change)
	}
	for kind, interval := range source.Checkpoint.Coverage {
		from := interval.From
		if from.Before(source.Checkpoint.PrunedThrough) {
			from = source.Checkpoint.PrunedThrough
		}
		page.Coverage[kind] = kubechanges.Gap{From: from, To: interval.To}
	}
	if since.Before(source.Checkpoint.PrunedThrough) {
		page.Gaps = append(page.Gaps, kubechanges.Gap{From: since, To: source.Checkpoint.PrunedThrough})
	}
	page.Gaps = mergeEvidenceGaps(page.Gaps, source.Since, source.Until)
	return page, nil
}

func mergeEvidenceGaps(gaps []kubechanges.Gap, since, until time.Time) []kubechanges.Gap {
	result := []kubechanges.Gap{}
	for _, gap := range gaps {
		if gap.To.Before(since) || gap.From.After(until) {
			continue
		}
		if gap.From.Before(since) {
			gap.From = since
		}
		if gap.To.After(until) {
			gap.To = until
		}
		result = append(result, gap)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].From.Before(result[j].From) })
	merged := []kubechanges.Gap{}
	for _, gap := range result {
		if len(merged) == 0 || gap.From.After(merged[len(merged)-1].To) {
			merged = append(merged, gap)
		} else if gap.To.After(merged[len(merged)-1].To) {
			merged[len(merged)-1].To = gap.To
		}
	}
	return merged
}

type inventoryRestoreAuthority struct {
	once      sync.Once
	gate      chan struct{}
	validated atomic.Bool
}

// Restoring a durable payload cannot turn a locally readable but revoked static
// credential into permission to replay its old protected inventory. A live
// capture establishes its own authority; cold restores prove it independently.
func (service *Service) authorizeInventoryRestore(ctx context.Context, generation inventoryGeneration) error {
	if len(generation.View.Kinds) == 0 || service.evidenceStore() == nil {
		return nil
	}
	authority := service.inventoryRestore
	if authority == nil {
		return ErrClusterUnavailable
	}
	if authority.validated.Load() {
		return nil
	}
	authority.once.Do(func() { authority.gate = make(chan struct{}, 1); authority.gate <- struct{}{} })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-authority.gate:
	}
	defer func() { authority.gate <- struct{}{} }()
	if authority.validated.Load() {
		return nil
	}
	bound, cancelIdentity := service.credentialContext(ctx)
	defer cancelIdentity()
	proofCtx, cancel := withOperationBudget(bound, 10*time.Second, 64, defaultOperationBytes, defaultOperationItems)
	defer cancel()
	scope, err := service.authorizeEvidence(proofCtx, "")
	if err != nil {
		// Restore callers retain their existing provider-error contract; the
		// history-query wrapper is only needed on the historical read surface.
		var read *evidenceReadError
		if errors.As(err, &read) {
			err = read.cause
		}
		return inventoryPayloadError{err}
	}
	expected := generation.EvidenceScope.ClusterUID
	if expected == "" {
		for _, record := range generation.View.Records {
			if record.Kind == "Namespace" && record.Name == "kube-system" {
				expected = record.UID
				break
			}
		}
	}
	if expected == "" || expected != scope.ClusterUID {
		return inventoryPayloadError{ErrClusterUnavailable}
	}
	checked := map[string]bool{}
	for kind, state := range generation.View.Kinds {
		if state.Absent || state.Error == "forbidden" || state.Error == "unauthorized" {
			continue
		}
		id := indexedResourceIDs[kind]
		if id == "" {
			return inventoryPayloadError{ErrInvalidArguments}
		}
		if err = service.authorizeEvidenceKind(proofCtx, "", id); err != nil {
			return inventoryPayloadError{err}
		}
		checked[id] = true
	}
	for id := range generation.Resources {
		if checked[id] {
			continue
		}
		if err = service.authorizeEvidenceKind(proofCtx, "", id); err != nil {
			return inventoryPayloadError{err}
		}
	}
	authority.validated.Store(true)
	return nil
}
