package storage

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestPGSetupHint_ReturnsGuide asserts the provisioning guide is emitted for
// each login/permission SQLSTATE and names the real database, and that the
// PG15+ schema grant line is always present so the operator sees the fix for
// "permission denied for schema public".
func TestPGSetupHint_ReturnsGuide(t *testing.T) {
	codes := []struct {
		code string
		name string
	}{
		{"42501", "insufficient_privilege"},
		{"3F000", "invalid_schema_name"},
		{"28P01", "invalid_password"},
		{"28000", "invalid_authorization"},
		{"3D000", "invalid_catalog_name"},
	}
	for _, tc := range codes {
		t.Run(tc.name, func(t *testing.T) {
			hint := pgSetupHint(&pgconn.PgError{Code: tc.code}, "versus_incident")
			if hint == "" {
				t.Fatalf("pgSetupHint(%s) returned empty, want guide", tc.code)
			}
			if !strings.Contains(hint, "GRANT ALL ON SCHEMA public") {
				t.Fatalf("pgSetupHint(%s) missing schema grant line: %q", tc.code, hint)
			}
			if !strings.Contains(hint, "versus_incident") {
				t.Fatalf("pgSetupHint(%s) does not name the database: %q", tc.code, hint)
			}
		})
	}
}

// TestPGSetupHint_FallbackDBName asserts the guide falls back to
// versus_enterprise when the DSN did not name a database.
func TestPGSetupHint_FallbackDBName(t *testing.T) {
	hint := pgSetupHint(&pgconn.PgError{Code: "42501"}, "")
	if !strings.Contains(hint, "versus_enterprise") {
		t.Fatalf("pgSetupHint fallback did not name versus_enterprise: %q", hint)
	}
}

// TestPGSetupHint_ReturnsEmpty asserts no guide is emitted for a nil error,
// a non-permission PgError, or a plain (non-PgError) error.
func TestPGSetupHint_ReturnsEmpty(t *testing.T) {
	if got := pgSetupHint(nil, "db"); got != "" {
		t.Fatalf("pgSetupHint(nil) = %q, want empty", got)
	}
	// 08006 is connection_failure — not an auth/permission problem.
	if got := pgSetupHint(&pgconn.PgError{Code: "08006"}, "db"); got != "" {
		t.Fatalf("pgSetupHint(08006) = %q, want empty", got)
	}
	if got := pgSetupHint(errors.New("boom"), "db"); got != "" {
		t.Fatalf("pgSetupHint(plain error) = %q, want empty", got)
	}
}

// TestPGSetupHint_NeverLeaksSecret asserts the static guide never contains a
// password or DSN — it is built from constant text plus the database name only.
func TestPGSetupHint_NeverLeaksSecret(t *testing.T) {
	hint := pgSetupHint(&pgconn.PgError{
		Code:    "28P01",
		Message: "password authentication failed for user \"versus\"",
	}, "versus_incident")
	for _, secret := range []string{"s3cr3t", "postgres://", "password=", "@host"} {
		if strings.Contains(hint, secret) {
			t.Fatalf("pgSetupHint leaked %q: %q", secret, hint)
		}
	}
}

// TestDSNDBName covers extracting the database name from both DSN shapes.
func TestDSNDBName(t *testing.T) {
	tests := []struct {
		dsn  string
		want string
	}{
		{"postgres://versus:s3cr3t@host:5432/versus_incident?sslmode=require", "versus_incident"},
		{"host=host port=5432 dbname=versus_incident password=s3cr3t", "versus_incident"},
		{"host=host port=5432", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := dsnDBName(tc.dsn); got != tc.want {
			t.Fatalf("dsnDBName(%q) = %q, want %q", tc.dsn, got, tc.want)
		}
	}
}
