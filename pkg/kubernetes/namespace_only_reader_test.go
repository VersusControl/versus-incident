package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

// sampleUntil checks invariant every few milliseconds until done reports true,
// failing on the first sample where the invariant does not hold.
func sampleUntil(t *testing.T, budget time.Duration, describe string, done func() bool, invariant func() error) {
	t.Helper()
	deadline := time.Now().Add(scaledTimeout(budget))
	samples := 0
	for {
		samples++
		if err := invariant(); err != nil {
			t.Fatalf("sample %d: %v", samples, err)
		}
		if done() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %d samples waiting for %s", samples, describe)
		}
		time.Sleep(3 * time.Millisecond)
	}
}

// readsAsNamespaceAccess reports whether a health report names the two
// namespaces the fixture's namespace-only role grants.
func readsAsNamespaceAccess(report SyncHealth) bool {
	return report.ClusterWide != nil && !*report.ClusterWide && strings.Join(report.ReadableNamespaces, ",") == "payments,shop"
}

// After a restart under a role bound only inside a few namespaces, the first
// sync reads those namespaces on its own: the cluster is never reported as
// failed with every kind denied, no view answers permission denied while the
// first sync runs, and the usage and Helm panels answer throughout.
func TestNamespaceOnlyRestartShowsNoWholeClusterFailure(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, false)
	cluster.start()
	cluster.cycle(nil, "Pod")
	cluster.denySecrets.Store(true)
	cluster.namespaceOnly.Store(true)
	cluster.restart()
	cluster.registry.StartCapture(t.Context())
	ctx := t.Context()
	var last SyncHealth
	sampleUntil(t, 10*time.Second, "the namespace-only view after the restart", func() bool {
		return readsAsNamespaceAccess(last) && last.State == "ready"
	}, func() error {
		report, err := cluster.service.SyncHealth(ctx)
		if err != nil {
			return fmt.Errorf("sync health failed (%s): %w", DiagnoseError(err).Code, err)
		}
		if report.State == "error" {
			return fmt.Errorf("sync health reported a whole-cluster failure: class=%s failing=%d unreadable=%d", report.Class, report.FailingKinds, report.UnreadableKinds)
		}
		last = report
		if _, err := cluster.service.Overview(ctx); err != nil && !errors.Is(err, ErrInventoryNotReady) {
			return fmt.Errorf("overview answered %s: %w", DiagnoseError(err).Code, err)
		}
		if _, err := cluster.service.Top(ctx, TopOptions{Kind: "pod", Sort: "cpu", Limit: 5}); err != nil {
			return fmt.Errorf("top pod usage answered %s: %w", DiagnoseError(err).Code, err)
		}
		releases, err := cluster.service.Releases(ctx, HelmReleaseOptions{Limit: 5})
		if err != nil || releases.Access != HelmAccessNeedsSecretList {
			return fmt.Errorf("helm releases answered access=%q err=%v", releases.Access, err)
		}
		return nil
	})
}

// After a namespace-only period, whose stored inventory carries no cluster
// identity, a restart under a wider role reports syncing, never that the
// cluster did not respond.
func TestRestartAfterNamespaceOnlyPeriodIsNeverUnreachable(t *testing.T) {
	t.Parallel()
	cluster := namespaceOnlyCluster(t)
	cluster.namespaceOnly.Store(false)
	cluster.restart()
	cluster.registry.StartCapture(t.Context())
	ctx := t.Context()
	var last SyncHealth
	sampleUntil(t, 10*time.Second, "the first sync after the restart", func() bool {
		return last.State == "ready" || last.State == "partial"
	}, func() error {
		report, err := cluster.service.SyncHealth(ctx)
		if err != nil {
			return fmt.Errorf("sync health failed (%s): %w", DiagnoseError(err).Code, err)
		}
		last = report
		for _, read := range []func() error{
			func() error { _, err := cluster.service.Overview(ctx); return err },
			func() error { _, err := cluster.service.Workloads(ctx, WorkloadListOptions{}); return err },
		} {
			if err := read(); err != nil && (errors.Is(err, ErrClusterUnavailable) || DiagnoseError(err).Code == "connector_unreachable") {
				return fmt.Errorf("a view said the cluster did not respond: %w", err)
			}
		}
		return nil
	})
}

// A role bound only inside a few namespaces gets every view across all
// namespaces scoped to the namespaces it reads, on every cycle: the health
// report and the overview name them, Workloads and Issues answer with their
// rows, and history says it is off rather than failing.
func TestNamespaceOnlyRoleReadsItsNamespacesAcrossViews(t *testing.T) {
	t.Parallel()
	cluster := namespaceOnlyCluster(t)
	ctx := t.Context()
	holdsOnEveryCycle(t, 4, func(cycle int) {
		due := map[string]bool(nil)
		if cycle%2 == 1 {
			due = map[string]bool{"Pod": true, "Event": true}
		}
		cluster.cycle(due, "Pod")
	}, func() error {
		report, err := cluster.service.SyncHealth(ctx)
		if err != nil || !readsAsNamespaceAccess(report) {
			return fmt.Errorf("sync health did not name the readable namespaces: cluster_wide=%v namespaces=%v err=%v", report.ClusterWide, report.ReadableNamespaces, err)
		}
		overview, err := cluster.service.Overview(ctx)
		if err != nil || overview.ClusterWide || strings.Join(overview.ReadableNamespaces, ",") != "payments,shop" {
			return fmt.Errorf("overview did not name the readable namespaces: cluster_wide=%v namespaces=%v err=%v", overview.ClusterWide, overview.ReadableNamespaces, err)
		}
		workloads, err := cluster.service.Workloads(ctx, WorkloadListOptions{})
		if err != nil || len(workloads.Items) == 0 {
			return fmt.Errorf("workloads across all namespaces answered items=%d err=%v", len(workloads.Items), err)
		}
		for _, item := range workloads.Items {
			if item.Namespace != "shop" && item.Namespace != "payments" {
				return fmt.Errorf("workload %s/%s is outside the readable namespaces", item.Namespace, item.Name)
			}
		}
		if _, err := cluster.service.Issues(ctx, IssueOptions{Limit: 20}); err != nil {
			return fmt.Errorf("issues across all namespaces answered %s: %w", DiagnoseError(err).Code, err)
		}
		timeline, err := cluster.service.Timeline(ctx, TimelineQuery{})
		if err != nil || timeline.History != HistoryOffNamespaceAccess {
			return fmt.Errorf("timeline answered history=%q err=%v", timeline.History, err)
		}
		changes, err := cluster.service.Changes(ctx, ChangeQuery{})
		if err != nil || changes.History != HistoryOffNamespaceAccess {
			return fmt.Errorf("changes answered history=%q err=%v", changes.History, err)
		}
		return nil
	})
}

// The overview's partial failures come in one order on every answer, each with
// a display name and a plain sentence.
func TestOverviewPartialFailuresHoldOneOrderInPlainWords(t *testing.T) {
	t.Parallel()
	cluster := namespaceOnlyCluster(t)
	ctx := t.Context()
	digest := func() (string, error) {
		overview, err := cluster.service.Overview(ctx)
		if err != nil {
			return "", err
		}
		rows := []string{}
		for _, failure := range overview.Partial {
			if failure.Kind == "" || failure.Message == "" || failure.Message[:1] != strings.ToUpper(failure.Message[:1]) || !strings.HasSuffix(failure.Message, ".") {
				return "", fmt.Errorf("partial failure %+v is not a plain sentence", failure)
			}
			rows = append(rows, failure.ResourceID+"="+failure.Class)
		}
		return strings.Join(rows, ";"), nil
	}
	want, err := digest()
	if err != nil || want == "" {
		t.Fatalf("no partial failures to order: %v", err)
	}
	holdsOnEveryCycle(t, 4, func(cycle int) {
		cluster.cycle(map[string]bool{"Pod": true, "Event": true}, "Pod")
	}, func() error {
		for i := 0; i < 5; i++ {
			got, err := digest()
			if err != nil {
				return err
			}
			if got != want {
				return fmt.Errorf("partial failures reordered:\n got %s\nwant %s", got, want)
			}
		}
		return nil
	})
}

// Without list access to Secrets, the release list says so in a typed field
// instead of answering an empty list that reads as no releases; one release
// answers the same state; with access the field is absent.
func TestHelmReleasesWithoutSecretAccessSayWhy(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, false)
	cluster.denySecrets.Store(true)
	ctx := t.Context()
	page, err := cluster.service.Releases(ctx, HelmReleaseOptions{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(page)
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if body["access"] != "needs_secret_list" || body["docs_url"] != HelmSecretAccessDocsURL || !strings.Contains(fmt.Sprint(body["access_message"]), "Secrets") {
		t.Fatalf("an unreadable release list did not say why: %s", encoded)
	}
	_, err = cluster.service.Release(ctx, "default", "web")
	if detail := DiagnoseError(err); detail.Code != "helm_needs_secret_list" || !strings.Contains(detail.Action, HelmSecretAccessDocsURL) {
		t.Fatalf("one release answered %+v (%v)", detail, err)
	}
	cluster.denySecrets.Store(false)
	cluster.service.client.metadataMu.Lock()
	clear(cluster.service.client.metadataDenied)
	cluster.service.client.metadataMu.Unlock()
	page, err = cluster.service.Releases(ctx, HelmReleaseOptions{Limit: 5})
	if err != nil || page.Access != "" {
		t.Fatalf("a readable release list carried access=%q err=%v", page.Access, err)
	}
}

// ageOptionalAPIs makes every listed discovered API look last attempted age
// ago, so a test can show what the worker does once that time has passed.
func (cluster *partialRoleCluster) ageOptionalAPIs(age time.Duration) {
	cluster.ageOptionalAPI(age, "extra-")
}

// ageOptionalAPI ages the discovered APIs whose ID contains match.
func (cluster *partialRoleCluster) ageOptionalAPI(age time.Duration, match string) {
	cluster.cache.mu.Lock()
	defer cluster.cache.mu.Unlock()
	candidate, err := cloneInventoryGeneration(cluster.cache.generation)
	if err != nil {
		cluster.t.Fatal(err)
	}
	states := make(map[string]kubeindex.KindStatus, len(candidate.ResourceStatus))
	for id, state := range candidate.ResourceStatus {
		if strings.Contains(id, match) {
			state.LastAttemptAt = time.Now().UTC().Add(-age)
		}
		states[id] = state
	}
	candidate.ResourceStatus = states
	cluster.cache.generation = candidate
}

// A discovered API the role may not list is not listed again on every cycle:
// it waits about an hour, unless discovery changes, and it never appears in
// the overview's partial failures.
func TestDeniedOptionalAPIBacksOffUntilDiscoveryChanges(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, true)
	cluster.extraGroups.Store(2)
	cluster.denyWidgets.Store(true)
	cluster.start()
	cluster.cycle(nil, "Pod")
	if cluster.widgetLists.Load() == 0 {
		t.Fatal("the denied optional API was never listed")
	}
	before := cluster.widgetLists.Load()
	holdsOnEveryCycle(t, 3, func(int) {
		cluster.ageOptionalAPIs(20 * time.Minute)
		cluster.cycle(nil, "Pod")
	}, func() error {
		if listed := cluster.widgetLists.Load() - before; listed != 0 {
			return fmt.Errorf("a steadily denied optional API was listed %d more times within its backoff", listed)
		}
		overview, err := cluster.service.Overview(cluster.t.Context())
		if err != nil {
			return err
		}
		for _, failure := range overview.Partial {
			if strings.Contains(failure.ResourceID+failure.GroupVersion, "extra-") {
				return fmt.Errorf("a denied optional API the product does not show is a partial failure: %+v", failure)
			}
		}
		return nil
	})
	cluster.ageOptionalAPIs(61 * time.Minute)
	cluster.cycle(nil, "Pod")
	if cluster.widgetLists.Load() == before {
		t.Fatal("the denied optional API was not listed again after its backoff")
	}
	before = cluster.widgetLists.Load()
	cluster.denyWidgets.Store(false)
	cluster.extraGroups.Store(3)
	cluster.cycle(nil, "Pod")
	if cluster.widgetLists.Load() == before {
		t.Fatal("a changed discovery did not list the denied optional API again")
	}
}

// A connector that could not be built says why and what to change, and never
// echoes a fault text that did not come from this package.
func TestConnectorBuildFailureSaysWhy(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config")
	kubeconfig := `apiVersion: v1
kind: Config
current-context: cloud
clusters:
- name: cloud
  cluster:
    server: https://cloud.example.com
contexts:
- name: cloud
  context:
    cluster: cloud
    user: cloud
users:
- name: cloud
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: aws
`
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	_, execErr := ResolveAuthentication(AuthOptions{Mode: "kubeconfig", KubeconfigPath: path})
	if execErr == nil {
		t.Fatal("an exec kubeconfig was accepted")
	}
	registry := NewClusterRegistry(false, []ClusterEntry{{Info: ClusterInfo{ID: "default", Provider: "generic"}, Err: execErr}})
	_, err := registry.ResolveCluster("default", "")
	if !errors.Is(err, ErrClusterUnavailable) {
		t.Fatalf("a build failure no longer reads as an unavailable cluster: %v", err)
	}
	detail := DiagnoseError(err)
	if detail.Code != "connector_configuration_invalid" || !strings.Contains(detail.Message, "exec") || !strings.Contains(detail.Action, "eks, aks or gke") || strings.Contains(detail.Message, path) {
		t.Fatalf("an exec kubeconfig answered %+v", detail)
	}
	summaries := registry.Summaries(t.Context(), "default", nil)
	if len(summaries) != 1 || summaries[0].Error == nil || summaries[0].Error.Code != "connector_configuration_invalid" {
		t.Fatalf("the cluster list did not say why the connector failed: %+v", summaries)
	}
	foreign := NewClusterRegistry(false, []ClusterEntry{{Info: ClusterInfo{ID: "default"}, Err: errors.New("open /var/run/secret-token-abc123: denied")}})
	_, err = foreign.ResolveCluster("default", "")
	if detail := DiagnoseError(err); detail.Code != "connector_configuration_invalid" || strings.Contains(detail.Message+detail.Action, "abc123") || strings.Contains(detail.Message, "did not respond") {
		t.Fatalf("a foreign build fault answered %+v", detail)
	}
}

// A refused API-server address is named, with the setting that allows it.
func TestRefusedEndpointNamesAddressAndSetting(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ endpoint, address, setting string }{
		{"https://10.1.2.3", "10.1.2.3", "allow_private_networks"},
		{"https://127.0.0.1:6443", "127.0.0.1", "allow_loopback"},
	} {
		_, err := NewClient(Config{Endpoint: test.endpoint, Credentials: staticCredential(t, "refused-endpoint")})
		if !errors.Is(err, ErrInvalidEndpoint) {
			t.Fatalf("%s was not refused: %v", test.endpoint, err)
		}
		detail := DiagnoseError(err)
		if detail.Code != "connector_endpoint_refused" || !strings.Contains(detail.Message, test.address) || !strings.Contains(detail.Action, test.setting) {
			t.Fatalf("%s answered %+v", test.endpoint, detail)
		}
	}
}

const argoApplications = "argoproj.io~v1alpha1~applications"

func (cluster *partialRoleCluster) optionalState(id string) kubeindex.KindStatus {
	cluster.cache.mu.RLock()
	defer cluster.cache.mu.RUnlock()
	return cluster.cache.generation.ResourceStatus[id]
}

// A newly granted API the product shows is read again within minutes, not
// after the hour a denied API it does not show waits, and a live list that
// reads it ends the denial on the next cycle without waiting at all.
func TestGrantOnAShownAPIReachesCoverageWithoutDiscoveryChange(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, true)
	cluster.argoApps.Store(true)
	cluster.denyArgo.Store(true)
	cluster.start()
	cluster.cycle(nil, "Pod")
	if state := cluster.optionalState(argoApplications); state.Error != "forbidden" {
		t.Fatalf("the Argo CD API was not denied first: %+v", state)
	}
	cluster.denyArgo.Store(false)
	cluster.ageOptionalAPI(deniedShownRetry+time.Minute, "argoproj.io")
	cluster.cycle(nil, "Pod")
	if state := cluster.optionalState(argoApplications); state.State != "ready" || state.Error != "" {
		t.Fatalf("a granted Argo CD API was not read within its retry: %+v", state)
	}

	cluster.denyArgo.Store(true)
	cluster.ageOptionalAPI(deniedShownRetry+time.Minute, "argoproj.io")
	cluster.cycle(nil, "Pod")
	if state := cluster.optionalState(argoApplications); state.Error != "forbidden" {
		t.Fatalf("the Argo CD API was not denied again: %+v", state)
	}
	cluster.denyArgo.Store(false)
	apps, err := cluster.service.GitOpsApps(t.Context(), GitOpsAppOptions{})
	if err != nil || len(apps.Items) == 0 {
		t.Fatalf("the live GitOps list did not read the granted API: items=%d err=%v", len(apps.Items), err)
	}
	waitUntil(t, 5*time.Second, "the live list to end the denial", func() bool {
		state := cluster.optionalState(argoApplications)
		return state.State == "ready" && state.Error == ""
	})
}

// seedPaymentsWorkload adds a Deployment in payments, one of the namespaces
// the namespace-only role grants.
func (cluster *partialRoleCluster) seedPaymentsWorkload() {
	cluster.t.Helper()
	recorder := httptest.NewRecorder()
	cluster.fixture.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/_fake/mutate", strings.NewReader(`{"operation":"upsert","resource":"deployments","namespace":"payments","name":"ledger","object":{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"uid":"ledger-uid"},"spec":{"replicas":1,"selector":{"matchLabels":{"app":"ledger"}},"template":{"metadata":{"labels":{"app":"ledger"}},"spec":{"containers":[{"name":"c","image":"x"}]}}}}}`)))
	if recorder.Code != http.StatusOK {
		cluster.t.Fatalf("seed payments deployment: %d %s", recorder.Code, recorder.Body.String())
	}
}

// paymentsRowsAcrossNamespaces counts the payments rows the all-namespace
// Workloads and Issues answers serve.
func (cluster *partialRoleCluster) paymentsRowsAcrossNamespaces() (int, error) {
	ctx := cluster.t.Context()
	served := 0
	workloads, err := cluster.service.Workloads(ctx, WorkloadListOptions{})
	if err != nil && !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrInventoryNotReady) {
		return 0, err
	}
	for _, item := range workloads.Items {
		if item.Namespace == "payments" {
			served++
		}
	}
	issues, err := cluster.service.Issues(ctx, IssueOptions{Limit: 200})
	if err != nil && !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrInventoryNotReady) {
		return 0, err
	}
	for _, issue := range issues.Items {
		if issue.Root.Namespace == "payments" {
			served++
		}
	}
	return served, nil
}

// A namespace-only identity that loses its grant in one namespace stops
// serving that namespace's rows in all-namespace answers as soon as a read
// there is denied, not after the next inventory cycle; the namespace it still
// reads keeps its rows.
func TestLostNamespaceGrantHidesItsRowsBeforeTheNextCycle(t *testing.T) {
	t.Parallel()
	cluster := namespaceOnlyCluster(t)
	cluster.seedPaymentsWorkload()
	cluster.cycle(nil, "Pod")
	if served, err := cluster.paymentsRowsAcrossNamespaces(); err != nil || served == 0 {
		t.Fatalf("baseline served no payments rows: %d %v", served, err)
	}
	denied := "payments"
	cluster.denyNamespace.Store(&denied)
	// Let a cached workload proof expire, so the next answer proves again.
	time.Sleep(inventoryAuthorityProbeInterval + 200*time.Millisecond)
	for i := 0; i < 20; i++ {
		served, err := cluster.paymentsRowsAcrossNamespaces()
		if err != nil {
			t.Fatal(err)
		}
		if served != 0 {
			t.Fatalf("answer %d served %d payments rows after the payments grant was removed", i, served)
		}
		workloads, err := cluster.service.Workloads(t.Context(), WorkloadListOptions{})
		if err != nil || len(workloads.Items) == 0 {
			t.Fatalf("answer %d lost the shop rows: items=%d err=%v", i, len(workloads.Items), err)
		}
	}
	holdsOnEveryCycle(t, 2, func(int) { cluster.cycle(nil, "Pod") }, func() error {
		served, err := cluster.paymentsRowsAcrossNamespaces()
		if err != nil || served != 0 {
			return fmt.Errorf("served %d payments rows after the grant was removed: %v", served, err)
		}
		return nil
	})
}

// After a restart under a role that lost one namespace, no answer serves the
// stored rows of that namespace, neither before the worker starts nor while
// its first sync runs.
func TestRestartAfterLostNamespaceGrantServesNoStoredRows(t *testing.T) {
	t.Parallel()
	cluster := namespaceOnlyCluster(t)
	cluster.seedPaymentsWorkload()
	cluster.cycle(nil, "Pod")
	if served, err := cluster.paymentsRowsAcrossNamespaces(); err != nil || served == 0 {
		t.Fatalf("baseline served no payments rows: %d %v", served, err)
	}
	denied := "payments"
	cluster.denyNamespace.Store(&denied)
	cluster.restart()
	for i := 0; i < 5; i++ {
		if served, err := cluster.paymentsRowsAcrossNamespaces(); err != nil || served != 0 {
			t.Fatalf("before the worker started, answer %d served %d payments rows: %v", i, served, err)
		}
		listed, _ := cluster.service.CachedResources(t.Context(), ListOptions{ResourceID: "apps~v1~deployments"})
		for _, item := range listed.Items {
			if item.Namespace == "payments" {
				t.Fatalf("before the worker started, answer %d listed the stored payments Deployment %s", i, item.Name)
			}
		}
	}
	cluster.registry.StartCapture(t.Context())
	ctx := t.Context()
	var last SyncHealth
	sampleUntil(t, 10*time.Second, "the first sync under the narrower role", func() bool {
		return last.ClusterWide != nil && !*last.ClusterWide && strings.Join(last.ReadableNamespaces, ",") == "shop"
	}, func() error {
		if report, err := cluster.service.SyncHealth(ctx); err == nil {
			last = report
		}
		served, err := cluster.paymentsRowsAcrossNamespaces()
		if err != nil || served != 0 {
			return fmt.Errorf("served %d payments rows during the first sync: %v", served, err)
		}
		return nil
	})
}

// Secrets left out of the reader role on purpose and configured as not
// collected are never asked of the cluster and never reported as a permission
// gap; the Helm view still says it needs Secret access. Without the setting
// the same gap is reported as not readable.
func TestExcludedSecretsAreNotCollectedRatherThanDenied(t *testing.T) {
	t.Parallel()
	honest := newPartialRoleCluster(t, false)
	honest.denySecrets.Store(true)
	honest.start()
	if report, err := honest.service.SyncHealth(t.Context()); err != nil || !strings.Contains(strings.Join(unreadableKinds(report), ","), "Secret") || len(report.NotCollected) != 0 {
		t.Fatalf("without the setting the Secret gap was not reported as not readable: %v not_collected=%v %v", unreadableKinds(report), report.NotCollected, err)
	}

	cluster := newPartialRoleCluster(t, false)
	cluster.denySecrets.Store(true)
	cluster.excludeResources = []string{"secrets"}
	cluster.connect()
	cluster.start()
	ctx := t.Context()
	holdsOnEveryCycle(t, 3, func(int) { cluster.cycle(nil, "Pod") }, func() error {
		report, err := cluster.service.SyncHealth(ctx)
		if err != nil {
			return err
		}
		if report.State != "ready" || len(report.Unreadable) != 0 || strings.Join(report.NotCollected, ",") != "Secret" {
			return fmt.Errorf("sync health state=%s unreadable=%v not_collected=%v", report.State, unreadableKinds(report), report.NotCollected)
		}
		overview, err := cluster.service.Overview(ctx)
		if err != nil {
			return err
		}
		for _, failure := range overview.Partial {
			if failure.ResourceID == "core~v1~secrets" {
				return fmt.Errorf("the overview reports a not collected kind: %+v", failure)
			}
		}
		releases, err := cluster.service.Releases(ctx, HelmReleaseOptions{Limit: 5})
		if err != nil || releases.Access != HelmAccessNeedsSecretList || len(releases.Partial) != 0 {
			return fmt.Errorf("helm releases answered access=%q partial=%v err=%v", releases.Access, releases.Partial, err)
		}
		if requests := cluster.secretRequests.Load(); requests != 0 {
			return fmt.Errorf("Secrets were requested %d times although they are not collected", requests)
		}
		return nil
	})
}

// A namespace-only identity on a fresh install with its namespaces configured
// holds a steady partial view of both from the first sync, and again after a
// restart, with no whole-cluster error and no permission denied answer.
func TestConfiguredNamespacesHoldFromTheFirstSync(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, false)
	cluster.denySecrets.Store(true)
	cluster.namespaceOnly.Store(true)
	cluster.namespaces = []string{"payments", "shop"}
	cluster.connect()
	ctx := t.Context()
	for round := 0; round < 2; round++ {
		if round == 1 {
			cluster.restart()
		}
		cluster.registry.StartCapture(ctx)
		var last SyncHealth
		sampleUntil(t, 10*time.Second, "the namespace-only view from the first sync", func() bool {
			return readsAsNamespaceAccess(last) && last.State == "ready"
		}, func() error {
			report, err := cluster.service.SyncHealth(ctx)
			if err != nil {
				return fmt.Errorf("round %d: sync health failed (%s): %w", round, DiagnoseError(err).Code, err)
			}
			if report.State == "error" {
				return fmt.Errorf("round %d: sync health reported a whole-cluster failure: class=%s failing=%d", round, report.Class, report.FailingKinds)
			}
			last = report
			if _, err := cluster.service.Overview(ctx); errors.Is(err, ErrForbidden) {
				return fmt.Errorf("round %d: overview answered permission denied: %w", round, err)
			}
			if _, err := cluster.service.Workloads(ctx, WorkloadListOptions{}); errors.Is(err, ErrForbidden) {
				return fmt.Errorf("round %d: workloads across all namespaces answered permission denied: %w", round, err)
			}
			return nil
		})
	}
}

// A cluster-wide identity granted Secrets while their cluster-wide denial is
// still remembered stays cluster-wide: the kind is read cluster-wide again on
// the next cycle, and the report never switches to namespace access.
func TestWidenedGrantStaysClusterWide(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, false)
	cluster.denySecrets.Store(true)
	cluster.start()
	cluster.cycle(map[string]bool{"Secret": true, "Pod": true}, "Pod")
	cluster.denySecrets.Store(false)
	// The scoped denial memo has run out while the cluster-wide one has not,
	// so the next read inside a namespace answers before the cluster-wide one.
	scope := cluster.service.indexScope()
	cluster.service.client.clearMetadataDenial(metadataDenyKey{OrgID: scope.OrgID, ClusterID: scope.ClusterID, CredentialID: scope.CredentialID, Kind: "Secret", Namespace: "default"})
	ctx := t.Context()
	holdsOnEveryCycle(t, 3, func(cycle int) {
		due := map[string]bool{"Pod": true}
		if cycle == 0 {
			due["Secret"] = true
		}
		cluster.cycle(due, "Pod")
	}, func() error {
		report, err := cluster.service.SyncHealth(ctx)
		if err != nil || report.ClusterWide == nil || !*report.ClusterWide {
			return fmt.Errorf("a cluster-wide identity reported namespace access: cluster_wide=%v namespaces=%v err=%v", report.ClusterWide, report.ReadableNamespaces, err)
		}
		return nil
	})
	if state := cluster.committed().View.Kinds["Secret"]; state.State != "ready" || state.Error != "" {
		t.Fatalf("the granted kind was not read cluster-wide within two cycles: %+v", state)
	}
}

// A role that reads Pods cluster-wide and Secrets only inside a namespace is a
// stable grant, not a widening one: the cluster-wide Secret denial is asked
// again at most once, not on every cycle, and the report stays cluster-wide.
func TestStableNamespaceGrantIsNotRecheckedEveryCycle(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, false)
	cluster.denyClusterSecrets.Store(true)
	cluster.start()
	cluster.cycle(nil, "Pod")
	cluster.cycle(nil, "Pod")
	before := cluster.clusterSecretLists.Load()
	ctx := t.Context()
	holdsOnEveryCycle(t, 6, func(int) { cluster.cycle(map[string]bool{"Pod": true}, "Pod") }, func() error {
		if listed := cluster.clusterSecretLists.Load() - before; listed > 1 {
			return fmt.Errorf("a stable namespace grant re-listed Secrets cluster-wide %d times", listed)
		}
		report, err := cluster.service.SyncHealth(ctx)
		if err != nil || report.ClusterWide == nil || !*report.ClusterWide {
			return fmt.Errorf("a cluster-wide identity reported namespace access: %v %v", report.ClusterWide, err)
		}
		return nil
	})
}

// An excluded name no served resource has, such as a misspelling, is named so
// it can be logged; a served one is not.
func TestUnmatchedExclusionsAreNamed(t *testing.T) {
	t.Parallel()
	service := NewService(nil, Scope{ClusterID: "exclusions"}, 0)
	service.SetCollectionScope([]string{"secrets", "secret"}, nil)
	discovery := Discovery{Resources: []ResourceDefinition{{ID: "core~v1~secrets", Resource: "secrets", Kind: "Secret"}}}
	if got := strings.Join(service.unmatchedExclusions(discovery), ","); got != "secret" {
		t.Fatalf("unmatched exclusions = %q, want secret", got)
	}
}

// adopt binds the test to the inventory the running worker owns, as start does,
// for a cluster whose first sync the test watched itself.
func (cluster *partialRoleCluster) adopt() {
	cluster.service.changes.mu.Lock()
	cluster.cache = cluster.service.changes.inventories[cluster.service.indexScope()]
	cluster.service.changes.mu.Unlock()
	if cluster.cache == nil {
		cluster.t.Fatal("inventory worker did not start")
	}
	cluster.settle()
}

// A namespace-only identity with its namespaces configured, as the docs set it
// up, reaches its first ready moment within the first sync. Progress never moves
// back while it prepares, and afterwards the requests the page makes answer:
// Workloads and Issues across all namespaces, the live update stream, and
// GitOps saying it is not readable rather than empty.
func TestNamespaceOnlySetupReachesFirstReadyForThePageRequests(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, true)
	cluster.denySecrets.Store(true)
	cluster.namespaceOnly.Store(true)
	cluster.argoApps.Store(true)
	cluster.denyArgo.Store(true)
	cluster.namespaces = []string{"payments", "shop"}
	cluster.excludeResources = []string{"secrets"}
	cluster.connect()
	cluster.registry.StartCapture(t.Context())
	ctx := t.Context()
	var last SyncHealth
	step, done := 0, 0
	sampleUntil(t, 10*time.Second, "the first ready moment", func() bool { return !last.FirstReadyAt.IsZero() }, func() error {
		report, err := cluster.service.SyncHealth(ctx)
		if err != nil {
			return fmt.Errorf("sync health failed (%s): %w", DiagnoseError(err).Code, err)
		}
		last = report
		if progress := report.Progress; progress != nil && report.FirstReadyAt.IsZero() {
			if progress.StepIndex < step || progress.KindsDone < done {
				return fmt.Errorf("first-sync progress moved back: step %d->%d, kinds done %d->%d", step, progress.StepIndex, done, progress.KindsDone)
			}
			step, done = progress.StepIndex, progress.KindsDone
		}
		return nil
	})
	cluster.adopt()
	holdsOnEveryCycle(t, 3, func(int) { cluster.cycle(map[string]bool{"Pod": true}, "Pod") }, func() error {
		report, err := cluster.service.SyncHealth(ctx)
		if err != nil || report.State != "ready" || report.FirstReadyAt.IsZero() || len(report.Failing) != 0 || !readsAsNamespaceAccess(report) {
			return fmt.Errorf("sync health state=%s first_ready=%s failing=%+v namespaces=%v err=%v", report.State, report.FirstReadyAt, report.Failing, report.ReadableNamespaces, err)
		}
		workloads, err := cluster.service.Workloads(ctx, WorkloadListOptions{Limit: 20})
		if err != nil || len(workloads.Items) == 0 {
			return fmt.Errorf("workloads as the page asks answered items=%d err=%v", len(workloads.Items), err)
		}
		if _, err := cluster.service.Issues(ctx, IssueOptions{Limit: 20}); err != nil {
			return fmt.Errorf("issues as the page asks answered %s: %w", DiagnoseError(err).Code, err)
		}
		commit, err := cluster.service.CommitStatus(ctx)
		if err != nil || commit.History != HistoryOffNamespaceAccess {
			return fmt.Errorf("the live update stream answered history=%q err=%v", commit.History, err)
		}
		gitops, err := cluster.service.GitOpsApps(ctx, GitOpsAppOptions{Limit: 20})
		if err != nil || gitops.Access != AccessNotReadable {
			return fmt.Errorf("gitops answered access=%q items=%d err=%v", gitops.Access, len(gitops.Items), err)
		}
		return nil
	})
}

// A cluster-wide identity granted one kind only inside a namespace reaches its
// first ready moment: the kind is a namespace-scoped capability limit, not a
// sync that never finishes, and the report stays ready and cluster-wide.
func TestMixedRoleReachesFirstReadyWithANamespaceScopedKind(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, false)
	cluster.denyClusterSecrets.Store(true)
	cluster.start()
	ctx := t.Context()
	holdsOnEveryCycle(t, 3, func(int) { cluster.cycle(map[string]bool{"Pod": true}, "Pod") }, func() error {
		report, err := cluster.service.SyncHealth(ctx)
		if err != nil {
			return err
		}
		scoped := []string{}
		for _, row := range report.NamespaceScoped {
			scoped = append(scoped, row.Kind)
		}
		if report.State != "ready" || report.FirstReadyAt.IsZero() || len(report.Failing) != 0 || strings.Join(scoped, ",") != "Secret" || report.ClusterWide == nil || !*report.ClusterWide {
			return fmt.Errorf("mixed role report state=%s first_ready=%s failing=%+v namespace_scoped=%v cluster_wide=%v", report.State, report.FirstReadyAt, report.Failing, scoped, report.ClusterWide)
		}
		return nil
	})
}

// The commit stream of an identity that keeps no history reports the cluster's
// own sync state, never a stale collection, and repeats it only when it changes
// or as a keep-alive, not on every poll.
func TestStreamWithoutHistoryReportsSyncStateCalmly(t *testing.T) {
	t.Parallel()
	cluster := namespaceOnlyCluster(t)
	ctx := t.Context()
	pacer := &StreamSyncPacer{}
	sent := 0
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		commit, err := cluster.service.CommitStatus(ctx)
		if err != nil || commit.History != HistoryOffNamespaceAccess {
			t.Fatalf("the stream's commit status answered history=%q err=%v", commit.History, err)
		}
		event, send := cluster.service.NextStreamSync(ctx, commit, pacer)
		if send {
			sent++
			if event.Reason == "collection_stale" || event.State != "ready" {
				t.Fatalf("the stream reported state=%s reason=%s for a ready namespace-only cluster", event.State, event.Reason)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if sent == 0 || sent > 3 {
		t.Fatalf("the stream sent %d sync events in 10 s of polls", sent)
	}
}

// A namespace-only identity whose reads fail in one of its two namespaces does
// not report the cluster as ready through the other: the failing kind is a
// sync failure that names the namespace, and its age is the oldest read.
func TestFailingNamespaceIsNotReportedReadyThroughAnother(t *testing.T) {
	t.Parallel()
	cluster := namespaceOnlyCluster(t)
	ctx := t.Context()
	if report, err := cluster.service.SyncHealth(ctx); err != nil || report.State != "ready" {
		t.Fatalf("the steady namespace-only report is not ready: state=%s err=%v", report.State, err)
	}
	failing := "payments"
	cluster.failNamespace.Store(&failing)
	cluster.cycle(nil, "Pod")
	cluster.cycle(nil, "Pod")
	report, err := cluster.service.SyncHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pod *SyncKindProblem
	for index := range report.Failing {
		if report.Failing[index].Kind == "Pod" {
			pod = &report.Failing[index]
		}
	}
	if report.State == "ready" || pod == nil || pod.Namespace != "payments" || pod.Class == "namespace_scope" {
		t.Fatalf("a failing namespace was reported through the other: state=%s failing=%+v", report.State, report.Failing)
	}
	committed := cluster.committed()
	if shop := committed.Namespaces["shop"].Kinds["Pod"]; !committed.View.Kinds["Pod"].ObservedAt.Before(shop.ObservedAt) {
		t.Fatalf("the Pod age follows the fresh namespace %s, not the failing one %s", shop.ObservedAt, committed.View.Kinds["Pod"].ObservedAt)
	}
	cluster.failNamespace.Store(nil)
	cluster.cycle(nil, "Pod")
	if report, err := cluster.service.SyncHealth(ctx); err != nil || report.State != "ready" {
		t.Fatalf("after the namespace recovered the report did not return to ready: state=%s failing=%+v err=%v", report.State, report.Failing, err)
	}
}

// stageReason is the reason the worker last recorded for one capture stage.
func (cluster *partialRoleCluster) stageReason(stage string) string {
	recovery := cluster.service.recovery
	recovery.mu.Lock()
	defer recovery.mu.Unlock()
	return recovery.stages[stage].reason
}

// Optional kinds whose reads did not start before the window closed wait for
// a later cycle. That pacing is planned, so the optional stage still ends
// ready and writes no timeout line.
func TestPacedOptionalReadsAreNotACaptureFailure(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, true)
	cluster.extraGroups.Store(4)
	cluster.service.optionalStartWindow = time.Nanosecond
	cluster.start()
	cluster.cycle(nil, "Pod")
	if listed := cluster.widgetLists.Load(); listed != 0 {
		t.Fatalf("%d optional reads started after the start window closed", listed)
	}
	if reason := cluster.stageReason("additional"); reason != "ready" {
		t.Fatalf("paced optional reads recorded the stage as %q, want ready: %s", reason, cluster.stages())
	}
}

// An optional kind whose read fails is a real failure of the optional stage,
// while a kind the reader role may not list is a steady gap shown on the kind.
func TestOptionalStageRecordsFailedReadsButNotDenials(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, true)
	cluster.extraGroups.Store(2)
	cluster.start()
	cluster.cycle(nil, "Pod")
	if cluster.widgetLists.Load() == 0 {
		t.Fatal("the optional kinds were never listed")
	}
	cluster.failWidgets.Store(true)
	cluster.ageOptionalAPIs(20 * time.Minute)
	cluster.cycle(nil, "Pod")
	if reason := cluster.stageReason("additional"); reason != "collection_unavailable" {
		t.Fatalf("a failed optional read recorded the stage as %q, want collection_unavailable: %s", reason, cluster.stages())
	}
	cluster.failWidgets.Store(false)
	cluster.denyWidgets.Store(true)
	cluster.ageOptionalAPIs(20 * time.Minute)
	cluster.cycle(nil, "Pod")
	if reason := cluster.stageReason("additional"); reason != "ready" {
		t.Fatalf("a denied optional kind recorded the stage as %q, want ready: %s", reason, cluster.stages())
	}
}

// When a cycle can start only some of the due optional reads, the kinds left
// over lead the next cycle. Every kind is read within the cycles the pace
// needs, also beside kinds that fail and are retried on every cycle, and none
// waits longer than a bounded number of cycles afterwards.
func TestPacedOptionalReadsReachEveryKind(t *testing.T) {
	t.Parallel()
	service := &Service{}
	const kinds, failing, perCycle = 40, 3, 7
	resources := make([]ResourceDefinition, 0, kinds+failing)
	for index := range kinds + failing {
		name := fmt.Sprintf("kind%02d", index)
		resources = append(resources, ResourceDefinition{ID: "example.com~v1~" + name, Group: "example.com", Version: "v1", Resource: name, Kind: name, Namespaced: true, Verbs: []string{"list"}, Available: true})
	}
	fails := map[string]bool{}
	for _, definition := range resources[:failing] {
		fails[definition.ID] = true
	}
	states := map[string]kubeindex.KindStatus{}
	lastRead := map[string]int{}
	firstRound := (kinds + failing + perCycle - 1) / perCycle
	longestWait := 0
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for cycle := range 60 {
		now := start.Add(time.Duration(cycle) * 30 * time.Second)
		due := service.dueOptionalInventory(resources, states, now, func(string) bool { return false })
		for _, definition := range due[:min(perCycle, len(due))] {
			if fails[definition.ID] {
				states[definition.ID] = kubeindex.KindStatus{State: "error", Partial: true, Error: "unavailable", LastAttemptAt: now}
			} else {
				states[definition.ID] = kubeindex.KindStatus{State: "ready", ObservedAt: now, CompletedAt: now, LastAttemptAt: now}
			}
			if previous, read := lastRead[definition.ID]; read {
				longestWait = max(longestWait, cycle-previous)
			}
			lastRead[definition.ID] = cycle
		}
		if cycle == firstRound-1 && len(lastRead) != len(resources) {
			t.Fatalf("after %d cycles only %d of %d optional kinds were read", firstRound, len(lastRead), len(resources))
		}
	}
	for _, definition := range resources {
		if last := lastRead[definition.ID]; 59-last > 2*firstRound+10 {
			t.Fatalf("%s was last read on cycle %d of 60", definition.ID, last)
		}
	}
	if longestWait > 2*firstRound+10 {
		t.Fatalf("an optional kind waited %d cycles between reads", longestWait)
	}
}

// A discovered optional kind the paced reads have not reached yet is shown as
// loading, not as a type the cluster does not serve.
func TestQueuedOptionalKindIsShownAsLoading(t *testing.T) {
	t.Parallel()
	service := &Service{}
	queued := "example.com~v1~widgets"
	generation := inventoryGeneration{
		Discovery: &Discovery{Resources: []ResourceDefinition{{ID: queued, Group: "example.com", Version: "v1", Resource: "widgets", Kind: "Widget", Namespaced: true, Verbs: []string{"list"}, Available: true}}},
		View:      kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}}},
	}
	ctx := context.WithValue(t.Context(), inventoryReadContextKey{}, generation)
	page, err := service.CachedResources(ctx, ListOptions{ResourceID: queued})
	if err != nil || page.Sync.State != "partial" || !page.Sync.Partial || !containsString(page.Sync.Pending, queued) {
		t.Fatalf("a queued optional kind was not shown as loading: %+v err=%v", page.Sync, err)
	}
	if _, err := service.CachedResources(ctx, ListOptions{ResourceID: "example.com~v1~gadgets"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a type discovery does not serve answered %v, want not found", err)
	}
}
