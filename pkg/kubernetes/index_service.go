package kubernetes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

const kubernetesIndexRefreshInterval = 15 * time.Second
const kubernetesIndexRefreshTimeout = 2 * time.Minute
const kubernetesIndexReadinessWait = 2 * time.Second
const kubernetesIndexPageSize = 500
const kubernetesIndexMaxPages = 100
const kubernetesIndexMaxRecords = 50000

const MaxOrgIndexStreams = 8
const MaxProcessIndexStreams = 64

var ErrIndexStreamBusy = errors.New("kubernetes: index stream concurrency limit reached")

var indexStreamAdmissions = struct {
	sync.Mutex
	total int
	orgs  map[string]int
}{orgs: make(map[string]int)}

func acquireIndexStream(orgID string) (func(), error) {
	indexStreamAdmissions.Lock()
	defer indexStreamAdmissions.Unlock()
	if indexStreamAdmissions.total >= MaxProcessIndexStreams || indexStreamAdmissions.orgs[orgID] >= MaxOrgIndexStreams {
		return nil, ErrIndexStreamBusy
	}
	indexStreamAdmissions.total++
	indexStreamAdmissions.orgs[orgID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			indexStreamAdmissions.Lock()
			defer indexStreamAdmissions.Unlock()
			indexStreamAdmissions.total--
			indexStreamAdmissions.orgs[orgID]--
			if indexStreamAdmissions.orgs[orgID] == 0 {
				delete(indexStreamAdmissions.orgs, orgID)
			}
		})
	}, nil
}

var metadataOnlyIndexKinds = map[string]bool{"Secret": true, "ConfigMap": true, "ServiceAccount": true}
var streamIndexKinds = map[string]bool{
	"Node": true, "Namespace": true, "Pod": true, "Deployment": true, "ReplicaSet": true,
	"StatefulSet": true, "DaemonSet": true, "Job": true, "CronJob": true, "Service": true, "Ingress": true,
}

var indexedResourceIDs = map[string]string{
	"Pod":                     "core~v1~pods",
	"Node":                    "core~v1~nodes",
	"Namespace":               "core~v1~namespaces",
	"Event":                   "core~v1~events",
	"Service":                 "core~v1~services",
	"Deployment":              "apps~v1~deployments",
	"StatefulSet":             "apps~v1~statefulsets",
	"DaemonSet":               "apps~v1~daemonsets",
	"ReplicaSet":              "apps~v1~replicasets",
	"ControllerRevision":      "apps~v1~controllerrevisions",
	"Job":                     "batch~v1~jobs",
	"CronJob":                 "batch~v1~cronjobs",
	"Ingress":                 "networking.k8s.io~v1~ingresses",
	"HorizontalPodAutoscaler": "autoscaling~v2~horizontalpodautoscalers",
	"PersistentVolumeClaim":   "core~v1~persistentvolumeclaims",
	"ConfigMap":               "core~v1~configmaps",
	"Secret":                  "core~v1~secrets",
	"ServiceAccount":          "core~v1~serviceaccounts",
}

// IndexSnapshot polls requested kinds within the normal operation budget and
// returns the shared last-good view plus its per-kind freshness state.
func (service *Service) IndexSnapshot(ctx context.Context, kinds ...string) (kubeindex.Snapshot, kubeindex.Status, error) {
	if err := service.validateCredential(ctx); err != nil {
		return kubeindex.Snapshot{}, kubeindex.Status{}, err
	}
	if service == nil || service.indexes == nil {
		return kubeindex.Snapshot{}, kubeindex.Status{State: "unavailable", Partial: true, Kinds: map[string]kubeindex.KindStatus{}}, ErrInvalidArguments
	}
	if service.client == nil {
		return kubeindex.Snapshot{}, kubeindex.Status{State: "unavailable", Partial: true, Kinds: map[string]kubeindex.KindStatus{}}, ErrInvalidArguments
	}
	if len(kinds) == 0 {
		return kubeindex.Snapshot{}, kubeindex.Status{}, ErrInvalidArguments
	}
	for _, kind := range kinds {
		if _, ok := indexedResourceIDs[kind]; !ok {
			return kubeindex.Snapshot{}, kubeindex.Status{}, ErrInvalidArguments
		}
	}
	requestContext, cancelRequest := ensureOperationBudgetRequests(ctx, overviewOperationRequests)
	defer cancelRequest()
	scope := kubeindex.Scope{OrgID: service.scope.OrgID, ClusterID: service.scope.ClusterID, CredentialID: service.scope.CredentialID}
	index, err := service.indexes.ForScope(scope)
	if err != nil {
		return kubeindex.Snapshot{}, kubeindex.Status{}, err
	}
	requested := make(map[string]struct{}, len(kinds))
	for _, kind := range kinds {
		requested[kind] = struct{}{}
	}
	uniqueKinds := make([]string, 0, len(requested))
	for kind := range requested {
		uniqueKinds = append(uniqueKinds, kind)
	}
	sort.Strings(uniqueKinds)
	changeStore := service.changeStore()
	_, background := ctx.Value(captureReadContextKey{}).(string)
	key := indexRefreshKey{scope: scope, background: background}
	call := &indexRefreshCall{done: make(chan struct{}), requested: make(map[string]struct{}, len(uniqueKinds))}
	startRefresh := true
	if service.changes != nil {
		service.changes.mu.Lock()
		if service.changes.stopped[scope] {
			service.changes.mu.Unlock()
			return kubeindex.Snapshot{}, kubeindex.Status{}, context.Canceled
		}
		if existing := service.changes.refreshes[key]; existing != nil {
			call = existing
			startRefresh = false
		}
		for _, kind := range uniqueKinds {
			call.requested[kind] = struct{}{}
		}
		if startRefresh {
			parent := service.changes.lifecycle
			if parent == nil {
				parent = context.Background()
			}
			if loop := service.changes.captures[scope]; loop != nil && background {
				parent = loop.ctx
			}
			call.ctx, call.cancel = context.WithCancel(parent)
			if service.identity != nil {
				cancel := call.cancel
				stopIdentity := context.AfterFunc(service.identity.ctx, cancel)
				call.cancel = func() { stopIdentity(); cancel() }
			}
			service.changes.refreshes[key] = call
		}
		service.changes.mu.Unlock()
	}
	if startRefresh {
		go service.runIndexRefresh(ctx, key, index, changeStore, call)
	}
	wait := kubernetesIndexReadinessWait
	if requested, ok := ctx.Value(indexReadinessContextKey{}).(time.Duration); ok && requested > wait && requested <= 30*time.Second {
		wait = requested
	}
	waitTimer := time.NewTimer(wait)
	defer waitTimer.Stop()
	select {
	case <-call.done:
	case <-requestContext.Done():
		return kubeindex.Snapshot{}, scopedIndexStatus(index.Status(time.Now().UTC()), kinds), requestContext.Err()
	case <-waitTimer.C:
	}
	snapshot := index.Snapshot()
	if err := service.validateCredential(ctx); err != nil {
		return kubeindex.Snapshot{}, kubeindex.Status{}, err
	}
	status := scopedIndexStatus(kubeindex.Status{Kinds: snapshot.Kinds}, kinds)
	status.Capture = captureIndexStatus(service.CaptureStatus())
	if err := requestContext.Err(); err != nil {
		return snapshot, status, err
	}
	return snapshot, status, nil
}

func (service *Service) runIndexRefresh(requestContext context.Context, key indexRefreshKey, index *kubeindex.Index, changeStore *kubechanges.Store, call *indexRefreshCall) {
	defer call.cancel()
	for {
		var kinds []string
		if service.changes != nil {
			service.changes.mu.Lock()
			for kind := range call.requested {
				kinds = append(kinds, kind)
			}
			clear(call.requested)
			if len(kinds) == 0 {
				if len(call.requested) == 0 {
					if service.changes.refreshes[key] == call {
						delete(service.changes.refreshes, key)
					}
					close(call.done)
					service.changes.mu.Unlock()
					return
				}
				service.changes.mu.Unlock()
				continue
			}
			service.changes.mu.Unlock()
		} else {
			for kind := range call.requested {
				kinds = append(kinds, kind)
			}
			clear(call.requested)
			if len(kinds) == 0 {
				close(call.done)
				return
			}
		}
		sort.Strings(kinds)
		parent := call.ctx
		owner, background := requestContext.Value(captureReadContextKey{}).(string)
		if background {
			parent = captureReadContext(parent, owner)
		}
		refreshContext, cancelRefresh := context.WithTimeout(parent, kubernetesIndexRefreshTimeout)
		refreshContext = context.WithValue(refreshContext, credentialEpochContextKey{}, service)
		cancelBudget := func() {}
		if !background {
			refreshContext, cancelBudget = ensureOperationBudgetRequests(refreshContext, overviewOperationRequests)
		}
		stopLifetime := func() bool { return false }
		stopOwnership := func() bool { return false }
		refreshOwner := ""
		if background {
			refreshOwner = owner
			stopOwnership = context.AfterFunc(requestContext, cancelRefresh)
		}
		if loop := service.captureState(); loop != nil {
			if background {
				stopLifetime = context.AfterFunc(loop.ctx, cancelRefresh)
			}
			loop.mu.Lock()
			if loop.owned && !background {
				refreshOwner = loop.owner
			}
			loop.mu.Unlock()
		}
		started := time.Now().UTC()
		crashes := &crashCaptureBatch{}
		refreshContext = context.WithValue(refreshContext, crashCaptureContextKey{}, crashes)
		var deltas <-chan kubeindex.Delta
		unsubscribe := func() {}
		var captureLock *sync.Mutex
		if changeStore != nil && service.changes != nil {
			captureLock = service.changes.captureLock(key.scope)
			deltas, unsubscribe = index.Subscribe(1024)
		}
		semaphore := make(chan struct{}, 4)
		var workers sync.WaitGroup
		for _, kind := range kinds {
			kind := kind
			workers.Add(1)
			go func() {
				defer workers.Done()
				select {
				case semaphore <- struct{}{}:
				case <-refreshContext.Done():
					return
				}
				defer func() { <-semaphore }()
				if loop := service.captureState(); loop != nil {
					loop.mu.Lock()
					fresh := loop.owned && !loop.completed[kind].IsZero() && time.Since(loop.completed[kind]) < kubernetesIndexRefreshInterval
					loop.mu.Unlock()
					if fresh {
						return
					}
				}
				refreshErr := index.RefreshGuarded(refreshContext, kind, kubernetesIndexRefreshInterval, time.Now().UTC(), func(loadContext context.Context) ([]kubeindex.Record, bool, error) {
					return service.loadIndexKind(loadContext, kind)
				}, func(commit func() error) error {
					return service.commitCredential(refreshContext, commit)
				})
				var throttled *ErrThrottled
				if errors.As(refreshErr, &throttled) {
					if loop := service.captureState(); loop != nil {
						loop.mu.Lock()
						loop.backoff = maxTime(loop.backoff, time.Now().UTC().Add(throttled.RetryAfter))
						loop.status.Behind = true
						loop.mu.Unlock()
					}
				}
			}()
		}
		workers.Wait()
		if captureLock != nil {
			captureLock.Lock()
		}
		if changeStore != nil && refreshContext.Err() == nil && service.validateCredential(refreshContext) == nil {
			service.changes.mu.Lock()
			crashes.omitted = crashes.omitted || service.changes.journalFailed[key.scope]
			service.changes.mu.Unlock()
			commitErr := service.commitCredential(refreshContext, func() error {
				return service.captureCycleWithCrashes(changeStore, deltas, index.Snapshot(), kinds, started, crashes, refreshOwner)
			})
			if !errors.Is(commitErr, kubechanges.ErrLeaseLost) {
				service.changes.mu.Lock()
				if commitErr != nil {
					service.changes.journalFailed[key.scope] = true
				} else {
					delete(service.changes.journalFailed, key.scope)
				}
				service.changes.mu.Unlock()
			}
			if commitErr != nil {
				if loop := service.captureState(); loop != nil {
					loop.mu.Lock()
					loop.status.Behind = true
					loop.mu.Unlock()
				}
			}
		}
		unsubscribe()
		if captureLock != nil {
			captureLock.Unlock()
		}
		cancelBudget()
		cancelRefresh()
		stopLifetime()
		stopOwnership()
	}
}

func scopedIndexStatus(status kubeindex.Status, requested []string) kubeindex.Status {
	allKinds := status.Kinds
	status.Kinds = make(map[string]kubeindex.KindStatus, len(requested))
	status.State = "ready"
	status.Partial = false
	oldest := time.Time{}
	for _, kind := range requested {
		kindStatus, exists := allKinds[kind]
		if !exists {
			if status.State != "error" {
				status.State = "warming"
			}
			continue
		}
		status.Kinds[kind] = kindStatus
		if kindStatus.Partial || kindStatus.State != "ready" {
			status.Partial = true
			if kindStatus.State == "error" {
				status.State = "error"
			} else if status.State == "ready" {
				status.State = "partial"
			}
		}
		if !kindStatus.ObservedAt.IsZero() && (oldest.IsZero() || kindStatus.ObservedAt.Before(oldest)) {
			oldest = kindStatus.ObservedAt
		}
	}
	if !oldest.IsZero() {
		status.AgeSeconds = time.Since(oldest).Seconds()
		if status.AgeSeconds < 0 {
			status.AgeSeconds = 0
		}
	}
	return status
}

func (service *Service) loadIndexKind(ctx context.Context, kind string, namespaces ...string) (recordsResult []kubeindex.Record, completeResult bool, resultErr error) {
	if kind == "Pod" {
		if batch, ok := ctx.Value(crashCaptureContextKey{}).(*crashCaptureBatch); ok {
			batch.attempted = true
		}
	}
	if err := service.validateCredential(ctx); err != nil {
		return nil, false, err
	}
	ctx, cancelIdentity := service.credentialContext(ctx)
	defer cancelIdentity()
	metadataScope := kind
	if len(namespaces) > 0 {
		metadataScope += "\x00" + namespaces[0]
	}
	defer func() {
		if resultErr != nil {
			resultErr = indexReadError{error: resultErr, class: errorClass(resultErr)}
		}
	}()
	if metadataOnlyIndexKinds[kind] {
		service.client.metadataMu.Lock()
		denied := service.client.metadataDenied[metadataScope]
		service.client.metadataMu.Unlock()
		if time.Now().Before(denied) {
			return nil, false, ErrForbidden
		}
	}
	ctx, cancel := withOperationBudget(ctx, kubernetesIndexRefreshTimeout, kubernetesIndexMaxPages+32, 64<<20, kubernetesIndexMaxRecords)
	defer cancel()
	definition, err := service.resolve(ctx, indexedResourceIDs[kind])
	if err != nil {
		return nil, false, err
	}
	namespace := ""
	if len(namespaces) > 0 && definition.Namespaced {
		namespace = namespaces[0]
		if !safeSegment(namespace) {
			return nil, false, ErrInvalidArguments
		}
	}
	apiPath, err := resourcePath(definition, namespace, "")
	if err != nil {
		return nil, false, err
	}
	records := make([]kubeindex.Record, 0)
	seenTokens := make(map[string]bool)
	continueToken := ""
	pageLimit := kubernetesIndexPageSize
	var revisionValidator *kubeindex.Index
	if kind == "ControllerRevision" {
		revisionValidator = kubeindex.New(1)
	}
	for pageNumber := 0; pageNumber < kubernetesIndexMaxPages; pageNumber++ {
		query := url.Values{"limit": {strconv.Itoa(pageLimit)}}
		if kind == "Event" {
			query.Set("fieldSelector", "type=Warning")
		}
		if continueToken != "" {
			query.Set("continue", continueToken)
		}
		pagePath := apiPath + "?" + query.Encode()
		if kind == "ControllerRevision" {
			var page struct {
				Items []struct {
					Metadata struct {
						Name      string    `json:"name"`
						Namespace string    `json:"namespace"`
						UID       string    `json:"uid"`
						CreatedAt time.Time `json:"creationTimestamp"`
						Owners    []struct {
							Kind       string `json:"kind"`
							Name       string `json:"name"`
							UID        string `json:"uid"`
							Controller *bool  `json:"controller"`
						} `json:"ownerReferences"`
					} `json:"metadata"`
					Revision int64           `json:"revision"`
					Data     json.RawMessage `json:"data"`
				} `json:"items"`
				Metadata struct {
					Continue string `json:"continue"`
				} `json:"metadata"`
			}
			if err := service.client.GetJSON(ctx, pagePath, &page); err != nil {
				if errors.Is(err, ErrResponseTooLarge) && pageLimit > minimumPageSize {
					pageLimit = max(minimumPageSize, pageLimit/2)
					pageNumber--
					continue
				}
				return nil, false, err
			}
			for _, item := range page.Items {
				record := kubeindex.Record{Kind: kind, UID: item.Metadata.UID, Name: item.Metadata.Name, Namespace: item.Metadata.Namespace, CreatedAt: item.Metadata.CreatedAt, Revision: strconv.FormatInt(item.Revision, 10)}
				for _, owner := range item.Metadata.Owners {
					if (owner.Kind == "StatefulSet" || owner.Kind == "DaemonSet") && (owner.Controller == nil || *owner.Controller) {
						record.Owners = append(record.Owners, kubeindex.OwnerRef{Kind: owner.Kind, Name: owner.Name, Namespace: record.Namespace, UID: owner.UID, ControllerKnown: owner.Controller != nil, Controller: owner.Controller != nil && *owner.Controller})
						break
					}
				}
				if len(record.Owners) == 0 {
					continue
				}
				images, err := kubechanges.DecodeRevision(bytes.NewReader(item.Data))
				if err != nil {
					record.Summary = map[string]string{"revision_history_partial": "true"}
				} else {
					record.Images = images
					if err := revisionValidator.Ingest(kind, []kubeindex.Record{record}, true, time.Now().UTC()); err != nil {
						record.Images = nil
						record.Summary = map[string]string{"revision_history_partial": "true"}
					}
				}
				records = append(records, record)
			}
			continueToken = page.Metadata.Continue
		} else if metadataOnlyIndexKinds[kind] {
			var page metadataIndexPage
			if err := service.client.GetMetadataJSON(ctx, pagePath, &page); err != nil {
				if errors.Is(err, ErrForbidden) && kind == "Secret" {
					service.client.metadataMu.Lock()
					service.client.metadataDenied[metadataScope] = time.Now().Add(time.Hour)
					service.client.metadataMu.Unlock()
				}
				if errors.Is(err, ErrResponseTooLarge) && pageLimit > minimumPageSize {
					pageLimit = max(minimumPageSize, pageLimit/2)
					pageNumber--
					continue
				}
				return nil, false, err
			}
			for _, item := range page.Items {
				records = append(records, item.record(kind))
			}
			service.client.metadataMu.Lock()
			delete(service.client.metadataDenied, metadataScope)
			service.client.metadataMu.Unlock()
			continueToken = page.Metadata.Continue
		} else {
			var page struct {
				Items    []map[string]any `json:"items"`
				Metadata struct {
					Continue string `json:"continue"`
				} `json:"metadata"`
			}
			if err := service.client.GetJSON(ctx, pagePath, &page); err != nil {
				if errors.Is(err, ErrResponseTooLarge) && pageLimit > minimumPageSize {
					pageLimit = max(minimumPageSize, pageLimit/2)
					pageNumber--
					continue
				}
				return nil, false, err
			}
			if kind == "Pod" {
				if batch, ok := ctx.Value(crashCaptureContextKey{}).(*crashCaptureBatch); ok {
					batch.read = true
				}
			}
			for _, item := range page.Items {
				record := indexRecord(service.projectResource(definition, item))
				if kind == "Pod" {
					if batch, ok := ctx.Value(crashCaptureContextKey{}).(*crashCaptureBatch); ok {
						status, _ := item["status"].(map[string]any)
						batch.add(service.scope.ClusterID, record, status, time.Now().UTC())
					}
				}
				records = append(records, record)
			}
			continueToken = page.Metadata.Continue
		}
		if len(records) >= kubernetesIndexMaxRecords {
			if continueToken != "" {
				return records[:kubernetesIndexMaxRecords], false, nil
			}
			if len(records) > kubernetesIndexMaxRecords {
				return records[:kubernetesIndexMaxRecords], false, nil
			}
		}
		if continueToken == "" {
			return records, true, nil
		}
		if len(continueToken) > 4096 || seenTokens[continueToken] {
			return records, false, nil
		}
		seenTokens[continueToken] = true
	}
	return records, false, nil
}

type indexReadError struct {
	error
	class string
}
type indexReadinessContextKey struct{}

func (err indexReadError) Unwrap() error { return err.error }
func (err indexReadError) Class() string { return err.class }
func maxTime(left, right time.Time) time.Time {
	if left.After(right) {
		return left
	}
	return right
}

func (service *Service) namespaceIssueSnapshot(ctx context.Context, namespace string, kinds []string) (snapshot kubeindex.Snapshot, status kubeindex.Status) {
	snapshot = kubeindex.Snapshot{Records: []kubeindex.Record{}, Kinds: map[string]kubeindex.KindStatus{}}
	scope := service.indexScope()
	service.changes.mu.Lock()
	if service.changes.stopped[scope] {
		service.changes.mu.Unlock()
		return snapshot, kubeindex.Status{State: "unavailable", Partial: true}
	}
	parent := service.changes.lifecycle
	if loop := service.changes.captures[scope]; loop != nil {
		parent = loop.ctx
	}
	ctx, cancel := context.WithCancel(ctx)
	stopLifetime := func() bool { return false }
	if parent != nil {
		stopLifetime = context.AfterFunc(parent, cancel)
		if parent.Err() != nil {
			cancel()
		}
	}
	call := &indexRefreshCall{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	if service.changes.fallbacks[scope] == nil {
		service.changes.fallbacks[scope] = make(map[*indexRefreshCall]struct{})
	}
	service.changes.fallbacks[scope][call] = struct{}{}
	service.changes.mu.Unlock()
	defer func() {
		credentialErr := service.validateCredential(ctx)
		service.changes.mu.Lock()
		if credentialErr != nil || ctx.Err() != nil || service.changes.stopped[scope] || parent != nil && parent.Err() != nil {
			snapshot = kubeindex.Snapshot{Records: []kubeindex.Record{}, Kinds: map[string]kubeindex.KindStatus{}}
			status = kubeindex.Status{State: "unavailable", Partial: true}
		}
		delete(service.changes.fallbacks[scope], call)
		if len(service.changes.fallbacks[scope]) == 0 {
			delete(service.changes.fallbacks, scope)
		}
		close(call.done)
		service.changes.mu.Unlock()
		stopLifetime()
		cancel()
	}()
	var lock sync.Mutex
	var workers sync.WaitGroup
	slots := make(chan struct{}, 4)
	for _, kind := range kinds {
		workers.Add(1)
		go func(kind string) {
			defer workers.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slots }()
			observed := time.Now().UTC()
			records, complete, err := service.loadIndexKind(ctx, kind, namespace)
			state := kubeindex.KindStatus{State: "ready", ObservedAt: observed, Records: len(records), Partial: !complete}
			if err != nil {
				state.State, state.Error, state.Partial = "error", errorClass(err), true
				records = nil
			} else if !complete {
				state.State = "partial"
			}
			lock.Lock()
			snapshot.Kinds[kind] = state
			snapshot.Records = append(snapshot.Records, records...)
			lock.Unlock()
		}(kind)
	}
	workers.Wait()
	sort.Slice(snapshot.Records, func(left, right int) bool { return snapshot.Records[left].UID < snapshot.Records[right].UID })
	return snapshot, scopedIndexStatus(kubeindex.Status{Kinds: snapshot.Kinds}, kinds)
}

func captureIndexStatus(status CaptureStatus) *kubeindex.CaptureStatus {
	return &kubeindex.CaptureStatus{Mode: status.Mode, Reason: status.Reason, LastCycleAt: status.LastCycleAt, Behind: status.Behind, Verification: status.Verification}
}

type metadataIndexPage struct {
	Items    []metadataIndexItem `json:"items"`
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
}

type metadataIndexItem struct {
	Metadata struct {
		Name            string            `json:"name"`
		Namespace       string            `json:"namespace"`
		UID             string            `json:"uid"`
		ResourceVersion string            `json:"resourceVersion"`
		Labels          map[string]string `json:"labels"`
		Owners          []struct {
			APIVersion string `json:"apiVersion"`
			Controller *bool  `json:"controller"`
			Kind       string `json:"kind"`
			Namespace  string `json:"namespace,omitempty"`
			Name       string `json:"name"`
			UID        string `json:"uid,omitempty"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
}

func (item metadataIndexItem) record(kind string) kubeindex.Record {
	projected := ProjectedResource{
		Kind: kind, Name: item.Metadata.Name, Namespace: item.Metadata.Namespace,
		UID: item.Metadata.UID, Labels: item.Metadata.Labels,
		Summary: map[string]any{"resource_version": item.Metadata.ResourceVersion},
	}
	for _, owner := range item.Metadata.Owners {
		ref := ObjectRef{APIVersion: owner.APIVersion, Kind: owner.Kind, Namespace: owner.Namespace, Name: owner.Name, UID: owner.UID}
		if owner.Controller != nil {
			ref.controllerKnown, ref.controller = true, *owner.Controller
		}
		projected.Owners = append(projected.Owners, ref)
	}
	return indexRecord(projected)
}

func persistIndexDeltas(store *kubechanges.Store, cluster string, deltas <-chan kubeindex.Delta, now time.Time) error {
	collected := make([]kubechanges.Change, 0)
	resync := false
	for deltas != nil {
		select {
		case delta, open := <-deltas:
			if !open {
				deltas = nil
				continue
			}
			if delta.Resync {
				resync = true
				continue
			}
			collected = append(collected, kubechanges.DetectAll(cluster, delta, now)...)
		default:
			deltas = nil
		}
	}
	if resync {
		observed := time.Now().UTC()
		if observed.Before(now) {
			observed = now
		}
		return store.MarkGap(kubechanges.Gap{From: now, To: observed})
	}
	return store.Append(collected, time.Now().UTC())
}

// SubscribeIndex primes the shared index, then subscribes to its bounded deltas.
func (service *Service) SubscribeIndex(ctx context.Context, buffer int, kinds ...string) (<-chan kubeindex.Delta, func(), kubeindex.Status, error) {
	if service == nil {
		return nil, func() {}, kubeindex.Status{}, ErrInvalidArguments
	}
	for _, kind := range kinds {
		if !streamIndexKinds[kind] {
			return nil, func() {}, kubeindex.Status{State: "unavailable", Partial: true}, ErrInvalidArguments
		}
	}
	release, admissionErr := acquireIndexStream(service.scope.OrgID)
	if admissionErr != nil {
		return nil, func() {}, kubeindex.Status{}, admissionErr
	}
	_, status, err := service.IndexSnapshot(ctx, kinds...)
	if err != nil {
		release()
		return nil, func() {}, status, err
	}
	index, err := service.indexes.ForScope(kubeindex.Scope{OrgID: service.scope.OrgID, ClusterID: service.scope.ClusterID, CredentialID: service.scope.CredentialID})
	if err != nil {
		release()
		return nil, func() {}, status, err
	}
	deltas, unsubscribe := index.Subscribe(buffer)
	return deltas, func() { unsubscribe(); release() }, status, nil
}

func indexRecord(resource ProjectedResource) kubeindex.Record {
	summary := safeIndexedSummary(resource.Summary)
	for _, category := range resource.ProjectionTruncated {
		if category == "references" || category == "summary" || category == "owners" {
			summary["references_partial"] = "true"
		}
	}
	uid := resource.UID
	if uid == "" {
		digest := sha256.Sum256([]byte(resource.Kind + "\x00" + resource.Namespace + "\x00" + resource.Name))
		uid = "projected:" + hex.EncodeToString(digest[:])
	}
	record := kubeindex.Record{
		APIVersion: resource.APIVersion,
		UID:        uid, Kind: resource.Kind, Namespace: resource.Namespace, Name: resource.Name,
		ResourceVersion: summaryString(resource.Summary, "resource_version"),
		Labels:          resource.Labels, Phase: summaryString(resource.Summary, "phase"),
		EventType: summaryString(resource.Summary, "type"), NodeName: summaryString(resource.Summary, "node"),
		Selector:           indexStringMap(resource.Summary["selector"]),
		NodeReady:          conditionTrue(resource.Conditions, "Ready"),
		Replicas:           summaryInt32(summary, "desired_replicas"),
		Ready:              summaryInt32(summary, "ready_replicas"),
		Available:          summaryInt32(summary, "available_replicas"),
		ObservedGeneration: int64(summaryInt32(summary, "observed_generation")),
		Summary:            summary,
		Containers:         indexedContainers(resource.Summary["container_states"]),
		ReadyStatus:        summary["ready_status"], ReadyTransitionAt: indexedTimestamp(summary, "ready_transition_at"),
		ProgressingStatus: summary["progressing_status"], ProgressingReason: summary["progressing_reason"], ProgressingTransitionAt: indexedTimestamp(summary, "progressing_transition_at"),
		AvailableStatus: summary["available_status"], AvailableTransitionAt: indexedTimestamp(summary, "available_transition_at"),
		UpdateRevision: summary["update_revision"], CurrentRevision: summary["current_revision"], Revision: summary["revision"], Updated: summaryInt32(summary, "updated_replicas"),
		EventFirstAt: indexedTimestamp(summary, "firstTimestamp"), EventLastAt: indexedTimestamp(summary, "lastTimestamp"), EventCount: summaryInt32(summary, "count"),
	}
	for _, key := range []string{"services", "gateways", "config_maps", "secrets", "persistent_volume_claims", "pods", "service_accounts"} {
		if names, ok := resource.Summary[key].([]string); ok && len(names) > 32 {
			summary["references_partial"] = "true"
		}
		if names, ok := resource.Summary[key].([]any); ok && len(names) > 32 {
			summary["references_partial"] = "true"
		}
		if names := indexStringSlice(resource.Summary[key]); len(names) > 0 {
			if record.References == nil {
				record.References = make(map[string][]string)
			}
			record.References[key] = names
		}
	}
	if target, ok := resource.Summary["target"].(ObjectRef); ok {
		record.Target = &kubeindex.OwnerRef{Kind: target.Kind, Namespace: target.Namespace, Name: target.Name, UID: target.UID}
	}
	if target, ok := resource.Summary["involved_object"].(ObjectRef); ok {
		record.Target = &kubeindex.OwnerRef{Kind: target.Kind, Namespace: target.Namespace, Name: target.Name, UID: target.UID}
	}
	if resource.Kind == "DaemonSet" {
		record.Replicas = summaryInt32(summary, "desired_number_scheduled")
		record.Ready = summaryInt32(summary, "number_ready")
		record.Available = record.Ready
	}
	if created := summaryString(resource.Summary, "created_at"); created != "" {
		record.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	}
	if generation := summary["generation"]; generation != "" {
		record.Generation, _ = strconv.ParseInt(generation, 10, 64)
	}
	for _, owner := range resource.Owners {
		namespace := owner.Namespace
		if namespace == "" {
			namespace = resource.Namespace
		}
		record.Owners = append(record.Owners, kubeindex.OwnerRef{APIVersion: owner.APIVersion, Kind: owner.Kind, Namespace: namespace, Name: owner.Name, UID: owner.UID, Controller: owner.controller, ControllerKnown: owner.controllerKnown})
	}
	containers, typed := resource.Summary["containers"].([]map[string]any)
	if !typed {
		containers = mapSlice(resource.Summary["containers"])
	}
	for _, container := range containers {
		if image := boundString(stringValue(container["image"])); image != "" && len(record.Images) < 32 {
			record.Images = append(record.Images, image)
		}
	}
	return record
}

func safeIndexedSummary(summary map[string]any) map[string]string {
	allowed := map[string]bool{
		"phase": true, "type": true, "reason": true, "generation": true,
		"observed_generation": true, "desired_replicas": true, "replicas": true,
		"ready_replicas": true, "available_replicas": true, "unavailable_replicas": true, "updated_replicas": true,
		"restart_count": true, "allocatable_cpu": true, "allocatable_memory": true,
		"requested_cpu": true, "requested_memory": true, "limited_cpu": true, "limited_memory": true, "created_at": true,
		"ready_status": true, "ready_reason": true, "ready_transition_at": true, "progressing_status": true, "progressing_reason": true, "progressing_transition_at": true,
		"pod_reason": true, "scheduled_status": true, "scheduled_reason": true, "scheduled_transition_at": true,
		"disruption_status": true, "disruption_reason": true, "disruption_transition_at": true,
		"job_failed_status": true, "job_failed_reason": true, "job_failed_transition_at": true,
		"available_status": true, "available_transition_at": true, "update_revision": true, "current_revision": true, "revision": true,
		"desired_number_scheduled": true, "number_ready": true, "failed": true, "count": true, "firstTimestamp": true, "lastTimestamp": true,
		"node_pressure":          true,
		"break_evidence_partial": true,
		"references_partial":     true,
	}
	result := make(map[string]string)
	for key, value := range summary {
		if !allowed[key] || len(result) >= 64 {
			continue
		}
		var text string
		switch value := value.(type) {
		case string:
			text = value
		case float64:
			text = strconv.FormatFloat(value, 'f', -1, 64)
		case bool:
			text = strconv.FormatBool(value)
		case int:
			text = strconv.Itoa(value)
		case int64:
			text = strconv.FormatInt(value, 10)
		default:
			continue
		}
		text = strings.TrimSpace(text)
		if len(text) <= 512 && !strings.ContainsAny(text, "\x00\r\n") {
			result[key] = text
		}
	}
	return result
}

func summaryInt32(summary map[string]string, key string) int32 {
	value := summary[key]
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil || parsed < 0 {
		return 0
	}
	return int32(parsed)
}

func indexStringMap(value any) map[string]string {
	result := make(map[string]string)
	switch values := value.(type) {
	case map[string]string:
		for key, item := range values {
			if len(result) < 128 && key != "" && len(key) <= 253 && len(item) <= 256 {
				result[key] = item
			}
		}
	case map[string]any:
		for key, item := range values {
			text, ok := item.(string)
			if ok && len(result) < 128 && key != "" && len(key) <= 253 && len(text) <= 256 {
				result[key] = text
			}
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func indexStringSlice(value any) []string {
	var values []string
	switch items := value.(type) {
	case []string:
		values = items
	case []any:
		for _, item := range items {
			if text, ok := item.(string); ok {
				values = append(values, text)
			}
		}
	}
	if len(values) > 32 {
		values = values[:32]
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && len(value) <= 253 && !strings.ContainsAny(value, "\x00\r\n") {
			result = append(result, value)
		}
	}
	return result
}
