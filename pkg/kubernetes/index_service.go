package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	call := &indexRefreshCall{done: make(chan struct{}), requested: make(map[string]struct{}, len(uniqueKinds))}
	startRefresh := true
	if service.changes != nil {
		service.changes.mu.Lock()
		if existing := service.changes.refreshes[scope]; existing != nil {
			call = existing
			startRefresh = false
		}
		for _, kind := range uniqueKinds {
			call.requested[kind] = struct{}{}
		}
		if startRefresh {
			service.changes.refreshes[scope] = call
		}
		service.changes.mu.Unlock()
	}
	if startRefresh {
		go service.runIndexRefresh(scope, index, changeStore, call)
	}
	waitTimer := time.NewTimer(kubernetesIndexReadinessWait)
	defer waitTimer.Stop()
	select {
	case <-call.done:
	case <-requestContext.Done():
		return index.Snapshot(), scopedIndexStatus(index.Status(time.Now().UTC()), kinds), requestContext.Err()
	case <-waitTimer.C:
	}
	snapshot := index.Snapshot()
	status := scopedIndexStatus(index.Status(time.Now().UTC()), kinds)
	if err := requestContext.Err(); err != nil {
		return snapshot, status, err
	}
	return snapshot, status, nil
}

func (service *Service) runIndexRefresh(scope kubeindex.Scope, index *kubeindex.Index, changeStore *kubechanges.Store, call *indexRefreshCall) {
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
					if service.changes.refreshes[scope] == call {
						delete(service.changes.refreshes, scope)
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
		refreshContext, cancelRefresh := context.WithTimeout(context.Background(), kubernetesIndexRefreshTimeout)
		var deltas <-chan kubeindex.Delta
		unsubscribe := func() {}
		var captureLock *sync.Mutex
		if changeStore != nil && service.changes != nil {
			captureLock = service.changes.captureLock(scope)
			captureLock.Lock()
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
				_ = index.Refresh(refreshContext, kind, kubernetesIndexRefreshInterval, time.Now().UTC(), func(loadContext context.Context) ([]kubeindex.Record, bool, error) {
					return service.loadIndexKind(loadContext, kind)
				})
			}()
		}
		workers.Wait()
		if changeStore != nil {
			_ = persistIndexDeltas(changeStore, service.scope.ClusterID, deltas, time.Now().UTC())
		}
		unsubscribe()
		if captureLock != nil {
			captureLock.Unlock()
		}
		cancelRefresh()
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

func (service *Service) loadIndexKind(ctx context.Context, kind string) ([]kubeindex.Record, bool, error) {
	ctx, cancel := withOperationBudget(ctx, kubernetesIndexRefreshTimeout, kubernetesIndexMaxPages+32, 64<<20, kubernetesIndexMaxRecords)
	defer cancel()
	definition, err := service.resolve(ctx, indexedResourceIDs[kind])
	if err != nil {
		return nil, false, err
	}
	apiPath, err := resourcePath(definition, "", "")
	if err != nil {
		return nil, false, err
	}
	records := make([]kubeindex.Record, 0)
	seenTokens := make(map[string]bool)
	continueToken := ""
	pageLimit := kubernetesIndexPageSize
	for pageNumber := 0; pageNumber < kubernetesIndexMaxPages; pageNumber++ {
		query := url.Values{"limit": {strconv.Itoa(pageLimit)}}
		if continueToken != "" {
			query.Set("continue", continueToken)
		}
		pagePath := apiPath + "?" + query.Encode()
		if metadataOnlyIndexKinds[kind] {
			var page metadataIndexPage
			if err := service.client.GetMetadataJSON(ctx, pagePath, &page); err != nil {
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
			for _, item := range page.Items {
				records = append(records, indexRecord(service.projectResource(definition, item)))
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
			Kind      string `json:"kind"`
			Namespace string `json:"namespace,omitempty"`
			Name      string `json:"name"`
			UID       string `json:"uid,omitempty"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
}

func (item metadataIndexItem) record(kind string) kubeindex.Record {
	projected := ProjectedResource{
		Kind: kind, Name: item.Metadata.Name, Namespace: item.Metadata.Namespace,
		UID: item.Metadata.UID, Labels: item.Metadata.Labels,
	}
	for _, owner := range item.Metadata.Owners {
		projected.Owners = append(projected.Owners, ObjectRef{Kind: owner.Kind, Namespace: owner.Namespace, Name: owner.Name, UID: owner.UID})
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
			if change, ok := kubechanges.Detect(cluster, delta, time.Now().UTC()); ok {
				collected = append(collected, change)
			}
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
	for _, kind := range kinds {
		if !streamIndexKinds[kind] {
			return nil, func() {}, kubeindex.Status{State: "unavailable", Partial: true}, ErrInvalidArguments
		}
	}
	_, status, err := service.IndexSnapshot(ctx, kinds...)
	if err != nil {
		return nil, func() {}, status, err
	}
	index, err := service.indexes.ForScope(kubeindex.Scope{OrgID: service.scope.OrgID, ClusterID: service.scope.ClusterID, CredentialID: service.scope.CredentialID})
	if err != nil {
		return nil, func() {}, status, err
	}
	deltas, unsubscribe := index.Subscribe(buffer)
	return deltas, unsubscribe, status, nil
}

func indexRecord(resource ProjectedResource) kubeindex.Record {
	uid := resource.UID
	if uid == "" {
		digest := sha256.Sum256([]byte(resource.Kind + "\x00" + resource.Namespace + "\x00" + resource.Name))
		uid = "projected:" + hex.EncodeToString(digest[:])
	}
	record := kubeindex.Record{
		UID: uid, Kind: resource.Kind, Namespace: resource.Namespace, Name: resource.Name,
		Labels: resource.Labels, Phase: summaryString(resource.Summary, "phase"),
		EventType: summaryString(resource.Summary, "type"), NodeName: summaryString(resource.Summary, "node"),
		Selector:           indexStringMap(resource.Summary["selector"]),
		NodeReady:          conditionTrue(resource.Conditions, "Ready"),
		Replicas:           summaryInt32(resource.Summary, "desired_replicas"),
		Ready:              summaryInt32(resource.Summary, "ready_replicas"),
		Available:          summaryInt32(resource.Summary, "available_replicas"),
		ObservedGeneration: int64(summaryInt32(resource.Summary, "observed_generation")),
		Summary:            safeIndexedSummary(resource.Summary),
	}
	for _, key := range []string{"services", "gateways", "config_maps", "secrets", "persistent_volume_claims", "pods"} {
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
	if created := summaryString(resource.Summary, "created_at"); created != "" {
		record.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	}
	if generation := summaryString(resource.Summary, "generation"); generation != "" {
		record.Generation, _ = strconv.ParseInt(generation, 10, 64)
	}
	for _, owner := range resource.Owners {
		namespace := owner.Namespace
		if namespace == "" {
			namespace = resource.Namespace
		}
		record.Owners = append(record.Owners, kubeindex.OwnerRef{Kind: owner.Kind, Namespace: namespace, Name: owner.Name, UID: owner.UID})
	}
	for _, container := range mapSlice(resource.Summary["containers"]) {
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
		"ready_replicas": true, "available_replicas": true, "updated_replicas": true,
		"restart_count": true, "allocatable_cpu": true, "allocatable_memory": true,
		"requested_cpu": true, "requested_memory": true, "limited_cpu": true, "limited_memory": true, "created_at": true,
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

func summaryInt32(summary map[string]any, key string) int32 {
	value := summaryString(summary, key)
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
