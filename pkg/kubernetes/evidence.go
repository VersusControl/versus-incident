package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
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
	if stage != evidenceIdentityProof || identityProofLog.allow(class) {
		log.Printf("kubernetes: historical evidence failed stage=%s class=%s cause_type=%T", stage, class, cause)
	}
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

// listedBefore reports whether the previous generation holds a listing of kind
// that a new listing can be compared with. History records only what was seen
// to change. The first generation a cluster commits has no earlier listing of
// any kind, and a kind the cluster starts serving later has none of its own, so
// the objects such a listing returns already existed: they are its baseline,
// not creations. A restarted process compares with the generation it restored
// from storage, which keeps its listings.
func listedBefore(previous kubeindex.Snapshot, kind string) bool {
	state := previous.Kinds[kind]
	return !state.ObservedAt.IsZero() && !state.Absent
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
		// The capture is published without a save, and that is a store outage.
		return storageStageFailure("journal", err)
	}
	oldest, latest, complete := issueObservationBounds(generation.View)
	if latest.IsZero() {
		return ErrIncompleteInventory
	}
	viewIdentity, _ := json.Marshal(generation.View)
	batchID := "capture:" + logIdentity(identity+"\x00"+string(viewIdentity)+"\x00"+generation.PodSampledAt.UTC().Format(time.RFC3339Nano))
	batch := storage.EvidenceBatch{ObservationHeartbeat: service.evidenceHeartbeat, ScopeBinding: service.historyScopeKey(), ID: batchID, Expected: checkpoint.ID, Generation: checkpoint.Generation, At: latest, Epoch: service.scope.CredentialID, Principal: service.historyPrincipal(), Authority: historyProofs(*generation)}
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
	// A capture over the save limits is saved without its oldest, least
	// needed records rather than lost whole. What "now" needs (issues, crash
	// status and coverage gaps) is produced first and always kept; change
	// records and crash terminations are then folded into a set bounded by
	// the save limits as they are produced, so neither preparing nor
	// publishing the capture holds more history than one save may. Kept
	// records stay in capture order: changes, crashes, then gaps.
	fit := newCaptureFit(batch.Issues)
	const crashOrder, gapOrder = 1 << 40, 2 << 40
	changeObservedAt := batch.At
	var terminations []storage.EvidenceRecord
	for i, crash := range generation.Crashes {
		raw, err := json.Marshal(crash)
		if err != nil {
			return err
		}
		observed := maxTime(latest, crash.ObservedAt)
		batch.At = maxTime(batch.At, observed)
		record := storage.EvidenceRecord{AuthorizationKind: "Pod", ID: "crash:" + crash.ID, Type: "crash", At: crash.At, ObservedAt: crash.ObservedAt, Namespace: crash.Namespace, Kind: crash.RootKind, UID: crash.PodUID, RootUID: crash.RootUID, PodUID: crash.PodUID, Epoch: batch.Epoch, Payload: raw}
		if crash.Type == "termination" {
			terminations = append(terminations, record)
			continue
		}
		fit.keep(crashOrder+i, record)
	}
	gaps := 0
	if !checkpoint.At.IsZero() && oldest.Sub(checkpoint.At) > 2*time.Minute {
		raw, _ := json.Marshal(kubechanges.Gap{From: checkpoint.At, To: oldest})
		fit.keep(gapOrder+gaps, storage.EvidenceRecord{ID: "gap:" + batch.ID, Type: "coverage_gap", GapFrom: checkpoint.At, GapTo: oldest, At: oldest, ObservedAt: batch.At, Epoch: batch.Epoch, Payload: raw})
		gaps++
	}
	for kind, interval := range batch.Coverage {
		prior := checkpoint.Coverage[kind]
		state := previous.View.Kinds[kind]
		// A kind with no earlier listing to compare with is a baseline: what
		// changed since its saved coverage was never seen, so the interval is
		// recorded as a gap rather than as a stretch with no changes.
		knownFailure := previous.EvidenceFailure != "" || state.State == "error" || state.Partial || !listedBefore(previous.View, kind)
		if prior.To.IsZero() || !interval.To.After(prior.To) || !knownFailure && interval.To.Sub(prior.To) <= 2*inventoryCadence(kind) {
			continue
		}
		raw, _ := json.Marshal(kubechanges.Gap{From: prior.To, To: interval.To})
		fit.keep(gapOrder+gaps, storage.EvidenceRecord{ID: "gap:" + batch.ID + ":" + kind, Type: "coverage_gap", GapFrom: prior.To, GapTo: interval.To, Kind: kind, At: interval.To, ObservedAt: batch.At, Epoch: batch.Epoch, Payload: raw})
		gaps++
	}
	byUID := map[string]kubeindex.Record{}
	for _, record := range previous.View.Records {
		byUID[record.UID] = record
	}
	for _, record := range generation.View.Records {
		byUID[record.UID] = record
	}
	changes := 0
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
		fit.offer(changes, storage.EvidenceRecord{AuthorizationKind: authorizationKind, Priority: priority, ID: "change:" + change.ID, Type: "change", At: change.At, ObservedAt: changeObservedAt, Namespace: change.Namespace, Name: change.Name, Kind: change.Kind, UID: change.UID, RootUID: change.RootUID, Rule: string(change.Type), Epoch: batch.Epoch, Payload: raw})
		changes++
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
		if state.State != "ready" || state.Partial || !listedBefore(previous.View, record.Kind) {
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
	for i, crash := range generation.Crashes {
		if crash.Type == "termination" {
			fit.offer(crashOrder+i, terminations[0])
			terminations = terminations[1:]
		}
	}
	var trim captureTrim
	batch.Records, trim = fit.records()
	if err = batch.Validate(); err != nil {
		return capturePreparationRefused(err)
	}
	if trim.droppedTotal() > 0 {
		logCaptureTrim(trim)
	}
	generation.EvidenceScope = scope
	generation.EvidenceBatch = &batch
	return nil
}

// capturePreparationRefused names a capture that could not be prepared for
// saving because the store would refuse it: over the store's limits or invalid
// in itself. A retry of the same capture cannot succeed, so history pages
// report it as rejected rather than as an outage.
func capturePreparationRefused(err error) error {
	if errors.Is(err, storage.ErrInvalidEvidence) && !errors.Is(err, storage.ErrEvidenceUnstorable) {
		err = fmt.Errorf("%w: %v", errInvalidCapture, err)
	}
	return storageStageFailure("journal", err)
}

func (service *Service) commitEvidence(ctx context.Context, generation inventoryGeneration) error {
	if generation.EvidenceBatch == nil || !strings.HasPrefix(generation.EvidenceBatch.ID, "capture:") {
		return nil
	}
	store := service.evidenceStore()
	if store == nil {
		return storageStageFailure("journal", storage.ErrUnsupported)
	}
	batch := *generation.EvidenceBatch
	batch.Records = append([]storage.EvidenceRecord(nil), batch.Records...)
	for i, record := range batch.Records {
		if !json.Valid(record.Payload) {
			return storageStageFailure("journal", errInvalidCapture)
		}
		// Publish semantic validity with the indexed type/source metadata.
		// A count must not classify a malformed payload as application data
		// and discover the mismatch only after page hydration.
		record = service.projectEvidence(record)
		if record.Type != "coverage_gap" {
			record.AuthorizationKind, _ = service.evidenceRecordAuthorizationKind(record)
		}
		batch.Records[i] = record
	}
	// The credential epoch fence is held for one store transaction at a time,
	// never for the whole save: a large capture is saved in several
	// transactions, and live reads that need the fence run between them.
	var fenced error
	batch.Fence = func(ctx context.Context, transaction func() error) error {
		fenced = service.commitStorage(ctx, "journal", transaction)
		return fenced
	}
	err := store.CommitEvidence(ctx, generation.EvidenceScope, batch)
	if errors.Is(err, storage.ErrEvidenceConflict) {
		checkpoint, readErr := store.EvidenceCheckpoint(ctx, generation.EvidenceScope)
		switch {
		case readErr != nil:
			err = readErr
		case checkpoint.Generation != batch.Generation:
			// Explicit journal deletion invalidates queued old evidence. The next
			// live capture starts normally; do not repopulate it by replaying a
			// stale batch.
			err = nil
		default:
			batch.Expected = checkpoint.ID
			if !batch.At.After(checkpoint.At) {
				// A concurrent authorized producer advanced coverage. Preserve this
				// older observation without changing current lifecycle or
				// inventing a recovery.
				batch.At, batch.HistoricalOnly = checkpoint.At, true
				batch.CoveredThrough, batch.Coverage = time.Time{}, nil
			}
			fenced = nil
			err = store.CommitEvidence(ctx, generation.EvidenceScope, batch)
		}
	}
	// An answer from inside the fence is already classified; anything the
	// store answered outside it (validation, a checkpoint read) is a storage
	// answer of this stage.
	if err != nil && (fenced == nil || !errors.Is(err, fenced)) {
		err = storageStageFailure("journal", err)
	}
	if err != nil && historySaveRejected(err) {
		service.recordHistorySaveRejection(generation.EvidenceBatch.ID)
	}
	if err == nil {
		err = service.rememberHistoryScope(generation.EvidenceScope)
		service.notifyEvidenceCommit()
	}
	return err
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
	if record.Type == "coverage_gap" {
		var gap kubechanges.Gap
		if json.Unmarshal(record.Payload, &gap) == nil {
			// A gap is already unavailable evidence, not an object requiring a
			// source grant. Preserve its actual interval through publication.
			return record
		}
		return unavailableEvidence(record)
	}
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
	return service.persistedHistoryQuery(ctx, query)
}

func (service *Service) evidenceTimeline(ctx context.Context, query TimelineQuery) (TimelinePage, error) {
	kind := "timeline"
	if query.BreaksOnly {
		kind = "break"
	}
	// Gaps are kept out of the keyset, so Total and the pages it implies count
	// exactly the rows the walk shows; the page's gaps come from their own read.
	source, err := service.queryEvidence(ctx, storage.EvidenceQuery{Since: query.Since, Until: query.Until, Namespace: query.Namespace, Kind: query.Kind, RootUID: query.RootUID, PodUID: query.PodUID, ResourceUID: query.ResourceUID, Type: kind, GapsApart: true, Limit: normalizePageLimit(query.Limit), Cursor: query.Cursor})
	if err != nil {
		return TimelinePage{}, err
	}
	page := TimelinePage{Authority: source.Authority, CountCapped: source.CountCapped, Items: []TimelineItem{}, Gaps: []kubechanges.Gap{}, Counts: map[string]int{"change": 0, "crash": 0, "break": 0}, Next: source.Next, Since: source.Since, Until: source.Until, BreaksOnly: query.BreaksOnly, Sync: SyncStatus{State: "ready"}}
	page.Limit = normalizePageLimit(query.Limit)
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
	if page.Gaps, err = appendEvidenceGaps(page.Gaps, source.Gaps); err != nil {
		return page, err
	}
	page.Counts = map[string]int{"change": source.Counts["change"], "crash": source.Counts["crash"], "break": source.Counts["issue_opened"] + source.Counts["issue_resolved"]}
	page.CrashCount, page.Total = page.Counts["crash"], page.Counts["change"]+page.Counts["crash"]+page.Counts["break"]
	if !source.Checkpoint.CoveredThrough.IsZero() {
		page.Sync.AgeSeconds = max(0, time.Since(source.Checkpoint.CoveredThrough).Seconds())
	}
	page.Gaps = mergeEvidenceGaps(page.Gaps, source.Since, source.Until)
	page.Capture = service.historyCapture(source.Checkpoint)
	return page, nil
}

func (service *Service) historicalIssues(ctx context.Context, options IssueOptions) (IssuePage, error) {
	page := IssuePage{Items: []Issue{}, Totals: map[string]int{}, Broken: BrokenTotals{BySeverity: map[string]int{}}, Sync: SyncStatus{State: "ready"}}
	variant := options.Severity
	if options.Broken {
		variant += ":broken"
	}
	root, namespace := options.RootUID, options.Namespace
	if options.OccurrenceID != "" {
		root, namespace = "", ""
	}
	source, err := service.queryEvidence(ctx, storage.EvidenceQuery{Lookup: options.Context == "persisted", AuthorityNamespace: options.Namespace, OccurrenceID: options.OccurrenceID, Center: options.Center, Variant: variant, Since: options.Since, Until: options.Until, Namespace: namespace, Kind: options.Kind, RootUID: root, Type: "occurrence", Limit: normalizePageLimit(options.Limit), Cursor: options.Cursor})
	if err != nil {
		return page, evidenceReadFailure(evidenceStoreQuery, err)
	}
	page.Authority, page.CountCapped = source.Authority, source.CountCapped
	page.Total, page.Limit = source.Total, normalizePageLimit(options.Limit)
	page.Capture = service.historyCapture(source.Checkpoint)
	for _, record := range source.Items {
		if record.Type == "coverage_gap" {
			if options.Context == "persisted" || options.OccurrenceID != "" || !options.Center.IsZero() {
				return IssuePage{}, errHistoryAuthority
			}
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
		if options.RootUID != "" && issue.Root.UID != options.RootUID || options.Namespace != "" && issue.Root.Namespace != options.Namespace {
			return IssuePage{}, errHistoryMismatch
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
	if !source.CountCapped && source.Total == 0 && options.Cursor != "" {
		page.Totals = map[string]int{}
		page.Broken = BrokenTotals{BySeverity: map[string]int{}}
	} else if !source.CountCapped || source.Total > 0 {
		totals := map[string]int{}
		broken := BrokenTotals{BySeverity: map[string]int{}}
		for key, count := range source.Counts {
			if severity, ok := strings.CutPrefix(key, "severity:"); ok {
				totals[severity] = count
			}
			if severity, ok := strings.CutPrefix(key, "broken:"); ok {
				broken.BySeverity[severity] = count
				broken.Total += count
			}
		}
		if len(totals) > 0 {
			page.Totals, page.Broken = totals, broken
		}
	}
	sort.SliceStable(page.Items, func(i, j int) bool { return page.Items[i].FirstSeen.After(page.Items[j].FirstSeen) })
	return page, nil
}

func (service *Service) persistedRootContext(ctx context.Context, options IssueOptions) (IssuePage, error) {
	options.Context = "persisted"
	page := IssuePage{Items: []Issue{}, Totals: map[string]int{}, Broken: BrokenTotals{BySeverity: map[string]int{}}, Sync: SyncStatus{State: "ready"}, SelectionStatus: "not_requested"}
	selecting := options.OccurrenceID != "" || !options.Center.IsZero()
	if selecting {
		page.SelectionStatus = "not_found"
	}
	// Point selectors are pushed into the occurrence index, not searched in
	// pages of journal revisions. Two rows suffice to prove center ambiguity.
	options.Since, options.Until, options.Limit = time.Unix(1, 0).UTC(), time.Now().UTC(), 2
	if !selecting {
		options.Limit = 1
	}
	{
		history, err := service.historicalIssues(ctx, options)
		if err != nil {
			return page, err
		}
		page.Sync.Partial = page.Sync.Partial || history.Sync.Partial
		page.Authority, page.Capture = history.Authority, history.Capture
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
		if history.Next == "" || options.OccurrenceID != "" || !selecting || page.SelectionStatus == "ambiguous" {
			if selecting && page.Sync.Partial && page.SelectionStatus != "ambiguous" {
				page.SelectedOccurrence, page.SelectionStatus = nil, "partial"
			}
			return page, nil
		}
	}
	return page, nil
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
	// Total counts changes only: coverage gaps, and changes this reader may not
	// see, are kept out of the keyset and arrive as the page's gaps.
	source, err := service.queryEvidence(ctx, storage.EvidenceQuery{Since: query.Since, Until: query.Until, Namespace: query.Namespace, Kind: query.Kind, RootUID: query.RootUID, Name: query.Name, Rule: string(query.Type), Type: "change", GapsApart: true, Limit: normalizePageLimit(query.Limit), Cursor: query.Cursor})
	page := ChangePage{Authority: source.Authority, CountCapped: source.CountCapped, Items: []Change{}, Gaps: []kubechanges.Gap{}, Next: source.Next, Sync: SyncStatus{State: "ready"}}
	page.Total, page.Limit = source.Total, normalizePageLimit(query.Limit)
	if err != nil {
		return page, evidenceReadFailure(evidenceStoreQuery, err)
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
		page.resume = append(page.resume, source.ResumeAfter(record))
	}
	if page.Gaps, err = appendEvidenceGaps(page.Gaps, source.Gaps); err != nil {
		return page, err
	}
	if !source.Checkpoint.PrunedThrough.IsZero() && query.Since.Before(source.Checkpoint.PrunedThrough) {
		page.Gaps = append(page.Gaps, kubechanges.Gap{From: query.Since, To: source.Checkpoint.PrunedThrough})
	}
	page.Gaps = mergeEvidenceGaps(page.Gaps, source.Since, source.Until)
	page.Capture = service.historyCapture(source.Checkpoint)
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
	source, err := service.queryEvidence(ctx, storage.EvidenceQuery{Since: query.Since, Until: query.Until, Namespace: query.Namespace, RootUID: query.RootUID, PodUID: query.PodUID, ByObservation: true, Type: "crash", GapsApart: true, Limit: normalizePageLimit(query.Limit), Cursor: query.Cursor})
	page := CrashPage{Authority: source.Authority, CountCapped: source.CountCapped, CrashPage: kubechanges.CrashPage{Items: []kubechanges.CrashEvent{}, Gaps: []kubechanges.Gap{}, CaptureGaps: []kubechanges.Gap{}, Coverage: map[string]kubechanges.Gap{}, Since: query.Since, Until: query.Until, Next: source.Next, RetentionBasis: source.Policy.Mode}, Sync: SyncStatus{State: "ready"}, Capture: service.CaptureStatus()}
	page.Total, page.Limit = source.Total, normalizePageLimit(query.Limit)
	if err != nil {
		return page, evidenceReadFailure(evidenceStoreQuery, err)
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
	if page.CaptureGaps, err = appendEvidenceGaps(page.CaptureGaps, source.Gaps); err != nil {
		return page, err
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
	page.Capture = *service.historyCapture(source.Checkpoint)
	return page, nil
}

func (service *Service) evidenceChangeWindow(ctx context.Context, since, until time.Time, namespace, kind string) (kubechanges.Page, error) {
	// Gaps are read apart from changes, so only changes fill the 500-row cap.
	source, err := service.queryEvidence(ctx, storage.EvidenceQuery{Since: since, Until: until, Namespace: namespace, Kind: kind, Type: "change", GapsApart: true, Limit: 500})
	page := kubechanges.Page{Items: []Change{}, Next: source.Next, Coverage: map[string]kubechanges.Gap{}}
	if err != nil {
		return page, evidenceReadFailure(evidenceStoreQuery, err)
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
	if page.Gaps, err = appendEvidenceGaps(page.Gaps, source.Gaps); err != nil {
		return page, err
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

// appendEvidenceGaps decodes the gap records a page read apart from its items.
func appendEvidenceGaps(gaps []kubechanges.Gap, records []storage.EvidenceRecord) ([]kubechanges.Gap, error) {
	for _, record := range records {
		var gap kubechanges.Gap
		if err := json.Unmarshal(record.Payload, &gap); err != nil {
			return gaps, evidenceReadFailure(evidencePayloadDecode, err)
		}
		gaps = append(gaps, gap)
	}
	return gaps, nil
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

// identityProofLog keeps an unreachable cluster from logging one identity
// failure per read: each class is logged at most once per interval.
var identityProofLog = &intervalLog{interval: 10 * time.Second, last: map[string]time.Time{}}

type intervalLog struct {
	mu       sync.Mutex
	interval time.Duration
	last     map[string]time.Time
}

func (limit *intervalLog) allow(key string) bool {
	limit.mu.Lock()
	defer limit.mu.Unlock()
	now := time.Now()
	if at, seen := limit.last[key]; seen && now.Sub(at) < limit.interval {
		return false
	}
	limit.last[key] = now
	return true
}

// restoreRetryWindow is how long an unanswered restore proof stands for every
// caller before the cluster is asked again, so reads during an outage fail at
// once instead of each sending its own proof.
const restoreRetryWindow = time.Second

// restoreProvedLimit bounds how many proved stored generations one credential
// remembers.
const restoreProvedLimit = 8

// inventoryRestoreAuthority is one credential epoch's proof of stored
// inventory. A proof covers one stored generation: every kind it holds, both
// cluster-wide and in each namespace it was stored for. Stored data is served
// only for a generation this epoch proved, so neither a proof of another
// generation nor a live publication can vouch for it.
//
// The worker's proof has priority: it waits for the gate as long as its own
// budget allows and cancels a reader's proof that holds it. A reader proves
// only when the gate is free and no worker waits, so readers can never keep the
// worker's proof from running.
type inventoryRestoreAuthority struct {
	once         sync.Once
	gate         chan struct{}
	mu           sync.Mutex
	proved       []string
	unanswered   error
	unansweredAt time.Time
	// resets counts reset calls, so a proof that started before a reset cannot
	// record its result after it.
	resets uint64
	// workers counts worker proofs waiting for the gate; readerCancel stops the
	// reader proof holding it.
	workers      int
	readerCancel context.CancelFunc
	// outcome is how the latest proof of a stored generation ended, and
	// settled is closed whenever a proof ends, so readers of a scope with a
	// worker wait for its proof instead of running their own.
	outcome struct {
		revision string
		err      error
	}
	settled chan struct{}
}

// notifyLocked wakes every reader waiting for a proof to end.
func (authority *inventoryRestoreAuthority) notifyLocked() {
	if authority.settled != nil {
		close(authority.settled)
	}
	authority.settled = make(chan struct{})
}

// epoch returns the reset count a proof starting now records against.
func (authority *inventoryRestoreAuthority) epoch() uint64 {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return authority.resets
}

// covers reports whether this epoch proved the stored generation revision.
func (authority *inventoryRestoreAuthority) covers(revision string) bool {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return revision != "" && slices.Contains(authority.proved, revision)
}

// prove records a generation this epoch collected itself.
func (authority *inventoryRestoreAuthority) prove(revision string) {
	if authority == nil {
		return
	}
	authority.proveSince(authority.epoch(), revision)
}

// proveSince records a generation proved by a proof that started at epoch; a
// reset since then discards it. It reports whether the proof stands: false
// means a reset discarded it, and its caller must not be served as if it had
// proved anything.
func (authority *inventoryRestoreAuthority) proveSince(epoch uint64, revision string) bool {
	if authority == nil {
		return false
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.resets != epoch {
		return false
	}
	if revision == "" {
		return true
	}
	defer authority.notifyLocked()
	authority.unanswered = nil
	if slices.Contains(authority.proved, revision) {
		return true
	}
	authority.proved = append(authority.proved, revision)
	if len(authority.proved) > restoreProvedLimit {
		authority.proved = append([]string(nil), authority.proved[len(authority.proved)-restoreProvedLimit:]...)
	}
	return true
}

// reset forgets every proof, for a denial or a different store.
func (authority *inventoryRestoreAuthority) reset() {
	if authority == nil {
		return
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.proved, authority.unanswered = nil, nil
	authority.outcome.revision, authority.outcome.err = "", nil
	authority.resets++
	authority.notifyLocked()
}

// recentUnanswered returns the last unanswered proof while it still stands.
func (authority *inventoryRestoreAuthority) recentUnanswered() error {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.unanswered != nil && time.Since(authority.unansweredAt) < restoreRetryWindow {
		return authority.unanswered
	}
	return nil
}

// settle records how a proof of revision ended: an unanswered proof stands for
// the retry window, an answered one clears it. Waiting readers wake either way.
func (authority *inventoryRestoreAuthority) settle(revision string, err error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	defer authority.notifyLocked()
	authority.outcome.revision, authority.outcome.err = revision, err
	var unanswered restoreProofUnanswered
	if errors.As(err, &unanswered) {
		authority.unanswered, authority.unansweredAt = unanswered, time.Now()
		return
	}
	authority.unanswered = nil
}

// errWorkerPublished tells a waiting reader that the worker now holds its own
// generation, which answers the read instead of the stored one.
var errWorkerPublished = errors.New("kubernetes: worker published its own generation")

// awaitWorkerProof serves a reader of a scope whose worker restores it. It
// never sends a request: it answers with the proof the worker already made of
// this revision, or waits, within the reader's own limit, for the worker's
// proof to end or for the worker to publish its own generation.
func (authority *inventoryRestoreAuthority) awaitWorkerProof(ctx context.Context, revision string, published func() bool) error {
	for {
		if published() {
			return errWorkerPublished
		}
		authority.mu.Lock()
		if authority.settled == nil {
			authority.settled = make(chan struct{})
		}
		covered := revision != "" && slices.Contains(authority.proved, revision)
		outcome, settled := authority.outcome, authority.settled
		authority.mu.Unlock()
		if covered {
			return nil
		}
		if outcome.revision == revision && outcome.err != nil {
			return outcome.err
		}
		select {
		case <-settled:
		case <-ctx.Done():
			return restoreProofUnanswered{ErrInventoryNotReady}
		}
	}
}

// restoreProofUnanswered is a restore proof the cluster as a whole could not
// answer: the identity read failed, timed out or was throttled. It says nothing
// about the stored generation, which stays the cluster's last good inventory;
// the error reads exactly like its cause.
type restoreProofUnanswered struct{ error }

func (err restoreProofUnanswered) Unwrap() error { return err.error }

// identityProofFailure classifies a failed identity read. When the cluster
// could not answer it, the cluster is unreachable as a whole: collecting it
// again would publish failures over the stored generation, and after a restart
// that loses the last good inventory and the listing history compares with. Any
// other failure, and every failure after the identity was answered, makes the
// stored generation unusable, so the worker collects the cluster again and a
// kind that keeps failing is reported with that kind's own error.
func identityProofFailure(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, ErrClusterUnavailable) || errors.Is(err, ErrCredentialUnavailable) || errors.Is(err, ErrInvalidArguments) || errors.Is(err, ErrInventoryNotReady) || countsAgainstCredential(err) {
		return inventoryPayloadError{err}
	}
	// The identity read is the proof's first request, so an exhausted operation
	// budget here means its time ran out before the cluster answered.
	switch errorClass(err) {
	case "unavailable", "timeout", "throttled", "budget_exhausted":
		return restoreProofUnanswered{err}
	}
	return inventoryPayloadError{err}
}

// Restoring a durable payload cannot turn a locally readable but revoked static
// credential into permission to replay its old protected inventory. A live
// capture establishes its own authority; cold restores prove it independently.
func (service *Service) authorizeInventoryRestore(ctx context.Context, generation inventoryGeneration) error {
	return service.authorizeRestore(ctx, generation, restoreByWorker)
}

// restoreCaller says who asks for a stored generation and so how it may prove
// it.
type restoreCaller int

const (
	// restoreByWorker proves with priority over readers.
	restoreByWorker restoreCaller = iota
	// restoreByReader proves only when the gate is free and no worker waits,
	// within the reader's own limit.
	restoreByReader
	// restoreByCoveredOnly never proves: a scope whose worker runs is restored
	// by that worker, and its readers are served only what it proved.
	restoreByCoveredOnly
)

func (service *Service) authorizeRestore(ctx context.Context, generation inventoryGeneration, caller restoreCaller) error {
	if len(generation.View.Kinds) == 0 || service.evidenceStore() == nil {
		return nil
	}
	authority := service.inventoryRestore
	if authority == nil {
		return ErrClusterUnavailable
	}
	if caller == restoreByCoveredOnly {
		return authority.awaitWorkerProof(ctx, generation.Revision, func() bool { return false })
	}
	if authority.covers(generation.Revision) {
		return nil
	}
	// The worker's backoff already spaces its proofs; the window holds back
	// readers only.
	if caller == restoreByReader {
		if unanswered := authority.recentUnanswered(); unanswered != nil {
			return unanswered
		}
	}
	notProved := restoreProofUnanswered{ErrInventoryNotReady}
	authority.once.Do(func() { authority.gate = make(chan struct{}, 1); authority.gate <- struct{}{} })
	proofCtx := ctx
	if caller == restoreByWorker {
		authority.mu.Lock()
		authority.workers++
		if authority.readerCancel != nil {
			authority.readerCancel()
		}
		authority.mu.Unlock()
		select {
		case <-ctx.Done():
			authority.mu.Lock()
			authority.workers--
			authority.mu.Unlock()
			return ctx.Err()
		case <-authority.gate:
		}
		authority.mu.Lock()
		authority.workers--
		authority.mu.Unlock()
	} else {
		select {
		case <-authority.gate:
		default:
			return notProved
		}
		authority.mu.Lock()
		if authority.workers > 0 {
			authority.mu.Unlock()
			authority.gate <- struct{}{}
			return notProved
		}
		var cancel context.CancelFunc
		proofCtx, cancel = context.WithCancel(ctx)
		authority.readerCancel = cancel
		authority.mu.Unlock()
		defer func() {
			authority.mu.Lock()
			authority.readerCancel = nil
			authority.mu.Unlock()
			cancel()
		}()
	}
	defer func() { authority.gate <- struct{}{} }()
	if authority.covers(generation.Revision) {
		return nil
	}
	if caller == restoreByReader {
		if unanswered := authority.recentUnanswered(); unanswered != nil {
			return unanswered
		}
	}
	epoch := authority.epoch()
	err := service.proveStoredGeneration(proofCtx, generation)
	if err != nil && caller == restoreByReader && proofCtx.Err() != nil {
		if ctx.Err() == nil {
			// A worker took over the gate; its proof decides.
			return notProved
		}
		// The reader's own limit ran out before the cluster answered: that is
		// an unanswered proof, which also holds back further reader proofs.
		err = restoreProofUnanswered{err}
	}
	authority.settle(generation.Revision, err)
	if err == nil && !authority.proveSince(epoch, generation.Revision) {
		// A reset during the proof discarded it: the credential or store it
		// proved may no longer hold, so the caller fails closed as if unproved.
		return notProved
	}
	return err
}

// proveStoredGeneration proves that this credential may read everything one
// stored generation holds: the physical cluster identity, every kind it stored
// cluster-wide, and every kind it stored only for a namespace, in that
// namespace.
func (service *Service) proveStoredGeneration(ctx context.Context, generation inventoryGeneration) error {
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
			if read.stage == evidenceIdentityProof {
				return identityProofFailure(read.cause)
			}
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
	held := map[string]bool{}
	for _, record := range generation.View.Records {
		held[record.Kind] = true
	}
	checked := map[string]bool{}
	for kind, state := range generation.View.Kinds {
		// A kind denied cluster-wide holds no cluster-wide records to prove; one
		// that still holds some is proved like any other.
		if state.Absent || (state.Error == "forbidden" || state.Error == "unauthorized") && !held[kind] {
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
	// Records stored only for a namespace were listed with a namespace grant,
	// which a cluster-wide proof of another kind says nothing about. A kind
	// proved cluster-wide above covers every namespace.
	namespaces := make([]string, 0, len(generation.Namespaces))
	for namespace := range generation.Namespaces {
		namespaces = append(namespaces, namespace)
	}
	sort.Strings(namespaces)
	for _, namespace := range namespaces {
		kinds := map[string]bool{}
		for _, record := range generation.Namespaces[namespace].Records {
			kinds[record.Kind] = true
		}
		for kind := range kinds {
			id := indexedResourceIDs[kind]
			if id == "" {
				return inventoryPayloadError{ErrInvalidArguments}
			}
			if checked[id] {
				continue
			}
			if err = service.authorizeEvidenceKind(proofCtx, namespace, id); err != nil {
				return inventoryPayloadError{err}
			}
		}
	}
	return nil
}
