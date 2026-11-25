package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	"github.com/VersusControl/versus-incident/pkg/logging"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// System reads are fixed, read-only cluster reads made by a background worker
// that has no user behind it. They are a separate door from every viewer
// read: the handle is created once per process by the program that wires the
// worker, is never stored on a Service or ServiceRegistry, and decides on its
// own, from the cluster's saved learning setting, what it may read. It never
// builds a caller decision, and it refuses any context that carries one, so a
// wiring mistake on a request path cannot drive it.
//
// The resource IDs, paths and selectors below are written here and nowhere
// else; the caller names a cluster and nothing more.

const (
	// systemMetricsResponseDefault is the largest Pod metrics body read when
	// the program does not lower it. metrics-server answers a cluster-wide
	// list in one body of about 700 bytes a Pod.
	systemMetricsResponseDefault = int64(16 << 20)
	// systemMetricsResponseCeiling is the most the program may set. It is a
	// hard limit, not a default.
	systemMetricsResponseCeiling = int64(32 << 20)
	// systemMetricsPodCeiling is the most Pods one metrics read returns.
	// metrics-server ignores the list limit, so a larger cluster-wide answer
	// is listed again namespace by namespace.
	systemMetricsPodCeiling = 5000
	// systemMetricsContainerCeiling bounds the containers kept for one Pod.
	systemMetricsContainerCeiling = maxProjectionItems
	// systemReadNamespacesPerCall is the most namespaces one call lists one
	// by one; the rest are listed by the next calls, in turn.
	systemReadNamespacesPerCall = 50
	// systemReadSettingsMaxAge is how long a loaded learning setting is
	// trusted before it is read from storage again.
	systemReadSettingsMaxAge = 30 * time.Second
	// systemReadSettingsWait is how long a read waits for the saved setting
	// before it refuses as settings_unavailable.
	systemReadSettingsWait = 5 * time.Second
	// systemReadTimeout bounds one request of a system read.
	systemReadTimeout = 20 * time.Second
	// systemReadCallTimeout bounds one whole call, every namespace included.
	// The caller's context may end it sooner, never later.
	systemReadCallTimeout = 60 * time.Second
	// systemEventRequests bounds the requests one namespace's event list may
	// make; the list itself is capped by listAllEvents.
	systemEventRequests = 8
)

const (
	systemPodMetricsResource = "metrics.k8s.io~v1beta1~pods"
	systemPodMetricsBase     = "/apis/metrics.k8s.io/v1beta1"
	systemEventsResource     = "core~v1~events"
)

// systemEventsDefinition is the only resource the event read lists. It is
// pinned so the read never walks discovery.
var systemEventsDefinition = ResourceDefinition{ID: systemEventsResource, Version: "v1", Resource: "events", Kind: "Event", Namespaced: true, Verbs: []string{"list"}, Available: true}

// SystemReadLane names the learning toggle that admits a system read.
type SystemReadLane string

// SystemReadLaneMetrics is admitted by the saved Learning.Metrics toggle. It
// covers the Pod metrics and the Warning event reads.
const SystemReadLaneMetrics SystemReadLane = "metrics"

// SystemReadPolicy is the program's own say over a system read. It is asked
// after the saved learning setting has admitted the read, so it can only
// refuse: first with an empty namespace, for the cluster as a whole before
// any request is made, then once for each namespace the saved scope allows.
// It is never asked about a namespace the saved scope refuses. Returning
// false, or panicking, refuses.
type SystemReadPolicy func(ctx context.Context, clusterID string, lane SystemReadLane, namespace string) bool

// SystemReadsOptions configures EnableSystemReads. Registry, Scrubber and
// Policy are required.
type SystemReadsOptions struct {
	// Registry resolves cluster IDs. Clusters are read under the
	// deployment's own organization, the registry's write organization.
	Registry *ServiceRegistry
	// Scrubber removes secrets from every text field a read returns.
	Scrubber Scrubber
	// Policy may refuse any read the saved setting allows.
	Policy SystemReadPolicy
	// MetricsResponseLimit lowers the largest Pod metrics body accepted.
	// Zero keeps the 16 MiB default; it can never be raised above 32 MiB.
	MetricsResponseLimit int64
}

// Refusal reasons a SystemReadError carries when a read was refused before
// or instead of asking the cluster.
const (
	SystemReadRefusedNotEnabled    = "not_enabled"
	SystemReadRefusedCallerContext = "caller_context"
	SystemReadRefusedNoScrubber    = "no_scrubber"
	SystemReadRefusedUnknown       = "unknown_cluster"
	SystemReadRefusedSettings      = "settings_unavailable"
	SystemReadRefusedLaneOff       = "lane_off"
	SystemReadRefusedOutOfScope    = "namespace_out_of_scope"
	SystemReadRefusedPolicy        = "policy"
	SystemReadRefusedExcluded      = "resource_excluded"
	SystemReadRefusedCredential    = "credential"
	// SystemReadRefusedScrubberFailed is a read whose scrubber panicked; no
	// text from it is returned.
	SystemReadRefusedScrubberFailed = "scrubber_failed"
)

// Cause classes of a SystemReadError. Only the class leaves the package,
// never an API response body.
const (
	SystemReadRefused     = "refused"
	SystemReadForbidden   = "forbidden"
	SystemReadUnavailable = "unavailable"
	SystemReadTimeout     = "timeout"
	SystemReadTooLarge    = "too_large"
	// SystemReadMalformed is an answer that is not a valid list, or that
	// nests deeper than the read accepts.
	SystemReadMalformed = "malformed"
)

// SystemReadError is the only error a system read returns. Class is one of
// the cause classes; Reason says why a refused read was refused. A refusal
// and a forbidden answer match ErrForbidden, too_large matches
// ErrResponseTooLarge, a timeout matches context.DeadlineExceeded, not_found
// matches ErrNotFound and the rest match ErrClusterUnavailable.
type SystemReadError struct {
	Class  string
	Reason string
}

func (err *SystemReadError) Error() string {
	if err.Reason != "" {
		return "kubernetes: system read " + err.Class + " (" + err.Reason + ")"
	}
	return "kubernetes: system read " + err.Class
}

func (err *SystemReadError) Unwrap() error {
	switch err.Class {
	case SystemReadRefused, SystemReadForbidden:
		return ErrForbidden
	case SystemReadTooLarge:
		return ErrResponseTooLarge
	case SystemReadTimeout:
		return context.DeadlineExceeded
	case SystemReadNotFound:
		return ErrNotFound
	default:
		return ErrClusterUnavailable
	}
}

// SystemReadCounters is one cluster's, lane's and read's tally since the
// handle was enabled, for the worker's status record.
type SystemReadCounters struct {
	Cluster string         `json:"cluster"`
	Lane    SystemReadLane `json:"lane"`
	Read    string         `json:"read"`
	// Reads counts calls the gate admitted; Requests the API requests they
	// made.
	Reads    uint64 `json:"reads"`
	Requests uint64 `json:"requests"`
	// Refusals counts refused calls by reason.
	Refusals map[string]uint64 `json:"refusals,omitempty"`
	// DroppedByScope and DroppedByPolicy count items a cluster-wide list
	// returned from a namespace the saved scope or the policy refuses.
	DroppedByScope  uint64 `json:"dropped_by_scope"`
	DroppedByPolicy uint64 `json:"dropped_by_policy"`
	// CutShort counts lists that stopped at their item limit with more
	// left on the cluster.
	CutShort uint64 `json:"cut_short"`
	// NamespacesSkipped counts namespaces a call did not reach because it
	// stopped early.
	NamespacesSkipped uint64 `json:"namespaces_skipped"`
	// DroppedInvalid counts items left out as malformed.
	DroppedInvalid uint64 `json:"dropped_invalid"`
	// Causes counts failed reads, in full or in part, by cause class.
	Causes map[string]uint64 `json:"causes,omitempty"`
	// Bytes counts response bytes received.
	Bytes      int64     `json:"bytes"`
	LastListAt time.Time `json:"last_list_at,omitzero"`
	// LastCause is the cause class of the last read that failed in full or
	// in part; it clears when a read succeeds in full.
	LastCause string `json:"last_cause,omitempty"`
	// StreamsOpen counts log streams open now. Lines counts log lines
	// returned, Truncated the lines cut to the line limit, and Closes the
	// streams that ended, by close reason.
	StreamsOpen int               `json:"streams_open,omitempty"`
	Lines       uint64            `json:"lines,omitempty"`
	Truncated   uint64            `json:"truncated,omitempty"`
	Closes      map[string]uint64 `json:"closes,omitempty"`
}

// ContainerUsage is one container's usage in a Pod metrics sample.
type ContainerUsage struct {
	Name          string  `json:"name"`
	CPUMillicores float64 `json:"cpu_millicores"`
	MemoryBytes   float64 `json:"memory_bytes"`
}

// PodMetricsSample is one Pod's metrics-server sample.
type PodMetricsSample struct {
	Namespace  string           `json:"namespace"`
	Pod        string           `json:"pod"`
	Timestamp  time.Time        `json:"timestamp,omitzero"`
	Window     time.Duration    `json:"window"`
	Containers []ContainerUsage `json:"containers"`
}

// PodMetricsList is the answer of one Pod metrics read.
type PodMetricsList struct {
	Pods       []PodMetricsSample `json:"pods"`
	ObservedAt time.Time          `json:"observed_at"`
	// PerNamespace is set when the read listed namespace by namespace.
	PerNamespace bool `json:"per_namespace,omitempty"`
	// NamespacesSkipped counts allowed namespaces left for a later call;
	// NamespacesFailed counts namespaces whose list failed.
	NamespacesSkipped int `json:"namespaces_skipped,omitempty"`
	NamespacesFailed  int `json:"namespaces_failed,omitempty"`
	// Truncated is set when the Pod ceiling cut the answer short.
	Truncated bool `json:"truncated,omitempty"`
	// Invalid counts samples left out because they were malformed.
	Invalid int `json:"invalid,omitempty"`
	// Partial is set when the call stopped early, on a throttle, a timeout or
	// its time limit; the namespaces it did not reach are in
	// NamespacesSkipped.
	Partial bool `json:"partial,omitempty"`
}

// WarningEvent is one Warning event. Every text field has been checked or
// scrubbed: reference fields are valid Kubernetes values passed through the
// scrubber, and Reason and Message are scrubbed. The reference fields are
// display text for status, findings and prompts. They must not drive another
// read or an access decision: the scrubber may have changed them, and anyone
// allowed to create events in a namespace can write them.
type WarningEvent struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	UID       string    `json:"uid,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Message   string    `json:"message,omitempty"`
	Count     int64     `json:"count,omitempty"`
	FirstSeen time.Time `json:"first_seen,omitzero"`
	LastSeen  time.Time `json:"last_seen,omitzero"`
	Object    ObjectRef `json:"object"`
	Source    string    `json:"source,omitempty"`
}

// WarningEventList is the answer of one Warning event read.
type WarningEventList struct {
	Events            []WarningEvent `json:"events"`
	ObservedAt        time.Time      `json:"observed_at"`
	PerNamespace      bool           `json:"per_namespace,omitempty"`
	NamespacesSkipped int            `json:"namespaces_skipped,omitempty"`
	NamespacesFailed  int            `json:"namespaces_failed,omitempty"`
	Truncated         bool           `json:"truncated,omitempty"`
	// Invalid counts events left out because a reference field was not a
	// valid Kubernetes value.
	Invalid int `json:"invalid,omitempty"`
	// Partial is set when the call stopped early; see PodMetricsList.
	Partial bool `json:"partial,omitempty"`
}

// systemReadsEnabled is set by the one EnableSystemReads call a process may
// make.
var systemReadsEnabled atomic.Bool

// SystemReads is the handle for gated system reads. Only EnableSystemReads
// makes a working one; the zero value and a nil pointer refuse every call.
type SystemReads struct {
	enabled      bool
	registry     *ServiceRegistry
	orgID        string
	scrubber     Scrubber
	policy       SystemReadPolicy
	metricsLimit int64
	// callTimeout is systemReadCallTimeout; tests shorten it.
	callTimeout time.Duration
	now         func() time.Time
	// warn writes a throttled warning keyed by what failed, with its cause
	// class: a refused named namespace, or a read the cluster forbids. clear
	// ends a key's failure once the read succeeds.
	// by cluster and namespace.
	warn  func(key, cause, format string, args ...any)
	clear func(key string)

	// logLimits bounds log reads; tests lower them.
	logLimits systemLogLimits
	// newLogPace makes one stream's byte rate limiter; tests replace it.
	newLogPace func() systemLogPace

	// settingsWait bounds how long a read waits for the saved setting.
	settingsWait time.Duration

	mu         sync.Mutex
	logStreams systemLogStreams
	settings   map[string]systemReadSettings
	loading    map[string]*systemSettingsLoad
	rotation   map[systemReadKey]int
	counters   map[systemReadKey]*SystemReadCounters
}

type systemReadSettings struct {
	learning breaklogs.Learning
	loadedAt time.Time
}

type systemReadKey struct {
	cluster string
	lane    SystemReadLane
	read    string
}

const (
	systemReadPodMetrics    = "pod_metrics"
	systemReadWarningEvents = "warning_events"
)

// EnableSystemReads returns the process's system read handle. It succeeds
// once per process; a later call returns an error. The program that wires the
// worker passes the handle to it by hand; nothing in this package keeps or
// hands it out.
func EnableSystemReads(options SystemReadsOptions) (*SystemReads, error) {
	if options.Registry == nil {
		return nil, errors.New("kubernetes: system reads need a cluster registry")
	}
	if scrubberMissing(options.Scrubber) {
		return nil, errors.New("kubernetes: system reads need a scrubber")
	}
	if options.Policy == nil {
		return nil, errors.New("kubernetes: system reads need a policy")
	}
	limit := options.MetricsResponseLimit
	if limit == 0 {
		limit = systemMetricsResponseDefault
	}
	if limit < 0 || limit > systemMetricsResponseCeiling {
		return nil, fmt.Errorf("kubernetes: system read metrics response limit must be between 1 and %d bytes", systemMetricsResponseCeiling)
	}
	if !systemReadsEnabled.CompareAndSwap(false, true) {
		return nil, errors.New("kubernetes: system reads are already enabled in this process")
	}
	throttle := logging.NewThrottle(0, nil)
	return &SystemReads{
		enabled:      true,
		registry:     options.Registry,
		orgID:        options.Registry.bootScope.Normalized().Write,
		scrubber:     options.Scrubber,
		policy:       options.Policy,
		metricsLimit: limit,
		callTimeout:  systemReadCallTimeout,
		now:          time.Now,
		warn: func(key, cause, format string, args ...any) {
			throttle.Fail(slog.LevelWarn, key, cause, format, args...)
		},
		clear: func(key string) {
			throttle.Clear(key, "kubernetes: system read %s recovered", key)
		},
		logLimits:    defaultSystemLogLimits(),
		newLogPace:   newSystemLogPace,
		logStreams:   systemLogStreams{perCluster: map[string]int{}},
		settings:     map[string]systemReadSettings{},
		loading:      map[string]*systemSettingsLoad{},
		settingsWait: systemReadSettingsWait,
		rotation:     map[systemReadKey]int{},
		counters:     map[systemReadKey]*SystemReadCounters{},
	}, nil
}

// scrubberMissing reports a nil scrubber, including a typed nil pointer.
func scrubberMissing(scrubber Scrubber) bool {
	if scrubber == nil {
		return true
	}
	value := reflect.ValueOf(scrubber)
	switch value.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface, reflect.Chan:
		return value.IsNil()
	}
	return false
}

// Counters returns a copy of the tallies, ordered by cluster, lane and read.
func (reads *SystemReads) Counters() []SystemReadCounters {
	if reads == nil || !reads.enabled {
		return nil
	}
	reads.mu.Lock()
	defer reads.mu.Unlock()
	result := make([]SystemReadCounters, 0, len(reads.counters))
	for _, counter := range reads.counters {
		copied := *counter
		if counter.Refusals != nil {
			copied.Refusals = make(map[string]uint64, len(counter.Refusals))
			maps.Copy(copied.Refusals, counter.Refusals)
		}
		if counter.Causes != nil {
			copied.Causes = make(map[string]uint64, len(counter.Causes))
			maps.Copy(copied.Causes, counter.Causes)
		}
		if counter.Closes != nil {
			copied.Closes = make(map[string]uint64, len(counter.Closes))
			maps.Copy(copied.Closes, counter.Closes)
		}
		result = append(result, copied)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Cluster != result[right].Cluster {
			return result[left].Cluster < result[right].Cluster
		}
		if result[left].Lane != result[right].Lane {
			return result[left].Lane < result[right].Lane
		}
		return result[left].Read < result[right].Read
	})
	return result
}

// systemAdmission is a read the gate admitted: the resolved cluster and its
// saved scope, with the policy's per-namespace answers for this call.
type systemAdmission struct {
	key     systemReadKey
	service *Service
	scope   breaklogs.Scope
	policy  map[string]bool
}

// systemTally collects what one call did, for the counters.
type systemTally struct {
	requests        uint64
	bytes           int64
	droppedByScope  uint64
	droppedByPolicy uint64
	cutShort        uint64
	skipped         uint64
	invalid         uint64
	listed          bool
	cause           string
}

func (tally *systemTally) fail(err error) string {
	tally.cause = systemReadCause(err)
	return tally.cause
}

// admit runs the gate every system read passes before it may ask the
// cluster: an enabled handle, no caller decision on the context, a scrubber,
// a known cluster that is not held for credential recovery, the saved lane
// toggle, the policy for the cluster as a whole, and a credential that is
// still accepted.
func (reads *SystemReads) admit(ctx context.Context, clusterID string, lane SystemReadLane, read string) (*systemAdmission, error) {
	if reads == nil || !reads.enabled || reads.registry == nil || reads.policy == nil {
		return nil, &SystemReadError{Class: SystemReadRefused, Reason: SystemReadRefusedNotEnabled}
	}
	key := systemReadKey{cluster: reads.countedCluster(clusterID), lane: lane, read: read}
	if core.CallerAuthorizationPresent(ctx) {
		return nil, reads.refuse(key, SystemReadRefusedCallerContext)
	}
	if scrubberMissing(reads.scrubber) {
		return nil, reads.refuse(key, SystemReadRefusedNoScrubber)
	}
	if clusterID == "" || len(clusterID) > 253 {
		return nil, reads.refuse(key, SystemReadRefusedUnknown)
	}
	// The current service is used as it is: a retired credential is not
	// rebuilt here, so a system read never starts credential recovery.
	service, err := reads.registry.LocalReportService(reads.orgID, clusterID)
	if err != nil {
		if errors.Is(err, ErrClusterNotFound) || errors.Is(err, ErrClusterRequired) {
			return nil, reads.refuse(key, SystemReadRefusedUnknown)
		}
		return nil, reads.unavailable(key, SystemReadRefusedCredential)
	}
	if service.recovery.requestGateError(reads.orgID) != nil {
		return nil, reads.unavailable(key, SystemReadRefusedCredential)
	}
	// A log read keeps its stream open, so it opens only on a setting as
	// fresh as the stream's own recheck would read.
	maxAge := systemReadSettingsMaxAge
	if lane == SystemReadLaneLogs {
		maxAge = reads.logLimits.recheck
	}
	learning, err := reads.learningWithin(service, clusterID, maxAge)
	if err != nil {
		return nil, reads.refuse(key, SystemReadRefusedSettings)
	}
	if !laneEnabled(learning, lane) {
		return nil, reads.refuse(key, SystemReadRefusedLaneOff)
	}
	if !reads.askPolicy(ctx, clusterID, lane, "") {
		return nil, reads.refuse(key, SystemReadRefusedPolicy)
	}
	if err := service.validateCredential(ctx); err != nil {
		return nil, reads.unavailable(key, SystemReadRefusedCredential)
	}
	return &systemAdmission{key: key, service: service, scope: learning.Scope, policy: map[string]bool{}}, nil
}

func laneEnabled(learning breaklogs.Learning, lane SystemReadLane) bool {
	switch lane {
	case SystemReadLaneMetrics:
		return learning.Metrics
	case SystemReadLaneLogs:
		return learning.Logs
	default:
		return false
	}
}

// countedCluster keys counters by a cluster the registry knows; any other ID
// shares one bucket, so a caller cannot grow the counters with made-up IDs.
func (reads *SystemReads) countedCluster(clusterID string) string {
	for _, cluster := range reads.registry.Clusters() {
		if cluster.ID == clusterID && clusterID != "" {
			return clusterID
		}
	}
	return ""
}

// learning returns the cluster's saved learning setting, read from storage at
// most every systemReadSettingsMaxAge. A failed load is never cached.
func (reads *SystemReads) learning(service *Service, clusterID string) (breaklogs.Learning, error) {
	return reads.learningWithin(service, clusterID, systemReadSettingsMaxAge)
}

// learningWithin is learning with a shorter trust period, for an open stream
// that must see a change sooner.
func (reads *SystemReads) learningWithin(service *Service, clusterID string, maxAge time.Duration) (breaklogs.Learning, error) {
	maxAge = min(maxAge, systemReadSettingsMaxAge)
	now := reads.now()
	reads.mu.Lock()
	cached, ok := reads.settings[clusterID]
	reads.mu.Unlock()
	if age := now.Sub(cached.loadedAt); ok && age >= 0 && age < maxAge {
		return cached.learning, nil
	}
	// The store read takes no context, so it runs on its own and is waited
	// for a bounded time: a store that does not answer refuses the read
	// instead of holding it. At most one read per cluster is in flight; a
	// caller that finds one waits for it rather than starting another.
	reads.mu.Lock()
	load := reads.loading[clusterID]
	if load == nil {
		load = &systemSettingsLoad{done: make(chan struct{}), startedAt: now}
		reads.loading[clusterID] = load
		provider, key := service.logSettingsStore()
		go reads.loadSettings(clusterID, provider, key, now, load)
	} else {
		load.joined++
	}
	reads.mu.Unlock()
	timer := time.NewTimer(reads.settingsWait)
	defer timer.Stop()
	select {
	case <-load.done:
		// A read joined late may get an answer that started before its own
		// trust period; it refuses that answer, as it would a cached one.
		if load.err == nil && now.Sub(load.startedAt) >= maxAge {
			return breaklogs.Learning{}, errSystemSettingsSlow
		}
		return load.learning, load.err
	case <-timer.C:
		return breaklogs.Learning{}, errSystemSettingsSlow
	}
}

// errSystemSettingsSlow is a settings load that did not answer in time.
var errSystemSettingsSlow = errors.New("kubernetes: learning settings did not load in time")

// systemSettingsLoad is one settings read in flight for a cluster.
type systemSettingsLoad struct {
	done      chan struct{}
	startedAt time.Time
	// joined counts the reads that waited on this load instead of starting
	// one; guarded by SystemReads.mu.
	joined   int
	learning breaklogs.Learning
	err      error
}

// loadSettings reads one cluster's setting, caches a success and ends the
// in-flight read. A failed load is never cached.
func (reads *SystemReads) loadSettings(clusterID string, provider storage.Provider, key string, now time.Time, load *systemSettingsLoad) {
	state, err := breaklogs.LoadSettings(provider, key)
	reads.mu.Lock()
	defer reads.mu.Unlock()
	delete(reads.loading, clusterID)
	if err != nil {
		delete(reads.settings, clusterID)
		load.err = err
	} else {
		reads.settings[clusterID] = systemReadSettings{learning: state.Settings.Learning, loadedAt: now}
		load.learning = state.Settings.Learning
	}
	close(load.done)
}

func (reads *SystemReads) askPolicy(ctx context.Context, clusterID string, lane SystemReadLane, namespace string) bool {
	if ctx.Err() != nil {
		return false
	}
	answer := make(chan bool, 1)
	go func() {
		allowed := false
		defer func() {
			_ = recover()
			answer <- allowed
		}()
		allowed = reads.policy(ctx, clusterID, lane, namespace)
	}()
	// A policy that does not answer before the call's time limit refuses.
	select {
	case allowed := <-answer:
		return allowed
	case <-ctx.Done():
		return false
	}
}

// allowNamespace decides one namespace for an admitted read: the saved scope
// first, then the policy, which is never asked about a namespace the scope
// refuses. named is set when the caller asked for this namespace itself; a
// refusal of a named namespace is a worker bug and gets one warning.
func (reads *SystemReads) allowNamespace(ctx context.Context, admission *systemAdmission, namespace string, named bool) (bool, string) {
	if !admission.scope.Allows(namespace) {
		if named {
			reads.warn(admission.key.cluster+"\x00"+string(admission.key.lane)+"\x00"+namespace, SystemReadRefusedOutOfScope, "kubernetes: cluster %s refused a system read in namespace %s (out of scope); the learning scope saved for the cluster excludes it, check which namespaces the worker asks for", admission.key.cluster, namespace)
		}
		return false, SystemReadRefusedOutOfScope
	}
	allowed, asked := admission.policy[namespace]
	if !asked {
		allowed = reads.askPolicy(ctx, admission.key.cluster, admission.key.lane, namespace)
		admission.policy[namespace] = allowed
	}
	if !allowed {
		return false, SystemReadRefusedPolicy
	}
	return true, ""
}

func (reads *SystemReads) counterLocked(key systemReadKey) *SystemReadCounters {
	counter := reads.counters[key]
	if counter == nil {
		counter = &SystemReadCounters{Cluster: key.cluster, Lane: key.lane, Read: key.read}
		reads.counters[key] = counter
	}
	return counter
}

func (reads *SystemReads) refuse(key systemReadKey, reason string) error {
	reads.countRefusal(key, reason)
	logging.Debugf("kubernetes: cluster %s system read %s refused (%s)", key.cluster, key.read, reason)
	return &SystemReadError{Class: SystemReadRefused, Reason: reason}
}

func (reads *SystemReads) unavailable(key systemReadKey, reason string) error {
	reads.countRefusal(key, reason)
	logging.Debugf("kubernetes: cluster %s system read %s not made (%s)", key.cluster, key.read, reason)
	return &SystemReadError{Class: SystemReadUnavailable, Reason: reason}
}

func (reads *SystemReads) countRefusal(key systemReadKey, reason string) {
	reads.mu.Lock()
	defer reads.mu.Unlock()
	counter := reads.counterLocked(key)
	if counter.Refusals == nil {
		counter.Refusals = map[string]uint64{}
	}
	counter.Refusals[reason]++
}

// finish records an admitted call. err is the call's own failure, if any.
func (reads *SystemReads) finish(admission *systemAdmission, tally systemTally, err error) error {
	reads.mu.Lock()
	counter := reads.counterLocked(admission.key)
	counter.Reads++
	counter.Requests += tally.requests
	counter.Bytes += tally.bytes
	counter.DroppedByScope += tally.droppedByScope
	counter.DroppedByPolicy += tally.droppedByPolicy
	counter.CutShort += tally.cutShort
	counter.NamespacesSkipped += tally.skipped
	counter.DroppedInvalid += tally.invalid
	if tally.cause != "" {
		if counter.Causes == nil {
			counter.Causes = map[string]uint64{}
		}
		counter.Causes[tally.cause]++
	}
	if tally.listed {
		counter.LastListAt = reads.now().UTC()
	}
	counter.LastCause = tally.cause
	reads.mu.Unlock()
	readKey := admission.key.cluster + "/" + admission.key.read
	switch tally.cause {
	case SystemReadForbidden:
		verb, resource := systemReadAccess(admission.key.read)
		reads.warn(readKey, SystemReadForbidden, "kubernetes: cluster %s cannot %s %s for learning (forbidden); grant %s on %s to the reader role, or turn off %s learning for the cluster", admission.key.cluster, verb, resource, verb, resource, admission.key.lane)
	case SystemReadUnauthorized:
		reads.warn(readKey, SystemReadUnauthorized, "kubernetes: cluster %s refused a Pod log read for learning (unauthorized); the kubelet of the Pod's node may be refusing the API server, check the kubelet's client certificate settings on that node", admission.key.cluster)
	case "":
		reads.clear(readKey)
	}
	if err != nil {
		class := systemReadCause(err)
		logging.Debugf("kubernetes: cluster %s system read %s failed (%s)", admission.key.cluster, admission.key.read, class)
		return &SystemReadError{Class: class}
	}
	logging.Debugf("kubernetes: cluster %s system read %s requests=%d bytes=%d dropped=%d", admission.key.cluster, admission.key.read, tally.requests, tally.bytes, tally.droppedByScope+tally.droppedByPolicy)
	return nil
}

// systemReadAccess names the verb and Kubernetes resource a read needs, for
// the operator.
func systemReadAccess(read string) (verb, resource string) {
	switch read {
	case systemReadPodMetrics:
		return "list", "pods.metrics.k8s.io"
	case systemReadPodLog, systemReadPreviousPodLog:
		return "get", "pods/log"
	default:
		return "list", "events"
	}
}

// systemReadCause reduces an error to its cause class.
func systemReadCause(err error) string {
	var typed *SystemReadError
	var network net.Error
	switch {
	case err == nil:
		return ""
	case errors.As(err, &typed):
		return typed.Class
	case errors.Is(err, ErrForbidden):
		return SystemReadForbidden
	case errors.Is(err, ErrResponseTooLarge):
		return SystemReadTooLarge
	case errors.Is(err, errInvalidMetricsList):
		return SystemReadMalformed
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrOperationBudget), errors.As(err, &network) && network.Timeout():
		return SystemReadTimeout
	default:
		return SystemReadUnavailable
	}
}

// systemNamespacePlan says how one call lists: cluster-wide, or one
// namespace at a time over namespaces (already decided and rotated).
type systemNamespacePlan struct {
	clusterWide bool
	namespaces  []string
	skipped     int
	// start and size place namespaces in the sorted allowed list, so a
	// call that stops early can move the next call's start past where it
	// stopped.
	start, size int
	// known are the cluster's namespaces from the published inventory, used
	// when a cluster-wide answer is too large and the read is repeated per
	// namespace.
	known []string
}

// plan picks the namespaces of one call. A non-empty include list is listed
// one namespace at a time, so excluded namespaces are never asked; so is an
// identity the inventory found to read only inside namespaces. Otherwise the
// read is cluster-wide and filtered here.
func (reads *SystemReads) plan(ctx context.Context, admission *systemAdmission) systemNamespacePlan {
	clusterWide, readable, known := systemReadableNamespaces(admission.service)
	switch {
	case len(admission.scope.Include) > 0 && clusterWide:
		return reads.perNamespace(ctx, admission, admission.scope.Include)
	case len(admission.scope.Include) > 0:
		both := []string{}
		for _, namespace := range admission.scope.Include {
			if slices.Contains(readable, namespace) {
				both = append(both, namespace)
			}
		}
		return reads.perNamespace(ctx, admission, both)
	case !clusterWide:
		return reads.perNamespace(ctx, admission, readable)
	default:
		return systemNamespacePlan{clusterWide: true, known: known}
	}
}

// perNamespace keeps the candidates the scope and policy allow, in name
// order, and takes at most systemReadNamespacesPerCall of them, starting
// where the previous call for this cluster and read stopped.
func (reads *SystemReads) perNamespace(ctx context.Context, admission *systemAdmission, candidates []string) systemNamespacePlan {
	allowed := []string{}
	seen := map[string]bool{}
	for _, namespace := range candidates {
		if seen[namespace] || !validPodLogName(namespace, 63, false) {
			continue
		}
		seen[namespace] = true
		if ok, _ := reads.allowNamespace(ctx, admission, namespace, false); ok {
			allowed = append(allowed, namespace)
		}
	}
	sort.Strings(allowed)
	if len(allowed) == 0 {
		return systemNamespacePlan{namespaces: allowed}
	}
	count := min(len(allowed), systemReadNamespacesPerCall)
	reads.mu.Lock()
	start := reads.rotation[admission.key] % len(allowed)
	reads.rotation[admission.key] = (start + count) % len(allowed)
	reads.mu.Unlock()
	taken := make([]string, 0, count)
	for offset := range count {
		taken = append(taken, allowed[(start+offset)%len(allowed)])
	}
	return systemNamespacePlan{namespaces: taken, skipped: len(allowed) - len(taken), start: start, size: len(allowed)}
}

// resumeAfter makes the next call start after the namespace at position in
// plan, the one that stopped this call: it is then asked last, so one
// namespace that always times out cannot starve the others.
func (reads *SystemReads) resumeAfter(admission *systemAdmission, plan systemNamespacePlan, position int) {
	if plan.size == 0 {
		return
	}
	reads.mu.Lock()
	reads.rotation[admission.key] = (plan.start + position + 1) % plan.size
	reads.mu.Unlock()
}

// systemReadableNamespaces reads the published inventory only, never the
// cluster: clusterWide is false with the readable namespaces for an identity
// bound inside namespaces; known lists the namespaces of a cluster-wide
// identity. With no inventory yet, the connector's configured namespaces
// stand for a namespace-bound identity.
func systemReadableNamespaces(service *Service) (clusterWide bool, readable, known []string) {
	var generation inventoryGeneration
	if service.changes != nil {
		service.changes.mu.Lock()
		cache := service.changes.inventories[service.indexScope()]
		service.changes.mu.Unlock()
		if cache != nil {
			cache.mu.RLock()
			generation = cache.generation
			cache.mu.RUnlock()
		}
	}
	if len(generation.View.Kinds) == 0 {
		if len(service.namespaces) > 0 {
			return false, append([]string(nil), service.namespaces...), nil
		}
		return true, nil, nil
	}
	clusterWide, readable = readableNamespaceScope(generation)
	if !clusterWide {
		return false, readable, nil
	}
	seen := map[string]bool{}
	for _, record := range generation.View.Records {
		name := record.Namespace
		if record.Kind == "Namespace" {
			name = record.Name
		}
		if name != "" && !seen[name] {
			seen[name] = true
			known = append(known, name)
		}
	}
	sort.Strings(known)
	return true, nil, known
}

// PodMetrics lists metrics-server Pod samples on clusterID, in the namespaces
// the saved learning scope and the policy allow.
func (reads *SystemReads) PodMetrics(ctx context.Context, clusterID string) (PodMetricsList, error) {
	if reads == nil || !reads.enabled {
		return PodMetricsList{}, &SystemReadError{Class: SystemReadRefused, Reason: SystemReadRefusedNotEnabled}
	}
	// The time limit covers the whole call, the gate included: the
	// settings load, the policy and the credential check.
	ctx, cancel := context.WithTimeout(ctx, reads.callTimeout)
	defer cancel()
	admission, err := reads.admit(ctx, clusterID, SystemReadLaneMetrics, systemReadPodMetrics)
	if err != nil {
		return PodMetricsList{}, err
	}
	if admission.service.resourceExcluded(systemPodMetricsResource) {
		return PodMetricsList{}, reads.refuse(admission.key, SystemReadRefusedExcluded)
	}
	var tally systemTally
	result := PodMetricsList{Pods: []PodMetricsSample{}}
	plan := reads.plan(ctx, admission)
	if plan.clusterWide {
		samples, oversized, invalid, err := reads.fetchPodMetrics(ctx, admission, "", &tally)
		if err == nil && !oversized {
			result.Invalid += invalid
			reads.keepPodMetrics(ctx, admission, "", samples, &result, &tally)
			result.ObservedAt = reads.now().UTC()
			return result, reads.finish(admission, tally, nil)
		}
		if err != nil && !errors.Is(err, ErrResponseTooLarge) {
			tally.fail(err)
			return PodMetricsList{}, reads.finish(admission, tally, err)
		}
		// One body would hold too many Pods: list namespace by namespace.
		plan = reads.perNamespace(ctx, admission, plan.known)
		if len(plan.namespaces) == 0 {
			err = ErrResponseTooLarge
			tally.fail(err)
			return PodMetricsList{}, reads.finish(admission, tally, err)
		}
	}
	result.PerNamespace = true
	result.NamespacesSkipped = plan.skipped
	var lastErr error
	attempted := 0
	for position, namespace := range plan.namespaces {
		if len(result.Pods) >= systemMetricsPodCeiling {
			result.Truncated = true
			result.NamespacesSkipped += len(plan.namespaces) - position
			break
		}
		if ctx.Err() != nil {
			result.Partial = true
			result.NamespacesSkipped += stopSkipping(&tally, len(plan.namespaces)-position)
			reads.resumeAfter(admission, plan, position-1)
			break
		}
		attempted++
		samples, oversized, invalid, err := reads.fetchPodMetrics(ctx, admission, namespace, &tally)
		if err != nil {
			result.NamespacesFailed++
			lastErr = err
			tally.fail(err)
			if systemReadStops(ctx, err) {
				result.Partial = true
				result.NamespacesSkipped += stopSkipping(&tally, len(plan.namespaces)-position-1)
				reads.resumeAfter(admission, plan, position)
				break
			}
			continue
		}
		result.Truncated = result.Truncated || oversized
		result.Invalid += invalid
		reads.keepPodMetrics(ctx, admission, namespace, samples, &result, &tally)
	}
	if lastErr != nil && result.NamespacesFailed == attempted {
		return PodMetricsList{}, reads.finish(admission, tally, lastErr)
	}
	result.ObservedAt = reads.now().UTC()
	return result, reads.finish(admission, tally, nil)
}

// keepPodMetrics adds the samples from allowed namespaces, up to the Pod
// ceiling. namespace is the namespace asked, or empty for a cluster-wide
// list; a sample from any other namespace is dropped.
func (reads *SystemReads) keepPodMetrics(ctx context.Context, admission *systemAdmission, namespace string, samples []PodMetricsSample, result *PodMetricsList, tally *systemTally) {
	for _, sample := range samples {
		if namespace != "" && sample.Namespace != namespace {
			tally.droppedByScope++
			continue
		}
		if ok, reason := reads.allowNamespace(ctx, admission, sample.Namespace, false); !ok {
			countDropped(tally, reason)
			continue
		}
		if len(result.Pods) >= systemMetricsPodCeiling {
			result.Truncated = true
			return
		}
		result.Pods = append(result.Pods, sample)
	}
}

// systemReadStops reports a failure after which the rest of a call would only
// wait: a throttle, a timeout, or the call's own time limit.
func systemReadStops(ctx context.Context, err error) bool {
	var throttled *ErrThrottled
	return ctx.Err() != nil || errors.As(err, &throttled) || systemReadCause(err) == SystemReadTimeout
}

// stopSkipping counts namespaces a stopped call leaves unread.
func stopSkipping(tally *systemTally, remaining int) int {
	tally.skipped += uint64(remaining)
	return remaining
}

func countDropped(tally *systemTally, reason string) {
	if reason == SystemReadRefusedPolicy {
		tally.droppedByPolicy++
		return
	}
	tally.droppedByScope++
}

// fetchPodMetrics makes the one metrics request for namespace (empty for the
// whole cluster). oversized reports an answer with more Pods than the
// ceiling, whether metrics-server ignored the limit or honoured it with a
// continuation.
func (reads *SystemReads) fetchPodMetrics(ctx context.Context, admission *systemAdmission, namespace string, tally *systemTally) ([]PodMetricsSample, bool, int, error) {
	service := admission.service
	path := systemPodMetricsBase + "/pods"
	if namespace != "" {
		if !validPodLogName(namespace, 63, false) {
			return nil, false, 0, ErrInvalidArguments
		}
		path = systemPodMetricsBase + "/namespaces/" + url.PathEscape(namespace) + "/pods"
	}
	path += "?limit=" + strconv.Itoa(systemMetricsPodCeiling)
	if service.client == nil {
		return nil, false, 0, ErrClusterUnavailable
	}
	if err := service.validateCredential(ctx); err != nil {
		return nil, false, 0, err
	}
	if err := service.client.systemThrottle(); err != nil {
		return nil, false, 0, err
	}
	bound, cancelIdentity := service.credentialContext(ctx)
	defer cancelIdentity()
	bound, cancel := withOperationBudget(systemRead(bound, reads.metricsLimit), systemReadTimeout, 1, reads.metricsLimit+1, defaultOperationItems)
	defer cancel()
	tally.requests++
	data, truncated, err := service.client.getBounded(bound, path, "application/json", reads.metricsLimit)
	tally.bytes += int64(len(data))
	if err != nil {
		return nil, false, 0, err
	}
	if truncated {
		return nil, false, 0, ErrResponseTooLarge
	}
	samples, oversized, invalid, err := decodePodMetrics(data)
	if err != nil {
		return nil, false, 0, err
	}
	tally.listed = true
	return samples, oversized, invalid, nil
}

// errInvalidMetricsList is the answer to a metrics body that is not a list,
// or that nests deeper than systemMetricsMaxDepth.
var errInvalidMetricsList = errors.New("kubernetes: invalid API response")

// systemMetricsMaxDepth is the deepest nesting a metrics body may have. A
// real list nests five levels; a deeper body is refused as malformed before
// its nesting costs memory.
const systemMetricsMaxDepth = 32

// decodePodMetrics walks a metrics list as a stream: it decodes one item at a
// time and stops after the Pod ceiling plus one, so a body of many small or
// empty items costs no more memory than the body itself. Containers and usage
// keys are walked the same way and bounded per item.
func decodePodMetrics(data []byte) ([]PodMetricsSample, bool, int, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if !jsonDelimiter(decoder, '{') {
		return nil, false, 0, errInvalidMetricsList
	}
	samples := []PodMetricsSample{}
	oversized, invalid, read := false, 0, 0
	for decoder.More() {
		key, ok := jsonKey(decoder)
		if !ok {
			return nil, false, 0, errInvalidMetricsList
		}
		switch key {
		case "metadata":
			var metadata struct {
				Continue string `json:"continue"`
			}
			if decoder.Decode(&metadata) != nil {
				return nil, false, 0, errInvalidMetricsList
			}
			oversized = oversized || metadata.Continue != ""
		case "items":
			if !jsonDelimiter(decoder, '[') {
				return nil, false, 0, errInvalidMetricsList
			}
			for decoder.More() {
				if read >= systemMetricsPodCeiling {
					// More Pods than one answer may hold: the rest is not read.
					return samples, true, invalid, nil
				}
				read++
				var item struct {
					Metadata struct {
						Namespace string `json:"namespace"`
						Name      string `json:"name"`
					} `json:"metadata"`
					Timestamp  string          `json:"timestamp"`
					Window     string          `json:"window"`
					Containers json.RawMessage `json:"containers"`
				}
				// The item is the decoder's buffered value either way; its
				// depth is checked before it is decoded. The list itself
				// is two levels deep.
				var encoded json.RawMessage
				if decoder.Decode(&encoded) != nil || !jsonDepthWithin(encoded, systemMetricsMaxDepth-2) || json.Unmarshal(encoded, &item) != nil {
					return nil, false, 0, errInvalidMetricsList
				}
				sample := PodMetricsSample{Namespace: item.Metadata.Namespace, Pod: item.Metadata.Name}
				containers, ok := decodeContainerUsage(item.Containers)
				if !ok || !validPodLogName(sample.Namespace, 63, false) || !validPodLogName(sample.Pod, 253, true) {
					invalid++
					continue
				}
				sample.Containers = containers
				sample.Timestamp, _ = time.Parse(time.RFC3339Nano, item.Timestamp)
				sample.Window, _ = time.ParseDuration(item.Window)
				samples = append(samples, sample)
			}
			if !jsonDelimiter(decoder, ']') {
				return nil, false, 0, errInvalidMetricsList
			}
		default:
			if skipJSONValue(decoder, systemMetricsMaxDepth-1) != nil {
				return nil, false, 0, errInvalidMetricsList
			}
		}
	}
	return samples, oversized, invalid, nil
}

// systemMetricsUsageKeys bounds the usage keys read for one container.
const systemMetricsUsageKeys = 16

// decodeContainerUsage reads at most systemMetricsContainerCeiling containers
// and systemMetricsUsageKeys usage keys each; anything beyond, or a container
// without a valid name and CPU and memory usage, makes the sample invalid.
func decodeContainerUsage(raw json.RawMessage) ([]ContainerUsage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if !jsonDelimiter(decoder, '[') {
		return nil, false
	}
	result := []ContainerUsage{}
	for decoder.More() {
		if len(result) >= systemMetricsContainerCeiling {
			return nil, false
		}
		if !jsonDelimiter(decoder, '{') {
			return nil, false
		}
		name, cpuText, memoryText := "", "", ""
		for decoder.More() {
			key, ok := jsonKey(decoder)
			if !ok {
				return nil, false
			}
			switch key {
			case "name":
				if decoder.Decode(&name) != nil {
					return nil, false
				}
			case "usage":
				if !jsonDelimiter(decoder, '{') {
					return nil, false
				}
				keys := 0
				for decoder.More() {
					keys++
					usageKey, ok := jsonKey(decoder)
					if !ok || keys > systemMetricsUsageKeys {
						return nil, false
					}
					var target *string
					switch usageKey {
					case "cpu":
						target = &cpuText
					case "memory":
						target = &memoryText
					}
					if target == nil {
						if skipJSONValue(decoder, systemMetricsMaxDepth) != nil {
							return nil, false
						}
					} else if decoder.Decode(target) != nil {
						return nil, false
					}
				}
				if !jsonDelimiter(decoder, '}') {
					return nil, false
				}
			default:
				if skipJSONValue(decoder, systemMetricsMaxDepth) != nil {
					return nil, false
				}
			}
		}
		if !jsonDelimiter(decoder, '}') {
			return nil, false
		}
		cpu, cpuErr := ParseQuantity(cpuText)
		memory, memoryErr := ParseQuantity(memoryText)
		if cpuErr != nil || memoryErr != nil || cpu.Sign() < 0 || memory.Sign() < 0 || !validPodLogName(name, 63, false) {
			return nil, false
		}
		cores, _ := cpu.Float64()
		bytes, _ := memory.Float64()
		result = append(result, ContainerUsage{Name: name, CPUMillicores: cores * 1000, MemoryBytes: bytes})
	}
	if !jsonDelimiter(decoder, ']') || len(result) == 0 {
		return nil, false
	}
	return result, true
}

func jsonDelimiter(decoder *json.Decoder, want json.Delim) bool {
	token, err := decoder.Token()
	delimiter, ok := token.(json.Delim)
	return err == nil && ok && delimiter == want
}

func jsonKey(decoder *json.Decoder) (string, bool) {
	token, err := decoder.Token()
	key, ok := token.(string)
	return key, err == nil && ok
}

var errMetricsTooDeep = errors.New("kubernetes: metrics body nests too deep")

// skipJSONValue steps over one value token by token, without keeping it,
// and refuses a value nested deeper than maxDepth.
func skipJSONValue(decoder *json.Decoder, maxDepth int) error {
	depth := 0
	for {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				depth++
				if depth > maxDepth {
					return errMetricsTooDeep
				}
			default:
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

// jsonDepthWithin reports whether encoded nests no deeper than maxDepth. It
// reads the bytes once and keeps nothing.
func jsonDepthWithin(encoded []byte, maxDepth int) bool {
	depth, inString, escaped := 0, false, false
	for _, character := range encoded {
		switch {
		case escaped:
			escaped = false
		case inString:
			switch character {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
		case character == '"':
			inString = true
		case character == '{' || character == '[':
			depth++
			if depth > maxDepth {
				return false
			}
		case character == '}' || character == ']':
			depth--
		}
	}
	return true
}

// WarningEvents lists Warning events on clusterID, in the namespaces the
// saved learning scope and the policy allow. Messages are scrubbed.
func (reads *SystemReads) WarningEvents(ctx context.Context, clusterID string) (WarningEventList, error) {
	if reads == nil || !reads.enabled {
		return WarningEventList{}, &SystemReadError{Class: SystemReadRefused, Reason: SystemReadRefusedNotEnabled}
	}
	// The time limit covers the whole call, the gate included: the
	// settings load, the policy and the credential check.
	ctx, cancel := context.WithTimeout(ctx, reads.callTimeout)
	defer cancel()
	admission, err := reads.admit(ctx, clusterID, SystemReadLaneMetrics, systemReadWarningEvents)
	if err != nil {
		return WarningEventList{}, err
	}
	if admission.service.resourceExcluded(systemEventsResource) {
		return WarningEventList{}, reads.refuse(admission.key, SystemReadRefusedExcluded)
	}
	var tally systemTally
	result := WarningEventList{Events: []WarningEvent{}}
	plan := reads.plan(ctx, admission)
	if plan.clusterWide {
		page, err := reads.fetchWarningEvents(ctx, admission, "", &tally)
		if err != nil {
			tally.fail(err)
			return WarningEventList{}, reads.finish(admission, tally, err)
		}
		if page.Truncated {
			tally.cutShort++
		}
		fallback := systemNamespacePlan{}
		if page.Truncated {
			// The list stops at its item limit before the scope filter, and
			// the API server answers in namespace order: events from
			// excluded namespaces could hide every allowed one. List the
			// allowed namespaces one by one instead.
			fallback = reads.perNamespace(ctx, admission, plan.known)
		}
		if len(fallback.namespaces) == 0 {
			result.Truncated = page.Truncated
			if err := reads.keepWarningEvents(ctx, admission, "", page.Items, &result, &tally); err != nil {
				return WarningEventList{}, reads.scrubberFailed(admission, tally)
			}
			result.ObservedAt = reads.now().UTC()
			return result, reads.finish(admission, tally, nil)
		}
		plan = fallback
	}
	result.PerNamespace = true
	result.NamespacesSkipped = plan.skipped
	var lastErr error
	attempted := 0
	for position, namespace := range plan.namespaces {
		if ctx.Err() != nil {
			result.Partial = true
			result.NamespacesSkipped += stopSkipping(&tally, len(plan.namespaces)-position)
			reads.resumeAfter(admission, plan, position-1)
			break
		}
		attempted++
		page, err := reads.fetchWarningEvents(ctx, admission, namespace, &tally)
		if err != nil {
			result.NamespacesFailed++
			lastErr = err
			tally.fail(err)
			if systemReadStops(ctx, err) {
				result.Partial = true
				result.NamespacesSkipped += stopSkipping(&tally, len(plan.namespaces)-position-1)
				reads.resumeAfter(admission, plan, position)
				break
			}
			continue
		}
		if page.Truncated {
			result.Truncated = true
			tally.cutShort++
		}
		if err := reads.keepWarningEvents(ctx, admission, namespace, page.Items, &result, &tally); err != nil {
			return WarningEventList{}, reads.scrubberFailed(admission, tally)
		}
	}
	if lastErr != nil && result.NamespacesFailed == attempted {
		return WarningEventList{}, reads.finish(admission, tally, lastErr)
	}
	result.ObservedAt = reads.now().UTC()
	return result, reads.finish(admission, tally, nil)
}

// fetchWarningEvents lists one namespace's Warning events (the whole cluster
// for an empty namespace) through listAllEvents, against the pinned event
// resource and under its own budget.
func (reads *SystemReads) fetchWarningEvents(ctx context.Context, admission *systemAdmission, namespace string, tally *systemTally) (ResourcePage, error) {
	if namespace != "" && !validPodLogName(namespace, 63, false) {
		return ResourcePage{}, ErrInvalidArguments
	}
	// The credential is proved before the system mark is added, as for the
	// metrics read, so a proof is paced with the user's reads.
	if err := admission.service.validateCredential(ctx); err != nil {
		return ResourcePage{}, err
	}
	if err := admission.service.client.systemThrottle(); err != nil {
		return ResourcePage{}, err
	}
	bound := context.WithValue(systemRead(ctx, 0), discoveryContextKey{}, Discovery{Resources: []ResourceDefinition{systemEventsDefinition}})
	bound, cancel := withOperationBudget(bound, systemReadTimeout, systemEventRequests, defaultOperationBytes, defaultOperationItems)
	defer cancel()
	budget := operationBudgetFrom(bound)
	page, err := admission.service.listAllEvents(bound, EventOptions{Namespace: namespace, Type: "Warning"})
	budget.mu.Lock()
	tally.bytes += defaultOperationBytes - budget.bytes
	tally.requests += uint64(systemEventRequests - budget.requests)
	budget.mu.Unlock()
	if err != nil {
		return ResourcePage{}, err
	}
	tally.listed = true
	return page, nil
}

// keepWarningEvents adds the Warning events from allowed namespaces, with
// reason and message scrubbed. namespace is the namespace asked, or empty for
// a cluster-wide list.
func (reads *SystemReads) keepWarningEvents(ctx context.Context, admission *systemAdmission, namespace string, items []ProjectedResource, result *WarningEventList, tally *systemTally) error {
	for _, item := range items {
		if summaryString(item.Summary, "type") != "Warning" {
			continue
		}
		involved, _ := item.Summary["involved_object"].(ObjectRef)
		source := summaryString(item.Summary, "source_component")
		// Every reference field is checked before the namespace is given to
		// the scope or the policy: they are text anyone who may create
		// events can write.
		if !validEventReferences(item, involved, source) {
			tally.invalid++
			result.Invalid++
			continue
		}
		if namespace != "" && item.Namespace != namespace {
			tally.droppedByScope++
			continue
		}
		if ok, reason := reads.allowNamespace(ctx, admission, item.Namespace, false); !ok {
			countDropped(tally, reason)
			continue
		}
		if involved.Namespace != "" && involved.Namespace != item.Namespace {
			if ok, reason := reads.allowNamespace(ctx, admission, involved.Namespace, false); !ok {
				countDropped(tally, reason)
				continue
			}
		}
		reason, reasonOK := reads.scrub(summaryString(item.Summary, "reason"))
		message, messageOK := reads.scrub(summaryString(item.Summary, "message"))
		if !reasonOK || !messageOK {
			return errSystemScrubberFailed
		}
		// Reference fields pass through the scrubber too, as reason and
		// message do: a valid name can still carry a secret.
		fields := []string{item.Namespace, item.Name, item.UID, involved.APIVersion, involved.Kind, involved.Namespace, involved.Name, involved.UID, source}
		for position, field := range fields {
			scrubbed, ok := reads.scrub(field)
			if !ok {
				return errSystemScrubberFailed
			}
			fields[position] = boundString(scrubbed)
		}
		event := WarningEvent{
			Namespace: fields[0],
			Name:      fields[1],
			UID:       fields[2],
			Reason:    boundString(reason),
			Message:   boundString(message),
			Object:    ObjectRef{APIVersion: fields[3], Kind: fields[4], Namespace: fields[5], Name: fields[6], UID: fields[7]},
			Source:    fields[8],
		}
		if count, ok := item.Summary["count"].(float64); ok && count >= 0 && count < 1<<53 {
			event.Count = int64(count)
		}
		event.FirstSeen, _ = time.Parse(time.RFC3339Nano, summaryString(item.Summary, "firstTimestamp"))
		event.LastSeen, _ = time.Parse(time.RFC3339Nano, summaryString(item.Summary, "lastTimestamp"))
		result.Events = append(result.Events, event)
	}
	return nil
}

var (
	eventKindPattern       = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,62}$`)
	eventAPIVersionPattern = regexp.MustCompile(`^([a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?/)?v[0-9]+((alpha|beta)[0-9]+)?$`)
	eventUIDPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,127}$`)
)

// validEventReferences checks an event's reference fields the way the
// metrics read checks names: namespaces and names as Kubernetes names, kind
// and API version against their patterns, UIDs short and plain, and the
// reporting component as the event projection accepts it.
func validEventReferences(item ProjectedResource, involved ObjectRef, source string) bool {
	if !validPodLogName(item.Namespace, 63, false) || !validPodLogName(item.Name, 253, true) {
		return false
	}
	if item.UID != "" && !eventUIDPattern.MatchString(item.UID) {
		return false
	}
	if !eventKindPattern.MatchString(involved.Kind) || !validPodLogName(involved.Name, 253, true) {
		return false
	}
	if involved.APIVersion != "" && !eventAPIVersionPattern.MatchString(involved.APIVersion) {
		return false
	}
	if involved.Namespace != "" && !validPodLogName(involved.Namespace, 63, false) {
		return false
	}
	if involved.UID != "" && !eventUIDPattern.MatchString(involved.UID) {
		return false
	}
	return source == "" || validEventSource(source, true)
}

var errSystemScrubberFailed = errors.New("kubernetes: system read scrubber failed")

// scrub runs the scrubber; a panic reports false, so the read fails closed
// instead of panicking into the worker.
func (reads *SystemReads) scrub(value string) (scrubbed string, ok bool) {
	defer func() {
		if recover() != nil {
			scrubbed, ok = "", false
		}
	}()
	return reads.scrubber.Scrub(value), true
}

// scrubberFailed records an admitted read whose scrubber panicked and
// refuses it; nothing it read is returned.
func (reads *SystemReads) scrubberFailed(admission *systemAdmission, tally systemTally) error {
	tally.cause = SystemReadRefused
	_ = reads.finish(admission, tally, nil)
	return reads.refuse(admission.key, SystemReadRefusedScrubberFailed)
}
