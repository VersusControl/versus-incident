// Package servicehealth stores and aggregates the OSS log and internal health projection.
package servicehealth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

const (
	DefaultIntervalSeconds = 60
	DefaultWindowSeconds   = 300
	MinIntervalSeconds     = 30
	MaxIntervalSeconds     = 900
	MinWindowSeconds       = 60
	MaxWindowSeconds       = 3600
	BucketDuration         = time.Minute
	MaxServicesPerSnapshot = 500
	MaxSnapshots           = 60
	MaxSnapshotAge         = 24 * time.Hour
	MaxSnapshotBytes       = 4 << 20
	MaxStateBytes          = 4 << 20
	MaxLogBuckets          = 100000
	MaxPatternIDsPerBucket = 256
	MaxPatternIDsPerWindow = 1024
	MaxStateSources        = 1024
	MaxLogRetention        = time.Duration(MaxWindowSeconds)*time.Second + 2*BucketDuration
	MaxPremiumSources      = 16
	MaxExternalRequests    = 64
	MaxRequestsPerSource   = 8
	MaxRowsPerSource       = 10000
	MaxResponseBytes       = 1 << 20
	MaxCumulativeBytes     = 16 << 20
	MaxConcurrency         = 2
	MaxRequestDuration     = 10 * time.Second
)

var (
	ErrConflict   = errors.New("service health settings changed concurrently")
	ErrNoStorage  = errors.New("service health storage unavailable")
	ErrBufferFull = errors.New("service health observation buffer is full")
)

// Settings is the always-enabled runtime timing configuration.
type Settings struct {
	IntervalSeconds int    `json:"interval_seconds"`
	WindowSeconds   int    `json:"window_seconds"`
	Revision        uint64 `json:"revision"`
	Diagnostic      string `json:"diagnostic,omitempty"`
}

// ValidationError carries field-specific settings errors.
type ValidationError struct {
	Fields map[string]string `json:"fields"`
}

func (err *ValidationError) Error() string { return "invalid service health settings" }

// DefaultSettings returns the fresh-install timing values.
func DefaultSettings() Settings {
	return Settings{IntervalSeconds: DefaultIntervalSeconds, WindowSeconds: DefaultWindowSeconds}
}

// ValidateSettings rejects values outside the server-owned bounds.
func ValidateSettings(settings Settings) error {
	fields := map[string]string{}
	if settings.IntervalSeconds < MinIntervalSeconds || settings.IntervalSeconds > MaxIntervalSeconds {
		fields["interval_seconds"] = "must be between 30 and 900"
	}
	if settings.WindowSeconds < MinWindowSeconds || settings.WindowSeconds > MaxWindowSeconds {
		fields["window_seconds"] = "must be between 60 and 3600"
	}
	if settings.WindowSeconds < settings.IntervalSeconds {
		fields["window_seconds"] = "must be greater than or equal to interval_seconds"
	}
	if len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}
	return nil
}

// Manager is the storage-backed Service Health recorder and snapshot store.
type Manager struct {
	store   storage.Provider
	now     func() time.Time
	mu      sync.Mutex
	pending map[string]*pendingState
}

// NewManager constructs a manager over the shared storage provider.
func NewManager(store storage.Provider) *Manager {
	return &Manager{store: store, now: time.Now, pending: map[string]*pendingState{}}
}

func orgKey(prefix, orgID string) string {
	sum := sha256.Sum256([]byte(storage.NormalizeOrgID(orgID)))
	return prefix + "/" + hex.EncodeToString(sum[:16])
}

func settingsKey(orgID string) string { return orgKey("models/settings/service-health", orgID) }
func stateKey(orgID string) string    { return orgKey("models/service-health/state", orgID) }
func snapshotKey(orgID string) string { return orgKey("models/service-health/snapshots", orgID) }

// LoadSettings returns defaults only for an absent record; storage and decode
// failures are returned so callers never mistake an outage for defaults.
func (manager *Manager) LoadSettings(orgID string) (Settings, error) {
	if manager == nil || manager.store == nil {
		return Settings{}, ErrNoStorage
	}
	data, err := manager.store.ReadBlob(settingsKey(orgID))
	if err != nil {
		return Settings{}, fmt.Errorf("load service health settings: %w", err)
	}
	if len(data) == 0 {
		return DefaultSettings(), nil
	}
	var settings Settings
	if err := json.Unmarshal(data, &settings); err != nil {
		defaults := DefaultSettings()
		defaults.Diagnostic = "stored_settings_invalid"
		return defaults, nil
	}
	if err := ValidateSettings(settings); err != nil {
		defaults := DefaultSettings()
		defaults.Diagnostic = "stored_settings_invalid"
		return defaults, nil
	}
	return settings, nil
}

// UpdateSettings performs an optimistic revision update and persists before returning.
func (manager *Manager) UpdateSettings(orgID string, next Settings) (Settings, error) {
	if err := ValidateSettings(next); err != nil {
		return Settings{}, err
	}
	if manager == nil || manager.store == nil {
		return Settings{}, ErrNoStorage
	}
	cas, ok := manager.store.(storage.BlobCAS)
	if !ok {
		return Settings{}, storage.ErrUnsupported
	}
	key := settingsKey(orgID)
	currentBytes, err := manager.store.ReadBlob(key)
	if err != nil {
		return Settings{}, fmt.Errorf("load service health settings: %w", err)
	}
	current := DefaultSettings()
	if len(currentBytes) > 0 {
		if err := json.Unmarshal(currentBytes, &current); err != nil {
			current = DefaultSettings()
		} else if ValidateSettings(current) != nil {
			current = DefaultSettings()
		}
	}
	if next.Revision != current.Revision {
		return Settings{}, ErrConflict
	}
	next.Revision++
	next.Diagnostic = ""
	replacement, err := json.Marshal(next)
	if err != nil {
		return Settings{}, err
	}
	swapped, err := cas.CompareAndSwapBlob(key, currentBytes, replacement)
	if err != nil {
		return Settings{}, fmt.Errorf("save service health settings: %w", err)
	}
	if !swapped {
		return Settings{}, ErrConflict
	}
	return next, nil
}

type persistedState struct {
	Buckets []bucketRecord          `json:"buckets"`
	Sources map[string]SourceStatus `json:"sources"`
}

type bucketKey struct {
	service  string
	sourceID string
	start    int64
}

type pendingState struct {
	buckets map[bucketKey]*bucketRecord
	sources map[string]SourceStatus
}

type bucketRecord struct {
	Service           string           `json:"service"`
	SourceID          string           `json:"source_id"`
	Start             time.Time        `json:"start"`
	MatchedLogs       int64            `json:"matched_logs"`
	SeverityCounts    map[string]int64 `json:"severity_counts,omitempty"`
	LatestObservation time.Time        `json:"latest_observation"`
	PatternIDs        []string         `json:"pattern_ids,omitempty"`
	NewPatternIDs     []string         `json:"new_pattern_ids,omitempty"`
	UnknownPatternIDs []string         `json:"unknown_pattern_ids,omitempty"`
	SpikingPatternIDs []string         `json:"spiking_pattern_ids,omitempty"`
	PatternsTruncated bool             `json:"patterns_truncated,omitempty"`
}

// SourceStatus is safe operational source coverage with no backend message.
type SourceStatus struct {
	SourceID      string    `json:"source_id"`
	LatestAttempt time.Time `json:"latest_attempt"`
	LatestSuccess time.Time `json:"latest_success,omitempty"`
	ErrorClass    string    `json:"error_class,omitempty"`
}

// RecordLogHealth buffers one safe worker observation in an indexed minute bucket.
// Durability is intentionally interval-based: QueryWindow flushes pending deltas,
// so a process crash can lose at most the observations since the last collection.
func (manager *Manager) RecordLogHealth(_ context.Context, observation core.LogHealthObservation) error {
	if observation.Frequency <= 0 || observation.SourceID == "" || observation.PatternID == "" || observation.ObservedAt.IsZero() {
		return nil
	}
	orgID := storage.NormalizeOrgID(observation.OrgID)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	state := manager.pendingFor(orgID)
	start := observation.ObservedAt.UTC().Truncate(BucketDuration)
	key := bucketKey{service: observation.Service, sourceID: observation.SourceID, start: start.Unix()}
	bucket := state.buckets[key]
	if bucket == nil {
		manager.prunePending(state, observation.ObservedAt.UTC().Add(-MaxLogRetention))
		if len(state.buckets) >= MaxLogBuckets {
			return ErrBufferFull
		}
		bucket = &bucketRecord{Service: observation.Service, SourceID: observation.SourceID, Start: start, SeverityCounts: map[string]int64{}}
		state.buckets[key] = bucket
	}
	patternID := patternIdentity(observation.PatternID)
	bucket.MatchedLogs += int64(observation.Frequency)
	bucket.PatternIDs, bucket.PatternsTruncated = appendUniqueBounded(bucket.PatternIDs, patternID, bucket.PatternsTruncated)
	identityRetained := contains(bucket.PatternIDs, patternID)
	if identityRetained && observation.NewPattern {
		bucket.NewPatternIDs, bucket.PatternsTruncated = appendUniqueBounded(bucket.NewPatternIDs, patternID, bucket.PatternsTruncated)
	}
	if identityRetained && observation.LifecycleClassified {
		switch observation.LifecycleClass {
		case "unknown":
			bucket.UnknownPatternIDs, bucket.PatternsTruncated = appendUniqueBounded(bucket.UnknownPatternIDs, patternID, bucket.PatternsTruncated)
		case "spike":
			bucket.SpikingPatternIDs, bucket.PatternsTruncated = appendUniqueBounded(bucket.SpikingPatternIDs, patternID, bucket.PatternsTruncated)
		}
	}
	severity := safeSeverity(observation.StrongestSeverity)
	if severity != "" {
		bucket.SeverityCounts[severity] += int64(observation.Frequency)
	}
	if observation.ObservedAt.After(bucket.LatestObservation) {
		bucket.LatestObservation = observation.ObservedAt.UTC()
	}
	return nil
}

// RecordSourceHealth stores attempt/success separately and allowlists error classes.
func (manager *Manager) RecordSourceHealth(_ context.Context, observation core.SourceHealthObservation) error {
	if observation.SourceID == "" || observation.AttemptedAt.IsZero() {
		return nil
	}
	orgID := storage.NormalizeOrgID(observation.OrgID)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	state := manager.pendingFor(orgID)
	if _, exists := state.sources[observation.SourceID]; !exists && len(state.sources) >= MaxStateSources {
		return ErrBufferFull
	}
	status := state.sources[observation.SourceID]
	status.SourceID = observation.SourceID
	status.LatestAttempt = observation.AttemptedAt.UTC()
	if observation.Succeeded {
		status.LatestSuccess = observation.AttemptedAt.UTC()
		status.ErrorClass = ""
	} else {
		status.ErrorClass = safeErrorClass(observation.ErrorClass)
	}
	state.sources[observation.SourceID] = status
	return nil
}

func (manager *Manager) pendingFor(orgID string) *pendingState {
	state := manager.pending[orgID]
	if state == nil {
		state = &pendingState{buckets: map[bucketKey]*bucketRecord{}, sources: map[string]SourceStatus{}}
		manager.pending[orgID] = state
	}
	return state
}

func (manager *Manager) prunePending(state *pendingState, cutoff time.Time) {
	for key, bucket := range state.buckets {
		if bucket.Start.Before(cutoff) {
			delete(state.buckets, key)
		}
	}
}

func mergeBucket(target *bucketRecord, delta bucketRecord) {
	target.MatchedLogs += delta.MatchedLogs
	target.PatternIDs, target.PatternsTruncated = mergeUniqueBounded(target.PatternIDs, delta.PatternIDs, target.PatternsTruncated || delta.PatternsTruncated)
	target.NewPatternIDs, target.PatternsTruncated = mergeUniqueBounded(target.NewPatternIDs, delta.NewPatternIDs, target.PatternsTruncated)
	target.UnknownPatternIDs, target.PatternsTruncated = mergeUniqueBounded(target.UnknownPatternIDs, delta.UnknownPatternIDs, target.PatternsTruncated)
	target.SpikingPatternIDs, target.PatternsTruncated = mergeUniqueBounded(target.SpikingPatternIDs, delta.SpikingPatternIDs, target.PatternsTruncated)
	if target.SeverityCounts == nil {
		target.SeverityCounts = map[string]int64{}
	}
	for severity, count := range delta.SeverityCounts {
		target.SeverityCounts[severity] += count
	}
	if delta.LatestObservation.After(target.LatestObservation) {
		target.LatestObservation = delta.LatestObservation
	}
}

func mergeUniqueBounded(target, values []string, truncated bool) ([]string, bool) {
	for _, value := range values {
		target, truncated = appendUniqueBounded(target, value, truncated)
	}
	return target, truncated
}

func (manager *Manager) restorePending(orgID string, pending *pendingState) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	current := manager.pendingFor(orgID)
	for key, delta := range pending.buckets {
		if current.buckets[key] == nil {
			copy := *delta
			current.buckets[key] = &copy
		} else {
			mergeBucket(current.buckets[key], *delta)
		}
	}
	for sourceID, status := range pending.sources {
		if current.sources[sourceID].LatestAttempt.Before(status.LatestAttempt) {
			current.sources[sourceID] = status
		}
	}
}

func (manager *Manager) flushPending(orgID string, end time.Time) error {
	if manager == nil || manager.store == nil {
		return ErrNoStorage
	}
	cas, ok := manager.store.(storage.BlobCAS)
	if !ok {
		return storage.ErrUnsupported
	}
	orgID = storage.NormalizeOrgID(orgID)
	manager.mu.Lock()
	pending := manager.pending[orgID]
	delete(manager.pending, orgID)
	manager.mu.Unlock()
	if pending == nil || len(pending.buckets) == 0 && len(pending.sources) == 0 {
		return nil
	}
	key := stateKey(orgID)
	for attempt := 0; attempt < 8; attempt++ {
		current, err := manager.store.ReadBlob(key)
		if err != nil {
			manager.restorePending(orgID, pending)
			return err
		}
		state := persistedState{Sources: map[string]SourceStatus{}}
		if len(current) > 0 {
			if err := decodeState(current, &state); err != nil {
				manager.restorePending(orgID, pending)
				return err
			}
		}
		if state.Sources == nil {
			state.Sources = map[string]SourceStatus{}
		}
		byKey := make(map[bucketKey]bucketRecord, len(state.Buckets)+len(pending.buckets))
		cutoff := end.UTC().Add(-MaxLogRetention)
		for index := range state.Buckets {
			bucket := state.Buckets[index]
			if !bucket.Start.Before(cutoff) {
				byKey[bucketKey{service: bucket.Service, sourceID: bucket.SourceID, start: bucket.Start.Unix()}] = bucket
			}
		}
		for pendingKey, delta := range pending.buckets {
			if target, exists := byKey[pendingKey]; exists {
				mergeBucket(&target, *delta)
				byKey[pendingKey] = target
			} else if !delta.Start.Before(cutoff) {
				byKey[pendingKey] = *delta
			}
		}
		kept := make([]bucketRecord, 0, len(byKey))
		for _, bucket := range byKey {
			kept = append(kept, bucket)
		}
		sort.Slice(kept, func(i, j int) bool { return kept[i].Start.Before(kept[j].Start) })
		if len(kept) > MaxLogBuckets {
			kept = kept[len(kept)-MaxLogBuckets:]
		}
		state.Buckets = kept
		for sourceID, status := range pending.sources {
			if state.Sources[sourceID].LatestAttempt.Before(status.LatestAttempt) {
				if _, exists := state.Sources[sourceID]; !exists && len(state.Sources) >= MaxStateSources {
					continue
				}
				state.Sources[sourceID] = status
			}
		}
		replacement, err := json.Marshal(state)
		if err != nil {
			manager.restorePending(orgID, pending)
			return err
		}
		if len(replacement) > MaxStateBytes {
			manager.restorePending(orgID, pending)
			return fmt.Errorf("service health state is %d bytes, limit is %d", len(replacement), MaxStateBytes)
		}
		swapped, err := cas.CompareAndSwapBlob(key, current, replacement)
		if err != nil {
			manager.restorePending(orgID, pending)
			return err
		}
		if swapped {
			return nil
		}
	}
	manager.restorePending(orgID, pending)
	return ErrConflict
}

func safeSeverity(value string) string {
	switch strings.ToLower(value) {
	case "debug", "info", "warning", "error", "critical":
		return strings.ToLower(value)
	default:
		return ""
	}
}

func patternIdentity(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}

func safeErrorClass(value string) string {
	switch value {
	case "authentication", "permission", "timeout", "connection", "rate_limit", "invalid_response", "unavailable":
		return value
	default:
		return "unavailable"
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func appendUniqueBounded(values []string, value string, truncated bool) ([]string, bool) {
	if contains(values, value) {
		return values, truncated
	}
	if len(values) >= MaxPatternIDsPerBucket {
		return values, true
	}
	return append(values, value), truncated
}

// WindowAggregate is the bounded count-only log view for one service/source.
type WindowAggregate struct {
	Service           string           `json:"service"`
	SourceID          string           `json:"source_id"`
	MatchedLogs       int64            `json:"matched_logs"`
	UniquePatterns    int              `json:"unique_patterns"`
	NewPatterns       int              `json:"new_patterns"`
	UnknownPatterns   int              `json:"unknown_patterns"`
	SpikingPatterns   int              `json:"spiking_patterns"`
	SeverityCounts    map[string]int64 `json:"severity_counts"`
	LatestObservation time.Time        `json:"latest_observation,omitempty"`
	Estimated         bool             `json:"estimated"`
	Truncated         bool             `json:"truncated,omitempty"`
	PartialReason     string           `json:"partial_reason,omitempty"`
}

// QueryWindow sums fixed buckets; catalog lifetime Pattern.Count is never read.
func (manager *Manager) QueryWindow(orgID string, end time.Time, window time.Duration) ([]WindowAggregate, []SourceStatus, error) {
	if manager == nil || manager.store == nil {
		return nil, nil, ErrNoStorage
	}
	if err := manager.flushPending(orgID, end); err != nil {
		return nil, nil, err
	}
	data, err := manager.store.ReadBlob(stateKey(orgID))
	if err != nil {
		return nil, nil, err
	}
	if len(data) == 0 {
		return []WindowAggregate{}, []SourceStatus{}, nil
	}
	var state persistedState
	if err := decodeState(data, &state); err != nil {
		return nil, nil, err
	}
	type aggregateState struct {
		row                              WindowAggregate
		patterns, fresh, unknown, spikes map[string]struct{}
	}
	byKey := map[string]*aggregateState{}
	start := end.UTC().Add(-window)
	for _, bucket := range state.Buckets {
		if bucket.Start.Before(start) || !bucket.Start.Before(end.UTC()) {
			continue
		}
		key := bucket.Service + "\x00" + bucket.SourceID
		aggregate := byKey[key]
		if aggregate == nil {
			aggregate = &aggregateState{row: WindowAggregate{Service: bucket.Service, SourceID: bucket.SourceID, SeverityCounts: map[string]int64{}}, patterns: map[string]struct{}{}, fresh: map[string]struct{}{}, unknown: map[string]struct{}{}, spikes: map[string]struct{}{}}
			byKey[key] = aggregate
		}
		aggregate.row.MatchedLogs += bucket.MatchedLogs
		aggregate.row.Estimated = true
		if bucket.PatternsTruncated {
			aggregate.row.Truncated = true
			aggregate.row.PartialReason = "pattern_cardinality_limited"
		}
		if bucket.LatestObservation.After(aggregate.row.LatestObservation) {
			aggregate.row.LatestObservation = bucket.LatestObservation
		}
		for severity, count := range bucket.SeverityCounts {
			aggregate.row.SeverityCounts[severity] += count
		}
		for _, id := range bucket.PatternIDs {
			if addWindowPattern(aggregate.patterns, id) {
				aggregate.row.Truncated = true
				aggregate.row.PartialReason = "pattern_cardinality_limited"
			}
		}
		for _, id := range bucket.NewPatternIDs {
			if addWindowPattern(aggregate.fresh, id) {
				aggregate.row.Truncated = true
				aggregate.row.PartialReason = "pattern_cardinality_limited"
			}
		}
		for _, id := range bucket.UnknownPatternIDs {
			if addWindowPattern(aggregate.unknown, id) {
				aggregate.row.Truncated = true
				aggregate.row.PartialReason = "pattern_cardinality_limited"
			}
		}
		for _, id := range bucket.SpikingPatternIDs {
			if addWindowPattern(aggregate.spikes, id) {
				aggregate.row.Truncated = true
				aggregate.row.PartialReason = "pattern_cardinality_limited"
			}
		}
	}
	rows := make([]WindowAggregate, 0, len(byKey))
	for _, aggregate := range byKey {
		aggregate.row.UniquePatterns = len(aggregate.patterns)
		aggregate.row.NewPatterns = len(aggregate.fresh)
		aggregate.row.UnknownPatterns = len(aggregate.unknown)
		aggregate.row.SpikingPatterns = len(aggregate.spikes)
		rows = append(rows, aggregate.row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Service == rows[j].Service {
			return rows[i].SourceID < rows[j].SourceID
		}
		return rows[i].Service < rows[j].Service
	})
	sources := make([]SourceStatus, 0, len(state.Sources))
	for _, status := range state.Sources {
		sources = append(sources, status)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].SourceID < sources[j].SourceID })
	return rows, sources, nil
}

func addWindowPattern(patterns map[string]struct{}, id string) bool {
	if _, exists := patterns[id]; exists {
		return false
	}
	if len(patterns) >= MaxPatternIDsPerWindow {
		return true
	}
	patterns[id] = struct{}{}
	return false
}

func decodeState(data []byte, state *persistedState) error {
	if len(data) > MaxStateBytes {
		return fmt.Errorf("service health state is %d bytes, limit is %d", len(data), MaxStateBytes)
	}
	if err := json.Unmarshal(data, state); err != nil {
		return err
	}
	if len(state.Buckets) > MaxLogBuckets || len(state.Sources) > MaxStateSources {
		return fmt.Errorf("service health state exceeds cardinality limits")
	}
	for _, bucket := range state.Buckets {
		if len(bucket.PatternIDs) > MaxPatternIDsPerBucket || len(bucket.NewPatternIDs) > MaxPatternIDsPerBucket || len(bucket.UnknownPatternIDs) > MaxPatternIDsPerBucket || len(bucket.SpikingPatternIDs) > MaxPatternIDsPerBucket {
			return fmt.Errorf("service health bucket exceeds pattern cardinality limit")
		}
		for _, identities := range [][]string{bucket.PatternIDs, bucket.NewPatternIDs, bucket.UnknownPatternIDs, bucket.SpikingPatternIDs} {
			for _, identity := range identities {
				if len(identity) != 32 {
					return fmt.Errorf("service health bucket contains invalid pattern identity")
				}
				if _, err := hex.DecodeString(identity); err != nil {
					return fmt.Errorf("service health bucket contains invalid pattern identity")
				}
			}
		}
	}
	return nil
}

// SnapshotEnvelope is the durable, API-ready result for one org and revision.
type SnapshotEnvelope struct {
	SnapshotID       string            `json:"snapshot_id"`
	GeneratedAt      time.Time         `json:"generated_at"`
	LatestAttempt    time.Time         `json:"latest_attempt"`
	LatestSuccess    time.Time         `json:"latest_success,omitempty"`
	NextCollectionAt time.Time         `json:"next_collection_at"`
	SettingsRevision uint64            `json:"settings_revision"`
	WindowSeconds    int               `json:"window_seconds"`
	Services         []ServiceSnapshot `json:"services"`
	Domains          []DomainSnapshot  `json:"domains"`
	Facets           map[string]int    `json:"facets"`
	Coverage         CoverageSummary   `json:"coverage"`
	Capabilities     []Capability      `json:"capabilities"`
	PendingSettings  *Settings         `json:"pending_settings,omitempty"`
}

type ServiceSnapshot struct {
	OrgID           string                              `json:"org_id"`
	Service         string                              `json:"service"`
	Domain          string                              `json:"domain"`
	Kind            string                              `json:"kind"`
	Severity        string                              `json:"severity"`
	AssessmentBasis string                              `json:"assessment_basis"`
	Logs            WindowAggregate                     `json:"logs"`
	Availability    map[string]core.MeasureAvailability `json:"availability"`
	ActiveIncidents *int                                `json:"active_incidents"`
}

type DomainSnapshot struct {
	Name          string `json:"name"`
	Severity      string `json:"severity"`
	ServiceCount  int    `json:"service_count"`
	ObservedCount int    `json:"observed_count"`
	AffectedCount int    `json:"affected_count"`
}

type CoverageSummary struct {
	TotalServices    int  `json:"total_services"`
	ObservedServices int  `json:"observed_services"`
	Partial          bool `json:"partial"`
}

type Capability struct {
	Family   string                              `json:"family"`
	Measures map[string]core.MeasureAvailability `json:"measures"`
}

type snapshotHistory struct {
	Snapshots []SnapshotEnvelope `json:"snapshots"`
}

// SaveSnapshot appends idempotently and rejects a result from an older settings revision.
func (manager *Manager) SaveSnapshot(orgID string, snapshot SnapshotEnvelope) error {
	settings, err := manager.LoadSettings(orgID)
	if err != nil {
		return err
	}
	if snapshot.SettingsRevision != settings.Revision {
		return ErrConflict
	}
	cas, ok := manager.store.(storage.BlobCAS)
	if !ok {
		return storage.ErrUnsupported
	}
	key := snapshotKey(orgID)
	for attempt := 0; attempt < 8; attempt++ {
		current, err := manager.store.ReadBlob(key)
		if err != nil {
			return err
		}
		var history snapshotHistory
		if len(current) > 0 && json.Unmarshal(current, &history) != nil {
			return fmt.Errorf("decode service health snapshots")
		}
		for _, existing := range history.Snapshots {
			if existing.SnapshotID == snapshot.SnapshotID {
				return nil
			}
		}
		history.Snapshots = append(history.Snapshots, snapshot)
		cutoff := manager.now().UTC().Add(-MaxSnapshotAge)
		kept := history.Snapshots[:0]
		for _, item := range history.Snapshots {
			if !item.GeneratedAt.Before(cutoff) {
				kept = append(kept, item)
			}
		}
		if len(kept) > MaxSnapshots {
			kept = kept[len(kept)-MaxSnapshots:]
		}
		history.Snapshots = kept
		var replacement []byte
		for {
			replacement, err = json.Marshal(history)
			if err != nil {
				return err
			}
			if len(replacement) <= MaxSnapshotBytes {
				break
			}
			if len(history.Snapshots) == 1 {
				return fmt.Errorf("service health newest snapshot is %d bytes, limit is %d", len(replacement), MaxSnapshotBytes)
			}
			history.Snapshots = history.Snapshots[1:]
		}
		swapped, err := cas.CompareAndSwapBlob(key, current, replacement)
		if err != nil {
			return err
		}
		if swapped {
			return nil
		}
	}
	return ErrConflict
}

// LoadSnapshot returns the newest snapshot for exactly one org.
func (manager *Manager) LoadSnapshot(orgID string) (SnapshotEnvelope, bool, error) {
	if manager == nil || manager.store == nil {
		return SnapshotEnvelope{}, false, ErrNoStorage
	}
	data, err := manager.store.ReadBlob(snapshotKey(orgID))
	if err != nil {
		return SnapshotEnvelope{}, false, err
	}
	if len(data) == 0 {
		return SnapshotEnvelope{}, false, nil
	}
	var history snapshotHistory
	if err := json.Unmarshal(data, &history); err != nil {
		return SnapshotEnvelope{}, false, err
	}
	if len(history.Snapshots) == 0 {
		return SnapshotEnvelope{}, false, nil
	}
	return history.Snapshots[len(history.Snapshots)-1], true, nil
}

// SnapshotOrEmpty reads the newest persisted snapshot or returns a static
// fresh-install envelope. It never aggregates buckets or calls a source.
func (manager *Manager) SnapshotOrEmpty(orgID string) (SnapshotEnvelope, error) {
	settings, err := manager.LoadSettings(orgID)
	if err != nil {
		return SnapshotEnvelope{}, err
	}
	snapshot, ok, err := manager.LoadSnapshot(orgID)
	if err != nil {
		return SnapshotEnvelope{}, err
	}
	if ok && snapshot.SettingsRevision == settings.Revision {
		if manager.now().UTC().After(snapshot.GeneratedAt.Add(2 * time.Duration(settings.IntervalSeconds) * time.Second)) {
			markSnapshotStale(&snapshot, "snapshot_stale")
		}
		return snapshot, nil
	}
	if ok {
		snapshot.PendingSettings = &settings
		snapshot.NextCollectionAt = manager.now().UTC()
		for serviceIndex := range snapshot.Services {
			if snapshot.Services[serviceIndex].Availability == nil {
				snapshot.Services[serviceIndex].Availability = defaultAvailability()
			}
			snapshot.Services[serviceIndex].Availability["logs"] = core.MeasureAvailability{State: core.HealthStale, ReasonCode: "settings_changed"}
		}
		for capabilityIndex := range snapshot.Capabilities {
			if snapshot.Capabilities[capabilityIndex].Family == "logs" {
				for measure := range snapshot.Capabilities[capabilityIndex].Measures {
					snapshot.Capabilities[capabilityIndex].Measures[measure] = core.MeasureAvailability{State: core.HealthStale, ReasonCode: "settings_changed"}
				}
			}
		}
		return snapshot, nil
	}
	return SnapshotEnvelope{
		GeneratedAt: manager.now().UTC(), SettingsRevision: settings.Revision,
		WindowSeconds: settings.WindowSeconds, Services: []ServiceSnapshot{},
		Domains: []DomainSnapshot{}, Facets: map[string]int{},
		Coverage: CoverageSummary{}, Capabilities: capabilities(false),
	}, nil
}

func markSnapshotStale(snapshot *SnapshotEnvelope, reason string) {
	for serviceIndex := range snapshot.Services {
		for measure, availability := range snapshot.Services[serviceIndex].Availability {
			switch availability.State {
			case core.HealthNotConfigured, core.HealthUnsupported, core.HealthRestricted:
				continue
			}
			snapshot.Services[serviceIndex].Availability[measure] = core.MeasureAvailability{State: core.HealthStale, ReasonCode: reason}
		}
	}
	for capabilityIndex := range snapshot.Capabilities {
		for measure, availability := range snapshot.Capabilities[capabilityIndex].Measures {
			switch availability.State {
			case core.HealthNotConfigured, core.HealthUnsupported, core.HealthRestricted:
				continue
			}
			snapshot.Capabilities[capabilityIndex].Measures[measure] = core.MeasureAvailability{State: core.HealthStale, ReasonCode: reason}
		}
	}
}
