package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const issueAccount = "123400000003"

func issueID(owner string) string {
	sum := sha256.Sum256([]byte("test-rule\x00" + owner))
	return hex.EncodeToString(sum[:])
}

func issueRecord(owner, state string, version int64, at time.Time) AWSIssueRecord {
	record := AWSIssueRecord{
		ID: issueID(owner), Rule: "test-rule", Kind: "security", Severity: "high", Tier: "act_now", State: state,
		Confirm: "confirmed", AccountID: issueAccount, Region: "eu-west-1", Owner: owner, Title: "bucket is public",
		FirstSeen: at, OpenedAt: at, Version: version, UpdatedAt: at, Payload: []byte(`{"owner":"` + owner + `"}`),
	}
	if state == AWSIssueStateRejected {
		record.RejectedAt = at
	}
	if state == "resolved" {
		record.ResolvedAt = at
	}
	return record
}

// The issue store behaves the same on every backend: each write is a
// compare-and-swap on the version, a batch is all or nothing, reads merge
// the read scope by issue ID with the write organization first, and the
// retention sweep never deletes a rejected issue or its events.
func TestTablesParityAWSIssues(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		store, ok := AWSIssuesOf(backend.provider)
		if !ok {
			t.Fatalf("%s provider has no issue store", backend.name)
		}
		ctx := context.Background()
		archive := tenancy.OrgScope{Write: orgs.def}
		licensed := tenancy.OrgScope{Write: orgs.ent, Read: []string{orgs.ent, orgs.def}}
		at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)

		// A new issue: read as absent, written at version 1, with its event.
		bucket := issueRecord("arn:aws:s3:::public-on-purpose", "open", 1, at)
		mustDo(t, store.WriteAWSIssues(ctx, archive, AWSIssueBatch{AccountID: issueAccount, Baselines: []AWSIssueBaseline{
			{AccountID: issueAccount, Rule: "test-rule", Region: "eu-west-1", FirstAssessedAt: at},
		}, Writes: []AWSIssueWrite{{
			Issue: bucket, Events: []AWSIssueEvent{{At: at, Kind: AWSIssueEventOpened, Actor: "evaluator", Detail: "existed when first read"}},
		}}}))
		// A second writer that also saw it absent loses.
		if err := store.WriteAWSIssues(ctx, archive, AWSIssueBatch{AccountID: issueAccount, Writes: []AWSIssueWrite{{Issue: bucket}}}); !errors.Is(err, ErrAWSIssueConflict) {
			t.Fatalf("second open = %v, want a conflict", err)
		}
		// A user rejects it at version 2.
		rejected := issueRecord("arn:aws:s3:::public-on-purpose", AWSIssueStateRejected, 2, at.Add(time.Minute))
		rejected.ReviewOn = "2027-01-15"
		mustDo(t, store.WriteAWSIssues(ctx, archive, AWSIssueBatch{AccountID: issueAccount, Writes: []AWSIssueWrite{{
			Issue: rejected, Read: 1, ReadOrg: orgs.def,
			Events: []AWSIssueEvent{{At: at.Add(time.Minute), Kind: AWSIssueEventRejected, Actor: "ana", Detail: "public on purpose: it serves the docs site"}},
		}}}))
		// An evaluator that read version 1 cannot write open over it, and
		// its batch leaves the other issue it carried unwritten.
		stale := issueRecord("arn:aws:s3:::public-on-purpose", "open", 2, at.Add(2*time.Minute))
		other := issueRecord("arn:aws:s3:::other", "open", 1, at.Add(2*time.Minute))
		if err := store.WriteAWSIssues(ctx, archive, AWSIssueBatch{AccountID: issueAccount, Writes: []AWSIssueWrite{
			{Issue: other}, {Issue: stale, Read: 1, ReadOrg: orgs.def},
		}}); !errors.Is(err, ErrAWSIssueConflict) {
			t.Fatalf("stale evaluator write = %v, want a conflict", err)
		}
		archived := must(store.AWSIssues(ctx, archive, AWSIssueQuery{}))
		if len(archived) != 1 || archived[0].State != AWSIssueStateRejected || archived[0].Version != 2 || archived[0].ReviewOn != "2027-01-15" {
			t.Fatalf("after the conflicting batch = %+v", archived)
		}
		record("archive issues", archived)

		// After a licensed upgrade the licensed scope reads the archived
		// rejection by ID, and its first write of the issue copies it into
		// the licensed organization from the version it read.
		read := must(store.AWSIssues(ctx, licensed, AWSIssueQuery{IDs: []string{bucket.ID}}))
		if len(read) != 1 || read[0].Org != orgs.def || read[0].State != AWSIssueStateRejected {
			t.Fatalf("licensed read of the archived rejection = %+v", read)
		}
		if err := store.WriteAWSIssues(ctx, licensed, AWSIssueBatch{AccountID: issueAccount, Writes: []AWSIssueWrite{{Issue: bucket}}}); !errors.Is(err, ErrAWSIssueConflict) {
			t.Fatalf("licensed open of an archived issue = %v, want a conflict", err)
		}
		kept := issueRecord("arn:aws:s3:::public-on-purpose", AWSIssueStateRejected, 3, at.Add(3*time.Minute))
		kept.ReviewOn = "2027-01-15"
		mustDo(t, store.WriteAWSIssues(ctx, licensed, AWSIssueBatch{AccountID: issueAccount, Baselines: []AWSIssueBaseline{
			{AccountID: issueAccount, Rule: "test-rule", Region: "eu-west-1", FirstAssessedAt: at.Add(3 * time.Minute)},
			{AccountID: issueAccount, Rule: "new-rule", FirstAssessedAt: at.Add(3 * time.Minute)},
		}, Writes: []AWSIssueWrite{{
			Issue: kept, Read: 2, ReadOrg: orgs.def,
			Events: []AWSIssueEvent{{At: at.Add(3 * time.Minute), Kind: AWSIssueEventReread, Actor: "evaluator", Detail: "read again: still found, still rejected"}},
		}}}))
		if err := store.WriteAWSIssues(ctx, licensed, AWSIssueBatch{AccountID: issueAccount, Writes: []AWSIssueWrite{{Issue: kept, Read: 2, ReadOrg: orgs.def}}}); !errors.Is(err, ErrAWSIssueConflict) {
			t.Fatalf("second copy from the archive = %v, want a conflict", err)
		}
		merged := must(store.AWSIssues(ctx, licensed, AWSIssueQuery{}))
		if len(merged) != 1 || merged[0].Org != orgs.ent || merged[0].Version != 3 {
			t.Fatalf("licensed read after the copy = %+v", merged)
		}
		record("licensed issues", merged)
		if open := must(store.AWSIssues(ctx, licensed, AWSIssueQuery{State: "open"})); len(open) != 0 {
			t.Fatalf("an older copy matched the open filter: %+v", open)
		}
		events := must(store.AWSIssueEvents(ctx, licensed, bucket.ID))
		if len(events) != 3 || events[1].Kind != AWSIssueEventRejected || events[1].Detail != "public on purpose: it serves the docs site" {
			t.Fatalf("merged events = %+v", events)
		}
		record("events", events)
		// Baselines merge across the read scope with the latest time per
		// key: a licensed baseline taken after a reset wins over the
		// archive's, and a rule first assessed after the upgrade has its
		// own. A reset is a row with no rule.
		baselines := must(store.AWSIssueBaselines(ctx, licensed, issueAccount))
		if len(baselines) != 2 || baselines[0].Rule != "new-rule" || baselines[0].Region != "" ||
			baselines[1].Rule != "test-rule" || !baselines[1].FirstAssessedAt.Equal(at.Add(3*time.Minute)) {
			t.Fatalf("baselines = %+v", baselines)
		}
		record("baselines", baselines)
		if archived := must(store.AWSIssueBaselines(ctx, archive, "")); len(archived) != 1 || !archived[0].FirstAssessedAt.Equal(at) {
			t.Fatalf("archive baselines = %+v", archived)
		}
		mustDo(t, store.WriteAWSIssues(ctx, licensed, AWSIssueBatch{AccountID: issueAccount, Baselines: []AWSIssueBaseline{
			{AccountID: issueAccount, Region: "eu-west-1", FirstAssessedAt: at.Add(4 * time.Minute)},
			{AccountID: issueAccount, Rule: "new-rule", FirstAssessedAt: at},
		}}))
		baselines = must(store.AWSIssueBaselines(ctx, licensed, issueAccount))
		if len(baselines) != 3 || baselines[0].Rule != "" || baselines[0].Region != "eu-west-1" ||
			!baselines[1].FirstAssessedAt.Equal(at.Add(3*time.Minute)) {
			t.Fatalf("after a reset and an older rewrite = %+v", baselines)
		}
		record("baselines after a reset", baselines)

		// Retention: an old resolved issue goes with its events; the
		// rejected issue, an acknowledged one opened long ago, and every
		// event of either stay, however old.
		gone := issueRecord("arn:aws:elasticloadbalancing:eu-west-1:123400000003:targetgroup/web/1", "resolved", 1, at)
		mustDo(t, store.WriteAWSIssues(ctx, licensed, AWSIssueBatch{AccountID: issueAccount, Writes: []AWSIssueWrite{{
			Issue: gone, Events: []AWSIssueEvent{{At: at, Kind: AWSIssueEventResolved, Actor: "evaluator", Detail: "not found in a complete read"}},
		}}}))
		seen := issueRecord("arn:aws:elasticloadbalancing:eu-west-1:123400000003:targetgroup/api/2", "acknowledged", 1, at)
		mustDo(t, store.WriteAWSIssues(ctx, licensed, AWSIssueBatch{AccountID: issueAccount, Writes: []AWSIssueWrite{{
			Issue: seen, Events: []AWSIssueEvent{
				{At: at, Kind: AWSIssueEventOpened, Actor: "evaluator", Detail: "found by rule test-rule"},
				{At: at, Kind: AWSIssueEventAcknowledged, Actor: "ana", Detail: "acknowledged"},
			},
		}}}))
		removed := must(store.PruneAWSIssues(ctx, licensed, at.Add(181*24*time.Hour)))
		if removed != 2 {
			t.Fatalf("pruned %d rows, want the resolved issue and its event", removed)
		}
		record("pruned", removed)
		left := must(store.AWSIssues(ctx, licensed, AWSIssueQuery{}))
		if len(left) != 2 {
			t.Fatalf("after retention = %+v", left)
		}
		if events := must(store.AWSIssueEvents(ctx, licensed, seen.ID)); len(events) != 2 {
			t.Fatalf("retention removed events of an acknowledged issue: %+v", events)
		}
		if events := must(store.AWSIssueEvents(ctx, licensed, bucket.ID)); len(events) != 3 {
			t.Fatalf("retention removed events of a rejected issue: %+v", events)
		}

		for name, bad := range map[string]AWSIssueRecord{
			"id":       func() AWSIssueRecord { r := issueRecord("x", "open", 1, at); r.ID = "abc"; return r }(),
			"state":    func() AWSIssueRecord { r := issueRecord("x", "open", 1, at); r.State = "accepted"; return r }(),
			"severity": func() AWSIssueRecord { r := issueRecord("x", "open", 1, at); r.Severity = "urgent"; return r }(),
			"rejected": func() AWSIssueRecord { r := issueRecord("x", "open", 1, at); r.State = AWSIssueStateRejected; return r }(),
			"review": func() AWSIssueRecord {
				r := issueRecord("x", AWSIssueStateRejected, 1, at)
				r.ReviewOn = "2027-02-30"
				return r
			}(),
			"account": func() AWSIssueRecord { r := issueRecord("x", "open", 1, at); r.AccountID = "12"; return r }(),
			"payload": func() AWSIssueRecord { r := issueRecord("x", "open", 1, at); r.Payload = []byte("{"); return r }(),
		} {
			account := bad.AccountID
			if err := store.WriteAWSIssues(ctx, licensed, AWSIssueBatch{AccountID: account, Writes: []AWSIssueWrite{{Issue: bad}}}); !errors.Is(err, ErrInvalidAWSIssue) {
				t.Fatalf("%s: write = %v, want invalid", name, err)
			}
		}
		badEvent := issueRecord("arn:aws:s3:::reason", "open", 1, at)
		if err := store.WriteAWSIssues(ctx, licensed, AWSIssueBatch{AccountID: issueAccount, Writes: []AWSIssueWrite{{
			Issue: badEvent, Events: []AWSIssueEvent{{At: at, Kind: AWSIssueEventRejected, Actor: "ana", Detail: "no"}},
		}}}); !errors.Is(err, ErrInvalidAWSIssueReason) {
			t.Fatalf("a two-character reason was stored: %v", err)
		}
	})
}

// A rejection reason is user input shown in the UI, the PDF, alerts and
// agent tools. It is stored as plain text of 3 to 500 characters, with
// secrets redacted and no control or hidden characters.
func TestCleanAWSIssueReason(t *testing.T) {
	for _, accepted := range []struct{ in, want string }{
		{"  public on purpose  ", "public on purpose"},
		{"abc", "abc"},
		{strings.Repeat("é", 500), strings.Repeat("é", 500)},
		{"line one\r\nline two", "line one\nline two"},
		{"<script>alert(1)</script>", "<script>alert(1)</script>"},
		{"see https://user:pass@wiki.example.com/page?token=abc", "see https://wiki.example.com"},
		{"pairing with 👩\u200d💻 and 🏳\ufe0f\u200d🌈 on it", "pairing with 👩\u200d💻 and 🏳\ufe0f\u200d🌈 on it"},
		{"text style ☎\ufe0e and emoji style ☎\ufe0f", "text style ☎\ufe0e and emoji style ☎\ufe0f"},
	} {
		got, err := CleanAWSIssueReason(accepted.in)
		if err != nil || got != accepted.want {
			t.Fatalf("CleanAWSIssueReason(%q) = %q, %v; want %q", accepted.in, got, err, accepted.want)
		}
	}
	for _, refused := range []string{
		"", "ab", "   ab   ", strings.Repeat("a", 501),
		"bell\a in it", "tab\tin it", "hidden​space", "right‮to left", "nul\x00byte", "bad \xff utf8",
		"line separator",
		"left\u202aembedding", "right\u202bembedding", "pop\u202cformat", "left\u202doverride",
		"isolate\u2066left", "isolate\u2067right", "isolate\u2068first", "pop\u2069isolate",
		"mark\u200eleft", "mark\u200fright", "zero\u200cnon-joiner", "word\u2060joiner", "byte order\ufeffmark",
	} {
		if got, err := CleanAWSIssueReason(refused); !errors.Is(err, ErrInvalidAWSIssueReason) {
			t.Fatalf("CleanAWSIssueReason(%q) = %q, %v; want refused", refused, got, err)
		}
	}
}

// The issue migration is schema only, and its tables refuse a state, a
// kind or an event outside the life cycle.
func TestAWSIssuesMigrationSchema(t *testing.T) {
	text, err := os.ReadFile("migrations/044_aws_issues.sql")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?i)\b(INSERT|UPDATE|DELETE)\b`).Match(stripSQLComments(string(text))) {
		t.Fatalf("migration carries a data statement:\n%s", text)
	}
	orgs := newTablesOrgs()
	for _, backend := range tablesBackends(t) {
		if backend.raw == nil {
			continue
		}
		t.Run(backend.name, func(t *testing.T) {
			insert := func(state string) error {
				_, err := backend.raw.Exec(`INSERT INTO aws_issues (org_id, id, rule, kind, severity, tier, state, confirm, account_id, owner,
 first_seen, opened_at, version, updated_at) VALUES ('` + orgs.ent + `', '` + issueID(state) + `', 'test-rule', 'security', 'high', 'act_now', '` + state + `',
 'confirmed', '123400000003', 'arn:aws:s3:::b', '2026-11-25T09:00:00Z', '2026-11-25T09:00:00Z', 1, '2026-11-25T09:00:00Z')`)
				return err
			}
			if err := insert("open"); err != nil {
				t.Fatal(err)
			}
			if err := insert("accepted"); err == nil {
				t.Fatal("a state outside the life cycle was stored")
			}
			if _, err := backend.raw.Exec(`INSERT INTO aws_issue_events (org_id, issue_id, seq, at, kind) VALUES ('` + orgs.ent + `', 'x', 1, '2026-11-25T09:00:00Z', 'expired')`); err == nil {
				t.Fatal("an event kind outside the life cycle was stored")
			}
			if backend.postgres {
				var indexes int
				if err := backend.raw.QueryRow(`SELECT count(*) FROM pg_indexes WHERE tablename = 'aws_issues'
 AND indexname IN ('aws_issues_state_severity', 'aws_issues_account', 'aws_issues_group', 'aws_issues_review_on')`).Scan(&indexes); err != nil {
					t.Fatal(err)
				}
				if indexes != 4 {
					t.Fatalf("issue indexes = %d, want 4", indexes)
				}
			}
		})
	}
}

// Secrets people paste into a reason are redacted before it is stored:
// GitHub and Slack tokens, a bearer JWT, a PEM block, a prefixed password
// assignment and a password told in words. Ordinary prose is kept as
// typed, and a cleaned reason cleans to itself, as the event check needs.
func TestCleanAWSIssueReasonRedactsPastedSecrets(t *testing.T) {
	for name, c := range map[string]struct{ in, secret string }{
		"github token":        {"token ghp_" + "0123456789abcdefghijklmnopqrstuvwxyzAB on purpose", "ghp_0123456789"},
		"github fine-grained": {"github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz on purpose", "github_pat_11ABC"},
		"slack token":         {"xoxb-" + "123456789012-1234567890123-abcdefghijklmnopqrstuvwx on purpose", "xoxb-1234"},
		"bearer jwt":          {"Authorization: Bearer eyJ" + "hbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghijklmnopqrstuvwxyz012345", "eyJhbGci"},
		"pem block":           {"-----BEGIN " + "RSA PRIVATE KEY----- MIIEowIBAAKCAQEA1234567890abcdefghijklmnop -----END RSA PRIVATE KEY-----", "MIIEowIBAAKCAQEA"},
		"pem block, lines":    {"kept:\n-----BEGIN " + "PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\n-----END PRIVATE KEY-----\nend", "MIIEvQIBADAN"},
		"pem cut off":         {"kept: -----BEGIN " + "OPENSSH PRIVATE KEY----- b3BlbnNzaC1rZXktdjEAAAAA", "b3BlbnNzaC1r"},
		"password assignment": {"DB_PASSWORD=hunter2-prod-2026 on purpose", "hunter2"},
		"nested name":         {"set APP_DB_PASSWORD: hunter2-prod-2026 for now", "hunter2"},
		"password in words":   {"the db password is hunter2-prod-2026 on purpose", "hunter2"},
	} {
		got, err := CleanAWSIssueReason(c.in)
		if err != nil {
			t.Fatalf("%s: CleanAWSIssueReason(%q) = %v", name, c.in, err)
		}
		if strings.Contains(got, c.secret) || !strings.Contains(got, "REDACTED") {
			t.Fatalf("%s: stored %q; want %q redacted", name, got, c.secret)
		}
		if again, err := CleanAWSIssueReason(got); err != nil || again != got {
			t.Fatalf("%s: a cleaned reason cleans to %q, %v; want itself", name, again, err)
		}
	}
	for _, prose := range []string{
		"public on purpose: it serves the docs site",
		"password policy reviewed with security, accepted until the migration",
		"token bucket limits cover this endpoint",
		"the GitHub runner needs this open; see ticket OPS-1234",
		"secret rotation is handled by the vault team",
		"PEM files are not stored on this host",
		"approved by the platform team on 2026-11-20",
	} {
		if got, err := CleanAWSIssueReason(prose); err != nil || got != prose {
			t.Fatalf("CleanAWSIssueReason(%q) = %q, %v; want it kept as typed", prose, got, err)
		}
	}
}
