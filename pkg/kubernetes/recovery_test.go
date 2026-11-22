package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/aws-sdk-go-v2/service/sts/types"
)

func TestRegistryRenewsTwentyEpochsWithoutBrowserOrResolveRequests(t *testing.T) {
	if testing.Short() {
		t.Skip("endurance: observe twenty autonomous credential-epoch replacements")
	}
	base := storage.NewMemory()
	fleet := newBootFleet(t, "org", base, "autonomous")
	fixture := newFakeCluster(t, "initially-unaccepted", "api")
	var clock atomic.Int64
	clock.Store(time.Now().UTC().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	role := &renewableRoleClock{now: now}
	credential := newEKSSource("synthetic-cluster", "us-east-1", assumeRoleCredentials(role, "synthetic-role"), now).(*expiringBearerSource)
	mint := credential.refresh
	credential.refresh = func(ctx context.Context) (string, time.Time, error) {
		token, expires, err := mint(ctx)
		if err == nil {
			fixture.acceptToken(token)
		}
		return token, expires, err
	}
	cluster := fleet.add("rotating", fixture, credential, 0)
	fleet.startWithin(time.Second)
	current := func() *Service {
		fleet.registry.mu.Lock()
		defer fleet.registry.mu.Unlock()
		return fleet.registry.entries[0].Service
	}
	waitUntil(t, 10*time.Second, "initial autonomous publication", func() bool {
		s := current()
		generation, ok := publishedGeneration(fleet.view, s.ActiveScope())
		return ok && len(podNames(generation)) > 0
	})
	waitUntil(t, 5*time.Second, "first complete publication", func() bool { return !current().recordedFirstReadyAt().IsZero() })
	first := current()
	for epoch := 1; epoch <= 20; epoch++ {
		previous := current()
		credential.mu.Lock()
		next := credential.expires.Add(-credential.skew)
		credential.mu.Unlock()
		clock.Store(next.UnixNano())
		waitUntil(t, 10*time.Second, "autonomous successor publication", func() bool {
			service := current()
			if service.identity == previous.identity {
				return false
			}
			generation, ok := publishedGeneration(fleet.view, service.ActiveScope())
			return ok && len(podNames(generation)) == 1
		})
		if previous.identity.ctx.Err() == nil {
			t.Fatal("retired epoch stayed active")
		}
		fleet.registry.mu.Lock()
		instances := len(fleet.registry.services)
		fleet.registry.mu.Unlock()
		if instances != 1 {
			t.Fatalf("unbounded successor instances: %d", instances)
		}
	}
	if role.calls.Load() < 7 || role.calls.Load() > 9 {
		t.Fatalf("role session renewals=%d; bearer refresh alone is insufficient", role.calls.Load())
	}
	if _, err := first.readInventory(t.Context()); err == nil {
		t.Fatal("retired credential read restricted inventory")
	}
	fleet.registry.DeregisterCluster(cluster.id)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := fleet.registry.CloseContext(ctx); err != nil {
		t.Fatalf("recovery did not join on removal: %v", err)
	}
}

func TestRecoveryGateNeverSwallowsAnAnsweredDenial(t *testing.T) {
	recovery := &clusterRecovery{}
	recovery.failed(ErrCredentialUnavailable, nil, "org")
	if !recovery.requestGate("org") {
		t.Fatal("a provider failure did not gate request traffic")
	}
	// The same cooldown deduplicates the failure streak. An answered denial that
	// arrives inside it must still be recorded, because a reader has to see the
	// refusal rather than an unavailable answer.
	recovery.failed(unverifiedInventoryAuthority(ErrForbidden), nil, "org")
	if recovery.requestGate("org") {
		t.Fatal("an active cooldown swallowed an answered denial")
	}
	if recovery.failures != 1 {
		t.Fatalf("the deduplicated streak changed: %d", recovery.failures)
	}
	// A denial is a positive answer: a later outage does not make the identity
	// able to read again.
	recovery.failed(ErrCredentialUnavailable, nil, "org")
	if recovery.requestGate("org") {
		t.Fatal("a later outage withdrew a recorded denial")
	}
	recovery.committed("revision")
	if recovery.requestGate("org") {
		t.Fatal("a committed publication left the request gate closed")
	}
	// The provider is shared per cluster, but the authority is per read scope: one
	// scope's cooldown never answers for another, and one scope's denial never
	// withdraws another scope's protection against probing the provider.
	shared := &clusterRecovery{}
	shared.failed(ErrCredentialUnavailable, nil, "org-b")
	if shared.requestGate("org-a") {
		t.Fatal("one scope's cooldown gated another scope")
	}
	shared.failed(ErrForbidden, nil, "org-a")
	if !shared.requestGate("org-b") {
		t.Fatal("one scope's denial withdrew another scope's gate")
	}
	if shared.requestGate("org-a") {
		t.Fatal("a scope's own denial was replaced by unavailability")
	}
	// A rejected bearer is answered for the credential every scope shares.
	credential := &clusterRecovery{}
	credential.failed(ErrCredentialUnavailable, nil, "org-b")
	credential.failed(ErrUnauthorized, nil, "")
	if credential.requestGate("org-b") || credential.requestGate("org-c") {
		t.Fatal("a credential-wide denial left a scope gated")
	}
	for _, err := range []error{ErrForbidden, ErrUnauthorized, unverifiedInventoryAuthority(ErrForbidden), &evidenceReadError{stage: evidenceStoreQuery, cause: ErrForbidden}, &permissionDeniedError{}} {
		if !answerDenied(err) {
			t.Fatalf("%T was not classified as an answered denial", err)
		}
	}
	for _, err := range []error{nil, ErrCredentialUnavailable, ErrInventoryNotReady, unverifiedInventoryAuthority(ErrCredentialUnavailable), &evidenceReadError{stage: evidenceStoreQuery, cause: context.DeadlineExceeded}} {
		if answerDenied(err) {
			t.Fatalf("%T was classified as an answered denial", err)
		}
	}
}

func TestRegistryProviderOutageHasStableCooldownAndRecoversWithoutRequests(t *testing.T) {
	fixture := newFakeCluster(t, "synthetic-a", "api")
	var fail atomic.Bool
	var calls atomic.Int32
	source := &credentialSource{authorization: func(context.Context) (string, error) {
		calls.Add(1)
		if fail.Load() {
			return "", ErrCredentialUnavailable
		}
		return "Bearer synthetic-a", nil
	}}
	client, err := NewClient(fixture.trustedConfig(source))
	if err != nil {
		t.Fatal(err)
	}
	client.indexLimiter = nil
	service := NewService(client, Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "configured"}, 0)
	service.SetChangeStorage(storage.NewMemory())
	registry := NewServiceRegistry(service)
	registry.StartCapture(t.Context())
	defer registry.Close()
	// Every wait below polls for the state it names and returns as soon as
	// it holds; the budgets only bound a hung run on a loaded machine.
	waitUntil(t, 30*time.Second, "initial publication", func() bool {
		service.changes.mu.Lock()
		cache := service.changes.inventories[service.indexScope()]
		service.changes.mu.Unlock()
		if cache == nil {
			return false
		}
		cache.mu.RLock()
		defer cache.mu.RUnlock()
		return cache.generation.Revision != ""
	})
	fail.Store(true)
	// A recorded failure sets a retry time, and only a publication clears
	// it. Waiting for the cooldown to be in the future instead could miss a
	// one-second first cooldown on a loaded machine.
	waitUntil(t, 30*time.Second, "outage cooldown", func() bool { return !service.recovery.deadline().IsZero() })
	if service.identity.ctx.Err() != nil {
		t.Fatal("transient provider outage retired accepted credential material")
	}
	waitUntil(t, 30*time.Second, "failed collection recorded", func() bool {
		service.changes.mu.Lock()
		cache := service.changes.inventories[service.indexScope()]
		service.changes.mu.Unlock()
		if cache == nil {
			return false
		}
		cache.mu.RLock()
		defer cache.mu.RUnlock()
		return cache.failed
	})
	// Request traffic inside one cooldown must neither call the credential
	// source nor move the retry time. The supervisor itself retries when a
	// cooldown ends, so a measurement that outlives its cooldown proves
	// nothing and is taken again inside the next, longer one.
	var before int32
	waitUntil(t, 60*time.Second, "request traffic measured inside one cooldown", func() bool {
		deadline := service.recovery.deadline()
		before = calls.Load()
		if !time.Now().Before(deadline) {
			return false
		}
		for i := 0; i < 100; i++ {
			if _, err := registry.ResolveCluster("org", "cluster"); err == nil {
				t.Fatal("outage served a successful warming response")
			}
		}
		if !time.Now().Before(deadline) {
			return false
		}
		if calls.Load() != before || !service.recovery.deadline().Equal(deadline) {
			t.Fatal("request traffic bypassed cooldown")
		}
		return true
	})
	fail.Store(false)
	waitUntil(t, 60*time.Second, "autonomous provider recovery", func() bool {
		registry.mu.Lock()
		fresh := registry.entries[0].Service
		registry.mu.Unlock()
		// Recovery keeps the accepted credential epoch; nothing restarts or rebuilds.
		if fresh.identity != service.identity || fresh.identity.ctx.Err() != nil {
			return false
		}
		fresh.changes.mu.Lock()
		cache := fresh.changes.inventories[fresh.indexScope()]
		fresh.changes.mu.Unlock()
		if cache == nil {
			return false
		}
		cache.mu.RLock()
		defer cache.mu.RUnlock()
		return !cache.failed && cache.generation.Revision != ""
	})
	if calls.Load() <= before {
		t.Fatal("recovery never retried the credential source")
	}
}

func TestNativeBearerRefreshThenRejectedLogTrafficNeverPublishesFalseZero(t *testing.T) {
	fixture := newFakeCluster(t, "initially-unaccepted")
	fixture.serve("pods", map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "api", "uid": "pod-uid", "namespace": "default"}, "spec": map[string]any{"containers": []any{map[string]any{"name": "app"}}}, "status": map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": "app", "restartCount": 2, "state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}}}}}})
	var clock atomic.Int64
	clock.Store(time.Now().UTC().UnixNano())
	var fail, acceptMint atomic.Bool
	acceptMint.Store(true)
	var signingCalls atomic.Int32
	provider := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		signingCalls.Add(1)
		if fail.Load() {
			return aws.Credentials{}, ErrCredentialUnavailable
		}
		return aws.Credentials{AccessKeyID: "synthetic-key", SecretAccessKey: "synthetic-secret"}, nil
	})
	source := newEKSSource("synthetic-cluster", "us-east-1", provider, func() time.Time { return time.Unix(0, clock.Load()) }).(*expiringBearerSource)
	mint := source.refresh
	source.refresh = func(ctx context.Context) (string, time.Time, error) {
		token, expires, err := mint(ctx)
		if err == nil && acceptMint.Load() {
			fixture.acceptToken(token)
		}
		return token, expires, err
	}
	fleet := newBootFleet(t, "org", storage.NewMemory(), "sequence")
	cluster := fleet.add("cluster", fixture, source, 0)
	fleet.startWithin(time.Second)
	current := func() *Service {
		fleet.registry.mu.Lock()
		defer fleet.registry.mu.Unlock()
		return fleet.registry.entries[0].Service
	}
	assertPublished := func(service *Service) bool {
		overview, err := service.Overview(t.Context())
		if err != nil || overview.Pods != 1 || overview.Nodes != 1 || overview.AllocatableCPU == "" || overview.AllocatableMemory == "" {
			return false
		}
		issues, err := service.Issues(t.Context(), IssueOptions{Broken: true})
		return err == nil && issues.Broken.Total == 1 && len(issues.Items) > 0
	}
	waitUntil(t, 10*time.Second, "nonzero broken workload and capacity publication", func() bool { return assertPublished(current()) })
	waitUntil(t, 5*time.Second, "first complete publication before renewal", func() bool { return !current().recordedFirstReadyAt().IsZero() })
	before := current()
	firstReady := before.recordedFirstReadyAt()
	clock.Add(int64(13 * time.Minute))
	waitUntil(t, 10*time.Second, "native renewal recollection without browser requests", func() bool {
		fresh := current()
		if fresh.identity == before.identity {
			return false
		}
		generation, ok := publishedGeneration(fleet.view, fresh.ActiveScope())
		return ok && len(podNames(generation)) == 1
	})
	renewed := current()
	waitUntil(t, 5*time.Second, "renewed snapshot bodies", func() bool { return assertPublished(renewed) })
	if _, err := before.Overview(t.Context()); err == nil {
		t.Fatal("retired credential returned protected inventory")
	}
	acceptMint.Store(false)
	fail.Store(true)
	fixture.mu.Lock()
	clear(fixture.extra)
	fixture.token = "reject-all"
	fixture.mu.Unlock()
	logCtx, cancelLog := renewed.credentialContext(t.Context())
	if body, err := renewed.client.openPodLogStream(logCtx, "/api/v1/namespaces/default/pods/api/log?follow=true", false); err == nil {
		body.Close()
		t.Fatal("rejected log stream succeeded")
	}
	cancelLog()
	if _, err := renewed.Overview(t.Context()); err == nil {
		t.Fatal("credential outage returned a successful zero-filled snapshot")
	}
	waitUntil(t, 8*time.Second, "provider outage after rejected bearer", func() bool { return signingCalls.Load() >= 3 })
	count := signingCalls.Load()
	for i := 0; i < 100; i++ {
		_, _ = fleet.registry.ResolveCluster("org", cluster.id)
	}
	if signingCalls.Load() != count {
		t.Fatal("request storm bypassed source/recovery cooldown")
	}
	fail.Store(false)
	acceptMint.Store(true)
	clock.Add(int64(2 * time.Second))
	waitUntil(t, 12*time.Second, "autonomous post-outage publication", func() bool {
		fresh := current()
		if fresh.identity == renewed.identity {
			return false
		}
		generation, ok := publishedGeneration(fleet.view, fresh.ActiveScope())
		return ok && len(podNames(generation)) == 1
	})
	recovered := current()
	waitUntil(t, 5*time.Second, "recovered nonzero snapshot bodies", func() bool { return assertPublished(recovered) })
	if !firstReady.IsZero() && !recovered.recordedFirstReadyAt().Equal(firstReady) {
		t.Fatal("recovery reopened first publication")
	}
	one, err := recovered.Overview(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	two, err := recovered.Overview(t.Context())
	if err != nil || !one.ObservedAt.Equal(two.ObservedAt) {
		t.Fatal("GET falsely refreshed the observation timestamp")
	}
	if signingCalls.Load() > 6 {
		t.Fatalf("unbounded bearer/provider attempts: %d", signingCalls.Load())
	}
}

func TestRegistryRestartsExitedProducerWithoutRetiringAuthorizedSnapshot(t *testing.T) {
	t.Parallel()
	fleet := newBootFleet(t, "org", storage.NewMemory(), "worker-restart")
	fixture := newFakeCluster(t, "synthetic", "api")
	fleet.add("cluster", fixture, staticCredential(t, "synthetic"), 0)
	fleet.startWithin(time.Second)
	fleet.registry.mu.Lock()
	service := fleet.registry.entries[0].Service
	fleet.registry.mu.Unlock()
	waitUntil(t, 8*time.Second, "initial committed inventory", func() bool {
		generation, ok := publishedGeneration(fleet.view, service.ActiveScope())
		return ok && len(podNames(generation)) == 1
	})
	originalScope := service.ActiveScope()
	originalIdentity := service.identity
	service.changes.mu.Lock()
	cache := service.changes.inventories[service.indexScope()]
	done, cancel := cache.done, cache.cancel
	service.changes.mu.Unlock()
	cancel()
	<-done
	waitUntil(t, 5*time.Second, "same-scope autonomous producer restart", func() bool {
		overview, err := service.Overview(t.Context())
		if err != nil || overview.Pods != 1 || overview.Nodes != 1 {
			t.Fatal("worker exit wiped an authorized committed snapshot")
		}
		service.changes.mu.Lock()
		current := service.changes.inventories[service.indexScope()]
		restarted := current.done != done
		service.changes.mu.Unlock()
		return restarted
	})
	fleet.registry.mu.Lock()
	current := fleet.registry.entries[0].Service
	fleet.registry.mu.Unlock()
	if current.identity != originalIdentity || current.ActiveScope() != originalScope || originalIdentity.ctx.Err() != nil {
		t.Fatal("producer exit was misclassified as credential retirement")
	}
}

type renewableRoleClock struct {
	now      func() time.Time
	calls    atomic.Int32
	lifetime time.Duration
}

func (role *renewableRoleClock) AssumeRole(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	count := role.calls.Add(1)
	lifetime := role.lifetime
	if lifetime == 0 {
		lifetime = 30 * time.Minute
	}
	return &sts.AssumeRoleOutput{Credentials: &types.Credentials{AccessKeyId: aws.String(fmt.Sprintf("synthetic-key-%d", count)), SecretAccessKey: aws.String("synthetic-secret"), SessionToken: aws.String(fmt.Sprintf("synthetic-session-%d", count)), Expiration: aws.Time(role.now().Add(lifetime))}}, nil
}

func TestNativeSigningSessionExpiryFencesPaginationAndPublication(t *testing.T) {
	for _, lifetime := range []time.Duration{3 * time.Minute, 30 * time.Minute} {
		for _, stage := range []string{"pagination", "publication"} {
			t.Run(lifetime.String()+"/"+stage, func(t *testing.T) {
				var clock atomic.Int64
				origin := time.Now().UTC()
				clock.Store(origin.UnixNano())
				now := func() time.Time { return time.Unix(0, clock.Load()) }
				role := &renewableRoleClock{now: now, lifetime: lifetime}
				source := newEKSSource("synthetic-cluster", "us-east-1", assumeRoleCredentials(role, "synthetic-role"), now)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/api/v1/pods" {
						http.NotFound(w, r)
						return
					}
					clock.Store(origin.Add(lifetime + time.Minute).UnixNano())
					writeJSON(w, map[string]any{"metadata": map[string]any{"continue": "next"}, "items": []any{map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"uid": "candidate", "name": "candidate", "namespace": "default"}}}})
				}))
				defer server.Close()
				client, err := NewClient(Config{Endpoint: server.URL, Credentials: source, AllowLoopbackHTTP: true})
				if err != nil {
					t.Fatal(err)
				}
				service := NewService(client, Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "configured"}, 0)
				seedInventoryDiscovery(service)
				if stage == "pagination" {
					ctx := context.WithValue(t.Context(), credentialEpochContextKey{}, service)
					records, complete, err := service.loadIndexKind(ctx, "Pod")
					if err == nil || complete || len(records) != 0 {
						t.Fatal("expired signing session admitted a half-listed candidate")
					}
				} else {
					clock.Store(origin.Add(lifetime + time.Minute).UnixNano())
				}
				committed := false
				if err := service.commitCredential(t.Context(), func() error { committed = true; return nil }); err == nil || committed {
					t.Fatal("retired signing-session work committed after renewal")
				}
				if role.calls.Load() != 2 {
					t.Fatalf("AWS role sessions=%d; bearer-only renewal is insufficient", role.calls.Load())
				}
			})
		}
	}
}

// A healthy capture cycle writes no stage lines: a stage that keeps
// succeeding is quiet. A failure is logged when it starts or changes, a
// repeat of the same failure is held back, and the success that ends a
// failure is logged once. A restarted worker is always logged.
func TestCaptureStageLogsOnlyFailuresAndRecoveries(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	recovery := &clusterRecovery{logf: func(format string, args ...any) { fmt.Fprintf(&output, format+"\n", args...) }}
	lines := func() []string {
		var logged []string
		for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
			if strings.Contains(line, "capture stage=") {
				logged = append(logged, line[strings.Index(line, "stage="):strings.Index(line, " duration_ms")])
			}
		}
		output.Reset()
		return logged
	}
	expect := func(want ...string) {
		t.Helper()
		if got := lines(); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("logged %q, want %q", got, want)
		}
	}
	for range 3 {
		for _, stage := range []string{"discovery", "indexed", "additional", "namespace", "usage", "payload", "cas", "journal"} {
			recovery.stage(stage, time.Now(), nil)
		}
	}
	expect()
	recovery.stage("additional", time.Now(), errors.New("list failed"))
	expect("stage=additional reason=collection_unavailable")
	recovery.stage("additional", time.Now(), errors.New("list failed"))
	expect()
	recovery.stage("additional", time.Now(), context.Canceled)
	expect("stage=additional reason=canceled")
	recovery.stage("additional", time.Now(), nil)
	expect("stage=additional reason=ready")
	recovery.stage("additional", time.Now(), nil)
	expect()
	recovery.stage("usage", time.Now(), context.DeadlineExceeded)
	expect("stage=usage reason=timeout")
	recovery.recordStage("worker_recovery", time.Now(), nil, true)
	expect("stage=worker_recovery reason=ready")
}

// Optional kinds that keep failing with different classes give the optional
// stage the same reason on every cycle, whichever read finishes first, so the
// failure is logged once and not again on each cycle.
func TestOptionalStageFailureReasonIsStableAcrossCycles(t *testing.T) {
	t.Parallel()
	logged := 0
	recovery := &clusterRecovery{logf: func(string, ...any) { logged++ }}
	ids := []string{"example.com~v1~widgets", "example.com~v1~gadgets", "example.com~v1~gizmos"}
	errs := []error{context.DeadlineExceeded, errors.New("list failed"), ErrForbidden}
	for cycle := range 12 {
		// Each cycle the reads finish in a different order.
		failed := map[string]error{}
		for offset := range ids {
			index := (cycle + offset) % len(ids)
			failed[ids[index]] = errs[index]
		}
		err := optionalStageError(failed)
		if recoveryReason(err, "additional") != "collection_unavailable" {
			t.Fatalf("cycle %d chose %v, want the failure of the lowest kind ID", cycle, err)
		}
		recovery.stage("additional", time.Now(), err)
	}
	if logged != 1 {
		t.Fatalf("a steady pair of failing kinds logged %d stage lines over 12 cycles, want 1", logged)
	}
	if err := optionalStageError(map[string]error{ids[2]: ErrForbidden}); err != nil {
		t.Fatalf("a denied optional kind failed the stage: %v", err)
	}
}

// bootTokenEndpoint is an in-process cloud token endpoint. The first
// transportFailures requests lose their connection before any answer, as a
// token host does while the network is not attached yet; reject answers every
// request with a definite 400 invalid_client instead.
type bootTokenEndpoint struct {
	transportFailures int32
	reject            bool
	requests          atomic.Int32
	server            *httptest.Server
}

func newBootTokenEndpoint(t *testing.T, transportFailures int32, reject bool) *bootTokenEndpoint {
	t.Helper()
	endpoint := &bootTokenEndpoint{transportFailures: transportFailures, reject: reject}
	endpoint.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		count := endpoint.requests.Add(1)
		if endpoint.reject {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		if count <= endpoint.transportFailures {
			hijacker, ok := writer.(http.Hijacker)
			if !ok {
				t.Error("token endpoint cannot drop its connection")
				return
			}
			connection, _, err := hijacker.Hijack()
			if err == nil {
				_ = connection.Close()
			}
			return
		}
		writeJSON(writer, map[string]any{"access_token": "synthetic-a", "expires_in": 3600})
	}))
	t.Cleanup(endpoint.server.Close)
	return endpoint
}

func (endpoint *bootTokenEndpoint) source() *expiringBearerSource {
	client := endpoint.server.Client()
	return &expiringBearerSource{now: time.Now, skew: 2 * time.Minute, refresh: func(ctx context.Context) (string, time.Time, error) {
		return requestAzureToken(ctx, client, endpoint.server.URL+"/tenant/oauth2/v2.0/token", nil)
	}}
}

// testClusterID names a test's cluster after the test, so a warning from a
// goroutine that outlives an earlier test never counts as this test's.
func testClusterID(t *testing.T) string {
	return strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
}

// credentialWarnings returns the credential warnings that name cluster.
func credentialWarnings(logs *lockedLog, cluster string) []string {
	var warnings []string
	for _, line := range visible(logs.all()) {
		if strings.Contains(line, "credential check failed") && strings.Contains(line, fmt.Sprintf("cluster %q", cluster)) {
			warnings = append(warnings, line)
		}
	}
	return warnings
}

// startBootCluster starts capture for one cluster whose credential comes from
// endpoint, the way a deployment starts it at boot. The cluster is named
// after the test.
func startBootCluster(t *testing.T, endpoint *bootTokenEndpoint) (*ServiceRegistry, *Service) {
	t.Helper()
	fixture := newFakeCluster(t, "synthetic-a", "api")
	client, err := NewClient(fixture.trustedConfig(endpoint.source()))
	if err != nil {
		t.Fatal(err)
	}
	client.indexLimiter = nil
	service := NewService(client, Scope{OrgID: "org", ClusterID: testClusterID(t), CredentialID: "configured"}, 0)
	service.SetChangeStorage(storage.NewMemory())
	registry := NewServiceRegistry(service)
	registry.StartCapture(t.Context())
	t.Cleanup(registry.Close)
	return registry, service
}

func bootClusterPublished(registry *ServiceRegistry) bool {
	registry.mu.Lock()
	current := registry.entries[0].Service
	registry.mu.Unlock()
	current.changes.mu.Lock()
	cache := current.changes.inventories[current.indexScope()]
	current.changes.mu.Unlock()
	if cache == nil {
		return false
	}
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return cache.generation.Revision != ""
}

// A token host that cannot be reached for the first seconds after start (DNS
// or egress not ready yet) is transient: recovery keeps retrying with its
// backoff and the cluster connects once the host answers, with one warning
// that names the token endpoint while it fails.
func TestCloudTokenTransportFailureAtBootIsRetriedUntilTheClusterConnects(t *testing.T) {
	logs := captureLogLines(t, "info")
	endpoint := newBootTokenEndpoint(t, 2, false)
	registry, _ := startBootCluster(t, endpoint)
	waitUntil(t, 30*time.Second, "publication after the token endpoint answers", func() bool { return bootClusterPublished(registry) })
	if got := endpoint.requests.Load(); got <= 2 {
		t.Fatalf("token requests = %d, want a retry after the transport failures", got)
	}
	warnings := credentialWarnings(logs, testClusterID(t))
	if len(warnings) != 1 || !strings.Contains(warnings[0], "reason=token_endpoint_unavailable") || !strings.Contains(warnings[0], "cloud token endpoint") {
		t.Fatalf("credential warnings while the token host was unreachable = %q, want one naming the token endpoint", warnings)
	}
}

// A definite refusal from the token host (400 invalid_client) is not treated
// as transient: it is reported as a credential to fix, not as a retrying host.
func TestCloudTokenRejectionIsReportedAsACredentialToFix(t *testing.T) {
	logs := captureLogLines(t, "info")
	endpoint := newBootTokenEndpoint(t, 0, true)
	source := endpoint.source()
	_, err := source.Authorization(t.Context())
	if err == nil || errors.Is(err, ErrTokenEndpointUnavailable) {
		t.Fatalf("400 invalid_client = %v, want a definite credential failure", err)
	}
	if detail := DiagnoseError(err); detail.Code != "credential_unavailable" || detail.Retryable {
		t.Fatalf("400 invalid_client described as %+v, want non-retryable credential_unavailable", detail)
	}
	if reason := recoveryReason(err, "credential_recovery"); reason != "credential_unavailable" {
		t.Fatalf("400 invalid_client recovery reason = %q", reason)
	}
	recovery := &clusterRecovery{cluster: "cluster"}
	recovery.failed(err, nil, "org")
	recovery.failed(err, nil, "org")
	var warnings []string
	for _, line := range visible(logs.all()) {
		if strings.Contains(line, "credential check failed") {
			warnings = append(warnings, line)
		}
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "reason=credential_unavailable") {
		t.Fatalf("credential warnings for a rejected token request = %q, want one", warnings)
	}
}

// Only a request with no definite answer is transient: no connection, a
// timeout, 429 or 5xx. Every other answer is for the caller to read.
func TestTokenEndpointUnavailableClassifiesOnlyMissingAnswers(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		if err := tokenEndpointUnavailable(nil, &http.Response{StatusCode: status}, nil, "token"); !errors.Is(err, ErrTokenEndpointUnavailable) {
			t.Fatalf("HTTP %d = %v, want transient", status, err)
		}
	}
	if err := tokenEndpointUnavailable(nil, nil, context.DeadlineExceeded, "token"); !errors.Is(err, ErrTokenEndpointUnavailable) {
		t.Fatalf("transport failure = %v, want transient", err)
	}
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusFound} {
		if err := tokenEndpointUnavailable(nil, &http.Response{StatusCode: status}, nil, "token"); err != nil {
			t.Fatalf("HTTP %d = %v, want a definite answer", status, err)
		}
	}
	transient := credentialFault(fmt.Errorf("%w: %w", ErrCredentialUnavailable, ErrTokenEndpointUnavailable))
	if detail := DiagnoseError(transient); detail.Code != "credential_unavailable" || !detail.Retryable {
		t.Fatalf("unreachable token host described as %+v, want retryable credential_unavailable", detail)
	}
}

// A request that the recovery gate holds back during an identity proof outage
// is answered with the proof failure, never with a bare credential fault that a
// surface would name an unresolved credential to rotate.
func TestRecoveryGateAnswersAProofOutageWithTheProofFailure(t *testing.T) {
	fixture := newFakeCluster(t, "synthetic-a", "api")
	client, err := NewClient(fixture.trustedConfig(&credentialSource{authorization: func(context.Context) (string, error) {
		return "Bearer synthetic-a", nil
	}}))
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "configured"}, 0)
	service.recovery = &clusterRecovery{cluster: "cluster"}
	registry := NewServiceRegistry(service)
	defer registry.Close()

	service.recovery.failed(&proofError{class: "unreachable"}, nil, "org")
	_, err = registry.ResolveCluster("org", "cluster")
	if !IsCredentialProofFailure(err) {
		t.Fatalf("gated resolution during a proof outage = %v, want the proof failure", err)
	}
	if detail := DiagnoseError(err); detail.Code != "proof_unreachable" || !detail.Retryable {
		t.Fatalf("gated resolution described as %+v, want proof_unreachable", detail)
	}
	if errors.Is(err, ErrConnectorCredentialUnresolved) {
		t.Fatal("a proof outage was named an unresolved credential")
	}
	// The gate still stands for a credential fault, so every check that keys on
	// one (report fallback, capture status) keeps working.
	if !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("gated proof failure %v no longer unwraps to the credential fault", err)
	}

	// A later failure of another kind replaces the cause: the gate never keeps
	// naming a proof outage that is no longer the reason.
	service.recovery.mu.Lock()
	service.recovery.next = time.Time{}
	service.recovery.mu.Unlock()
	service.recovery.failed(ErrCredentialUnavailable, nil, "org")
	_, err = registry.ResolveCluster("org", "cluster")
	if IsCredentialProofFailure(err) || !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("gate after a non-proof failure = %v, want the bare credential fault", err)
	}
	service.recovery.committed("revision")
	if gated := service.recovery.requestGateError("org"); gated != nil {
		t.Fatalf("a committed publication left the gate answering %v", gated)
	}
}

// A request whose own context ends while it waits on the identity proof says
// nothing about the credential. The gate keeps answering with the proof
// failure, never with a bare credential fault that names a credential to rotate.
func TestRecoveryGateKeepsTheProofCauseAfterAnAbandonedAttempt(t *testing.T) {
	for _, abandoned := range []error{context.DeadlineExceeded, context.Canceled, fmt.Errorf("token request: %w", context.DeadlineExceeded)} {
		isolateProblems(t, 0, nil)
		recovery := &clusterRecovery{cluster: "cluster"}
		recovery.failed(&proofError{class: "unreachable"}, nil, "org")
		recovery.failed(abandoned, nil, "org")
		err := recovery.requestGateError("org")
		var proof *proofError
		if !errors.As(err, &proof) || !errors.Is(err, ErrCredentialUnavailable) {
			t.Fatalf("gate after %v = %v, want the proof failure that is still a credential fault", abandoned, err)
		}
		if errors.Is(err, ErrConnectorCredentialUnresolved) {
			t.Fatalf("gate after %v named an unresolved credential", abandoned)
		}
		// The abandoned attempt still counts toward the retry schedule, so the
		// supervisor keeps backing off instead of retrying at once.
		recovery.mu.Lock()
		next := recovery.next
		recovery.mu.Unlock()
		if !time.Now().Before(next) {
			t.Fatalf("gate after %v has no retry scheduled", abandoned)
		}
	}
}

// A request that becomes the replacement attempt reserves the next retry and
// then stops waiting, so the cluster's retry time moves on while the scope gate
// has already expired. The next request is held back by that retry time and
// must still hear about the proof outage, not a credential to rotate.
func TestAbandonedReplacementAttemptStillAnswersWithTheProof(t *testing.T) {
	isolateProblems(t, 0, nil)
	fixture := newFakeCluster(t, "synthetic-a", "api")
	client, err := NewClient(fixture.trustedConfig(&credentialSource{authorization: func(context.Context) (string, error) {
		return "Bearer synthetic-a", nil
	}}))
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "configured"}, 0)
	service.recovery = &clusterRecovery{cluster: "cluster"}
	registry := NewServiceRegistry(service)
	defer registry.Close()

	service.recovery.failed(&proofError{class: "unreachable"}, nil, "org")
	service.recovery.mu.Lock()
	for _, scope := range service.recovery.scopes {
		scope.next = time.Now().Add(-time.Millisecond)
	}
	service.recovery.next = time.Now().Add(-time.Millisecond)
	service.recovery.mu.Unlock()
	service.identity.mu.Lock()
	service.identity.retired = true
	service.identity.mu.Unlock()
	service.recovery.reserveReplacement(false)
	if gated := service.recovery.requestGateError("org"); gated != nil {
		t.Fatalf("setup: scope gate still closed (%v), want only the reserved retry time ahead", gated)
	}

	_, err = registry.ResolveCluster("org", "cluster")
	if !IsCredentialProofFailure(err) || !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("request during the reserved retry = %v, want the proof failure", err)
	}
	if errors.Is(err, ErrConnectorCredentialUnresolved) {
		t.Fatal("request during the reserved retry named an unresolved credential")
	}
}

// An attempt that stopped waiting writes no warning: it names nothing the
// operator can fix, and during a proof outage a timeout warning would point
// at the cluster endpoint instead of the proof host.
func TestAbandonedAttemptWritesNoWarning(t *testing.T) {
	logs := captureLogLines(t, "info")
	recovery := &clusterRecovery{cluster: "cluster"}
	recovery.failed(context.DeadlineExceeded, nil, "org")
	recovery.failed(context.Canceled, nil, "org")
	if lines := visible(logs.all()); len(lines) != 0 {
		t.Fatalf("abandoned attempts wrote %q at info or above, want nothing", lines)
	}

	recovery.failed(&proofError{class: "unreachable"}, nil, "org")
	before := len(visible(logs.all()))
	recovery.failed(context.DeadlineExceeded, nil, "org")
	recovery.mu.Lock()
	recovery.next = time.Time{}
	recovery.mu.Unlock()
	recovery.failed(context.DeadlineExceeded, nil, "org")
	if lines := visible(logs.all()); len(lines) != before {
		t.Fatalf("abandoned attempts during a proof outage wrote %q, want nothing new", lines[before:])
	}
}

// While the cloud token host gives no answer, a held request is told the
// truth: the credential is unavailable for now and Versus keeps retrying. The
// answer is still a credential fault, so every surface fails closed. Feeding
// held answers back as failures changes nothing and writes no warning.
func TestHeldRequestsDuringATokenHostOutageAnswerRetryable(t *testing.T) {
	logs := captureLogLines(t, "info")
	recovery := &clusterRecovery{cluster: "cluster"}
	recovery.failed(credentialFault(fmt.Errorf("kubernetes: AKS credential endpoint unavailable: %w", ErrTokenEndpointUnavailable)), nil, "org")
	warned := len(visible(logs.all()))
	if warned != 1 {
		t.Fatalf("token host outage wrote %d lines, want one warning", warned)
	}
	recovery.mu.Lock()
	failures, next := recovery.failures, recovery.next
	recovery.mu.Unlock()

	for _, held := range []error{recovery.requestGateError("org"), recovery.gatedError("org")} {
		if !errors.Is(held, ErrCredentialUnavailable) || !errors.Is(held, ErrTokenEndpointUnavailable) {
			t.Fatalf("held answer %v, want a credential fault naming the token endpoint", held)
		}
		if detail := DiagnoseError(held); detail.Code != "credential_unavailable" || !detail.Retryable {
			t.Fatalf("held answer described as %+v, want retryable credential_unavailable", detail)
		}
		for range 20 {
			recovery.failed(held, nil, "org")
		}
	}
	recovery.mu.Lock()
	unchanged := recovery.failures == failures && recovery.next.Equal(next)
	recovery.mu.Unlock()
	if !unchanged {
		t.Fatal("held answers were recorded as credential failures")
	}
	if lines := visible(logs.all()); len(lines) != warned {
		t.Fatalf("held answers wrote %q, want nothing new", lines[warned:])
	}
	if detail := DiagnoseError(recovery.requestGateError("org")); !detail.Retryable {
		t.Fatal("held answers replaced the token endpoint cause")
	}

	// A definite answer from the token host replaces the cause.
	recovery.mu.Lock()
	recovery.next = time.Time{}
	recovery.mu.Unlock()
	recovery.failed(ErrCredentialUnavailable, nil, "org")
	held := recovery.requestGateError("org")
	if errors.Is(held, ErrTokenEndpointUnavailable) || DiagnoseError(held).Retryable {
		t.Fatalf("held answer after a definite failure = %v, want the bare credential fault", held)
	}
}

// A token host that alternates a missing answer with a definite one warns
// about each cause once per throttle window, not on every attempt.
func TestAlternatingCredentialCauseWarnsOncePerCausePerWindow(t *testing.T) {
	logs := captureLogLines(t, "info")
	clock := newFakeClock()
	isolateProblems(t, 10*time.Minute, clock.Now)
	recovery := &clusterRecovery{cluster: "cluster"}
	unreachable := credentialFault(fmt.Errorf("%w: %w", ErrCredentialUnavailable, ErrTokenEndpointUnavailable))
	warnings := func() int {
		count := 0
		for _, line := range visible(logs.all()) {
			if strings.Contains(line, "credential check failed") {
				count++
			}
		}
		return count
	}
	attempt := func(i int) {
		recovery.mu.Lock()
		recovery.next = time.Time{}
		recovery.mu.Unlock()
		if i%2 == 0 {
			recovery.failed(unreachable, nil, "org")
		} else {
			recovery.failed(ErrCredentialUnavailable, nil, "org")
		}
		clock.advance(10 * time.Second)
	}
	for i := range 50 {
		attempt(i)
	}
	if got := warnings(); got != 2 {
		t.Fatalf("alternating causes over 50 attempts wrote %d warnings, want one per cause", got)
	}
	clock.advance(10 * time.Minute)
	for i := range 50 {
		attempt(i)
	}
	if got := warnings(); got > 4 {
		t.Fatalf("alternating causes over a second window wrote %d warnings in total, want at most two more", got)
	}

	// A publication clears both causes, so an outage after the throttle
	// forgets them warns again, once.
	recovery.committed("revision")
	before := warnings()
	clock.advance(11 * time.Minute)
	recovery.failed(unreachable, nil, "org")
	if got := warnings(); got != before+1 {
		t.Fatalf("an outage after recovery wrote %d new warnings, want one", got-before)
	}
}

// A held answer is never recorded as a failure, but an answered denial joined
// to one is: the API server's refusal must always reach the reader.
func TestHeldAnswerJoinedWithADenialStillRecordsTheDenial(t *testing.T) {
	isolateProblems(t, 0, nil)
	for _, join := range []func(error) error{
		func(held error) error { return errors.Join(held, ErrForbidden) },
		func(held error) error { return fmt.Errorf("collect: %w", errors.Join(ErrUnauthorized, held)) },
	} {
		recovery := &clusterRecovery{cluster: "cluster"}
		recovery.failed(&proofError{class: "unreachable"}, nil, "org")
		recovery.failed(join(recovery.requestGateError("org")), nil, "org")
		recovery.mu.Lock()
		denied := recovery.scopes["org"].denied
		recovery.mu.Unlock()
		if !denied {
			t.Fatal("a denial joined to a held answer was dropped")
		}
		if gated := recovery.requestGateError("org"); gated != nil {
			t.Fatalf("gate after a recorded denial answers %v, want the read path to report the denial", gated)
		}
	}
}

// hangingTokenSource is a cloud token source whose host never answers: every
// dial blocks until the refresh's context ends, as when packets to the token
// host are dropped.
func hangingTokenSource(dials *atomic.Int32) *expiringBearerSource {
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		dials.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}}}
	return &expiringBearerSource{now: time.Now, skew: 2 * time.Minute, refresh: func(ctx context.Context) (string, time.Time, error) {
		return requestAzureToken(ctx, client, "https://token.invalid/tenant/oauth2/v2.0/token", nil)
	}}
}

// sourceState reads the recorded refresh outcome under the source's lock.
func sourceState(source *expiringBearerSource) (failures int, retryAt time.Time, unreachable bool) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.failures, source.retryAt, source.unreachable
}

// A caller with no token whose own deadline passes while the refresh waits on
// the token host is told the host has not answered, and nothing is recorded
// for it. A caller that cancelled gets only its cancellation.
func TestTokenRequestUnansweredBeforeTheCallerDeadlineIsATokenEndpointFailure(t *testing.T) {
	var dials atomic.Int32
	source := hangingTokenSource(&dials)
	defer source.closeSource()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := source.Authorization(ctx)
	if !errors.Is(err, ErrTokenEndpointUnavailable) || !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("token request past the caller's deadline = %v, want the token endpoint failure", err)
	}
	if detail := DiagnoseError(err); detail.Code != "credential_unavailable" || !detail.Retryable || !strings.Contains(detail.Action, "token endpoint") {
		t.Fatalf("token request past the caller's deadline described as %+v", detail)
	}
	if failures, retryAt, unreachable := sourceState(source); failures != 0 || !retryAt.IsZero() || unreachable {
		t.Fatal("a caller's own deadline was recorded against the token host")
	}
	ctx, cancel = context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, err := source.Authorization(ctx); errors.Is(err, ErrTokenEndpointUnavailable) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled token request = %v, want the cancellation only", err)
	}
	if dials.Load() != 1 {
		t.Fatalf("token requests = %d, want the one refresh in flight", dials.Load())
	}
}

// A cluster whose token host drops every packet at boot. Requests polled
// while credential rebuilds hang on the host never queue behind them: each
// is answered within about a second with the retryable token endpoint fault,
// the sync report never says unauthorized, and once a rebuild runs into its
// bound the default log warns exactly once, naming the token endpoint.
func TestHangingTokenHostAtBootAnswersRetryableAndWarnsOnce(t *testing.T) {
	logs := captureLogLines(t, "info")
	cluster := testClusterID(t)
	var dials atomic.Int32
	fixture := newFakeCluster(t, "synthetic-a", "api")
	// Each refresh gives up on the hanging host after half a second instead
	// of the production ten, so the polling window below spans several
	// failed refreshes and still sees exactly one warning.
	source := hangingTokenSource(&dials)
	source.refreshTimeout = 500 * time.Millisecond
	client, err := NewClient(fixture.trustedConfig(source))
	if err != nil {
		t.Fatal(err)
	}
	boot, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	service := newService(boot, client, Scope{OrgID: "org", ClusterID: cluster, CredentialID: "configured"}, 0)
	registry := NewServiceRegistry(service)
	defer registry.Close()

	deadline := time.Now().Add(3 * time.Second)
	polls := 0
	for time.Now().Before(deadline) {
		started := time.Now()
		_, err := registry.ResolveCluster("org", cluster)
		if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
			t.Fatalf("poll %d waited %v behind a hanging rebuild", polls, elapsed)
		}
		if !errors.Is(err, ErrTokenEndpointUnavailable) || !errors.Is(err, ErrCredentialUnavailable) || errors.Is(err, ErrConnectorCredentialUnresolved) {
			t.Fatalf("poll %d during the outage = %v, want the token endpoint fault", polls, err)
		}
		if detail := DiagnoseError(err); detail.Code != "credential_unavailable" || !detail.Retryable || !strings.Contains(detail.Action, "token endpoint") {
			t.Fatalf("poll %d described as %+v", polls, detail)
		}
		if class := service.failedSyncHealth(err).Class; class != "token_endpoint_unreachable" {
			t.Fatalf("sync class during the outage = %q, want token_endpoint_unreachable", class)
		}
		polls++
		time.Sleep(100 * time.Millisecond)
	}
	if dials.Load() == 0 {
		t.Fatal("the token host was never dialled")
	}
	warnings := credentialWarnings(logs, cluster)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "reason=token_endpoint_unavailable") {
		t.Fatalf("credential warnings during the outage = %q, want one naming the token endpoint", warnings)
	}
}

// A token is sent only while it stays valid for the margin: transit time or
// a cluster clock a little ahead must not turn the end of a token host
// outage into a rejection. Inside the margin, or expired, it is never sent.
func TestTokenSourceServesOnlyWithAMarginBeforeExpiry(t *testing.T) {
	source := &expiringBearerSource{now: time.Now, skew: 2 * time.Minute, refreshTimeout: 50 * time.Millisecond, refresh: func(context.Context) (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("dial: %w", ErrTokenEndpointUnavailable)
	}}
	defer source.closeSource()
	source.header, source.expires = "Bearer near-expiry", time.Now().Add(20*time.Second)
	source.current.Store(&expiringToken{header: source.header, expires: source.expires})
	if header, err := source.Authorization(t.Context()); err == nil || !errors.Is(err, ErrTokenEndpointUnavailable) {
		t.Fatalf("token inside the margin = %q, %v; want the token endpoint failure", header, err)
	}
	source.mu.Lock()
	source.expires = time.Now().Add(-time.Second)
	source.retryAt = time.Time{}
	source.mu.Unlock()
	if header, err := source.Authorization(t.Context()); err == nil {
		t.Fatalf("expired token served: %q", header)
	}
}

// A token still valid past the margin is sent at once, even while its
// renewal waits on a token host that does not answer: callers never wait on
// the refresh, and one refresh runs for all of them.
func TestTokenSourceNeverMakesACallerWaitForARenewal(t *testing.T) {
	var dials atomic.Int32
	source := hangingTokenSource(&dials)
	defer source.closeSource()
	source.header, source.expires = "Bearer valid", time.Now().Add(90*time.Second)
	source.current.Store(&expiringToken{header: source.header, expires: source.expires})
	for i := range 5 {
		ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
		started := time.Now()
		header, err := source.Authorization(ctx)
		cancel()
		if err != nil || header != "Bearer valid" || time.Since(started) > 20*time.Millisecond {
			t.Fatalf("caller %d = %q, %v after %v; want the valid token at once", i, header, err, time.Since(started))
		}
	}
	waitUntil(t, time.Second, "the renewal to dial", func() bool { return dials.Load() == 1 })
	if dials.Load() != 1 {
		t.Fatalf("renewal dials = %d, want one", dials.Load())
	}
}

// With the token host hanging, refreshes start at most once per backoff
// step whatever the callers do: each refresh runs to its own bound and is
// recorded, and callers in between get the recorded answer at once.
func TestTokenSourceSpacesRefreshesOnAHangingHost(t *testing.T) {
	var dials atomic.Int32
	source := hangingTokenSource(&dials)
	source.refreshTimeout = 100 * time.Millisecond
	defer source.closeSource()
	started := time.Now()
	for time.Since(started) < 1500*time.Millisecond {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		_, err := source.Authorization(ctx)
		cancel()
		if !errors.Is(err, ErrTokenEndpointUnavailable) {
			t.Fatalf("caller during the outage = %v, want the token endpoint failure", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Refresh 1 at 0 s (fails at 0.1 s, retry at 1.1 s), refresh 2 at
	// 1.1 s (fails at 1.2 s, retry at 3.2 s): two in 1.5 s.
	if got := dials.Load(); got < 1 || got > 2 {
		t.Fatalf("token requests in 1.5 s = %d, want at most one per backoff step", got)
	}
	if failures, retryAt, unreachable := sourceState(source); failures == 0 || retryAt.IsZero() || !unreachable {
		t.Fatal("a refresh that ran into its own bound was not recorded as a token host outage")
	}
}

// An established cluster whose token host goes dark while the token is in its
// renewal window: requests keep being served with the valid token until the
// margin, then are answered at once with the retryable token endpoint fault,
// never with a credential to fix and never after a long wait.
func TestEstablishedClusterWhenTheTokenHostGoesDark(t *testing.T) {
	cluster := testClusterID(t)
	var dials atomic.Int32
	var issued atomic.Bool
	hanging := hangingTokenSource(&dials)
	dark := hanging.refresh
	expires := time.Now().Add(proofGraceMargin + 2*time.Second)
	hanging.refresh = func(ctx context.Context) (string, time.Time, error) {
		if issued.CompareAndSwap(false, true) {
			return "synthetic-a", expires, nil
		}
		return dark(ctx)
	}
	fixture := newFakeCluster(t, "synthetic-a", "api")
	client, err := NewClient(fixture.trustedConfig(hanging))
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{OrgID: "org", ClusterID: cluster, CredentialID: "configured"}, 0)
	registry := NewServiceRegistry(service)
	defer registry.Close()

	served, held := 0, 0
	for time.Now().Before(expires.Add(-proofGraceMargin + 3*time.Second)) {
		asked := time.Now()
		got, err := registry.ResolveCluster("org", cluster)
		if waited := time.Since(asked); waited > 1500*time.Millisecond {
			t.Fatalf("request waited %v on the dark token host", waited)
		}
		switch {
		case err == nil && got != nil:
			if !time.Now().Before(expires.Add(-proofGraceMargin + 100*time.Millisecond)) {
				t.Fatal("a service was resolved with the token inside its margin")
			}
			served++
		case errors.Is(err, ErrTokenEndpointUnavailable) && DiagnoseError(err).Retryable && !errors.Is(err, ErrConnectorCredentialUnresolved):
			held++
		default:
			t.Fatalf("request during the dark window = %v (%+v)", err, DiagnoseError(err))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if served == 0 || held == 0 {
		t.Fatalf("served=%d held=%d; want the valid token used until the margin, then held answers", served, held)
	}
}

// Closing the registry stops a rebuild in flight and the token refresh it
// waits on: no token request is made and no warning is written afterwards.
func TestRegistryCloseStopsRebuildsAndRefreshes(t *testing.T) {
	logs := captureLogLines(t, "info")
	cluster := testClusterID(t)
	var dials atomic.Int32
	source := hangingTokenSource(&dials)
	fixture := newFakeCluster(t, "synthetic-a", "api")
	client, err := NewClient(fixture.trustedConfig(source))
	if err != nil {
		t.Fatal(err)
	}
	boot, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	service := newService(boot, client, Scope{OrgID: "org", ClusterID: cluster, CredentialID: "configured"}, 0)
	registry := NewServiceRegistry(service)
	if _, err := registry.ResolveCluster("org", cluster); err == nil {
		t.Fatal("a cluster with no token resolved")
	}
	registry.Close()
	waitUntil(t, time.Second, "the token refresh in flight to stop", func() bool {
		source.mu.Lock()
		defer source.mu.Unlock()
		return source.inflight == nil
	})
	closedAt := dials.Load()
	time.Sleep(1500 * time.Millisecond)
	if got := dials.Load(); got != closedAt {
		t.Fatalf("token requests after Close = %d, want none", got-closedAt)
	}
	if warnings := credentialWarnings(logs, cluster); len(warnings) != 0 {
		t.Fatalf("warnings after Close = %q, want none", warnings)
	}
	if _, err := registry.ResolveCluster("org", cluster); !errors.Is(err, ErrClusterUnavailable) {
		t.Fatalf("request after Close = %v, want cluster unavailable", err)
	}
}

// A provider whose tokens arrive already inside the renewal window (a short
// lifetime, or a clock ahead of the provider's) is not asked again at the
// callers' rate: such an answer counts as an invalid one and the backoff
// spaces the refreshes. A token with less than the margin left is never
// sent; a longer one is sent while the next refresh waits for its backoff.
func TestShortLivedTokensDoNotDriveTheRefreshRate(t *testing.T) {
	for _, lifetime := range []time.Duration{20 * time.Second, 90 * time.Second} {
		t.Run(lifetime.String(), func(t *testing.T) {
			var dials atomic.Int32
			source := &expiringBearerSource{now: time.Now, skew: 2 * time.Minute, refresh: func(context.Context) (string, time.Time, error) {
				dials.Add(1)
				return fmt.Sprintf("short-%d", dials.Load()), time.Now().Add(lifetime), nil
			}}
			defer source.closeSource()
			deadline := time.Now().Add(1500 * time.Millisecond)
			for time.Now().Before(deadline) {
				ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				header, err := source.Authorization(ctx)
				cancel()
				if lifetime <= proofGraceMargin {
					if err == nil {
						t.Fatalf("token issued with %v left was sent: %q", lifetime, header)
					}
					if detail := DiagnoseError(err); detail.Retryable || errors.Is(err, ErrTokenEndpointUnavailable) {
						t.Fatalf("token issued inside the margin answered %+v, want a definite provider fault", detail)
					}
				} else if err != nil {
					t.Fatalf("token with %v left refused: %v", lifetime, err)
				}
				time.Sleep(time.Millisecond)
			}
			// Refresh 1 at 0 s, refresh 2 after the 1 s backoff, refresh 3
			// not before 3 s: two in 1.5 s.
			if got := dials.Load(); got < 1 || got > 2 {
				t.Fatalf("refreshes in 1.5 s = %d, want them spaced by the backoff", got)
			}
		})
	}
}

// Five clients poll a cluster whose token host goes dark while capture is
// running, so supervised retries validate the epoch on their own schedule.
// No request queues behind them: every answer comes within 1.5 s, across
// several retries.
func TestDarkTokenHostWithCaptureRunningNeverQueuesRequests(t *testing.T) {
	cluster := testClusterID(t)
	var dials atomic.Int32
	var issued atomic.Bool
	source := hangingTokenSource(&dials)
	// Each refresh gives up on the dark host after half a second instead of
	// the production ten, so the polling window spans several failed
	// refreshes and the supervised retries they trigger.
	source.refreshTimeout = 500 * time.Millisecond
	dark := source.refresh
	expires := time.Now().Add(proofGraceMargin + 2*time.Second)
	source.refresh = func(ctx context.Context) (string, time.Time, error) {
		if issued.CompareAndSwap(false, true) {
			return "synthetic-a", expires, nil
		}
		return dark(ctx)
	}
	fixture := newFakeCluster(t, "synthetic-a", "api")
	client, err := NewClient(fixture.trustedConfig(source))
	if err != nil {
		t.Fatal(err)
	}
	client.indexLimiter = nil
	service := NewService(client, Scope{OrgID: "org", ClusterID: cluster, CredentialID: "configured"}, 0)
	service.SetChangeStorage(storage.NewMemory())
	registry := NewServiceRegistry(service)
	registry.StartCapture(t.Context())
	defer registry.Close()

	var slow atomic.Int32
	var answers atomic.Int32
	var pollers sync.WaitGroup
	for range 5 {
		pollers.Add(1)
		go func() {
			defer pollers.Done()
			for time.Until(expires.Add(-proofGraceMargin)) > -3*time.Second {
				asked := time.Now()
				_, err := registry.ResolveCluster("org", cluster)
				if waited := time.Since(asked); waited > 1500*time.Millisecond {
					slow.Add(1)
					t.Errorf("request at %v after the margin waited %v", asked.Sub(expires.Add(-proofGraceMargin)).Round(time.Second), waited)
				}
				if err != nil && !DiagnoseError(err).Retryable {
					t.Errorf("request during the dark window answered %+v", DiagnoseError(err))
				}
				answers.Add(1)
				time.Sleep(100 * time.Millisecond)
			}
		}()
	}
	pollers.Wait()
	if answers.Load() == 0 || slow.Load() != 0 {
		t.Fatalf("answers=%d slow=%d", answers.Load(), slow.Load())
	}
	if dials.Load() < 2 {
		t.Fatalf("the dark token host was dialled %d times, want several failed refreshes inside the window", dials.Load())
	}
}
