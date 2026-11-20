package kubernetes

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/logging"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// setLogLevel sets the leveled logger for one test and restores it afterwards.
func setLogLevel(t *testing.T, level string) {
	t.Helper()
	previous := logging.Level()
	logging.Setup(level)
	t.Cleanup(func() { logging.Setup(logging.LevelName(previous)) })
}

// isolateProblems gives one test its own process-wide warning throttle,
// reading the time from now (the real clock when nil), so no test sees
// another test's warning state. The previous throttle is restored afterwards.
func isolateProblems(t *testing.T, window time.Duration, now func() time.Time) *logging.Throttle {
	t.Helper()
	throttle := logging.NewThrottle(window, now)
	previous := problemLog.Swap(throttle)
	t.Cleanup(func() { problemLog.Store(previous) })
	return throttle
}

// fakeClock is a settable clock for throttle tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 19, 14, 5, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// newClockedKindLog returns an inventory logger whose throttle and failure
// episodes read the fake clock.
func newClockedKindLog(clock *fakeClock, window time.Duration) *inventoryKindLog {
	state := newInventoryKindLog()
	state.problems = logging.NewThrottle(window, clock.Now)
	state.now, state.window = clock.Now, window
	return state
}

// captureLogLines sends the standard logger, without timestamps, to a buffer
// at the given level for one test. The standard logger is process-wide, so a
// test that uses it never calls t.Parallel.
func captureLogLines(t *testing.T, level string) *lockedLog {
	t.Helper()
	collected := &lockedLog{}
	previousWriter, previousFlags := log.Writer(), log.Flags()
	log.SetOutput(collected)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	setLogLevel(t, level)
	isolateProblems(t, 0, nil)
	return collected
}

// all returns every line written so far.
func (l *lockedLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var lines []string
	for _, line := range strings.Split(l.buf.String(), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func (l *lockedLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Reset()
}

// visible returns the lines a default install shows: everything not at debug.
func visible(lines []string) []string {
	var shown []string
	for _, line := range lines {
		if !strings.HasPrefix(line, "DEBUG ") {
			shown = append(shown, line)
		}
	}
	return shown
}

// A healthy cluster's collection cycles write nothing a default install
// shows: per-cycle progress and stage results are debug. Its first readable
// cycle writes the single line saying the cluster is connected.
func TestHealthyClusterCyclesWriteNothingAtInfoOrAbove(t *testing.T) {
	logs := captureLogLines(t, "debug")
	cluster := newPartialRoleCluster(t, false)
	cluster.start()
	var connected []string
	for _, line := range visible(logs.all()) {
		if strings.Contains(line, "connected") {
			connected = append(connected, line)
		}
	}
	if len(connected) != 1 || !strings.HasPrefix(connected[0], `kubernetes: cluster "partial-role" connected, `) {
		t.Fatalf("connected lines = %q", connected)
	}
	logs.reset()
	cluster.cycle(nil, "Pod")
	cluster.cycle(nil, "Pod")
	if shown := visible(logs.all()); len(shown) != 0 {
		t.Fatalf("two healthy cycles wrote %d lines at info or above: %q", len(shown), shown)
	}
}

// A kind the reader cannot list writes one warning that names the cluster,
// the kind, the cause and what to check. The same failure on the next cycle
// writes nothing; a different cause writes one new warning.
func TestKindListFailureWarnsOnceAndAgainWhenItsCauseChanges(t *testing.T) {
	cluster := newPartialRoleCluster(t, false)
	cluster.start()
	logs := captureLogLines(t, "info")

	cluster.failSecrets.Store(true)
	cluster.cycle(nil, "Secret")
	shown := visible(logs.all())
	if len(shown) != 1 {
		t.Fatalf("a failing kind wrote %d lines, want one warning: %q\nstate: %+v", len(shown), shown, cluster.committed().View.Kinds["Secret"])
	}
	want := `WARN kubernetes: cluster "partial-role" cannot read kind=Secret (server error); check the API server serves these kinds; an aggregated API or a conversion webhook behind them may be failing`
	if shown[0] != want {
		t.Fatalf("warning = %q\nwant      %q", shown[0], want)
	}

	cluster.cycle(nil, "Secret")
	if again := visible(logs.all()); len(again) != 1 {
		t.Fatalf("the same failure on the next cycle wrote more: %q", again[1:])
	}

	cluster.failSecrets.Store(false)
	cluster.denySecrets.Store(true)
	cluster.cycle(nil, "Secret")
	shown = visible(logs.all())
	if len(shown) != 2 {
		t.Fatalf("a different cause wrote %d new lines, want one: %q\nstate: %+v", len(shown)-1, shown[1:], cluster.committed().View.Kinds["Secret"])
	}
	want = `WARN kubernetes: cluster "partial-role" cannot read kind=Secret (forbidden); grant list on these kinds to the reader role, or add them to exclude_resources`
	if shown[1] != want {
		t.Fatalf("second warning = %q\nwant             %q", shown[1], want)
	}
}

// A failure that lasts is repeated once per throttle window with a count; a
// kind that starts failing the same way is reported at once, and a kind that
// recovers writes nothing at info or above.
func TestKindWarningRepeatsOncePerWindowAndNamesNewKinds(t *testing.T) {
	logs := captureLogLines(t, "info")
	clock := newFakeClock()
	state := newClockedKindLog(clock, 10*time.Minute)
	scope := kubeindex.Scope{OrgID: "default", ClusterID: "prod", CredentialID: "epoch"}
	denied := kubeindex.KindStatus{State: "error", Partial: true, Error: "forbidden"}
	ready := kubeindex.KindStatus{State: "ready", Records: 3}

	state.report(scope, map[string]kubeindex.KindStatus{"Secret": denied, "Pod": ready})
	clock.advance(time.Minute)
	state.report(scope, map[string]kubeindex.KindStatus{"Secret": denied, "Pod": ready})
	clock.advance(10 * time.Minute)
	state.report(scope, map[string]kubeindex.KindStatus{"Secret": denied, "Pod": ready})
	lines := logs.all()
	if len(lines) != 2 || !strings.Contains(lines[0], "cannot read kind=Secret (forbidden)") || !strings.HasSuffix(lines[1], "(still failing, 3 times since 14:05)") {
		t.Fatalf("lasting failure lines = %q", lines)
	}

	state.report(scope, map[string]kubeindex.KindStatus{"Secret": denied, "Gateway": denied, "Pod": ready})
	lines = logs.all()
	if len(lines) != 3 || !strings.Contains(lines[2], "cannot read kind=Gateway,Secret (forbidden)") {
		t.Fatalf("a newly denied kind was not reported: %q", lines)
	}

	state.report(scope, map[string]kubeindex.KindStatus{"Secret": ready, "Gateway": ready, "Pod": ready})
	if after := logs.all(); len(after) != 3 {
		t.Fatalf("recovery wrote at info or above: %q", after[3:])
	}
}

// A kind that fails on and off next to one that keeps failing, or that
// recovers while the other keeps failing, is warned about once per episode,
// not on every change. Only a kind outside the episode warns at once, and the
// episode ends after one window with no failing kind.
func TestKindWarningIsNotRepeatedWhileAKindFlaps(t *testing.T) {
	logs := captureLogLines(t, "info")
	clock := newFakeClock()
	state := newClockedKindLog(clock, 10*time.Minute)
	scope := kubeindex.Scope{OrgID: "default", ClusterID: "prod", CredentialID: "epoch"}
	denied := kubeindex.KindStatus{State: "error", Partial: true, Error: "forbidden"}
	ready := kubeindex.KindStatus{State: "ready", Records: 3}
	cycle := func(secret, gateway, route kubeindex.KindStatus) {
		state.report(scope, map[string]kubeindex.KindStatus{"Secret": secret, "Gateway": gateway, "HTTPRoute": route})
		clock.advance(15 * time.Second)
	}

	// Secret fails steadily; Gateway flaps every other cycle for 30 cycles.
	for i := 0; i < 30; i++ {
		gateway := ready
		if i%2 == 0 {
			gateway = denied
		}
		cycle(denied, gateway, ready)
	}
	// Gateway recovers while Secret keeps failing.
	cycle(denied, ready, ready)
	lines := logs.all()
	if len(lines) != 1 || !strings.Contains(lines[0], "cannot read kind=Gateway,Secret (forbidden)") {
		t.Fatalf("a flapping kind warned %d times: %q", len(lines), lines)
	}

	// Everything recovers, then the same kinds fail again within a window:
	// the episode continues and nothing is written.
	cycle(ready, ready, ready)
	cycle(denied, denied, ready)
	if after := logs.all(); len(after) != 1 {
		t.Fatalf("a failure returning within the window warned again: %q", after[1:])
	}

	// A kind outside the episode warns at once.
	cycle(denied, denied, denied)
	lines = logs.all()
	if len(lines) != 2 || !strings.Contains(lines[1], "cannot read kind=Gateway,HTTPRoute,Secret (forbidden)") {
		t.Fatalf("a newly failing kind was not reported: %q", lines)
	}

	// After a full window with no failing kind, the episode is over and the
	// same failure is a new start.
	cycle(ready, ready, ready)
	clock.advance(11 * time.Minute)
	cycle(ready, ready, ready)
	cycle(denied, ready, ready)
	lines = logs.all()
	if len(lines) != 3 || !strings.HasSuffix(lines[2], "cannot read kind=Secret (forbidden); grant list on these kinds to the reader role, or add them to exclude_resources") {
		t.Fatalf("a failure after a clear window was not reported as new: %q", lines)
	}
}

// A one-off timeout listing an optional kind heals on the next cycle and stays
// at debug; Namespace or Pod timing out means the API server is not answering
// and warns once with the cluster's hint.
func TestKindTimeoutWarnsOnlyWhenTheAPIServerIsNotAnswering(t *testing.T) {
	logs := captureLogLines(t, "info")
	state := newInventoryKindLog()
	scope := kubeindex.Scope{OrgID: "default", ClusterID: "prod", CredentialID: "epoch"}
	timedOut := kubeindex.KindStatus{State: "error", Partial: true, Error: "timeout"}
	state.report(scope, map[string]kubeindex.KindStatus{"Widget": timedOut})
	if lines := logs.all(); len(lines) != 0 {
		t.Fatalf("an optional kind timing out warned: %q", lines)
	}
	state.report(scope, map[string]kubeindex.KindStatus{"Widget": timedOut, "Pod": timedOut, "Namespace": timedOut})
	want := `WARN kubernetes: cluster "prod" cannot read kind=Namespace,Pod (unreachable); check the cluster endpoint and the network path from Versus to the API server`
	if lines := logs.all(); len(lines) != 1 || lines[0] != want {
		t.Fatalf("API server timeout lines = %q", lines)
	}
}

// A rejected credential warns once with what to check; the retry that the
// recovery schedule suppresses writes nothing, and the retry detail is debug.
func TestCredentialFailureWarnsOnceWithWhatToCheck(t *testing.T) {
	logs := captureLogLines(t, "info")
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	recovery := &clusterRecovery{cluster: "credential-warning-test"}
	recovery.failed(ErrUnauthorized, nil, "default")
	recovery.failed(ErrUnauthorized, nil, "default")
	want := `WARN kubernetes: cluster "credential-warning-test" credential check failed reason=credential_rejected (credential rejected); restore or rotate the credential configured for this cluster`
	if lines := logs.all(); len(lines) != 1 || lines[0] != want {
		t.Fatalf("credential failure lines = %q", lines)
	}
	recovery.committed("revision")
	if lines := logs.all(); len(lines) != 1 {
		t.Fatalf("recovery wrote at info or above: %q", lines[1:])
	}
}

// Issue alerts that cannot be delivered warn once per window with the cause
// class only, never the sink's error text; a run ended by shutdown or a lost
// lease writes nothing, and delivery succeeding again ends the episode.
func TestIssueAlertDeliveryFailureWarnsOncePerWindow(t *testing.T) {
	logs := captureLogLines(t, "info")
	clock := newFakeClock()
	isolateProblems(t, 10*time.Minute, clock.Now)
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "alerts"}, 0)
	failure := errors.New("CANARY upstream response body")
	service.logIssueAlertDelivery(failure)
	service.logIssueAlertDelivery(failure)
	service.logIssueAlertDelivery(context.Canceled)
	service.logIssueAlertDelivery(kubechanges.ErrLeaseLost)
	want := `WARN kubernetes: cluster "alerts" issue alerts were not delivered (delivery failed); check the issue alert channel settings and that its destination is reachable`
	if lines := logs.all(); len(lines) != 1 || lines[0] != want {
		t.Fatalf("delivery failure lines = %q", lines)
	}
	clock.advance(11 * time.Minute)
	service.logIssueAlertDelivery(failure)
	if lines := logs.all(); len(lines) != 2 || !strings.HasSuffix(lines[1], "(still failing, 3 times since 14:05)") {
		t.Fatalf("a lasting failure was not repeated once with a count: %q", lines)
	}
	service.logIssueAlertDelivery(nil)
	clock.advance(11 * time.Minute)
	service.logIssueAlertDelivery(context.DeadlineExceeded)
	lines := logs.all()
	if len(lines) != 3 || !strings.Contains(lines[2], "were not delivered (timeout)") {
		t.Fatalf("a new failure after recovery = %q", lines)
	}
	for _, line := range lines {
		if strings.Contains(line, "CANARY") {
			t.Fatalf("sink error text reached the log: %q", line)
		}
	}
}

// A capture the history store rejects for good warns once with its reason; a
// successful save ends the episode.
func TestRejectedHistorySaveWarnsOnce(t *testing.T) {
	logs := captureLogLines(t, "info")
	clock := newFakeClock()
	isolateProblems(t, 10*time.Minute, clock.Now)
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "history"}, 0)
	cache := &inventoryCache{}
	rejected := &captureStorageError{stage: "journal", cause: storage.ErrEvidenceLimit, rejected: true}
	for _, batch := range []string{"one", "two"} {
		if err := service.abandonRejectedHistorySave(cache, batch, rejected); err != nil {
			t.Fatalf("a rejected save was not abandoned: %v", err)
		}
	}
	want := `WARN kubernetes: cluster "history" history store rejected a capture; collection continues and the next capture records the gap reason=over_save_limit (storage rejected); restarting does not help, so if it repeats on every capture report it with this line`
	if lines := logs.all(); len(lines) != 1 || lines[0] != want {
		t.Fatalf("rejected save lines = %q", lines)
	}
	// A temporary failure keeps its retry and is warned about by the cycle.
	temporary := &captureStorageError{stage: "journal", cause: errors.New("connection reset")}
	if err := service.abandonRejectedHistorySave(cache, "three", temporary); err != temporary {
		t.Fatalf("a temporary failure was abandoned: %v", err)
	}
	if err := service.abandonRejectedHistorySave(cache, "four", nil); err != nil {
		t.Fatal(err)
	}
	clock.advance(11 * time.Minute)
	if err := service.abandonRejectedHistorySave(cache, "five", rejected); err != nil {
		t.Fatal(err)
	}
	if lines := logs.all(); len(lines) != 2 || lines[1] != want {
		t.Fatalf("a rejection after a successful save = %q", lines)
	}
}

// Storage without atomic writes warns once per cluster, also from the
// successor service a credential rotation builds; a rotating credential whose
// identity is not verified yet is a normal start and stays at debug.
func TestDemandCaptureWarnsOnlyForStorageWithoutAtomicWrites(t *testing.T) {
	logs := captureLogLines(t, "info")
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	scope := Scope{OrgID: "default", ClusterID: "demand"}
	NewService(nil, scope, 0).logDemandCapture("stable_principal_unverified")
	NewService(nil, scope, 0).logDemandCapture("atomic_storage_unavailable")
	NewService(nil, scope, 0).logDemandCapture("atomic_storage_unavailable")
	want := `WARN kubernetes: cluster "demand" saves history only on demand reason=atomic_storage_unavailable (storage without atomic writes); set STORAGE_TYPE to postgres or file to save history continuously`
	if lines := logs.all(); len(lines) != 1 || lines[0] != want {
		t.Fatalf("demand mode lines = %q", lines)
	}
}

// An excluded name the cluster does not serve warns once per cluster: the
// successor service a credential rotation builds starts with a fresh cache
// but does not warn again.
func TestUnmatchedExclusionWarnsOncePerCluster(t *testing.T) {
	logs := captureLogLines(t, "info")
	isolateProblems(t, 10*time.Minute, newFakeClock().Now)
	discovery := Discovery{Resources: []ResourceDefinition{{ID: "core~v1~secrets", Resource: "secrets", Kind: "Secret"}}}
	for range 2 {
		service := NewService(nil, Scope{OrgID: "default", ClusterID: "exclusions"}, 0)
		service.SetCollectionScope([]string{"secrets", "secret"}, nil)
		service.reportUnmatchedExclusions(&inventoryCache{}, discovery)
	}
	want := `WARN kubernetes: cluster "exclusions" exclude_resources entry "secret" matches no resource this cluster serves (invalid config); check the plural API name, for example secrets`
	if lines := logs.all(); len(lines) != 1 || lines[0] != want {
		t.Fatalf("unmatched exclusion lines = %q", lines)
	}
}
