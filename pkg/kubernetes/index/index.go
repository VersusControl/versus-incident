package index

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalidRecord = errors.New("kubernetes index: invalid record")
	ErrRecordLimit   = errors.New("kubernetes index: record limit exceeded")
	ErrScopeLimit    = errors.New("kubernetes index: scope limit exceeded")
)

const (
	defaultMaxScopes  = 8
	defaultMaxRecords = 50000
	maxRecordBytes    = 4 << 10
	maxLabels         = 64
	maxSummaryFields  = 64
	maxOwners         = 8
	maxImages         = 16
)

type Scope struct {
	OrgID        string
	ClusterID    string
	CredentialID string
}

type OwnerRef struct {
	Kind      string
	Namespace string
	Name      string
	UID       string
}

type Record struct {
	UID                 string
	Kind                string
	Namespace           string
	Name                string
	ResourceVersion     string
	Generation          int64
	Labels              map[string]string
	Selector            map[string]string
	References          map[string][]string
	Owners              []OwnerRef
	Target              *OwnerRef
	CreatedAt           time.Time
	Phase               string
	EventType           string
	NodeName            string
	NodeReady           bool
	Images              []string
	Summary             map[string]string
	Replicas            int32
	Ready               int32
	Available           int32
	ObservedGeneration  int64
	RequestedCPUMilli   int64
	RequestedMemoryByte int64
	AllocatableCPUMilli int64
	AllocatableMemory   int64
	Health              string
}

type KindStatus struct {
	State           string    `json:"state"`
	ObservedAt      time.Time `json:"observed_at,omitempty"`
	LastAttemptAt   time.Time `json:"last_attempt_at,omitempty"`
	Records         int       `json:"records"`
	Partial         bool      `json:"partial"`
	Error           string    `json:"error,omitempty"`
	hasCompleteSync bool
}

type Snapshot struct {
	Records []Record
	Kinds   map[string]KindStatus
}

type Delta struct {
	Kind   string  `json:"kind"`
	Op     string  `json:"op,omitempty"`
	Old    *Record `json:"old,omitempty"`
	New    *Record `json:"new,omitempty"`
	Resync bool    `json:"resync,omitempty"`
}

type Index struct {
	mu             sync.RWMutex
	records        map[string]Record
	kinds          map[string]KindStatus
	refreshes      map[string]*refreshCall
	subscribers    map[uint64]chan Delta
	nextSubscriber uint64
	maxRecords     int
}

type refreshCall struct {
	done chan struct{}
	err  error
}

type Loader func(context.Context) ([]Record, bool, error)

type Status struct {
	State      string                `json:"state"`
	AgeSeconds float64               `json:"age_s"`
	Partial    bool                  `json:"partial"`
	Kinds      map[string]KindStatus `json:"kinds"`
}

type Registry struct {
	mu         sync.Mutex
	indexes    map[Scope]*Index
	maxScopes  int
	maxRecords int
}

func New(maxRecords int) *Index {
	if maxRecords <= 0 || maxRecords > defaultMaxRecords {
		maxRecords = defaultMaxRecords
	}
	return &Index{records: make(map[string]Record), kinds: make(map[string]KindStatus), refreshes: make(map[string]*refreshCall), subscribers: make(map[uint64]chan Delta), maxRecords: maxRecords}
}

func NewRegistry(maxScopes, maxRecords int) *Registry {
	if maxScopes <= 0 || maxScopes > defaultMaxScopes {
		maxScopes = defaultMaxScopes
	}
	if maxRecords <= 0 || maxRecords > defaultMaxRecords {
		maxRecords = defaultMaxRecords
	}
	return &Registry{indexes: make(map[Scope]*Index), maxScopes: maxScopes, maxRecords: maxRecords}
}

func (registry *Registry) ForScope(scope Scope) (*Index, error) {
	if registry == nil || scope.ClusterID == "" || len(scope.OrgID) > 256 || len(scope.ClusterID) > 256 || len(scope.CredentialID) > 256 {
		return nil, ErrInvalidRecord
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if existing := registry.indexes[scope]; existing != nil {
		return existing, nil
	}
	if len(registry.indexes) >= registry.maxScopes {
		return nil, ErrScopeLimit
	}
	created := New(registry.maxRecords)
	registry.indexes[scope] = created
	return created, nil
}

func (index *Index) Ingest(kind string, records []Record, complete bool, observedAt time.Time) error {
	if index == nil || !validText(kind, 128) || len(records) > index.maxRecords {
		return ErrInvalidRecord
	}
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	prepared := make(map[string]Record, len(records))
	for _, record := range records {
		if record.Kind != kind || !validRecord(record) {
			return ErrInvalidRecord
		}
		if _, duplicate := prepared[record.UID]; duplicate {
			return ErrInvalidRecord
		}
		prepared[record.UID] = cloneRecord(record)
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	kindCount := 0
	for _, current := range index.records {
		if current.Kind == kind {
			kindCount++
		}
	}
	newCount := len(index.records) - kindCount + len(prepared)
	if !complete {
		for uid := range prepared {
			if existing, ok := index.records[uid]; ok && existing.Kind != kind {
				return ErrInvalidRecord
			}
		}
		newCount = len(index.records)
		for uid := range prepared {
			if _, exists := index.records[uid]; !exists {
				newCount++
			}
		}
	}
	if newCount > index.maxRecords {
		return ErrRecordLimit
	}
	emitDeltas := index.kinds[kind].hasCompleteSync
	deltas := make([]Delta, 0, len(prepared))
	if complete {
		for uid, current := range index.records {
			if current.Kind == kind {
				if _, seen := prepared[uid]; !seen {
					delete(index.records, uid)
					if emitDeltas {
						previous := cloneRecord(current)
						deltas = append(deltas, Delta{Kind: kind, Op: "delete", Old: &previous})
					}
				}
			}
		}
	}
	for uid, record := range prepared {
		current, exists := index.records[uid]
		index.records[uid] = record
		if emitDeltas && (!exists || !reflect.DeepEqual(current, record)) {
			var previous *Record
			if exists {
				copy := cloneRecord(current)
				previous = &copy
			}
			updated := cloneRecord(record)
			deltas = append(deltas, Delta{Kind: kind, Op: "upsert", Old: previous, New: &updated})
		}
	}
	status := index.kinds[kind]
	status.State = map[bool]string{true: "ready", false: "partial"}[complete]
	status.LastAttemptAt = observedAt.UTC()
	if complete || status.ObservedAt.IsZero() {
		status.ObservedAt = observedAt.UTC()
	}
	status.Records = kindRecordCount(index.records, kind)
	status.Partial = !complete
	status.Error = ""
	status.hasCompleteSync = status.hasCompleteSync || complete
	index.kinds[kind] = status
	for _, delta := range deltas {
		index.publishLocked(delta)
	}
	return nil
}

// Subscribe returns a bounded stream of index deltas and an idempotent cancel function.
func (index *Index) Subscribe(buffer int) (<-chan Delta, func()) {
	if buffer <= 0 || buffer > 1024 {
		buffer = 64
	}
	if index == nil {
		closed := make(chan Delta)
		close(closed)
		return closed, func() {}
	}
	index.mu.Lock()
	index.nextSubscriber++
	id := index.nextSubscriber
	channel := make(chan Delta, buffer)
	index.subscribers[id] = channel
	index.mu.Unlock()
	var once sync.Once
	return channel, func() {
		once.Do(func() {
			index.mu.Lock()
			if subscribed, exists := index.subscribers[id]; exists {
				delete(index.subscribers, id)
				close(subscribed)
			}
			index.mu.Unlock()
		})
	}
}

func (index *Index) publishLocked(delta Delta) {
	for _, subscriber := range index.subscribers {
		select {
		case subscriber <- delta:
		default:
			for len(subscriber) > 0 {
				<-subscriber
			}
			subscriber <- Delta{Kind: delta.Kind, Resync: true}
		}
	}
}

// Refresh loads one kind at most once per interval. Concurrent readers wait
// for the same bounded load instead of issuing duplicate cluster requests.
func (index *Index) Refresh(ctx context.Context, kind string, interval time.Duration, now time.Time, load Loader) error {
	if index == nil || !validText(kind, 128) || interval < 0 || load == nil {
		return ErrInvalidRecord
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	index.mu.Lock()
	status := index.kinds[kind]
	if interval > 0 && !status.LastAttemptAt.IsZero() && now.Sub(status.LastAttemptAt) < interval {
		index.mu.Unlock()
		return nil
	}
	if running := index.refreshes[kind]; running != nil {
		index.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-running.done:
			return running.err
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	index.refreshes[kind] = call
	index.mu.Unlock()

	records, complete, err := load(ctx)
	if err == nil {
		err = index.Ingest(kind, records, complete, now)
	}
	index.mu.Lock()
	if err != nil {
		status = index.kinds[kind]
		status.State = "error"
		status.LastAttemptAt = now.UTC()
		status.Partial = true
		status.Error = "sync_failed"
		status.Records = kindRecordCount(index.records, kind)
		index.kinds[kind] = status
	}
	call.err = err
	delete(index.refreshes, kind)
	close(call.done)
	index.mu.Unlock()
	return err
}

// Status returns bounded freshness information for the current indexed view.
func (index *Index) Status(now time.Time) Status {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	snapshot := index.Snapshot()
	result := Status{State: "warming", Kinds: snapshot.Kinds}
	oldest := time.Time{}
	for _, kind := range snapshot.Kinds {
		if kind.Partial {
			result.Partial = true
		}
		if kind.State == "error" {
			result.State = "error"
		} else if kind.Partial && result.State != "error" {
			result.State = "partial"
		} else if kind.State == "ready" && result.State == "warming" {
			result.State = "ready"
		}
		if !kind.ObservedAt.IsZero() && (oldest.IsZero() || kind.ObservedAt.Before(oldest)) {
			oldest = kind.ObservedAt
		}
	}
	if !oldest.IsZero() {
		result.AgeSeconds = now.Sub(oldest).Seconds()
		if result.AgeSeconds < 0 {
			result.AgeSeconds = 0
		}
	}
	return result
}

func (index *Index) Snapshot() Snapshot {
	if index == nil {
		return Snapshot{Records: []Record{}, Kinds: map[string]KindStatus{}}
	}
	index.mu.RLock()
	defer index.mu.RUnlock()
	records := make([]Record, 0, len(index.records))
	for _, record := range index.records {
		records = append(records, cloneRecord(record))
	}
	sort.Slice(records, func(left, right int) bool {
		if records[left].Kind != records[right].Kind {
			return records[left].Kind < records[right].Kind
		}
		if records[left].Namespace != records[right].Namespace {
			return records[left].Namespace < records[right].Namespace
		}
		return records[left].Name < records[right].Name
	})
	kinds := make(map[string]KindStatus, len(index.kinds))
	for kind, status := range index.kinds {
		kinds[kind] = status
	}
	return Snapshot{Records: records, Kinds: kinds}
}

func (index *Index) KindStatus(kind string) (KindStatus, bool) {
	if index == nil {
		return KindStatus{}, false
	}
	index.mu.RLock()
	defer index.mu.RUnlock()
	status, exists := index.kinds[kind]
	return status, exists
}

func validRecord(record Record) bool {
	if !validText(record.UID, 256) || !validText(record.Kind, 128) || !validText(record.Name, 253) || !validOptionalText(record.Namespace, 253) || !validOptionalText(record.ResourceVersion, 256) || !validOptionalText(record.Phase, 128) || !validOptionalText(record.EventType, 128) || !validOptionalText(record.NodeName, 253) || !validOptionalText(record.Health, 128) {
		return false
	}
	if len(record.Labels) > maxLabels || len(record.Selector) > maxLabels || len(record.References) > 8 || len(record.Summary) > maxSummaryFields || len(record.Owners) > maxOwners || len(record.Images) > maxImages {
		return false
	}
	for key, value := range record.Labels {
		if !validText(key, 253) || len(value) > 256 {
			return false
		}
	}
	for key, value := range record.Selector {
		if !validText(key, 253) || len(value) > 256 {
			return false
		}
	}
	for key, names := range record.References {
		if !validText(key, 128) || len(names) > 32 {
			return false
		}
		for _, name := range names {
			if !validText(name, 253) {
				return false
			}
		}
	}
	for key, value := range record.Summary {
		if !validText(key, 128) || len(value) > 512 || strings.ContainsAny(value, "\x00\r\n") {
			return false
		}
	}
	for _, owner := range record.Owners {
		if !validText(owner.Kind, 128) || !validOptionalText(owner.Namespace, 253) || !validText(owner.Name, 253) || !validOptionalText(owner.UID, 256) {
			return false
		}
	}
	if target := record.Target; target != nil && (!validText(target.Kind, 128) || !validOptionalText(target.Namespace, 253) || !validText(target.Name, 253) || !validOptionalText(target.UID, 256)) {
		return false
	}
	for _, image := range record.Images {
		if len(image) > 512 {
			return false
		}
	}
	return estimatedRecordBytes(record) <= maxRecordBytes
}

func estimatedRecordBytes(record Record) int {
	size := len(record.UID) + len(record.Kind) + len(record.Namespace) + len(record.Name) + len(record.ResourceVersion) + len(record.Phase) + len(record.EventType) + len(record.NodeName) + len(record.Health) + 128
	for key, value := range record.Labels {
		size += len(key) + len(value) + 64
	}
	for key, value := range record.Selector {
		size += len(key) + len(value) + 64
	}
	for key, value := range record.Summary {
		size += len(key) + len(value) + 64
	}
	for key, names := range record.References {
		size += len(key) + 64
		for _, name := range names {
			size += len(name) + 32
		}
	}
	for _, owner := range record.Owners {
		size += len(owner.Kind) + len(owner.Namespace) + len(owner.Name) + len(owner.UID) + 64
	}
	for _, image := range record.Images {
		size += len(image) + 32
	}
	if record.Target != nil {
		size += len(record.Target.Kind) + len(record.Target.Namespace) + len(record.Target.Name) + len(record.Target.UID) + 64
	}
	return size
}

func kindRecordCount(records map[string]Record, kind string) int {
	count := 0
	for _, record := range records {
		if record.Kind == kind {
			count++
		}
	}
	return count
}

func cloneRecord(record Record) Record {
	if record.Labels != nil {
		labels := make(map[string]string, len(record.Labels))
		for key, value := range record.Labels {
			labels[key] = value
		}
		record.Labels = labels
	}
	if record.Summary != nil {
		summary := make(map[string]string, len(record.Summary))
		for key, value := range record.Summary {
			summary[key] = value
		}
		record.Summary = summary
	}
	if record.Selector != nil {
		selector := make(map[string]string, len(record.Selector))
		for key, value := range record.Selector {
			selector[key] = value
		}
		record.Selector = selector
	}
	if record.References != nil {
		references := make(map[string][]string, len(record.References))
		for key, names := range record.References {
			references[key] = append([]string(nil), names...)
		}
		record.References = references
	}
	record.Owners = append([]OwnerRef(nil), record.Owners...)
	record.Images = append([]string(nil), record.Images...)
	if record.Target != nil {
		target := *record.Target
		record.Target = &target
	}
	return record
}

func validText(value string, maxBytes int) bool {
	return value != "" && len(value) <= maxBytes && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func validOptionalText(value string, maxBytes int) bool {
	return value == "" || validText(value, maxBytes)
}
