package main

import (
	"testing"

	c "github.com/VersusControl/versus-incident/pkg/config"
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
