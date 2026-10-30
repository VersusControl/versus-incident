package kubernetes_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/controllers"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakekube"
	"github.com/gofiber/fiber/v2"
)

type notificationClaimProcessBarrier struct {
	storage.Provider
	storage.BlobCAS
	storage.EvidenceStore
	storage.DetectionEpisodeStore
	storage.ScopedDetectionEpisodeStore
	storage.DetectionIncidentStore
	paused atomic.Bool
}

func (p *notificationClaimProcessBarrier) RecordDetectionOccurrenceForOrg(org string, o storage.DetectionOccurrence) (storage.DetectionEpisodeDecision, error) {
	d, e := p.ScopedDetectionEpisodeStore.RecordDetectionOccurrenceForOrg(org, o)
	if e == nil && d.NotificationClaimed && o.Identity.Source == "kubernetes:alpha" && os.Getenv("HARNESS_ALERT_PAUSE_CLAIM") == "1" && p.paused.CompareAndSwap(false, true) {
		raw, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "incident_id": d.IncidentID, "episode_id": d.EpisodeID, "claim_expires": d.ClaimExpiresAt, "org": org, "source": o.Identity.Source})
		if e = os.WriteFile(os.Getenv("HARNESS_ALERT_CLAIM_MARKER"), raw, 0600); e != nil {
			return d, e
		}
		select {}
	}
	return d, e
}
func alertProcessKey(scope kubernetes.Scope, capture bool) string {
	sum := sha256.Sum256([]byte(scope.OrgID + "\x00" + scope.ClusterID + "\x00" + scope.CredentialID))
	if capture {
		return "kubernetes/capture-state/" + hex.EncodeToString(sum[:]) + ".json"
	}
	return "kubernetes/inventory/" + base64.RawURLEncoding.EncodeToString(sum[:])
}
func TestDurableAlertWriterChild(t *testing.T) {
	if os.Getenv("HARNESS_ALERT_CHILD") != "1" {
		t.Skip("explicit owned OS writer only")
	}
	base, e := storage.NewPostgres(storage.PostgresOptions{DSN: os.Getenv("TEST_POSTGRES_DSN")})
	if e != nil {
		t.Fatal("own PG unavailable")
	}
	defer base.Close()
	p := &notificationClaimProcessBarrier{Provider: base, BlobCAS: base.(storage.BlobCAS), EvidenceStore: base.(storage.EvidenceStore), DetectionEpisodeStore: base.(storage.DetectionEpisodeStore), ScopedDetectionEpisodeStore: base.(storage.ScopedDetectionEpisodeStore), DetectionIncidentStore: base.(storage.DetectionIncidentStore)}
	if e = config.LoadConfig(os.Getenv("HARNESS_ALERT_CONFIG")); e != nil {
		t.Fatal(e)
	}
	services.SetStorage(p)
	services.SetEmitInterceptor(nil)
	cfg := config.KubernetesConnectorConfig{Multiple: true}
	for _, id := range []string{"alpha", "beta"} {
		cfg.Clusters = append(cfg.Clusters, config.KubernetesClusterConfig{KubernetesToolConfig: config.KubernetesToolConfig{Endpoint: os.Getenv("HARNESS_ALERT_" + strings.ToUpper(id) + "_URL"), ClusterID: id, CredentialID: id + "-reader", AllowLoopback: true, Timeout: "3s", DiscoveryTTL: "1h", ChangeCapture: "continuous", Auth: config.KubernetesAuthConfig{Mode: "token", Token: "synthetic-" + id}}})
	}
	registry, errs := agent.NewKubernetesRegistry(cfg, tenancy.DefaultOrgScope())
	if len(errs) > 0 {
		t.Fatal("registry config failed")
	}
	redactor, _ := agent.NewRedactor(false, nil)
	registry.Each(func(s *kubernetes.Service) { s.SetChangeStorage(p); s.SetScrubber(redactor) })
	agent.ConfigureKubernetesIssueAlerts(registry, func(ctx context.Context, org string, a kubernetes.IssueAlert) (kubernetes.Outcome, error) {
		if a.Cluster == "alpha" {
			raw, _ := json.Marshal(a)
			_ = os.WriteFile(os.Getenv("HARNESS_ALERT_PENDING"), raw, 0600)
		}
		return services.CreateIncidentFromKubernetesIssueContext(ctx, org, a)
	})
	registry.StartCapture(t.Context())
	defer registry.Close()
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	controllers.NewKubernetesAdminControllerWithRegistry(registry).Register(app.Group("/api"))
	controllers.NewIncidentAdminController().Register(app.Group("/api"))
	harnessAuth := func(c *fiber.Ctx) bool {
		if c.Get("X-Gateway-Secret") != "synthetic-process-gateway" {
			_ = c.SendStatus(401)
			return false
		}
		return true
	}
	app.Get("/__harness/state", func(c *fiber.Ctx) error {
		if !harnessAuth(c) {
			return nil
		}
		out := map[string]any{"pid": os.Getpid(), "clusters": map[string]any{}}
		clusters := out["clusters"].(map[string]any)
		registry.Each(func(s *kubernetes.Service) {
			scope := s.ActiveScope()
			head, _ := p.ReadBlob(alertProcessKey(scope, false))
			lease, _ := p.ReadBlob(alertProcessKey(scope, true))
			cp, _ := p.EvidenceCheckpoint(c.UserContext(), storage.EvidenceScope{OrgID: scope.OrgID, ClusterID: scope.ClusterID, ClusterUID: "physical-" + scope.ClusterID})
			var h, l any
			_ = json.Unmarshal(head, &h)
			_ = json.Unmarshal(lease, &l)
			clusters[scope.ClusterID] = map[string]any{"scope": scope, "head": h, "capture_state": l, "capture": s.CaptureStatus(), "checkpoint": cp}
		})
		return c.JSON(out)
	})
	replay := func(c *fiber.Ctx, ambiguous bool) error {
		if !harnessAuth(c) {
			return nil
		}
		var a kubernetes.IssueAlert
		if ambiguous {
			at, _ := time.Parse(time.RFC3339Nano, os.Getenv("HARNESS_ALERT_AMBIGUOUS_AT"))
			a = kubernetes.IssueAlert{Cluster: "alpha", Namespace: "shop", WorkloadKind: "Deployment", Workload: "ambiguous", RootUID: "ambiguous-root", Rule: "pod.failed", RuleLabel: "Failed", Severity: "warning", BrokeAt: at}
		} else {
			raw, e := os.ReadFile(os.Getenv("HARNESS_ALERT_PENDING"))
			if e != nil {
				return c.SendStatus(503)
			}
			if e = json.Unmarshal(raw, &a); e != nil {
				return c.SendStatus(500)
			}
		}
		org := "default"
		if c.Query("org") == "isolated-org" {
			org = "isolated-org"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		out, e := services.CreateIncidentFromKubernetesIssueContext(ctx, org, a)
		if e != nil {
			return c.Status(503).JSON(fiber.Map{"error": "actual bridge failed"})
		}
		return c.JSON(out)
	}
	app.Post("/__harness/replay", func(c *fiber.Ctx) error { return replay(c, false) })
	app.Post("/__harness/ambiguous", func(c *fiber.Ctx) error { return replay(c, true) })
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "url": "http://" + listener.Addr().String()})
	if e = os.WriteFile(os.Getenv("HARNESS_ALERT_READY"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	if e = app.Listener(listener); e != nil {
		t.Fatal(e)
	}
}

type alertProcessReceipt struct {
	At                                         time.Time `json:"at"`
	Cluster, Workload, Rule, Severity, BrokeAt string
	BodySHA                                    string `json:"body_sha256"`
}

func TestTwoOSWritersCaptureHistoryAndDeliverAlertsAcrossTakeover(t *testing.T) {
	if os.Getenv("HARNESS_ALERT_PROCESS_GATE") != "1" {
		t.Skip("explicit recorded process/real-channel gate required")
	}
	data := os.Getenv("HARNESS_DATA_DIR")
	root, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	base, e := storage.NewPostgres(storage.PostgresOptions{DSN: os.Getenv("TEST_POSTGRES_DSN")})
	if e != nil {
		t.Fatal("own PG unavailable")
	}
	defer base.Close()
	db := base.(storage.SQLAccessor).DB()
	var mu sync.Mutex
	receipts := []alertProcessReceipt{}
	ambiguousHeld := make(chan struct{}, 1)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, e := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		if e != nil {
			w.WriteHeader(400)
			return
		}
		var b map[string]any
		if json.Unmarshal(raw, &b) != nil {
			w.WriteHeader(400)
			return
		}
		str := func(k string) string { s, _ := b[k].(string); return s }
		sum := sha256.Sum256(raw)
		rec := alertProcessReceipt{At: time.Now().UTC(), Cluster: str("Cluster"), Workload: str("Workload"), Rule: str("Rule"), Severity: str("KubernetesSeverity"), BrokeAt: str("BrokeAt"), BodySHA: hex.EncodeToString(sum[:])}
		mu.Lock()
		receipts = append(receipts, rec)
		snapshot, _ := json.MarshalIndent(receipts, "", "  ")
		_ = os.WriteFile(filepath.Join(os.Getenv("HARNESS_RESULTS_DIR"), "channel-receipts.json"), snapshot, 0600)
		firstAmbiguous := 0
		for _, x := range receipts {
			if x.Workload == "ambiguous" {
				firstAmbiguous++
			}
		}
		mu.Unlock()
		if rec.Workload == "ambiguous" && firstAmbiguous == 1 {
			select {
			case ambiguousHeld <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return
		}
		w.WriteHeader(200)
		fmt.Fprint(w, `{"accepted":true}`)
	}))
	defer receiver.Close()
	fixtures := map[string]*fakekube.Server{}
	servers := map[string]*httptest.Server{}
	var alphaDenied atomic.Bool
	for _, id := range []string{"alpha", "beta"} {
		f, e := fakekube.NewServer(fakekube.Config{})
		if e != nil {
			t.Fatal(e)
		}
		fixtures[id] = f
		cluster := id
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer synthetic-"+cluster {
				w.WriteHeader(401)
				return
			}
			if cluster == "alpha" && alphaDenied.Load() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(403)
				fmt.Fprint(w, `{"kind":"Status","reason":"Forbidden","code":403}`)
				return
			}
			f.ServeHTTP(w, r)
		}))
		servers[id] = server
		t.Cleanup(server.Close)
	}
	mutate := func(id, operation, resource, name string, object map[string]any) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"operation": operation, "resource": resource, "namespace": map[bool]string{true: "", false: "shop"}[resource == "nodes" || resource == "namespaces"], "name": name, "object": object})
		w := httptest.NewRecorder()
		fixtures[id].ServeHTTP(w, httptest.NewRequest("POST", "/_fake/mutate", bytes.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("fixture mutation %s/%s status%d", id, resource, w.Code)
		}
	}
	objectMeta := func(id, name, uid string) map[string]any {
		return map[string]any{"name": name, "uid": uid, "namespace": "shop"}
	}
	setWorkload := func(id string, broken bool, incarnation string) {
		deploymentUID := id + "-deployment-" + incarnation
		rsUID := id + "-rs-" + incarnation
		podUID := id + "-pod-" + incarnation
		mutate(id, "upsert", "deployments", "api", map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": objectMeta(id, "api", deploymentUID), "spec": map[string]any{"replicas": 1}, "status": map[string]any{"replicas": 1, "readyReplicas": 1, "availableReplicas": 1}})
		rsMeta := objectMeta(id, "api-rs", rsUID)
		rsMeta["ownerReferences"] = []any{map[string]any{"kind": "Deployment", "name": "api", "uid": deploymentUID, "controller": true}}
		mutate(id, "upsert", "replicasets", "api-rs", map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSet", "metadata": rsMeta})
		meta := objectMeta(id, "api-0", podUID)
		meta["ownerReferences"] = []any{map[string]any{"kind": "ReplicaSet", "name": "api-rs", "uid": rsUID, "controller": true}}
		state := map[string]any{"running": map[string]any{"startedAt": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}}
		restart := 0
		if broken {
			state = map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}}
			restart = 2
		}
		mutate(id, "upsert", "pods", "api-0", map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": meta, "spec": map[string]any{"containers": []any{map[string]any{"name": "app"}}}, "status": map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": "app", "ready": !broken, "restartCount": restart, "state": state}}}})
	}
	for _, id := range []string{"alpha", "beta"} {
		mutate(id, "upsert", "namespaces", "kube-system", map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "kube-system", "uid": "physical-" + id}})
		mutate(id, "upsert", "namespaces", "shop", map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "shop", "uid": id + "-shop"}})
		mutate(id, "upsert", "nodes", "node", map[string]any{"apiVersion": "v1", "kind": "Node", "metadata": map[string]any{"name": "node", "uid": id + "-node"}, "status": map[string]any{"allocatable": map[string]any{"cpu": "2", "memory": "4Gi"}, "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}})
		setWorkload(id, false, "one")
	}
	template := filepath.Join(data, "plain-kubernetes-channel.tmpl")
	if e = os.WriteFile(template, []byte(`cluster={{.Cluster}} namespace={{.Namespace}} workload={{.Workload}} rule={{.Rule}}`), 0600); e != nil {
		t.Fatal(e)
	}
	cfgFile := filepath.Join(data, "actual-channel.yaml")
	cfg := fmt.Sprintf("name: process-alerts\nhost: 127.0.0.1\nport: 8080\npublic_host: http://127.0.0.1:18099\ngateway_secret: synthetic-process-gateway\nqueue:\n  enable: false\nagent:\n  enable: false\noncall:\n  enable: false\nalert:\n  slack:\n    enable: false\n  telegram:\n    enable: false\n  email:\n    enable: false\n  lark:\n    enable: false\n  msteams:\n    enable: true\n    template_path: %s\n    kubernetes_issue_template_path: %s\n    power_automate_url: %s\n", filepath.Join(root, "config/msteams_message.tmpl"), template, receiver.URL)
	if e = os.WriteFile(cfgFile, []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	pending := filepath.Join(data, "pending-alert.json")
	claimMarker := filepath.Join(data, "pending-claim.json")
	ambiguousAt := time.Now().UTC().Format(time.RFC3339Nano)
	type writer struct {
		cmd    *exec.Cmd
		url    string
		reaped bool
	}
	writers := []*writer{}
	wait := func(label string, seconds int, probe func() bool) {
		t.Helper()
		deadline := time.Now().Add(time.Duration(seconds) * time.Second)
		for time.Now().Before(deadline) {
			if probe() {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("bounded deadline: %s", label)
	}
	start := func(name string, pause bool) *writer {
		t.Helper()
		ready := filepath.Join(data, name+"-ready.json")
		cmd := exec.Command(executable, "-test.run=^TestDurableAlertWriterChild$", "-test.v", "-test.timeout=16m")
		cmd.Env = append(os.Environ(), "HARNESS_ALERT_CHILD=1", "HARNESS_ALERT_CONFIG="+cfgFile, "HARNESS_ALERT_ALPHA_URL="+servers["alpha"].URL, "HARNESS_ALERT_BETA_URL="+servers["beta"].URL, "HARNESS_ALERT_PENDING="+pending, "HARNESS_ALERT_CLAIM_MARKER="+claimMarker, "HARNESS_ALERT_READY="+ready, "HARNESS_ALERT_AMBIGUOUS_AT="+ambiguousAt, fmt.Sprintf("HARNESS_ALERT_PAUSE_CLAIM=%d", map[bool]int{true: 1, false: 0}[pause]))
		log, e := os.OpenFile(filepath.Join(data, name+".log"), os.O_CREATE|os.O_WRONLY, 0600)
		if e != nil {
			t.Fatal(e)
		}
		cmd.Stdout = log
		cmd.Stderr = log
		if e = cmd.Start(); e != nil {
			t.Fatal(e)
		}
		w := &writer{cmd: cmd}
		writers = append(writers, w)
		t.Cleanup(func() {
			if !w.reaped {
				_ = w.cmd.Process.Kill()
				_ = w.cmd.Wait()
				w.reaped = true
			}
			_ = log.Close()
		})
		wait(name+" real HTTP ready", 45, func() bool {
			b, e := os.ReadFile(ready)
			if e != nil {
				return false
			}
			var v struct {
				PID int    `json:"pid"`
				URL string `json:"url"`
			}
			if json.Unmarshal(b, &v) != nil {
				return false
			}
			if v.PID != cmd.Process.Pid {
				t.Fatal("childPID mismatch")
			}
			w.url = v.URL
			return true
		})
		return w
	}
	request := func(w *writer, method, path string, auth bool) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, w.url+path, nil)
		if auth {
			req.Header.Set("X-Gateway-Secret", "synthetic-process-gateway")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 28*time.Second)
		defer cancel()
		res, e := http.DefaultClient.Do(req.WithContext(ctx))
		if e != nil {
			return 0, map[string]any{"transport_error": true}
		}
		defer res.Body.Close()
		var b map[string]any
		_ = json.NewDecoder(res.Body).Decode(&b)
		return res.StatusCode, b
	}
	state := func(w *writer) map[string]any { _, b := request(w, "GET", "/__harness/state", true); return b }
	clusterState := func(w *writer, id string) map[string]any {
		b := state(w)
		clusters, _ := b["clusters"].(map[string]any)
		v, _ := clusters[id].(map[string]any)
		return v
	}
	first := start("writer-a", true)
	wait("first actual ready capture2clusters", 60, func() bool {
		for _, id := range []string{"alpha", "beta"} {
			code, b := request(first, "GET", "/api/admin/kubernetes/sync?cluster="+id, true)
			if code != 200 || b["state"] != "ready" {
				return false
			}
			cs := clusterState(first, id)
			cp, _ := cs["checkpoint"].(map[string]any)
			if s, _ := cp["id"].(string); !strings.HasPrefix(s, "capture:") {
				return false
			}
		}
		return true
	})
	if code, _ := request(first, "GET", "/api/admin/kubernetes/workloads?cluster=alpha", false); code != 401 {
		t.Fatalf("real product anonymous permission status=%d", code)
	}
	second := start("writer-b", false)
	enable := func(w *writer, id string) {
		req, _ := http.NewRequest("PUT", w.url+"/api/admin/kubernetes/issue-alerts?cluster="+id, strings.NewReader(`{"enabled":true,"level":"warning"}`))
		req.Header.Set("X-Gateway-Secret", "synthetic-process-gateway")
		req.Header.Set("Content-Type", "application/json")
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("actual alert enable status=%d", res.StatusCode)
		}
	}
	// Healthy owned capture must establish absence before a new issue is admitted.
	time.Sleep(12 * time.Second)
	enable(first, "alpha")
	enable(first, "beta")
	setWorkload("alpha", true, "one")
	wait("actual alert notification claim paused BEFORE HTTP egress", 75, func() bool { _, e := os.Stat(claimMarker); return e == nil })
	mu.Lock()
	beforeDelivery := len(receipts)
	mu.Unlock()
	if beforeDelivery != 0 {
		t.Fatal("provider barrier did not precede actual egress")
	}
	var claimed struct {
		PID          int       `json:"pid"`
		IncidentID   string    `json:"incident_id"`
		ClaimExpires time.Time `json:"claim_expires"`
	}
	raw, e := os.ReadFile(claimMarker)
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &claimed); e != nil {
		t.Fatal(e)
	}
	if claimed.PID != first.cmd.Process.Pid {
		t.Fatal("only owned writer may be killed")
	}
	oldState := clusterState(first, "alpha")
	oldHead, _ := oldState["head"].(map[string]any)
	oldOwner, _ := oldHead["Owner"].(string)
	oldRevision, _ := oldHead["Revision"].(string)
	if e = first.cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = first.cmd.Wait()
	first.reaped = true
	wait("surviving OS writer naturally takes inventory lease and republishes", 240, func() bool {
		h, _ := clusterState(second, "alpha")["head"].(map[string]any)
		v, _ := h["Revision"].(string)
		owner, _ := h["Owner"].(string)
		dataOwner, _ := h["DataOwner"].(string)
		return v != "" && v != oldRevision && owner != "" && owner != oldOwner && dataOwner == owner
	})
	code, out := request(second, "POST", "/__harness/replay", true)
	if code != 200 {
		t.Fatalf("actual production pending replay failed:%d %v", code, out)
	}
	wait("one real HTTP channel delivery after takeover", 20, func() bool { mu.Lock(); defer mu.Unlock(); return len(receipts) == 1 })
	replacement := start("writer-a-replacement", false)
	for _, w := range []*writer{second, replacement} {
		for i := 0; i < 3; i++ {
			code, _ := request(w, "POST", "/__harness/replay", true)
			if code != 200 {
				t.Fatal("repeat emission failed")
			}
		}
	}
	mu.Lock()
	normalCount := len(receipts)
	mu.Unlock()
	if normalCount != 1 {
		t.Fatalf("duplicate channel deliveries for stable episode=%d", normalCount)
	}
	wait("currentcaptured issue and history nonzero", 30, func() bool {
		code, b := request(second, "GET", "/api/admin/kubernetes/issues?cluster=alpha&broken=true", true)
		items, _ := b["items"].([]any)
		return code == 200 && len(items) > 0
	})
	// The second cluster must remain independently healthy, not inherit alpha data.
	if code, b := request(second, "GET", "/api/admin/kubernetes/issues?cluster=beta&broken=true", true); code != 200 {
		t.Fatalf("healthy beta HTTP%d", code)
	} else if items, _ := b["items"].([]any); len(items) != 0 {
		t.Fatal("two-cluster issue isolation failed")
	}
	foreign, err := base.(storage.EvidenceStore).QueryEvidence(t.Context(), storage.EvidenceScope{OrgID: "isolated-org", ClusterID: "alpha", ClusterUID: "physical-alpha"}, storage.EvidenceQuery{Since: time.Now().Add(-time.Hour), Until: time.Now()})
	if err != nil || len(foreign.Items) != 0 {
		t.Fatal("foreign org read captured history")
	}
	code, other := request(second, "POST", "/__harness/replay?org=isolated-org", true)
	if code != 200 || other["IncidentID"] == out["IncidentID"] {
		t.Fatal("real scoped episode org isolation failed")
	}
	var defaultOrg, otherOrg string
	if e = db.QueryRow("SELECT org_id FROM vs_incidents WHERE id=$1", out["IncidentID"]).Scan(&defaultOrg); e != nil {
		t.Fatal(e)
	}
	if e = db.QueryRow("SELECT org_id FROM vs_incidents WHERE id=$1", other["IncidentID"]).Scan(&otherOrg); e != nil {
		t.Fatal(e)
	}
	if defaultOrg != "default" || otherOrg != "isolated-org" {
		t.Fatal("durable incident org attribution failed")
	}
	setWorkload("alpha", false, "one")
	wait("complete successful recovery resolves actual captured occurrence", 60, func() bool {
		page, e := base.(storage.EvidenceStore).QueryEvidence(t.Context(), storage.EvidenceScope{OrgID: "default", ClusterID: "alpha", ClusterUID: "physical-alpha"}, storage.EvidenceQuery{Since: time.Now().Add(-time.Hour), Until: time.Now(), Type: "issue_resolved"})
		return e == nil && len(page.Items) > 0
	})
	for _, entry := range []struct{ resource, name string }{{"pods", "api-0"}, {"replicasets", "api-rs"}, {"deployments", "api"}} {
		mutate("alpha", "delete", entry.resource, entry.name, nil)
	}
	wait("complete deletion removes current workload but keeps history", 60, func() bool {
		code, b := request(second, "GET", "/api/admin/kubernetes/workloads?cluster=alpha", true)
		items, _ := b["items"].([]any)
		return code == 200 && len(items) == 0
	})
	if code, _ = request(second, "POST", "/api/admin/incidents/"+out["IncidentID"].(string)+"/resolve", true); code != 200 {
		t.Fatal("existing operator incident resolve failed")
	}
	mu.Lock()
	countBeforeRecurrence := len(receipts)
	mu.Unlock()
	setWorkload("alpha", true, "two")
	wait("new recurrence creates exactly one new delivery", 75, func() bool { mu.Lock(); defer mu.Unlock(); return len(receipts) > countBeforeRecurrence })
	time.Sleep(5 * time.Second)
	mu.Lock()
	countAfterRecurrence := len(receipts)
	mu.Unlock()
	if countAfterRecurrence != countBeforeRecurrence+1 {
		t.Fatalf("recurrence duplicate notifications:%d ->%d", countBeforeRecurrence, countAfterRecurrence)
	}
	scope := storage.EvidenceScope{OrgID: "default", ClusterID: "alpha", ClusterUID: "physical-alpha"}
	wait("current journal records two distinct crash-loop occurrences across recovery", 30, func() bool {
		page, e := base.(storage.EvidenceStore).QueryEvidence(t.Context(), scope, storage.EvidenceQuery{Since: time.Now().Add(-time.Hour), Until: time.Now(), Type: "issue_opened", Rule: "pod.crash_loop", Limit: 100})
		return e == nil && page.Total == 2
	})
	alphaDenied.Store(true)
	wait("actual current alpha credential permission loss fails closed", 60, func() bool {
		code, _ := request(second, "GET", "/api/admin/kubernetes/workloads?cluster=alpha", true)
		return code == 403
	})
	if code, b := request(second, "GET", "/api/admin/kubernetes/workloads?cluster=beta", true); code != 200 {
		t.Fatalf("healthy beta while alpha denied HTTP%d", code)
	} else if items, _ := b["items"].([]any); len(items) == 0 {
		t.Fatal("alpha permission failure stalled healthy beta")
	}
	if code, _ := request(second, "GET", "/api/admin/kubernetes/issues?cluster=alpha&since="+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), true); code != 403 {
		t.Fatal("current denied alpha replayed history")
	}
	alphaDenied.Store(false)
	wait("same-credential alpha recovers without restarting surviving writer", 60, func() bool {
		code, b := request(second, "GET", "/api/admin/kubernetes/workloads?cluster=alpha", true)
		items, _ := b["items"].([]any)
		return code == 200 && len(items) > 0
	})
	// Existing delivery semantics must also be observed at the unavoidable
	// accepted-by-receiver / unacknowledged-by-sender crash boundary.
	ambiguousDone := make(chan struct{})
	go func() { defer close(ambiguousDone); _, _ = request(replacement, "POST", "/__harness/ambiguous", true) }()
	select {
	case <-ambiguousHeld:
	case <-time.After(20 * time.Second):
		t.Fatal("real HTTP receiver never admitted ambiguous-ACK barrier")
	}
	if e = replacement.cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = replacement.cmd.Wait()
	replacement.reaped = true
	<-ambiguousDone
	time.Sleep(storage.DefaultDetectionNotificationLease + 150*time.Millisecond)
	if code, _ = request(second, "POST", "/__harness/ambiguous", true); code != 200 {
		t.Fatal("production replay after ambiguous sender death failed")
	}
	mu.Lock()
	ambiguousCount := 0
	for _, r := range receipts {
		if r.Workload == "ambiguous" {
			ambiguousCount++
		}
	}
	mu.Unlock()
	result := map[string]any{"actual_writer_processes": true, "capture_pending_claim_takeover": true, "normal_episode_deliveries": normalCount, "recurrence_new_deliveries": countAfterRecurrence - countBeforeRecurrence, "org_isolation": true, "two_cluster_isolation": true, "ambiguous_ack_deliveries": ambiguousCount, "owned_killed_pids": []int{first.cmd.Process.Pid, replacement.cmd.Process.Pid}, "storage_retained": true}
	encoded, _ := json.MarshalIndent(result, "", "  ")
	_ = os.WriteFile(filepath.Join(os.Getenv("HARNESS_RESULTS_DIR"), "alert-process-outcome.json"), encoded, 0600)
	t.Log(string(encoded))
	if ambiguousCount != 1 {
		t.Fatalf("MANDATORY no-duplicate channel guarantee absent at accepted-but-unacknowledged crash: actual deliveries=%d (existing claim/HTTP semantics, no exactly-once transport assumed)", ambiguousCount)
	}
}
