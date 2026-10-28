package kubernetes

import (
	"context"
	"errors"
	"sort"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

const (
	// maxSyncHealthKinds bounds the complete health payload. The index knows a
	// fixed set of kinds, so this only trims a pathological cluster; failing
	// kinds are kept first, so a failure is never the entry that is dropped.
	maxSyncHealthKinds = 64
	// maxSyncHealthProblems bounds the problem rows a surface has to render. The
	// count of failing kinds stays exact even when the rows are bounded.
	maxSyncHealthProblems = 32
	// maxSyncHealthOmitted bounds the omitted kind names themselves.
	maxSyncHealthOmitted = 32
)

// SyncHealth is the complete inventory health of one cluster: every kind the
// index knows, not only the kinds one view asked for. View payloads stay narrow
// on purpose; this is what a caller gates on, because a kind outside a view's own
// request would otherwise fail without anyone noticing.
type SyncHealth struct {
	Progress *SyncProgress `json:"progress,omitempty"`
	State    string        `json:"state"`
	Partial  bool          `json:"partial"`
	// FirstReadyAt is when this cluster first recorded a ready generation. It is
	// recorded once, kept with the cluster's inventory metadata, and carried
	// forward from then on, so a credential fault, a restart, a re-index, or any
	// later syncing, partial, stale, or error report still says the cluster has
	// been ready before. It is a property of the cluster, not of the process, and
	// the moment it keeps is always the first one. A cluster that has never been
	// ready omits it.
	FirstReadyAt time.Time `json:"first_ready_at,omitzero"`
	// Class names why the whole read failed, drawn from the same closed
	// vocabulary a kind's own failure class uses, so a surface gets one word it
	// already knows. It is set only on an error report and stays empty otherwise.
	Class string `json:"class,omitempty"`
	// AgeSeconds is how old the snapshot this report describes is, in seconds:
	// the oldest observation it stands behind. It is absent when no snapshot has
	// ever been published, because zero seconds would claim the empty screen is
	// current data.
	AgeSeconds *float64 `json:"age_s,omitempty"`
	// Kinds carries one entry per known kind. A kind the cluster does not serve is
	// present with Absent set, so it is never confused with a missing kind.
	Kinds map[string]kubeindex.KindStatus `json:"kinds"`
	// Failing lists the kinds whose latest listing did not finish cleanly, ready
	// for a surface to render: name, class, counts, and since when. It carries the
	// blocking faults only.
	Failing []SyncKindProblem `json:"failing"`
	// Unreadable lists the kinds this connector identity is not allowed to read.
	// A capability gap is not a sync fault: these kinds are excluded from Failing
	// and from the aggregate state, so a least-privilege install still reaches
	// ready and can show everything it is allowed to see.
	Unreadable []SyncKindProblem `json:"unreadable"`
	// TotalKinds counts every kind before the payload bound is applied.
	TotalKinds      int  `json:"total_kinds"`
	ReadyKinds      int  `json:"ready_kinds"`
	FailingKinds    int  `json:"failing_kinds"`
	UnreadableKinds int  `json:"unreadable_kinds"`
	PendingKinds    int  `json:"pending_kinds"`
	Truncated       bool `json:"truncated"`
	// OmittedKinds names the kinds the bound dropped, bounded itself.
	OmittedKinds []string `json:"omitted_kinds,omitempty"`
}

// SyncKindProblem is one failing kind in plain terms. Since is when the failing
// attempt started, so a surface can say how long the kind has been failing.
type SyncKindProblem struct {
	Kind    string `json:"kind"`
	State   string `json:"state"`
	Class   string `json:"class,omitempty"`
	Skipped int    `json:"skipped,omitempty"`
	Records int    `json:"records"`
	// Since is when this failure began, kept until the kind recovers. A zero time
	// is omitted rather than rendered as a date.
	Since time.Time `json:"since,omitzero"`
	// ObservedAt is the last successful listing start, omitted when there is none.
	ObservedAt time.Time `json:"observed_at,omitzero"`
}

// SyncHealth reports every known kind's inventory state from the published
// generation. It never lists the cluster and never waits for a cycle, so a caller
// can gate on it on every render.
//
// A cluster that cannot be read is still answered. The failure comes back typed
// and carries a report whose state is error, so a surface keeps the one fact that
// has to outlive the credential — whether the cluster was ever ready — instead of
// losing it to an error status. Callers that only care whether the read succeeded
// keep using err.
func (service *Service) SyncHealth(ctx context.Context) (SyncHealth, error) {
	generation, err := service.readInventory(ctx)
	if err != nil {
		if !reportableSyncFailure(ctx, err) {
			return SyncHealth{}, err
		}
		return SyncHealth{}, &syncHealthFailure{Report: service.failedSyncHealth(err), cause: err}
	}
	// The publication fact outlives credentials, unlike the inventory itself.
	if at := service.recordedFirstReadyAt(); !at.IsZero() {
		generation.FirstReadyAt = at
	}
	report := buildSyncHealth(syncHealthKinds(generation), generation.FirstReadyAt)
	if generation.FirstReadyAt.IsZero() {
		report.Progress = service.coldSyncProgress()
	}
	return report, nil
}

// syncHealthFailure reports a health read that could not reach the cluster while
// still carrying the report that answers the question. It wraps its cause, so the
// fault is diagnosed exactly like every other typed Kubernetes failure rather
// than becoming a second error vocabulary.
type syncHealthFailure struct {
	Report SyncHealth
	cause  error
}

func (failure *syncHealthFailure) Error() string {
	return "kubernetes: sync health unavailable (" + failure.Report.Class + ")"
}

func (failure *syncHealthFailure) Unwrap() error { return failure.cause }

// SyncHealthReport returns the report a failed health read can still answer, and
// false when the failure carries none. A caller that gets false keeps its usual
// error answer, so no other failure changes shape.
func SyncHealthReport(err error) (SyncHealth, bool) {
	var failure *syncHealthFailure
	if errors.As(err, &failure) {
		return failure.Report, true
	}
	return SyncHealth{}, false
}

// reportableSyncFailure reports whether a failed read can still answer with a
// report. A cluster that is no longer configured has no report to give, and a
// request the caller has already abandoned has nobody left to read one.
func reportableSyncFailure(ctx context.Context, err error) bool {
	return ctx.Err() == nil && !errors.Is(err, ErrClusterUnavailable)
}

// failedSyncHealth is the report a read answers with when the cluster cannot be
// read at all. Every known kind is a whole-cycle fault, because a connector that
// read nothing has no per-kind capability gap to report, and the cluster's
// durable first ready moment is read from local state, so a dead credential
// cannot hide whether the cluster ever worked.
//
// The age is read from the same local state, so it describes the snapshot the
// report stands behind rather than the request that failed: a surface that keeps
// the last data on screen can say how old it is. A cluster that never published
// a snapshot has no age to give, and the field is left out — zero seconds would
// present an empty screen as current.
func (service *Service) failedSyncHealth(err error) SyncHealth {
	class := syncFailureClass(err)
	observedAt := service.recordedSnapshotObservedAt()
	kinds := cycleFailureKinds(class)
	if !observedAt.IsZero() {
		for kind, state := range kinds {
			state.ObservedAt = observedAt
			kinds[kind] = state
		}
	}
	report := buildSyncHealth(kinds, service.recordedFirstReadyAt())
	if observedAt.IsZero() {
		report.AgeSeconds = nil
	}
	report.Class = class
	return report
}

// syncFailureClass names a whole-read failure in the same closed vocabulary a
// kind's own failure class uses, so a report never carries a word a surface does
// not know.
func syncFailureClass(err error) string {
	var throttled *ErrThrottled
	class := "unavailable"
	switch {
	case errors.Is(err, ErrCredentialUnavailable), errors.Is(err, ErrUnauthorized):
		// A credential that expired, was revoked, or was rejected is one fault
		// with one remedy, whichever layer noticed it.
		class = "unauthorized"
	case errors.Is(err, ErrForbidden):
		class = "forbidden"
	case errors.As(err, &throttled):
		class = "throttled"
	case errors.Is(err, context.DeadlineExceeded):
		class = "timeout"
	case errors.Is(err, ErrOperationBudget):
		class = "budget_exhausted"
	}
	return inventoryCycleClass(class)
}

// syncHealthKinds completes a generation's kind set with every known kind it has
// not listed yet. Every known kind the view does not list is loading: never empty
// and never failed. This is derived from what is *listed*, not from the
// generation's pending list, because a view can also be filled by the on-demand
// scope index (capture, log, and alert reads). Treating a missing kind as ready
// would let one demand-loaded kind report a whole cluster ready before its first
// generation was ever published.
func syncHealthKinds(generation inventoryGeneration) map[string]kubeindex.KindStatus {
	kinds := make(map[string]kubeindex.KindStatus, len(generation.View.Kinds)+len(indexedResourceIDs))
	for kind, state := range generation.View.Kinds {
		kinds[kind] = state
	}
	for kind := range indexedResourceIDs {
		if _, listed := kinds[kind]; listed {
			continue
		}
		// Loading is not failing, and the wire shape has to say so on its own: a
		// kind that carries partial alongside pending reads as a failed partial to
		// any consumer that checks partial before state.
		kinds[kind] = kubeindex.KindStatus{State: "pending"}
	}
	return kinds
}

// markFirstReady stamps the moment a cluster first publishes a generation its own
// health report calls ready, so nothing else has to guess whether a syncing
// report is a first boot. It only fills a zero value, and only for a ready
// generation: a failing, degraded, or stale generation is never stamped, and one
// that already carries the moment keeps it, so the stamp can neither be cleared
// nor moved.
func (generation *inventoryGeneration) markFirstReady(now time.Time) {
	if generation == nil || !generation.FirstReadyAt.IsZero() {
		return
	}
	if state, _, _, _, _ := summarizeSyncHealth(syncHealthKinds(*generation)); state != "ready" {
		return
	}
	generation.FirstReadyAt = now.UTC()
}

// buildSyncHealth aggregates a complete kind set into the health surface.
func buildSyncHealth(all map[string]kubeindex.KindStatus, firstReadyAt time.Time) SyncHealth {
	names := make([]string, 0, len(all))
	for kind := range all {
		names = append(names, kind)
	}
	sort.Strings(names)
	result := SyncHealth{FirstReadyAt: firstReadyAt, TotalKinds: len(all), Failing: []SyncKindProblem{}, Unreadable: []SyncKindProblem{}}
	result.Failing, result.FailingKinds = syncKindProblems(names, all)
	result.Unreadable, result.UnreadableKinds = syncUnreadableKinds(names, all)
	kept, omitted := boundedSyncKinds(names, all)
	result.Kinds = kept
	if len(omitted) > 0 {
		result.Truncated = true
		result.OmittedKinds = omitted
	}
	state, partial, ageSeconds, ready, pending := summarizeSyncHealth(all)
	result.State, result.Partial, result.AgeSeconds = state, partial, &ageSeconds
	result.ReadyKinds, result.PendingKinds = ready, pending
	return result
}

// summarizeSyncHealth aggregates every known kind. A kind the cluster does not
// serve cannot hold the aggregate back, and any failing kind keeps it out of
// ready — that is the whole point of a cluster-wide answer. Ready also requires
// at least one kind to be ready, so an empty published set is loading.
func summarizeSyncHealth(kinds map[string]kubeindex.KindStatus) (string, bool, float64, int, int) {
	failed, degraded, stale, loading := false, false, false, false
	ready, pending, unreadable := 0, 0, 0
	oldest := time.Time{}
	for kind, state := range kinds {
		if !state.ObservedAt.IsZero() && (oldest.IsZero() || state.ObservedAt.Before(oldest)) {
			oldest = state.ObservedAt
		}
		if state.Absent {
			// Not served here: it is not loading, not failing, and not ready.
			continue
		}
		if state.State == "pending" || state.State == "warming" {
			loading, pending = true, pending+1
			continue
		}
		if isUnreadableSyncKind(state) {
			// A permission gap is not loading and not failing: it is a known
			// capability limit, reported on its own.
			unreadable++
			continue
		}
		// One rule decides what failing means, for the aggregate and for the
		// problem list alike, so the two can never disagree.
		if isFailingSyncKind(state) {
			if state.State == "error" {
				failed = true
			} else {
				degraded = true
			}
			continue
		}
		if state.ObservedAt.IsZero() {
			loading = true
			continue
		}
		ready++
		if syncKindStale(kind, state) {
			stale = true
		}
	}
	ageSeconds := 0.0
	if !oldest.IsZero() {
		if age := time.Since(oldest).Seconds(); age > 0 {
			ageSeconds = age
		}
	}
	switch {
	case failed:
		return "error", true, ageSeconds, ready, pending
	case degraded:
		return "partial", true, ageSeconds, ready, pending
	case stale:
		return "stale", true, ageSeconds, ready, pending
	case loading:
		return "syncing", false, ageSeconds, ready, pending
	}
	if ready == 0 {
		if unreadable > 0 && pending == 0 {
			// Nothing is readable and nothing is still loading: this is not a
			// cluster that is preparing, it is a connector that cannot see
			// anything. Saying syncing here is a promise that never resolves.
			return "error", true, ageSeconds, ready, pending
		}
		// Nothing is ready: the cluster has not published a generation yet, or it
		// serves none of the kinds we know. Either way there is no data to show,
		// so this is loading, never ready — a gate that opens on ready would hand
		// an operator empty views and call them current.
		return "syncing", false, ageSeconds, ready, pending
	}
	return "ready", false, ageSeconds, ready, pending
}

// syncKindStale applies the same freshness rule a view uses for a listed kind.
func syncKindStale(kind string, state kubeindex.KindStatus) bool {
	return time.Since(state.ObservedAt) > max(3*time.Minute, 2*inventoryCadence(kind))
}

// syncKindProblems lists every failing kind, most recent failure first.
func syncKindProblems(names []string, all map[string]kubeindex.KindStatus) ([]SyncKindProblem, int) {
	problems := []SyncKindProblem{}
	total := 0
	for _, kind := range names {
		state := all[kind]
		if !isFailingSyncKind(state) {
			continue
		}
		total++
		if len(problems) >= maxSyncHealthProblems {
			continue
		}
		problems = append(problems, syncProblemRow(kind, state))
	}
	sort.SliceStable(problems, func(left, right int) bool {
		if !problems[left].Since.Equal(problems[right].Since) {
			return problems[left].Since.After(problems[right].Since)
		}
		return problems[left].Kind < problems[right].Kind
	})
	return problems, total
}

// syncProblemRow renders one kind as a surface row. Since is the stable start of
// this failure, not the latest retry, and a kind that was never observed carries
// no observation at all rather than a zero date.
func syncProblemRow(kind string, state kubeindex.KindStatus) SyncKindProblem {
	since := state.FailingSince
	if since.IsZero() {
		// A generation published before the failure start was recorded can still
		// say when it was last attempted; it is the best evidence available.
		since = state.LastAttemptAt
	}
	observed := state.ObservedAt
	if observed.IsZero() {
		observed = time.Time{}
	}
	return SyncKindProblem{Kind: kind, State: state.State, Class: state.Error, Skipped: state.Skipped, Records: state.Records, Since: since, ObservedAt: observed}
}

// syncUnreadableKinds lists the kinds this identity may not read: a capability
// gap, reported separately so it cannot block a view.
func syncUnreadableKinds(names []string, all map[string]kubeindex.KindStatus) ([]SyncKindProblem, int) {
	rows := []SyncKindProblem{}
	total := 0
	for _, kind := range names {
		state := all[kind]
		if !isUnreadableSyncKind(state) {
			continue
		}
		total++
		if len(rows) >= maxSyncHealthProblems {
			continue
		}
		rows = append(rows, syncProblemRow(kind, state))
	}
	return rows, total
}

// isUnreadableSyncKind reports a permission gap rather than a sync fault. Only a
// forbidden listing is a capability limit: an invalid or revoked credential is a
// dead connector, and a cycle that failed as a whole never proves anything about
// one kind, so both stay faults.
func isUnreadableSyncKind(state kubeindex.KindStatus) bool {
	if state.Absent || state.CycleFailure {
		return false
	}
	return state.State == "error" && state.Error == "forbidden"
}

// boundedSyncKinds keeps every failing kind first and then fills up to the
// payload bound in kind order, so a bounded answer can never hide a failure.
func boundedSyncKinds(names []string, all map[string]kubeindex.KindStatus) (map[string]kubeindex.KindStatus, []string) {
	kept := make(map[string]kubeindex.KindStatus, min(len(names), maxSyncHealthKinds))
	if len(names) <= maxSyncHealthKinds {
		for _, kind := range names {
			kept[kind] = all[kind]
		}
		return kept, nil
	}
	for _, kind := range names {
		if len(kept) >= maxSyncHealthKinds {
			break
		}
		if isFailingSyncKind(all[kind]) {
			kept[kind] = all[kind]
		}
	}
	for _, kind := range names {
		if len(kept) >= maxSyncHealthKinds {
			break
		}
		if _, exists := kept[kind]; exists {
			continue
		}
		kept[kind] = all[kind]
	}
	omitted := []string{}
	for _, kind := range names {
		if _, exists := kept[kind]; exists {
			continue
		}
		if len(omitted) == maxSyncHealthOmitted {
			break
		}
		omitted = append(omitted, kind)
	}
	return kept, omitted
}

// isFailingSyncKind reports whether a kind's latest listing did not finish
// cleanly. Loading, and a kind the cluster does not serve, are not failures. A
// loading kind also never reports partial, so the wire shape cannot be misread.
func isFailingSyncKind(state kubeindex.KindStatus) bool {
	if state.Absent || isUnreadableSyncKind(state) {
		return false
	}
	switch state.State {
	case "pending", "warming":
		return false
	case "error", "partial":
		return true
	}
	return state.Partial
}
