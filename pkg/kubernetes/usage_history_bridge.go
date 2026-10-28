package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

const maxUsageManifestBytes = 2 << 20
const maxUsageBridgeRecordBytes = 128 << 10

var errUsageInterestContention = errors.New("kubernetes: usage interest update contention")

type usageInterest struct {
	Namespace string    `json:"namespace"`
	Pod       string    `json:"pod"`
	UID       string    `json:"uid,omitempty"`
	Opened    time.Time `json:"opened,omitempty"`
	Touched   time.Time `json:"touched"`
}

type usageBridgeRecord struct {
	Namespace string        `json:"namespace"`
	Pod       string        `json:"pod"`
	UID       string        `json:"uid,omitempty"`
	Since     time.Time     `json:"since"`
	At        time.Time     `json:"at"`
	Samples   []UsageSample `json:"samples"`
}

func (service *Service) usageProvider() storage.Provider {
	if service.changes == nil {
		return nil
	}
	service.changes.mu.Lock()
	defer service.changes.mu.Unlock()
	return service.changes.provider
}

func usageBridgePrefix(scope string) string {
	return "kubernetes/usage-history/" + logIdentity(scope) + "/"
}

func usageRecordKey(scope, namespace, pod string) string {
	return usageBridgePrefix(scope) + logIdentity(namespace+"/"+pod)
}

func readUsageManifest(provider storage.Provider, key string) (map[string]usageInterest, []byte, error) {
	raw, err := provider.ReadBlob(key)
	if err != nil {
		return nil, nil, err
	}
	result := make(map[string]usageInterest)
	if len(raw) > maxUsageManifestBytes || len(raw) > 0 && json.Unmarshal(raw, &result) != nil || len(result) > usageHistoryPods {
		return nil, nil, ErrInvalidArguments
	}
	if result == nil {
		result = make(map[string]usageInterest)
	}
	for id, item := range result {
		if !validPodLogName(item.Namespace, 63, false) || !validPodLogName(item.Pod, 253, true) || id != item.Namespace+"/"+item.Pod {
			return nil, nil, ErrInvalidArguments
		}
	}
	return result, raw, nil
}

// The bounded provider bridge transfers drawer interest and owner snapshots
// between replicas. Sampling and the hot six-hour ring remain owner-local;
// neither capture progress nor its lease renewal waits for metrics or bridge IO.
func (service *Service) updateUsageManifest(ctx context.Context, provider storage.Provider, updates []usageInterest, now time.Time) error {
	cas, ok := provider.(storage.BlobCAS)
	if !ok {
		return nil
	}
	key := usageBridgePrefix(service.cacheKey()) + "interest"
	for attempt := 0; attempt < 8; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, expected, err := readUsageManifest(provider, key)
		if err != nil {
			return err
		}
		expired := []usageInterest{}
		for id, item := range entries {
			if now.Sub(item.Touched) > usageHistoryRetention && now.Sub(item.Opened) > time.Hour {
				delete(entries, id)
				expired = append(expired, item)
			}
		}
		for _, item := range updates {
			id := item.Namespace + "/" + item.Pod
			old, exists := entries[id]
			if !exists && len(entries) >= usageHistoryPods {
				return ErrResponseTooLarge
			}
			if old.Opened.After(item.Opened) {
				item.Opened = old.Opened
			}
			if old.Touched.After(item.Touched) {
				item.Touched = old.Touched
			}
			entries[id] = item
		}
		raw, _ := json.Marshal(entries)
		if len(raw) > maxUsageManifestBytes {
			return ErrInvalidArguments
		}
		if bytes.Equal(raw, expected) {
			return nil
		}
		swapped := false
		err = service.guardedUsageCacheWrite(ctx, func() error {
			var writeErr error
			swapped, writeErr = cas.CompareAndSwapBlob(key, expected, raw)
			return writeErr
		})
		if err != nil {
			return err
		}
		if !swapped {
			continue
		}
		for _, item := range expired {
			if _, renewed := entries[item.Namespace+"/"+item.Pod]; renewed {
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			recordKey := usageRecordKey(service.cacheKey(), item.Namespace, item.Pod)
			prior, err := provider.ReadBlob(recordKey)
			if err == nil && len(prior) > 0 {
				var record usageBridgeRecord
				if json.Unmarshal(prior, &record) == nil && now.Sub(record.At) <= usageHistoryRetention {
					continue
				}
				_ = service.guardedUsageCacheWrite(ctx, func() error { _, err := cas.CompareAndSwapBlob(recordKey, prior, nil); return err })
			}
		}
		return nil
	}
	return errUsageInterestContention
}

func readUsageBridgeRecord(provider storage.Provider, scope, namespace, pod string, now time.Time) (usageBridgeRecord, []byte, error) {
	var result usageBridgeRecord
	raw, err := provider.ReadBlob(usageRecordKey(scope, namespace, pod))
	if err != nil {
		return result, nil, err
	}
	if len(raw) == 0 {
		return result, raw, nil
	}
	if len(raw) > maxUsageBridgeRecordBytes || json.Unmarshal(raw, &result) != nil || result.Namespace != namespace || result.Pod != pod || len(result.Samples) > 360 || result.At.After(now.Add(time.Minute)) {
		return usageBridgeRecord{}, raw, ErrInvalidArguments
	}
	if now.Sub(result.At) > usageHistoryRetention {
		return usageBridgeRecord{}, raw, nil
	}
	for _, sample := range result.Samples {
		if sample.Timestamp.After(now.Add(time.Minute)) || sample.CPUMilli < 0 || sample.MemoryBytes < 0 {
			return usageBridgeRecord{}, raw, ErrInvalidArguments
		}
	}
	return result, raw, nil
}

func mergeUsageSamples(old, new []UsageSample, now time.Time) []UsageSample {
	points := make(map[time.Time]UsageSample, len(old)+len(new))
	for _, samples := range [][]UsageSample{old, new} {
		for _, sample := range samples {
			if !sample.Timestamp.Before(now.Add(-usageHistoryRetention)) {
				points[sample.Timestamp] = sample
			}
		}
	}
	result := make([]UsageSample, 0, len(points))
	for _, sample := range points {
		result = append(result, sample)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Timestamp.Before(result[j].Timestamp) })
	if len(result) > 360 {
		result = result[len(result)-360:]
	}
	return result
}

func (service *Service) bridgeUsageRequest(ctx context.Context, namespace, pod, uid string, result *UsageHistory) {
	provider := service.usageProvider()
	if provider == nil || ctx.Err() != nil {
		return
	}
	now := result.ObservedAt
	if _, ok := provider.(storage.BlobCAS); !ok {
		return
	}
	service.queueUsageInterest(usageInterest{Namespace: namespace, Pod: pod, UID: uid, Opened: now, Touched: now})
}

func (service *Service) importUsageInterest(ctx context.Context, now time.Time) {
	provider := service.usageProvider()
	if provider == nil || ctx.Err() != nil {
		return
	}
	entries, _, err := readUsageManifest(provider, usageBridgePrefix(service.cacheKey())+"interest")
	if err != nil || ctx.Err() != nil {
		if err != nil && ctx.Err() == nil {
			log.Printf("kubernetes: usage history bridge interest_read failed: code=%s", DiagnoseError(err).Code)
		}
		return
	}
	state := service.usageHistory
	state.Lock()
	defer state.Unlock()
	scope := state.scope(service.cacheKey())
	total := state.podCount(now)
	for key, item := range entries {
		if now.Sub(item.Opened) > time.Hour || item.Opened.After(now.Add(time.Minute)) {
			continue
		}
		entry := scope.pods[key]
		if entry == nil {
			if total >= usageHistoryPods {
				continue
			}
			entry = &usagePodHistory{uid: item.UID, since: item.Opened, last: now}
			scope.pods[key] = entry
			total++
		}
		if item.Opened.After(entry.opened) {
			entry.opened = item.Opened
		}
	}
}

func (service *Service) publishUsageSamples(ctx context.Context, publications map[string]usagePodHistory, now time.Time) {
	provider := service.usageProvider()
	if provider == nil {
		return
	}
	cas, ok := provider.(storage.BlobCAS)
	if !ok {
		return
	}
	updates := []usageInterest{}
	for key, entry := range publications {
		namespace, pod, ok := strings.Cut(key, "/")
		if ok && len(entry.samples) > 0 {
			updates = append(updates, usageInterest{Namespace: namespace, Pod: pod, UID: entry.uid, Opened: entry.opened, Touched: now})
		}
	}
	if err := service.updateUsageManifest(ctx, provider, updates, now); err != nil {
		if ctx.Err() == nil {
			log.Printf("kubernetes: usage history bridge snapshot_manifest failed: code=%s", DiagnoseError(err).Code)
		}
		return
	}
	admitted, _, err := readUsageManifest(provider, usageBridgePrefix(service.cacheKey())+"interest")
	if err != nil {
		return
	}
	for key, entry := range publications {
		if ctx.Err() != nil {
			return
		}
		namespace, pod, ok := strings.Cut(key, "/")
		if !ok || len(entry.samples) == 0 {
			continue
		}
		if selected, exists := admitted[key]; !exists || selected.UID != entry.uid {
			continue
		}
		record := usageBridgeRecord{Namespace: namespace, Pod: pod, UID: entry.uid, Since: entry.since, At: now, Samples: entry.samples}
		for attempt := 0; attempt < 8 && ctx.Err() == nil; attempt++ {
			old, expected, _ := readUsageBridgeRecord(provider, service.cacheKey(), namespace, pod, now)
			if old.UID == record.UID {
				record.Samples = mergeUsageSamples(old.Samples, entry.samples, now)
				if !old.Since.IsZero() && old.Since.Before(record.Since) {
					record.Since = old.Since
				}
			} else if old.At.After(record.At) {
				break
			}
			raw, _ := json.Marshal(record)
			if len(raw) > maxUsageBridgeRecordBytes {
				break
			}
			swapped := false
			err := service.guardedUsageCacheWrite(ctx, func() error {
				var err error
				swapped, err = cas.CompareAndSwapBlob(usageRecordKey(service.cacheKey(), namespace, pod), expected, raw)
				return err
			})
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("kubernetes: usage history bridge snapshot_write failed: code=%s", DiagnoseError(err).Code)
				}
				break
			}
			if swapped {
				break
			}
		}
	}
}

func (service *Service) runUsageSampler(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		service.sampleUsageHistory(ctx, time.Now().UTC())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
