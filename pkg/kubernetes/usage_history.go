package kubernetes

import (
	"context"
	"math"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

const usageHistoryRetention = 6 * time.Hour
const usageHistoryPods = 2000

type UsageSample struct {
	Timestamp   time.Time `json:"timestamp"`
	CPUMilli    float64   `json:"cpu_milli"`
	MemoryBytes float64   `json:"memory_bytes"`
}

type UsageHistory struct {
	ObservedAt       time.Time     `json:"observed_at"`
	Source           string        `json:"source"`
	Availability     string        `json:"availability"`
	LiveSince        time.Time     `json:"live_since"`
	Samples          []UsageSample `json:"samples"`
	RetentionSeconds int           `json:"retention_seconds"`
}

type usagePodHistory struct {
	uid     string
	opened  time.Time
	since   time.Time
	last    time.Time
	samples []UsageSample
}

type usageHistoryScope struct {
	next time.Time
	pods map[string]*usagePodHistory
}

type usageHistoryState struct {
	sync.Mutex
	scopes  map[string]*usageHistoryScope
	workers map[string]*usageInterestWorker
}

func newUsageHistoryState() *usageHistoryState {
	return &usageHistoryState{scopes: make(map[string]*usageHistoryScope), workers: make(map[string]*usageInterestWorker)}
}

func (state *usageHistoryState) scope(key string) *usageHistoryScope {
	value := state.scopes[key]
	if value == nil {
		// Credential epochs are intentionally not reused.
		if len(state.scopes) >= 16 {
			for old := range state.scopes {
				delete(state.scopes, old)
				break
			}
		}
		value = &usageHistoryScope{pods: make(map[string]*usagePodHistory)}
		state.scopes[key] = value
	}
	return value
}

func pruneUsage(scope *usageHistoryScope, now time.Time) {
	for key, pod := range scope.pods {
		cut := 0
		for cut < len(pod.samples) && pod.samples[cut].Timestamp.Before(now.Add(-usageHistoryRetention)) {
			cut++
		}
		if cut > 0 {
			pod.samples = append([]UsageSample(nil), pod.samples[cut:]...)
		}
		if now.Sub(pod.last) > usageHistoryRetention && now.Sub(pod.opened) > time.Hour {
			delete(scope.pods, key)
		}
	}
}

func (state *usageHistoryState) podCount(now time.Time) int {
	total := 0
	for _, scope := range state.scopes {
		pruneUsage(scope, now)
		total += len(scope.pods)
	}
	return total
}

// UsageHistory records drawer interest, but never starts a sampler per viewer.
func (service *Service) UsageHistory(ctx context.Context, namespace, pod string) (UsageHistory, error) {
	now := time.Now().UTC()
	result := UsageHistory{ObservedAt: now, Source: "capture", Availability: "unavailable", LiveSince: now, Samples: []UsageSample{}, RetentionSeconds: int(usageHistoryRetention / time.Second)}
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return result, ErrForbidden
	}
	if err := checkLogStreamAuthorization(ctx); err != nil {
		return result, err
	}
	if !validPodLogName(namespace, 63, false) || !validPodLogName(pod, 253, true) {
		return result, ErrInvalidArguments
	}
	if err := service.validateCredential(ctx); err != nil {
		return result, err
	}
	index, err := service.indexes.ForScope(service.indexScope())
	if err != nil {
		return result, err
	}
	uid := ""
	for _, record := range index.Snapshot().Records {
		if record.Kind == "Pod" && record.Namespace == namespace && record.Name == pod {
			uid = record.UID
			break
		}
	}
	state := service.usageHistory
	state.Lock()
	scope := state.scope(service.cacheKey())
	pruneUsage(scope, now)
	key := namespace + "/" + pod
	entry := scope.pods[key]
	if entry != nil && uid != "" && entry.uid != "" && uid != entry.uid {
		delete(scope.pods, key)
		entry = nil
	}
	if entry == nil && state.podCount(now) < usageHistoryPods {
		entry = &usagePodHistory{uid: uid, since: now, last: now}
		scope.pods[key] = entry
	}
	if entry != nil {
		entry.opened = now
		result.LiveSince = entry.since
		result.Samples = append(result.Samples, entry.samples...)
	}
	// Trusted upgrade reads may use the former community history, while drawer
	// interest and all new samples are written only to the active write scope.
	if len(result.Samples) == 0 {
		for _, org := range service.readOrgs {
			if org == service.scope.OrgID {
				continue
			}
			legacy := state.scopes[org+"\x00"+service.scope.ClusterID+"\x00"+service.scope.CredentialID]
			if legacy == nil {
				continue
			}
			pruneUsage(legacy, now)
			old := legacy.pods[key]
			if old != nil && (uid == "" || old.uid == uid) && len(old.samples) > 0 {
				result.Samples = append(result.Samples, old.samples...)
				result.LiveSince = old.since
				break
			}
		}
	}
	state.Unlock()
	service.bridgeUsageRequest(ctx, namespace, pod, uid, &result)
	if len(result.Samples) > 0 {
		result.Availability = "available"
	}
	return result, nil
}

// sampleUsageHistory is called only by the elected capture owner and shares its
// cancellation/credential epoch. Metrics failures leave a visible history gap.
func (service *Service) sampleUsageHistory(ctx context.Context, now time.Time) {
	if ctx.Err() != nil || service == nil || service.usageHistory == nil {
		return
	}
	state := service.usageHistory
	state.Lock()
	scope := state.scope(service.cacheKey())
	if now.Before(scope.next) {
		state.Unlock()
		return
	}
	scope.next = now.Add(time.Minute)
	pruneUsage(scope, now)
	state.Unlock()
	service.importUsageInterest(ctx, now)
	index, err := service.indexes.ForScope(service.indexScope())
	if err != nil {
		return
	}
	snapshot := index.Snapshot()
	byUID := make(map[string]kubeindex.Record, len(snapshot.Records))
	for _, record := range snapshot.Records {
		byUID[record.UID] = record
	}
	brokenUIDs := make(map[string]bool)
	for _, item := range kubebreaks.Classify(snapshot.Records, now) {
		brokenUIDs[item.Root.UID] = true
	}
	broken := func(record kubeindex.Record) bool {
		for depth := 0; depth < 4; depth++ {
			if brokenUIDs[record.UID] {
				return true
			}
			parent := ""
			for _, owner := range record.Owners {
				if owner.Controller {
					parent = owner.UID
					break
				}
			}
			next, ok := byUID[parent]
			if !ok || next.Namespace != record.Namespace {
				break
			}
			record = next
		}
		return false
	}
	targets := make(map[string]string)
	records := append([]kubeindex.Record(nil), snapshot.Records...)
	sort.Slice(records, func(i, j int) bool { return records[i].UID < records[j].UID })
	state.Lock()
	scope = state.scope(service.cacheKey())
	total := state.podCount(now)
	for _, record := range records {
		if record.Kind != "Pod" {
			continue
		}
		key := record.Namespace + "/" + record.Name
		entry := scope.pods[key]
		interested := entry != nil && now.Sub(entry.opened) <= time.Hour
		if !interested && !broken(record) {
			continue
		}
		if len(targets) >= usageHistoryPods {
			break
		}
		if entry == nil && total >= usageHistoryPods {
			continue
		}
		if entry == nil || entry.uid != record.UID {
			opened := time.Time{}
			if entry != nil {
				opened = entry.opened
			} else {
				total++
			}
			entry = &usagePodHistory{uid: record.UID, opened: opened, since: now, last: now}
			scope.pods[key] = entry
		}
		targets[key] = record.UID
	}
	state.Unlock()
	if len(targets) == 0 {
		return
	}
	request, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	page, err := service.listAll(request, ListOptions{ResourceID: "metrics.k8s.io~v1beta1~pods"})
	if err != nil || ctx.Err() != nil {
		return
	}
	state.Lock()
	scope = state.scope(service.cacheKey())
	defer func() {
		publications := make(map[string]usagePodHistory, len(targets))
		for key := range targets {
			if entry := scope.pods[key]; entry != nil {
				copied := *entry
				copied.samples = append([]UsageSample(nil), entry.samples...)
				publications[key] = copied
			}
		}
		state.Unlock()
		service.publishUsageSamples(ctx, publications, now)
	}()
	for _, item := range page.Items {
		if ctx.Err() != nil {
			return
		}
		key := item.Namespace + "/" + item.Name
		uid, wanted := targets[key]
		entry := scope.pods[key]
		if !wanted || entry == nil || entry.uid != uid || !item.metricsValid {
			continue
		}
		timestamp, err := time.Parse(time.RFC3339Nano, summaryString(item.Summary, "timestamp"))
		cpu, cpuOK := quantityFloat(summaryString(item.Summary, "usage_cpu"))
		memory, memoryOK := quantityFloat(summaryString(item.Summary, "usage_memory"))
		if err != nil || !cpuOK || !memoryOK || cpu < 0 || memory < 0 || timestamp.After(now.Add(time.Minute)) || timestamp.Before(now.Add(-2*time.Minute)) {
			continue
		}
		if len(entry.samples) > 0 && !timestamp.After(entry.samples[len(entry.samples)-1].Timestamp) {
			continue
		}
		entry.samples = append(entry.samples, UsageSample{Timestamp: timestamp, CPUMilli: cpu * 1000, MemoryBytes: memory})
		if len(entry.samples) > 360 {
			entry.samples = append([]UsageSample(nil), entry.samples[len(entry.samples)-360:]...)
		}
		entry.last = now
	}
}

func quantityFloat(value string) (float64, bool) {
	number, ok := new(big.Rat).SetString(value)
	if !ok {
		return 0, false
	}
	result, _ := number.Float64()
	return result, !math.IsInf(result, 0) && !math.IsNaN(result)
}
