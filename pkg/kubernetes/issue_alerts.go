package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubegraph "github.com/VersusControl/versus-incident/pkg/kubernetes/graph"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
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
}

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
}

func (service *Service) alertCheckpoint(window *issueAlertWindow, owner string, load bool) error {
	provider, key := service.logSettingsStore()
	key += ".alert-window"
	cas, ok := provider.(storage.BlobCAS)
	if !ok {
		return storage.ErrUnsupported
	}
	encoded, err := provider.ReadBlob(key)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if errors.Is(err, storage.ErrNotFound) {
		encoded = nil
	}
	if load {
		if len(encoded) > 8<<20 {
			return ErrInvalidArguments
		}
		var state issueAlertCheckpoint
		if len(encoded) > 0 && json.Unmarshal(encoded, &state) != nil {
			return ErrInvalidArguments
		}
		window.start, window.enabledAt, window.sent, window.seen, window.suppressed, window.rollup = state.Start, state.EnabledAt, state.Sent, state.Seen, state.Suppressed, state.Rollup
		window.rollupCount = state.RollupCount
		window.rollupSeverity = state.RollupSeverity
		window.enriched = state.Enriched
		return nil
	}
	replacement, err := json.Marshal(issueAlertCheckpoint{Start: window.start, EnabledAt: window.enabledAt, Sent: window.sent, Seen: window.seen, Enriched: window.enriched, Suppressed: window.suppressed, Rollup: window.rollup, RollupCount: window.rollupCount, RollupSeverity: window.rollupSeverity})
	if err != nil {
		return err
	}
	if len(replacement) > 8<<20 {
		return ErrInvalidArguments
	}
	if err := service.changeStore().CheckCaptureLease(owner); err != nil {
		return err
	}
	swapped, err := cas.CompareAndSwapBlob(key, encoded, replacement)
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
	result := IssueAlertSettings{Enabled: a.Enabled, Level: level, EnabledAt: a.EnabledAt, Available: service.captureMode != "demand"}
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
	return service.commitCredential(ctx, func() error {
		return breaklogs.UpdateSettings(provider, key, func(state *breaklogs.SettingsState) error {
			a := &state.Settings.Alerts
			if enabled && !a.Enabled {
				a.EnabledAt = time.Now().UTC()
			}
			a.Enabled, a.Level, a.UpdatedBy = enabled, level, actor
			return nil
		})
	})
}

// evaluateIssueAlerts only schedules work. Capture holds its own lock while calling us.
func (service *Service) evaluateIssueAlerts(items []kubebreaks.Break, owner string, now time.Time, partials ...bool) {
	partial := len(partials) > 0 && partials[0]
	service.evaluateIssueAlertsWithDeferred(items, nil, owner, now, partial)
}

func (service *Service) evaluateIssueAlertsWithDeferred(items, deferred []kubebreaks.Break, owner string, now time.Time, partial bool) {
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
		if err := service.emitIssueAlerts(ctx, copyItems, owner, now, window); err != nil {
			// Error text from a sink may include upstream payloads; keep the log bounded.
			log.Printf("kubernetes: issue alert delivery failed for cluster %q", service.scope.ClusterID)
		}
	}()
}

func (service *Service) emitIssueAlerts(ctx context.Context, items []kubebreaks.Break, owner string, now time.Time, window *issueAlertWindow) error {
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
	if err := service.alertCheckpoint(window, owner, true); err != nil {
		return err
	}
	if !window.enabledAt.Equal(settings.EnabledAt) {
		window.start, window.sent, window.rollup, window.seen, window.suppressed = now, 0, false, nil, nil
		window.enabledAt = settings.EnabledAt
		window.enriched = nil
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
	pending := []kubebreaks.Break{}
	keys := []string{}
	for _, b := range items {
		key := b.Root.UID + "\x00" + b.Rule + "\x00" + b.BrokeAt.UTC().Format(time.RFC3339Nano) + "\x00" + b.Severity
		if err := ctx.Err(); err != nil {
			return err
		}
		if window.suppressed[key] || b.BrokeAt.Before(settings.EnabledAt) || b.BrokeAt.IsZero() || (b.Severity != "critical" && (settings.Level != "warning" || b.Severity != "warning")) {
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
		if err := service.commitCredential(ctx, func() error { return store.CheckCaptureLease(owner) }); err != nil {
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
			if err := service.commitCredential(ctx, func() error { return service.alertCheckpoint(window, owner, false) }); err != nil {
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
			if err := service.alertCheckpoint(window, owner, false); err != nil {
				return err
			}
			continue
		}
		window.seen[keys[i]] = true
		if enrichKeys[keys[i]] {
			window.enriched[keys[i]] = true
		}
		if err := service.alertCheckpoint(window, owner, false); err != nil {
			return err
		}
	}
	return service.alertCheckpoint(window, owner, false)
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
		result.history, err = service.evidenceChangeWindow(ctx, from, to, "")
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
		log.Printf("kubernetes: issue alert history incomplete cluster=%q class=%s", service.scope.ClusterID, class)
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
