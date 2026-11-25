package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubegraph "github.com/VersusControl/versus-incident/pkg/kubernetes/graph"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/logging"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// IssueAlert contains only notification-safe facts, never raw object or log payloads.
type IssueAlert struct {
	OrgID                                                           string
	Cluster, Namespace, WorkloadKind, Workload, RootUID, ResourceID string
	Rule, RuleLabel, Severity                                       string
	Count, Of                                                       int
	BrokeAt                                                         time.Time
	Evidence, Changes                                               []string
	HistoryIncomplete                                               bool
	RollupCount                                                     int
}

type Outcome struct {
	IncidentID          string
	NotificationOutcome string
}

type issueAlertWindow struct {
	active         bool
	cancel         context.CancelFunc
	done           chan struct{}
	start          time.Time
	sent           int
	seen           map[string]bool
	enriched       map[string]bool
	suppressed     map[string]bool
	rollup         bool
	rollupCount    int
	rollupSeverity string
	enabledAt      time.Time
	baseline       issueAlertBaseline
	// stored is set when the durable checkpoint existed at the last load:
	// some earlier continuous cycle evaluated alerts for this cluster.
	stored bool
	// baselining is set while the breaks found before alerting could see
	// them are still being gathered, until a complete cycle ends it.
	baselining bool
	// baselineSince is the first cycle of the current baselining run that
	// was not complete; after alertBaselineBound of such cycles the baseline
	// closes on what it gathered, so alerts never stay off for good. It is
	// saved in the record, so a restart or a new lease holder keeps the bound.
	baselineSince time.Time
	// baselineRoots holds the workloads the baseline found broken, by UID,
	// with the kind of each. They stay quiet while they stay broken, whatever
	// issue type they show next.
	baselineRoots map[string]string
	// key is the durable checkpoint this window was last loaded from.
	key string
}

// alertBaselineBound is how long continuous cycles may keep the alert
// baseline open without a complete one.
const alertBaselineBound = 15 * time.Minute

type issueAlertBaseline struct {
	owner   string
	ready   bool
	present map[string]bool
	newAt   map[string]time.Time
}

// observe establishes absence only from complete owned cycles. A new owner
// first establishes its own baseline before admitting unattributed break times.
func (baseline *issueAlertBaseline) observe(items []kubebreaks.Break, owner string, now time.Time, partial bool, deferred ...kubebreaks.Break) []kubebreaks.Break {
	if baseline.owner != owner {
		*baseline = issueAlertBaseline{owner: owner, present: map[string]bool{}, newAt: map[string]time.Time{}}
	}
	current := map[string]bool{}
	for _, b := range deferred {
		key := b.Root.Namespace + "\x00" + b.Root.UID + "\x00" + b.Rule
		current[key] = true
		// Remember when the break was actually observed, not when its
		// owner later became authoritative or alerts were enabled.
		if baseline.ready && !baseline.present[key] {
			baseline.newAt[key] = now
		}
		baseline.present[key] = true
	}
	result := make([]kubebreaks.Break, 0, len(items))
	for _, b := range items {
		key := b.Root.Namespace + "\x00" + b.Root.UID + "\x00" + b.Rule
		current[key] = true
		if baseline.ready && !baseline.present[key] {
			baseline.newAt[key] = now
		}
		baseline.present[key] = true
		if b.Source == "first_seen" {
			at, eligible := baseline.newAt[key]
			if !eligible {
				continue
			}
			b.BrokeAt = at
		}
		result = append(result, b)
	}
	if !partial {
		for key := range baseline.present {
			if !current[key] {
				delete(baseline.present, key)
				delete(baseline.newAt, key)
			}
		}
		if len(deferred) == 0 {
			baseline.ready = true
		}
	}
	return result
}

type issueAlertCheckpoint struct {
	Start          time.Time       `json:"start"`
	EnabledAt      time.Time       `json:"enabled_at"`
	Sent           int             `json:"sent"`
	Seen           map[string]bool `json:"seen"`
	Enriched       map[string]bool `json:"enriched"`
	Suppressed     map[string]bool `json:"suppressed"`
	Rollup         bool            `json:"rollup"`
	RollupCount    int             `json:"rollup_count"`
	RollupSeverity string          `json:"rollup_severity"`
	Baselining     bool            `json:"baselining,omitempty"`
	BaselineSince  time.Time       `json:"baseline_since"`
	// BaselineRoots maps each held workload UID to its kind.
	BaselineRoots map[string]string `json:"held_roots,omitempty"`
}

// alertCheckpointKey names the durable alert record of one cluster. It is
// bound to the cluster's UID, as saved history is, and never to the
// credential or principal that reads it, so a renewed or replaced credential
// on the same cluster finds the record. Without a cluster UID, because there is
// no evidence store or the cluster has not established its UID (an identity
// that cannot list Namespaces never does), the record is per configured
// cluster under the ".alert-window.unscoped" suffix. Releases
// before this one kept a record per configured cluster under the bare
// ".alert-window" suffix; that record is left as it is for a replica of such a
// release still running alongside. The UID is text the API server reports,
// so the key holds a fixed-length digest of it, never the text itself: a UID
// with path separators must not steer where file storage writes the record.
func alertCheckpointKey(settingsKey, clusterUID string) string {
	if clusterUID == "" {
		return settingsKey + ".alert-window.unscoped"
	}
	digest := sha256.Sum256([]byte(clusterUID))
	return settingsKey + ".alert-window." + hex.EncodeToString(digest[:])
}

// alertClusterUID resolves the cluster UID the alert record is bound to. It
// reads the committed history scope, never the cluster. ok is false while a
// cluster that keeps history has not established its UID, which may be for
// good when the identity cannot list Namespaces. A store that cannot be read
// returns its error instead: the UID may well be established, and the
// unscoped record would then admit again a break the UID record already
// admitted.
func (service *Service) alertClusterUID(ctx context.Context) (uid string, ok bool, err error) {
	store := service.evidenceStore()
	if store == nil {
		return "", true, nil
	}
	scope, err := service.storedHistoryScope(storage.EvidenceReadContext(ctx), store)
	if isHistoryStoreError(err) {
		return "", false, err
	}
	if err != nil || scope.OrgID != service.scope.OrgID || scope.ClusterID != service.scope.ClusterID || scope.ClusterUID == "" {
		return "", false, nil
	}
	return scope.ClusterUID, true, nil
}

// isHistoryStoreError reports a history store that could not be read, as
// opposed to a history that is not established or a scope that does not
// belong to this cluster.
func isHistoryStoreError(err error) bool {
	return errors.Is(err, errHistoryUnavailable) || errors.Is(err, errHistoryTimeout) || errors.Is(err, errHistoryUpgrading)
}

// readAlertCheckpoint returns the stored record under key, or nil when there
// is none.
func readAlertCheckpoint(provider storage.Provider, key string) ([]byte, error) {
	encoded, err := provider.ReadBlob(key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	return encoded, err
}

// loadAlertCheckpoint loads the window from its record. When that record does
// not exist yet and adopt names another record, the window starts from that
// one instead; the next save writes it under the window's own key.
func (service *Service) loadAlertCheckpoint(window *issueAlertWindow, adopt string) error {
	provider, _ := service.logSettingsStore()
	if window.key == "" {
		return ErrInvalidArguments
	}
	encoded, err := readAlertCheckpoint(provider, window.key)
	if err != nil {
		return err
	}
	if len(encoded) == 0 && adopt != "" && adopt != window.key {
		if encoded, err = readAlertCheckpoint(provider, adopt); err != nil {
			return err
		}
	}
	if len(encoded) > 8<<20 {
		return ErrInvalidArguments
	}
	var state issueAlertCheckpoint
	if len(encoded) > 0 && json.Unmarshal(encoded, &state) != nil {
		return ErrInvalidArguments
	}
	window.stored = len(encoded) > 0
	window.start, window.enabledAt, window.sent, window.seen, window.suppressed, window.rollup = state.Start, state.EnabledAt, state.Sent, state.Seen, state.Suppressed, state.Rollup
	window.rollupCount = state.RollupCount
	window.rollupSeverity = state.RollupSeverity
	window.enriched = state.Enriched
	window.baselining = state.Baselining
	window.baselineSince = state.BaselineSince
	window.baselineRoots = state.BaselineRoots
	return nil
}

// alertCheckpoint saves the window to its record, provided the record did not
// change since it was read here.
func (service *Service) alertCheckpoint(ctx context.Context, window *issueAlertWindow, owner string) error {
	provider, _ := service.logSettingsStore()
	key := window.key
	if key == "" {
		return ErrInvalidArguments
	}
	cas, ok := provider.(storage.BlobCAS)
	if !ok {
		return storage.ErrUnsupported
	}
	encoded, err := storage.ReadBlobContext(ctx, provider, key)
	if errors.Is(err, storage.ErrNotFound) {
		encoded, err = nil, nil
	}
	if err != nil {
		return err
	}
	replacement, err := json.Marshal(issueAlertCheckpoint{Start: window.start, EnabledAt: window.enabledAt, Sent: window.sent, Seen: window.seen, Enriched: window.enriched, Suppressed: window.suppressed, Rollup: window.rollup, RollupCount: window.rollupCount, RollupSeverity: window.rollupSeverity, Baselining: window.baselining, BaselineSince: window.baselineSince, BaselineRoots: window.baselineRoots})
	if err != nil {
		return err
	}
	if len(replacement) > 8<<20 {
		return ErrInvalidArguments
	}
	if err := service.changeStore().CheckCaptureLeaseContext(ctx, owner); err != nil {
		return err
	}
	swapped, err := storage.CompareAndSwapBlobContext(ctx, cas, key, encoded, replacement)
	if err != nil {
		return err
	}
	if !swapped {
		return errors.New("kubernetes: issue alert window changed")
	}
	return nil
}

func (service *Service) SetIssueAlertSink(sink func(context.Context, IssueAlert) (Outcome, error)) {
	service.issueAlertSink = sink
}

type IssueAlertSettings struct {
	Enabled         bool      `json:"enabled"`
	Level           string    `json:"level"`
	EnabledAt       time.Time `json:"enabled_at"`
	CanWrite        bool      `json:"can_write"`
	Available       bool      `json:"available"`
	Reason          string    `json:"reason"`
	LinksReady      bool      `json:"links_ready"`
	ChannelsEnabled bool      `json:"channels_enabled"`
	// evaluatedFrom mirrors the stored setting; see breaklogs.Alerts.
	evaluatedFrom time.Time
}

func (service *Service) IssueAlertSettings(ctx context.Context) (IssueAlertSettings, error) {
	if err := service.validateCredential(ctx); err != nil {
		return IssueAlertSettings{}, err
	}
	provider, key := service.logSettingsStore()
	state, err := breaklogs.LoadSettings(provider, key)
	a := state.Settings.Alerts
	level := a.Level
	if level == "" {
		level = "critical"
	}
	result := IssueAlertSettings{Enabled: a.Enabled, Level: level, EnabledAt: a.EnabledAt, Available: service.captureMode != "demand", evaluatedFrom: a.EvaluatedFrom}
	if !result.Available {
		result.Reason = "demand_capture"
	}
	return result, err
}

func (service *Service) SaveIssueAlertSettings(ctx context.Context, enabled bool, level, actor string) error {
	if level != "critical" && level != "warning" {
		return ErrInvalidArguments
	}
	if err := service.validateCredential(ctx); err != nil {
		return err
	}
	if enabled && service.captureMode == "demand" {
		return ErrInvalidArguments
	}
	provider, key := service.logSettingsStore()
	return service.commitCredential(ctx, func(ctx context.Context) error {
		return breaklogs.UpdateSettingsContext(ctx, provider, key, func(state *breaklogs.SettingsState) error {
			a := &state.Settings.Alerts
			if enabled && !a.Enabled {
				a.EnabledAt = time.Now().UTC()
				a.EvaluatedFrom = a.EnabledAt
			}
			a.Enabled, a.Level, a.UpdatedBy = enabled, level, actor
			return nil
		})
	})
}

// alertKindsSettled reports whether a snapshot lists Pods and no captured
// kind is still warming or partly listed. A kind the identity may not read,
// or reads only inside its namespaces, counts as settled: waiting does not
// change it, and holding the alert baseline open for it would keep alerts off
// for good.
func alertKindsSettled(snapshot kubeindex.Snapshot) bool {
	if _, listed := snapshot.Kinds["Pod"]; !listed {
		return false
	}
	for kind, state := range snapshot.Kinds {
		if _, captured := captureCadences[kind]; !captured {
			continue
		}
		if alertBaselineSteadyGap(state) {
			continue
		}
		if state.State == "" || state.State == "warming" || state.Error == "syncing" || state.Partial && kind != "Secret" && kind != "ControllerRevision" {
			return false
		}
	}
	return true
}

// alertListedKinds lists the kinds a snapshot listed in full this cycle:
// ready, not partial, with no listing error. A workload of such a kind that
// the cycle shows without a break is healthy, or gone when it is not listed.
func alertListedKinds(snapshot kubeindex.Snapshot) map[string]bool {
	listed := map[string]bool{}
	for kind, state := range snapshot.Kinds {
		if state.State == "ready" && !state.Partial && state.Error == "" && !state.Absent && !state.CycleFailure {
			listed[kind] = true
		}
	}
	return listed
}

// alertBaselinePartial is the partial listing the alert baseline judges a
// cycle by. It is the capture's partial listing, except that a kind the
// identity may not read, or reads only inside its namespaces, does not count:
// that gap is steady, so it never makes a later cycle more complete.
func alertBaselinePartial(snapshot kubeindex.Snapshot) bool {
	for kind, state := range snapshot.Kinds {
		if state.Partial && kind != "Secret" && kind != "ControllerRevision" && !alertBaselineSteadyGap(state) {
			return true
		}
	}
	return false
}

// alertBaselineSteadyGap reports a kind whose listing gap is a capability
// limit of the identity rather than a sync still in progress.
func alertBaselineSteadyGap(state kubeindex.KindStatus) bool {
	return isUnreadableSyncKind(state) || isNamespaceScopedSyncKind(state)
}

// recordAlertsEvaluated saves when the first complete continuous cycle
// evaluated alerts that were enabled before any cycle could, unless the
// settings changed since.
func (service *Service) recordAlertsEvaluated(ctx context.Context, enabledAt, at time.Time) error {
	provider, key := service.logSettingsStore()
	return service.commitCredential(ctx, func(ctx context.Context) error {
		return breaklogs.UpdateSettingsContext(ctx, provider, key, func(state *breaklogs.SettingsState) error {
			a := &state.Settings.Alerts
			if a.Enabled && a.EnabledAt.Equal(enabledAt) && a.EvaluatedFrom.IsZero() {
				a.EvaluatedFrom = at.UTC()
			}
			return nil
		})
	})
}

// evaluateIssueAlerts only schedules work. Capture holds its own lock while calling us.
func (service *Service) evaluateIssueAlerts(items []kubebreaks.Break, owner string, now time.Time, partials ...bool) {
	partial := len(partials) > 0 && partials[0]
	service.evaluateIssueAlertsWithDeferred(items, nil, owner, now, partial, !partial, nil)
}

// evaluateIssueAlertsWithDeferred schedules one alert run. settled is set
// when the cycle saw every kind it reads fully listed, with no break left
// waiting on its root: only such a cycle may close the baseline of breaks
// that existed before alerting could see them. listed holds the kinds the
// cycle listed in full; see alertListedKinds.
func (service *Service) evaluateIssueAlertsWithDeferred(items, deferred []kubebreaks.Break, owner string, now time.Time, partial, settled bool, listed map[string]bool) {
	if owner == "" || service.captureMode == "demand" || service.issueAlertSink == nil || service.scrubber == nil {
		return
	}
	scope := service.indexScope()
	service.changes.mu.Lock()
	if service.changes.issueAlerts == nil {
		service.changes.issueAlerts = map[kubeindex.Scope]*issueAlertWindow{}
	}
	key := scope
	window := service.changes.issueAlerts[key]
	if window == nil {
		window = &issueAlertWindow{}
		service.changes.issueAlerts[key] = window
	}
	// Every workload broken in this cycle, including breaks the owned
	// baseline holds back and breaks still waiting on their root.
	roots := brokenRoots(items, deferred)
	items = window.baseline.observe(items, owner, now, partial, deferred...)
	if window.active || service.changes.stopped[scope] {
		service.changes.mu.Unlock()
		return
	}
	window.active = true
	parent := service.changes.lifecycle
	if loop := service.changes.captures[scope]; loop != nil && loop.ownedContext != nil {
		parent = loop.ownedContext
	}
	if parent == nil {
		parent = service.identity.ctx
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	window.cancel, window.done = cancel, make(chan struct{})
	copyItems := append([]kubebreaks.Break(nil), items...)
	complete := settled && !partial && len(deferred) == 0
	service.changes.mu.Unlock()
	go func() {
		defer cancel()
		stop := context.AfterFunc(service.identity.ctx, cancel)
		defer stop()
		defer func() {
			service.changes.mu.Lock()
			window.active = false
			window.cancel = nil
			close(window.done)
			service.changes.mu.Unlock()
		}()
		service.logIssueAlertDelivery(service.emitIssueAlertCycle(ctx, copyItems, roots, listed, owner, now, window, complete))
	}()
}

// logIssueAlertDelivery warns when issue alerts could not be delivered, at
// most once per throttle window while it lasts. A run ended by shutdown or by
// another replica taking the lease is not a failure. Error text from a sink may
// include upstream payloads, so only a cause class is logged.
func (service *Service) logIssueAlertDelivery(err error) {
	key := problemKey(service.scope.OrgID, service.scope.ClusterID, "issue_alerts")
	switch {
	case err == nil:
		problems().Clear(key, "kubernetes: cluster %q issue alerts delivered again", service.scope.ClusterID)
	case errors.Is(err, context.Canceled) || errors.Is(err, kubechanges.ErrLeaseLost) || service.identity.ctx.Err() != nil:
		logging.Debugf("kubernetes: issue alert run for cluster %q ended early", service.scope.ClusterID)
	case isHistoryStoreError(err):
		problems().Fail(slog.LevelWarn, key, "storage unavailable", "kubernetes: cluster %q issue alerts paused (storage unavailable); they resume on the next cycle the history store answers; check that the storage backend is reachable", service.scope.ClusterID)
	default:
		cause := "delivery failed"
		if errors.Is(err, context.DeadlineExceeded) {
			cause = "timeout"
		}
		problems().Fail(slog.LevelWarn, key, cause, "kubernetes: cluster %q issue alerts were not delivered (%s); check the issue alert channel settings and that its destination is reachable", service.scope.ClusterID, cause)
	}
}

func (service *Service) emitIssueAlerts(ctx context.Context, items []kubebreaks.Break, owner string, now time.Time, window *issueAlertWindow) error {
	return service.emitIssueAlertCycle(ctx, items, brokenRoots(items, nil), nil, owner, now, window, true)
}

// brokenRoots maps each workload with a break among items and deferred, by
// UID, to its kind.
func brokenRoots(items, deferred []kubebreaks.Break) map[string]string {
	roots := make(map[string]string, len(items)+len(deferred))
	for _, list := range [][]kubebreaks.Break{items, deferred} {
		for _, b := range list {
			if b.Root.UID != "" {
				roots[b.Root.UID] = b.Root.Kind
			}
		}
	}
	return roots
}

// emitIssueAlertCycle sends the alerts one cycle admits. roots lists every
// workload broken in the cycle, admitted or not, with its kind. listed holds
// the kinds the cycle listed in full. complete reports a cycle that listed
// everything it reads; see evaluateIssueAlertsWithDeferred.
func (service *Service) emitIssueAlertCycle(ctx context.Context, items []kubebreaks.Break, roots map[string]string, listed map[string]bool, owner string, now time.Time, window *issueAlertWindow, complete bool) error {
	settings, err := service.IssueAlertSettings(ctx)
	if err != nil || !settings.Enabled {
		return err
	}
	store := service.changeStore()
	if store == nil {
		return errors.New("kubernetes: issue alert storage unavailable")
	}
	if err := store.CheckCaptureLease(owner); err != nil {
		return err
	}
	// The record is bound to the cluster UID once the cluster has one. Until
	// then alerts run on the cluster's unscoped record, which may be for good:
	// an identity that cannot list Namespaces never establishes the UID. When
	// the UID arrives and has no record yet, the unscoped record carries over,
	// so open breaks are neither baselined again nor alerted again. A window
	// already bound to a UID keeps it while the scope cannot be read.
	clusterUID, known, err := service.alertClusterUID(ctx)
	if err != nil {
		logging.Debugf("kubernetes: cluster %q issue alert run skipped: history scope unreadable; retrying next cycle", service.scope.ClusterID)
		return err
	}
	_, settingsKey := service.logSettingsStore()
	unscoped := alertCheckpointKey(settingsKey, "")
	if known || window.key == "" {
		window.key = alertCheckpointKey(settingsKey, clusterUID)
	}
	if err := service.loadAlertCheckpoint(window, unscoped); err != nil {
		return err
	}
	if !window.enabledAt.Equal(settings.EnabledAt) {
		// Alerts were turned off and on again: whatever an earlier baseline
		// gathered, or was still gathering, belongs to the earlier setting.
		window.start, window.sent, window.rollup, window.seen, window.suppressed = now, 0, false, nil, nil
		window.enabledAt = settings.EnabledAt
		window.enriched = nil
		window.baselining = false
		window.baselineSince = time.Time{}
		window.baselineRoots = nil
		window.rollupCount = 0
		window.rollupSeverity = ""
	}
	if window.start.IsZero() || now.Sub(window.start) >= 5*time.Minute {
		window.start, window.sent, window.rollup = now, 0, false
		window.rollupCount = 0
		window.rollupSeverity = ""
	}
	if window.seen == nil {
		window.seen = map[string]bool{}
	}
	if window.enriched == nil {
		window.enriched = map[string]bool{}
	}
	if window.suppressed == nil {
		window.suppressed = map[string]bool{}
	}
	if window.baselineRoots == nil {
		window.baselineRoots = map[string]string{}
	}
	// Alerts enabled in this release record when evaluation began at the
	// moment they are enabled; breaks older than that never alert, so a
	// first record needs no baseline.
	enabledHere := !settings.evaluatedFrom.IsZero() && settings.evaluatedFrom.Equal(settings.EnabledAt)
	if !window.stored && !enabledHere || window.baselining {
		// There is no alert record for this cluster although alerts were
		// enabled before: an earlier release evaluated them under a record it
		// keyed differently, the cluster captured on demand only, or the
		// record was lost. What the first cycles find was already broken
		// before alerting here could see it, so it joins the baseline without
		// a message. The baseline holds each broken workload, not one issue:
		// a workload that flips between issue types, or whose break time
		// moves with every restart, stays quiet until a complete cycle sees
		// it healthy. Cycles that still wait on a kind or on a break's root
		// only add to it; the first complete cycle closes it, and from then
		// on a later break alerts as usual.
		for _, b := range items {
			window.suppressed[b.Root.UID+"\x00"+b.Rule+"\x00"+b.BrokeAt.UTC().Format(time.RFC3339Nano)+"\x00"+b.Severity] = true
		}
		for root, kind := range roots {
			window.baselineRoots[root] = kind
		}
		baselineNow := now
		if service.alertBaselineClock != nil {
			baselineNow = service.alertBaselineClock()
		}
		if complete {
			window.baselineSince = time.Time{}
		} else if window.baselineSince.IsZero() {
			window.baselineSince = baselineNow
		} else if baselineNow.Sub(window.baselineSince) >= alertBaselineBound {
			// No complete cycle came within the bound: the breaks gathered so
			// far are the baseline, and later breaks alert as usual.
			logging.Debugf("kubernetes: cluster %q issue alert baseline closed after %s without a complete cycle", service.scope.ClusterID, alertBaselineBound)
			complete, window.baselineSince = true, time.Time{}
			service.changes.mu.Lock()
			if window.baseline.owner == owner {
				window.baseline.ready = true
			}
			service.changes.mu.Unlock()
		}
		window.start, window.baselining = now, !complete
		if err := service.commitCredential(ctx, func(ctx context.Context) error { return service.alertCheckpoint(ctx, window, owner) }); err != nil {
			return err
		}
		if !complete {
			return nil
		}
		return service.recordAlertsEvaluated(ctx, settings.EnabledAt, now)
	}
	// Bound memory to current breaks while retaining severity-specific transitions.
	active := map[string]bool{}
	for _, b := range items {
		active[b.Root.UID+"\x00"+b.Rule+"\x00"+b.BrokeAt.UTC().Format(time.RFC3339Nano)+"\x00"+b.Severity] = true
	}
	for key := range window.seen {
		if !active[key] {
			delete(window.seen, key)
			delete(window.enriched, key)
		}
	}
	for key := range window.suppressed {
		if !active[key] {
			delete(window.suppressed, key)
		}
	}
	// A workload the baseline found broken alerts again only after a cycle
	// that listed it in full saw it healthy: Pods and the workload's own kind
	// listed completely, with no break on it. A workload that no longer exists
	// is dropped on the same terms. A cycle that did not list its kind in full
	// may simply have missed it, so it stays held; another kind that never
	// lists in full does not keep it held.
	for root, kind := range window.baselineRoots {
		if _, broken := roots[root]; broken {
			continue
		}
		if complete || listed["Pod"] && kind != "" && listed[kind] {
			delete(window.baselineRoots, root)
		}
	}
	pending := []kubebreaks.Break{}
	keys := []string{}
	for _, b := range items {
		key := b.Root.UID + "\x00" + b.Rule + "\x00" + b.BrokeAt.UTC().Format(time.RFC3339Nano) + "\x00" + b.Severity
		if err := ctx.Err(); err != nil {
			return err
		}
		_, held := window.baselineRoots[b.Root.UID]
		if window.suppressed[key] || held || b.BrokeAt.Before(settings.EnabledAt) || b.BrokeAt.IsZero() || (b.Severity != "critical" && (settings.Level != "warning" || b.Severity != "warning")) {
			continue
		}
		pending = append(pending, b)
		keys = append(keys, key)
	}
	// Only new admissions need history. Repeated observations still refresh the
	// shared episode, and overflow uses a cluster rollup without graph work.
	enrichItems := []kubebreaks.Break{}
	enrichKeys := map[string]bool{}
	remaining := max(0, 10-window.sent)
	for i, b := range pending {
		if len(enrichItems) == 10 {
			break
		}
		if window.enriched[keys[i]] {
			continue
		}
		if !window.seen[keys[i]] {
			if remaining == 0 {
				continue
			}
			remaining--
		}
		enrichItems = append(enrichItems, b)
		enrichKeys[keys[i]] = true
	}
	var enrichment *issueAlertEnrichment
	for i, b := range pending {
		a := service.issueAlert(b)
		if window.suppressed[keys[i]] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := service.commitCredential(ctx, func(ctx context.Context) error { return store.CheckCaptureLeaseContext(ctx, owner) }); err != nil {
			return err
		}
		latest, err := service.IssueAlertSettings(ctx)
		if err != nil {
			return err
		}
		if !latest.Enabled || !latest.EnabledAt.Equal(settings.EnabledAt) {
			return nil
		}
		if latest.Level == "critical" && a.Severity != "critical" {
			continue
		}
		if !window.seen[keys[i]] && window.sent >= 10 {
			for offset, key := range keys[i:] {
				if !window.seen[key] && !window.suppressed[key] {
					window.suppressed[key] = true
					window.rollupCount++
					if pending[i+offset].Severity == "critical" {
						window.rollupSeverity = "critical"
					}
				}
			}
			if window.rollupSeverity == "" {
				window.rollupSeverity = "warning"
			}
			a = IssueAlert{OrgID: service.scope.OrgID, Cluster: service.scope.ClusterID, Rule: "storm_rollup", RuleLabel: "More issues", Severity: window.rollupSeverity, BrokeAt: window.start, RollupCount: window.rollupCount}
		} else if enrichKeys[keys[i]] {
			if enrichment == nil {
				prepared, err := service.prepareIssueAlertEnrichment(ctx, enrichItems, now)
				if err != nil {
					return err
				}
				enrichment = &prepared
			}
			enrichment.enrich(&a, b)
		}
		// Reserve admission durably before egress. A crash cannot reset the storm
		// budget; retrying an admitted occurrence still uses the shared episode claim.
		if a.RollupCount == 0 && !window.seen[keys[i]] {
			window.sent++
			window.seen[keys[i]] = true
			if err := service.commitCredential(ctx, func(ctx context.Context) error { return service.alertCheckpoint(ctx, window, owner) }); err != nil {
				return err
			}
		}
		if _, err := service.issueAlertSink(ctx, a); err != nil {
			return err
		}
		if a.RollupCount > 0 {
			window.rollup = true
			for _, key := range keys[i:] {
				if !window.seen[key] {
					window.suppressed[key] = true
				}
			}
			if err := service.alertCheckpoint(ctx, window, owner); err != nil {
				return err
			}
			continue
		}
		window.seen[keys[i]] = true
		if enrichKeys[keys[i]] {
			window.enriched[keys[i]] = true
		}
		if err := service.alertCheckpoint(ctx, window, owner); err != nil {
			return err
		}
	}
	return service.alertCheckpoint(ctx, window, owner)
}

func (service *Service) issueAlert(b kubebreaks.Break) IssueAlert {
	labels := map[string]string{
		"pod.crash_loop": "is crash looping", "pod.image_pull": "cannot pull an image",
		"pod.config_error": "has a configuration error", "pod.evicted": "was evicted",
		"pod.failed": "has failed", "pod.unschedulable": "cannot be scheduled",
		"workload.rollout_stalled": "has a stalled rollout", "workload.unavailable": "is unavailable",
		"job.failed": "has failed", "node.not_ready": "is not ready",
	}
	label := labels[b.Rule]
	if label == "" {
		label = strings.ReplaceAll(b.Rule, "_", " ")
	}
	a := IssueAlert{OrgID: service.scope.OrgID, Cluster: service.scope.ClusterID, Namespace: b.Root.Namespace, WorkloadKind: b.Root.Kind, Workload: b.Root.Name, RootUID: b.Root.UID, ResourceID: indexedResourceIDs[b.Root.Kind], Rule: b.Rule, RuleLabel: label, Severity: b.Severity, Count: b.Count, Of: b.Of, BrokeAt: b.BrokeAt}
	for _, t := range b.Evidence.Terminations {
		if len(a.Evidence) == 5 {
			break
		}
		a.Evidence = append(a.Evidence, fmt.Sprintf("Exit %d %s %d times", t.ExitCode, alertReason(t.Reason), t.Count))
	}
	if b.Evidence.Restarts > 0 && len(a.Evidence) < 5 {
		a.Evidence = append(a.Evidence, fmt.Sprintf("%d restarts", b.Evidence.Restarts))
	}
	if b.Evidence.Warnings > 0 && len(a.Evidence) < 5 {
		a.Evidence = append(a.Evidence, fmt.Sprintf("%d warning events", b.Evidence.Warnings))
	}
	for _, waiting := range b.Evidence.Waiting {
		if len(a.Evidence) == 5 {
			break
		}
		a.Evidence = append(a.Evidence, fmt.Sprintf("%s %d times", alertReason(waiting.Reason), waiting.Count))
	}
	for _, reasons := range [][]kubebreaks.ReasonCount{b.Evidence.PodReasons, b.Evidence.Nodes, b.Evidence.WarningReasons} {
		for _, reason := range reasons {
			if len(a.Evidence) == 5 {
				break
			}
			a.Evidence = append(a.Evidence, fmt.Sprintf("%s %d times", alertReason(reason.Reason), reason.Count))
		}
	}
	return a
}

var issueReasonPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

func alertReason(reason string) string {
	if !issueReasonPattern.MatchString(reason) {
		return "Unknown"
	}
	return reason
}

type issueAlertEnrichment struct {
	snapshot   kubeindex.Snapshot
	edges      kubegraph.Graph
	history    kubechanges.Page
	incomplete bool
	denied     map[string]bool
}

func (service *Service) prepareIssueAlertEnrichment(ctx context.Context, items []kubebreaks.Break, now time.Time) (issueAlertEnrichment, error) {
	result := issueAlertEnrichment{}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if len(items) == 0 {
		return result, nil
	}
	idx, err := service.indexes.ForScope(service.indexScope())
	if err != nil {
		return result, err
	}
	result.snapshot = idx.Snapshot() // cached projection only, never a cluster read
	from, to := items[0].BrokeAt.Add(-30*time.Minute), items[0].BrokeAt.Add(2*time.Minute)
	for _, b := range items[1:] {
		if at := b.BrokeAt.Add(-30 * time.Minute); at.Before(from) {
			from = at
		}
		if at := b.BrokeAt.Add(2 * time.Minute); at.After(to) {
			to = at
		}
	}
	if service.evidenceStore() != nil {
		result.history, err = service.evidenceChangeWindow(ctx, from, to, "", "")
	} else {
		result.history, err = service.changeStore().ReadWindow(from, to, now)
	}
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return result, contextErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, kubechanges.ErrLeaseLost) || errors.Is(err, kubechanges.ErrInvalidQuery) {
			return result, err
		}
		// Only optional history-read failures degrade to safe facts. Lease,
		// settings, credential, context and sink errors remain fatal to this run.
		class := "unavailable"
		if errors.Is(err, storage.ErrNotFound) {
			class = "not_found"
		}
		if errors.Is(err, kubechanges.ErrStoreLimit) {
			class = "invalid_history"
		}
		logging.Debugf("kubernetes: issue alert history incomplete cluster=%q class=%s", service.scope.ClusterID, class)
		result.incomplete = true
		return result, nil
	}
	var complete bool
	result.edges, complete = kubegraph.BuildComplete(result.snapshot, 200000)
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.incomplete = !complete
	result.denied = map[string]bool{}
	for _, kind := range []string{"Secret", "ConfigMap", "ServiceAccount", "PersistentVolumeClaim"} {
		result.denied[kind] = result.snapshot.Kinds[kind].Error == "forbidden"
	}
	return result, nil
}

func (enrichment issueAlertEnrichment) enrich(a *IssueAlert, b kubebreaks.Break) {
	a.Changes = []string{}
	linked := kubebreaks.Correlate(b, enrichment.snapshot.Records, enrichment.edges, enrichment.history, enrichment.incomplete, enrichment.denied)
	a.HistoryIncomplete = enrichment.incomplete || linked.Partial || linked.History != "complete"
	labels := map[kubechanges.Type]string{
		kubechanges.ImageChanged: "Image changed", kubechanges.ReplicasChanged: "Replicas changed",
		kubechanges.SpecChanged: "Specification changed", kubechanges.RolloutStarted: "Rollout started",
		kubechanges.RolloutCompleted: "Rollout completed", kubechanges.RolloutStalled: "Rollout stalled",
		kubechanges.ConfigRefChanged: "Configuration reference changed", kubechanges.ConfigChanged: "Configuration changed",
		kubechanges.Created: "Resource created", kubechanges.Deleted: "Resource deleted", kubechanges.WarningBurst: "Warning events increased",
	}
	for _, linked := range linked.Changes {
		label := labels[linked.Change.Type]
		if label == "" || linked.Change.At.After(b.BrokeAt) {
			continue
		}
		a.Changes = append(a.Changes, fmt.Sprintf("%s %s before break", label, b.BrokeAt.Sub(linked.Change.At).Round(time.Second)))
		if len(a.Changes) == 3 {
			break
		}
	}
}
