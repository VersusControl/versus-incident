package main

import (
	"context"
	"testing"
	"time"

	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/scheduler"
	"github.com/VersusControl/versus-incident/pkg/servicehealth"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/redis/go-redis/v9"
)

// TestHandlerRedisOptionsTLS covers the plaintext-Redis case: with TLS disabled the Redis
// client must not set TLSConfig (so a plaintext Redis can connect), while
// the existing default-on / insecure_skip_verify TLS behaviour is preserved.
func TestHandlerRedisOptionsTLS(t *testing.T) {
	tru := true
	fls := false

	t.Run("tls disabled produces no TLSConfig", func(t *testing.T) {
		opts := handlerRedisOptions(c.RedisConfig{Host: "localhost", Port: 6379, TLS: &fls})
		if opts.TLSConfig != nil {
			t.Fatalf("expected nil TLSConfig when redis.tls=false, got %#v", opts.TLSConfig)
		}
	})

	t.Run("tls omitted defaults to TLS", func(t *testing.T) {
		opts := handlerRedisOptions(c.RedisConfig{Host: "localhost", Port: 6379})
		if opts.TLSConfig == nil {
			t.Fatal("expected TLSConfig when redis.tls is omitted (default-on)")
		}
	})

	t.Run("tls enabled keeps TLS", func(t *testing.T) {
		opts := handlerRedisOptions(c.RedisConfig{Host: "localhost", Port: 6379, TLS: &tru})
		if opts.TLSConfig == nil {
			t.Fatal("expected TLSConfig when redis.tls=true")
		}
	})

	t.Run("tls enabled with insecure_skip_verify", func(t *testing.T) {
		opts := handlerRedisOptions(c.RedisConfig{Host: "localhost", Port: 6379, TLS: &tru, InsecureSkipVerify: true})
		if opts.TLSConfig == nil || !opts.TLSConfig.InsecureSkipVerify {
			t.Fatal("expected InsecureSkipVerify TLSConfig when redis.tls=true and insecure_skip_verify=true")
		}
	})
}

func TestServiceHealthRegistersOneOwnedSchedulerJob(t *testing.T) {
	scheduler.Reset()
	scheduler.SetOwnership(nil)
	t.Cleanup(func() {
		scheduler.Reset()
		scheduler.SetOwnership(nil)
	})
	if err := startServiceHealth(servicehealth.NewManager(storage.NewMemory()), storage.NewMemory(), nil, c.AgentConfig{}, nil); err != nil {
		t.Fatal(err)
	}
	jobs := scheduler.Registered()
	if len(jobs) != 1 || jobs[0].Name != "service-health" {
		t.Fatalf("registered jobs = %#v", jobs)
	}
	if err := startServiceHealth(servicehealth.NewManager(storage.NewMemory()), storage.NewMemory(), nil, c.AgentConfig{}, nil); err == nil {
		t.Fatal("duplicate Service Health registration succeeded")
	}
	scheduler.SetOwnership(func(name string) bool { return name != "service-health" })
	if scheduler.Owns("service-health") {
		t.Fatal("ownership predicate did not reject Service Health job")
	}
}

func TestServiceHealthScheduledCollectionIncludesRegisteredOrganization(t *testing.T) {
	scheduler.Reset()
	scheduler.SetOwnership(nil)
	servicehealth.SetOrganizationLister(func(context.Context) ([]string, error) { return []string{"org-a"}, nil })
	t.Cleanup(func() {
		scheduler.Reset()
		scheduler.SetOwnership(nil)
		servicehealth.SetOrganizationLister(nil)
	})
	store := storage.NewMemory()
	manager := servicehealth.NewManager(store)
	if err := startServiceHealth(manager, store, nil, c.AgentConfig{}, nil); err != nil {
		t.Fatal(err)
	}
	jobs := scheduler.Registered()
	if len(jobs) != 1 {
		t.Fatalf("registered jobs = %#v", jobs)
	}
	if err := jobs[0].Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := manager.LoadSnapshot("org-a"); err != nil || !ok {
		t.Fatalf("non-default snapshot exists = %v, err %v", ok, err)
	}
}

func TestServiceHealthUsesOnlyConfiguredLogSources(t *testing.T) {
	configured := c.AgentConfig{Sources: []c.AgentSourceConfig{
		{Name: "logs", Type: "loki", Enable: true},
		{Name: "disabled", Type: "file", Enable: false},
		{Name: "metrics", Type: "prometheus", Enable: true},
		{Name: "traces", Type: "traces", Enable: true},
		{Name: "unknown", Type: "custom", Enable: true},
	}}
	got := serviceHealthLogSourceIDs(configured, []core.SignalSource{
		&healthSource{name: "loki:logs"}, &healthSource{name: "file:disabled"}, &healthSource{name: "prometheus:metrics"}, &healthSource{name: "traces:traces"}, &healthSource{name: "custom:unknown"},
	})
	if len(got) != 1 || got[0] != "loki:logs" {
		t.Fatalf("log sources = %#v", got)
	}
}

type healthSource struct{ name string }

func (source *healthSource) Name() string { return source.name }
func (source *healthSource) Pull(context.Context, time.Time) ([]core.Signal, time.Time, error) {
	return nil, time.Time{}, nil
}

// TestNewRedisClientClusterType verifies that enabling cluster mode builds a
// cluster-aware client (*redis.ClusterClient) rather than a single-node one.
// The cluster client is what parses the Redis 7 / Valkey CLUSTER SLOTS reply
// (which carries a 4th per-node metadata element) correctly, so cursor
// persistence keeps working against ElastiCache in cluster mode instead of
// falling back to in-memory. Both concrete clients are threaded as the shared
// redis.UniversalClient interface, which the assertions below also confirm.
func TestNewRedisClientClusterType(t *testing.T) {
	tru := true
	fls := false

	t.Run("cluster enabled returns *redis.ClusterClient", func(t *testing.T) {
		client := newRedisClient(c.RedisConfig{Host: "localhost", Port: 6379, TLS: &fls, Cluster: &tru})
		defer client.Close()

		if _, ok := client.(*redis.ClusterClient); !ok {
			t.Fatalf("expected *redis.ClusterClient when redis.cluster=true, got %T", client)
		}
		var _ redis.UniversalClient = client
	})

	t.Run("cluster disabled returns single-node *redis.Client", func(t *testing.T) {
		client := newRedisClient(c.RedisConfig{Host: "localhost", Port: 6379, TLS: &fls})
		defer client.Close()

		if _, ok := client.(*redis.Client); !ok {
			t.Fatalf("expected *redis.Client when cluster is off, got %T", client)
		}
		var _ redis.UniversalClient = client
	})
}
