package storage

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// An empty retention means the 7-day default, "0" keeps everything, and an
// unreadable or negative value is refused; the environment overrides the file.
func TestEvidenceRetentionOptionResolvesDefaultAndRejectsInvalid(t *testing.T) {
	for _, test := range []struct {
		name, configured, env string
		want                  time.Duration
		invalid               bool
	}{
		{name: "empty means seven days", want: 168 * time.Hour},
		{name: "zero keeps everything", configured: "0", want: 0},
		{name: "zero with a unit keeps everything", configured: "0s", want: 0},
		{name: "configured value", configured: "720h", want: 720 * time.Hour},
		{name: "environment overrides the file", configured: "720h", env: "0", want: 0},
		{name: "environment alone", env: "9504h", want: 9504 * time.Hour},
		{name: "day unit is not a duration", configured: "7d", invalid: true},
		{name: "not a duration", configured: "forever", invalid: true},
		{name: "negative", configured: "-1h", invalid: true},
		{name: "invalid environment over a valid file", configured: "168h", env: "week", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("KUBERNETES_EVIDENCE_RETENTION", test.env)
			got, err := evidenceRetentionOption(test.configured)
			if test.invalid {
				if err == nil {
					t.Fatalf("retention %q (env %q) = %s, want a startup error", test.configured, test.env, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("retention %q (env %q) = %s, %v; want %s", test.configured, test.env, got, err, test.want)
			}
		})
	}
}

// An invalid retention stops startup before any database connection is tried.
func TestNewPostgresRefusesInvalidRetentionBeforeConnecting(t *testing.T) {
	t.Setenv("KUBERNETES_EVIDENCE_RETENTION", "")
	started := time.Now()
	provider, err := NewPostgres(PostgresOptions{DSN: "postgres://nobody:secret@127.0.0.1:1/none?sslmode=disable", EvidenceRetention: "7d"})
	if err == nil {
		_ = provider.Close()
		t.Fatal("an invalid retention started the store")
	}
	if !strings.Contains(err.Error(), "retention") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("startup error %q, want a retention error without the DSN secret", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("invalid retention was reported after %s: it waited for the database", time.Since(started))
	}
}

// On the bounded file store a page link issued before its 24h budget removed
// the records it points into answers expired, so the reader restarts at the
// first page; without the prune the same link keeps paging. The ephemeral
// memory store has no retention, so the same link keeps paging there. A
// malformed link stays invalid rather than expired on both.
func TestRetentionPruneExpiresLocalHistoryCursor(t *testing.T) {
	for _, backend := range []string{"memory", "file"} {
		for _, prune := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/prune=%t", backend, prune), func(t *testing.T) {
				provider, _ := localEvidenceTestProvider(t, backend)
				store := provider.(EvidenceStore)
				scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
				now := time.Now().UTC().Truncate(time.Second)
				oldAt := now.Add(-30 * time.Hour)
				first := evidenceTestBatch("old", "", oldAt, true, true)
				for i := 0; i < 6; i++ {
					first.Records = append(first.Records, EvidenceRecord{ID: fmt.Sprintf("old-%d", i), Type: "change", At: oldAt, ObservedAt: oldAt, Epoch: first.Epoch, Payload: json.RawMessage(`{}`)})
				}
				if err := store.CommitEvidence(t.Context(), scope, first); err != nil {
					t.Fatal(err)
				}
				query := EvidenceQuery{Since: oldAt.Add(-time.Hour), Until: now, Type: "change", Limit: 2}
				page, err := store.QueryEvidence(t.Context(), scope, query)
				if err != nil || page.Next == "" || len(page.Items) != 2 {
					t.Fatalf("page 1: items=%d next=%q err=%v", len(page.Items), page.Next, err)
				}
				// The second capture is 25h after the first when pruning, so
				// the 24h file budget removes the first capture's records;
				// otherwise it is 1h after and nothing is removed.
				laterAt := oldAt.Add(time.Hour)
				if prune {
					laterAt = oldAt.Add(25 * time.Hour)
				}
				later := evidenceTestBatch("later", "old", laterAt, true, true)
				later.Records = []EvidenceRecord{{ID: "new-0", Type: "change", At: laterAt, ObservedAt: laterAt, Epoch: later.Epoch, Payload: json.RawMessage(`{}`)}}
				if err = store.CommitEvidence(t.Context(), scope, later); err != nil {
					t.Fatal(err)
				}
				next := query
				next.Cursor = page.Next
				_, err = store.QueryEvidence(t.Context(), scope, next)
				pruned := prune && backend == "file"
				if pruned && !errors.Is(err, ErrEvidenceCursorExpired) {
					t.Fatalf("link issued before the retention prune: %v, want expired", err)
				}
				if !pruned && err != nil {
					t.Fatalf("link with no prune in between: %v, want the next page", err)
				}
				if pruned {
					fresh, err := store.QueryEvidence(t.Context(), scope, query)
					if err != nil || len(fresh.Items) != 1 || fresh.Items[0].ID != "new-0" {
						t.Fatalf("first page after the prune: %+v %v, want only the new record", fresh.Items, err)
					}
				}
				for _, malformed := range []string{"not-a-cursor", base64.RawURLEncoding.EncodeToString([]byte("{}")), base64.RawURLEncoding.EncodeToString([]byte(`{"Binding":"x"}`))} {
					bad := query
					bad.Cursor = malformed
					if _, err := store.QueryEvidence(t.Context(), scope, bad); !errors.Is(err, ErrInvalidEvidence) || errors.Is(err, ErrEvidenceCursorExpired) {
						t.Fatalf("malformed link %q: %v, want invalid and not expired", malformed, err)
					}
				}
			})
		}
	}
}
