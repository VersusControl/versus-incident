package config

import "testing"

// TestRedisConfigTLSEnabled covers the QA-002 escape hatch: TLS is the
// default when the flag is omitted, an explicit true keeps TLS, and an
// explicit false dials plaintext.
func TestRedisConfigTLSEnabled(t *testing.T) {
	tru := true
	fls := false

	tests := []struct {
		name string
		tls  *bool
		want bool
	}{
		{"omitted defaults to TLS", nil, true},
		{"explicit true", &tru, true},
		{"explicit false", &fls, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc := RedisConfig{TLS: tc.tls}
			if got := rc.TLSEnabled(); got != tc.want {
				t.Fatalf("TLSEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRedisTLSFromEnv covers the QA-005 fail-closed parse: only an explicit
// off-value disables TLS; every other (non-empty) value — including an
// enable-intent typo like "1"/"yes" or garbage — keeps TLS on. Unset is
// handled by the caller (no override) and stays on via TLSEnabled().
func TestRedisTLSFromEnv(t *testing.T) {
	tests := []struct {
		value string
		want  bool // resolved TLSEnabled()
	}{
		{"false", false},
		{"FALSE", false},
		{"0", false},
		{"no", false},
		{"off", false},
		{" false ", false},
		{"true", true},
		{"1", true},
		{"yes", true},
		{"on", true},
		{"enabled", true},
		{"ture", true}, // typo stays secure
		{"garbage", true},
	}

	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			got := RedisConfig{TLS: redisTLSFromEnv(tc.value)}.TLSEnabled()
			if got != tc.want {
				t.Fatalf("redisTLSFromEnv(%q) → TLSEnabled() = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}
