package main

import (
	"testing"

	c "github.com/VersusControl/versus-incident/pkg/config"
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
