package storage

// postgres_connect_test.go — covers the bounded, redacted, retried Postgres
// connect hardening: redactDSN must never leak a password, and an unreachable
// DSN must fail within its configured budget instead of hanging.

import (
	"strings"
	"testing"
	"time"
)

func TestRedactDSN(t *testing.T) {
	cases := []struct {
		name   string
		dsn    string
		want   string
		absent string // substring that must NOT appear (e.g. the password)
	}{
		{
			name:   "url form with password",
			dsn:    "postgres://versus:s3cr3t@db.internal:5432/incidents?sslmode=disable",
			want:   "versus@db.internal:5432/incidents",
			absent: "s3cr3t",
		},
		{
			name:   "url form without user",
			dsn:    "postgres://db.internal:5432/incidents",
			want:   "db.internal:5432/incidents",
			absent: "@",
		},
		{
			name:   "keyword form with password",
			dsn:    "host=db.internal port=5432 dbname=incidents user=versus password=s3cr3t sslmode=disable",
			want:   "db.internal:5432/incidents",
			absent: "s3cr3t",
		},
		{
			name:   "malformed dsn falls back to constant",
			dsn:    "::::not a dsn::::password=leakme",
			want:   "postgres",
			absent: "leakme",
		},
		{
			name: "empty dsn falls back to constant",
			dsn:  "",
			want: "postgres",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactDSN(tc.dsn)
			if got != tc.want {
				t.Fatalf("redactDSN(%q) = %q, want %q", tc.dsn, got, tc.want)
			}
			if tc.absent != "" && strings.Contains(got, tc.absent) {
				t.Fatalf("redactDSN(%q) = %q leaked %q", tc.dsn, got, tc.absent)
			}
		})
	}
}

// TestRedactDSN_NeverLeaksPassword sweeps a range of DSN shapes to assert the
// literal password token never survives redaction.
func TestRedactDSN_NeverLeaksPassword(t *testing.T) {
	const password = "TOPSECRET_PW"
	dsns := []string{
		"postgres://user:" + password + "@host:5432/db?sslmode=disable",
		"postgresql://user:" + password + "@host/db",
		"host=host port=5432 dbname=db user=user password=" + password,
		"password=" + password + " host=host dbname=db",
	}
	for _, dsn := range dsns {
		if got := redactDSN(dsn); strings.Contains(got, password) {
			t.Fatalf("redactDSN(%q) leaked password: %q", dsn, got)
		}
	}
}

// TestNewPostgres_UnreachableIsBounded proves the connect no longer hangs: a
// DSN pointing at a closed port with a tiny budget returns an error quickly
// instead of blocking on the OS TCP connect timeout.
func TestNewPostgres_UnreachableIsBounded(t *testing.T) {
	t.Setenv("POSTGRES_CONNECT_TIMEOUT", "2s")

	const dsn = "postgres://versus:versus@127.0.0.1:1/versus?sslmode=disable"

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := NewPostgres(PostgresOptions{DSN: dsn})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("NewPostgres to a closed port should return an error")
		}
		if strings.Contains(err.Error(), "versus:versus") {
			t.Fatalf("error leaked DSN credentials: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("NewPostgres hung on unreachable host (>%s); bound not enforced", time.Since(start))
	}
}
