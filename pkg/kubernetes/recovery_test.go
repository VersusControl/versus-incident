package kubernetes

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/aws-sdk-go-v2/service/sts/types"
)

func TestRegistryRenewsTwentyEpochsWithoutBrowserOrResolveRequests(t *testing.T) {
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
		<-credential.lock
		next := credential.expires.Add(-credential.skew)
		credential.lock <- struct{}{}
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
	waitUntil(t, 10*time.Second, "initial publication", func() bool {
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
	waitUntil(t, 5*time.Second, "outage cooldown", func() bool { return time.Now().Before(service.recovery.deadline()) })
	waitUntil(t, 5*time.Second, "provider outage retires the credential epoch", func() bool { return service.identity.ctx.Err() != nil })
	before := calls.Load()
	deadline := service.recovery.deadline()
	for i := 0; i < 100; i++ {
		if _, err := registry.ResolveCluster("org", "cluster"); err == nil {
			t.Fatal("outage served a successful warming response")
		}
	}
	if calls.Load() != before || !service.recovery.deadline().Equal(deadline) {
		t.Fatal("request traffic bypassed cooldown")
	}
	fail.Store(false)
	waitUntil(t, 10*time.Second, "autonomous provider recovery", func() bool {
		registry.mu.Lock()
		fresh := registry.entries[0].Service
		registry.mu.Unlock()
		if fresh.identity == service.identity {
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
		return cache.generation.Revision != ""
	})
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
