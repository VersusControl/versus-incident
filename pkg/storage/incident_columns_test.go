package storage_test

// incident_columns_test.go — proves the incident persistence round-trips every
// IncidentRecord property. On Postgres each property now lives in its own
// typed column (no `data` blob); the file/memory backends serialize the whole
// struct. Running the same assertions across all three keeps them behaviorally
// identical. Postgres is gated on TEST_POSTGRES_DSN (skipped when unset).

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// fullIncident builds a record with every field populated so the round-trip
// covers all promoted columns. Times are truncated to microseconds so the
// comparison holds across Postgres (timestamptz, microsecond precision) and
// the JSON backends (nanosecond precision).
func fullIncident() *storage.IncidentRecord {
	now := time.Now().UTC().Truncate(time.Microsecond)
	acked := now.Add(-time.Minute)
	resolvedAt := now.Add(-30 * time.Second)
	return &storage.IncidentRecord{
		ID:                "inc-full",
		OrgID:             "acme",
		TeamID:            "team-sre",
		Title:             "Checkout 500s",
		Source:            "agent:detect",
		Service:           "checkout",
		Origin:            storage.OriginAIDetect,
		Resolved:          true,
		ChannelsEnabled:   []string{"slack", "email"},
		ChannelsNotified:  []string{"slack"},
		OnCallTriggered:   true,
		OnCallError:       "pagerduty timeout",
		NotifyStatus:      "partial",
		NotifyError:       "email failed",
		CreatedAt:         now.Add(-2 * time.Minute),
		AckedAt:           &acked,
		ResolvedAt:        &resolvedAt,
		Content:           map[string]interface{}{"summary": "elevated 5xx", "count": float64(42)},
		AssignedTeamID:    "team-payments",
		AssignedMemberIDs: []string{"u1", "u2", "u3"},
	}
}

// runIncidentColumnRoundTrip saves a fully-populated record and asserts every
// field survives the write→read cycle unchanged.
func runIncidentColumnRoundTrip(t *testing.T, p storage.Provider) {
	t.Helper()
	want := fullIncident()
	if err := p.SaveIncident(want); err != nil {
		t.Fatalf("SaveIncident: %v", err)
	}
	got, err := p.GetIncident(want.ID)
	if err != nil {
		t.Fatalf("GetIncident: %v", err)
	}

	// Compare scalar/pointer/slice/map fields explicitly for clear failures.
	if got.ID != want.ID ||
		got.OrgID != want.OrgID ||
		got.TeamID != want.TeamID ||
		got.Title != want.Title ||
		got.Source != want.Source ||
		got.Service != want.Service ||
		got.Origin != want.Origin ||
		got.Resolved != want.Resolved ||
		got.OnCallTriggered != want.OnCallTriggered ||
		got.OnCallError != want.OnCallError ||
		got.NotifyStatus != want.NotifyStatus ||
		got.NotifyError != want.NotifyError ||
		got.AssignedTeamID != want.AssignedTeamID {
		t.Fatalf("scalar mismatch:\n got=%+v\nwant=%+v", got, want)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
	if got.AckedAt == nil || !got.AckedAt.Equal(*want.AckedAt) {
		t.Fatalf("AckedAt = %v, want %v", got.AckedAt, want.AckedAt)
	}
	if got.ResolvedAt == nil || !got.ResolvedAt.Equal(*want.ResolvedAt) {
		t.Fatalf("ResolvedAt = %v, want %v", got.ResolvedAt, want.ResolvedAt)
	}
	if !reflect.DeepEqual(got.ChannelsEnabled, want.ChannelsEnabled) {
		t.Fatalf("ChannelsEnabled = %v, want %v", got.ChannelsEnabled, want.ChannelsEnabled)
	}
	if !reflect.DeepEqual(got.ChannelsNotified, want.ChannelsNotified) {
		t.Fatalf("ChannelsNotified = %v, want %v", got.ChannelsNotified, want.ChannelsNotified)
	}
	if !reflect.DeepEqual(got.AssignedMemberIDs, want.AssignedMemberIDs) {
		t.Fatalf("AssignedMemberIDs = %v, want %v", got.AssignedMemberIDs, want.AssignedMemberIDs)
	}
	if !reflect.DeepEqual(got.Content, want.Content) {
		t.Fatalf("Content = %v, want %v", got.Content, want.Content)
	}
}

func TestMemoryIncidentColumnRoundTrip(t *testing.T) {
	runIncidentColumnRoundTrip(t, storage.NewMemory())
}

func TestFileIncidentColumnRoundTrip(t *testing.T) {
	p, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	defer p.Close()
	runIncidentColumnRoundTrip(t, p)
}

func TestPostgresIncidentColumnRoundTrip(t *testing.T) {
	p := newTestPostgres(t) // skips when TEST_POSTGRES_DSN is unset
	runIncidentColumnRoundTrip(t, p)
}

// runUnresolvedCount seeds a mix of resolved and open incidents and asserts
// CountIncidents tallies only the UNRESOLVED ones, split per origin.
func runUnresolvedCount(t *testing.T, p storage.Provider) {
	t.Helper()
	base := time.Now().UTC().Add(-time.Hour)
	specs := []struct {
		id       string
		origin   string
		source   string
		resolved bool
	}{
		{"a", storage.OriginAIDetect, "agent", false},  // open ai
		{"b", storage.OriginAIDetect, "agent", true},   // resolved ai (excluded)
		{"c", storage.OriginWebhook, "webhook", false}, // open webhook
		{"d", storage.OriginWebhook, "sns", true},      // resolved webhook (excluded)
		{"e", "", "agent:detect", false},               // legacy → ai, open
		{"f", "", "sqs", false},                        // legacy → webhook, open
	}
	for i, s := range specs {
		rec := &storage.IncidentRecord{
			ID:        s.id,
			Origin:    s.origin,
			Source:    s.source,
			Resolved:  s.resolved,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if err := p.SaveIncident(rec); err != nil {
			t.Fatalf("SaveIncident %s: %v", s.id, err)
		}
	}
	pager, ok := p.(storage.IncidentPager)
	if !ok {
		t.Skip("backend does not implement storage.IncidentPager")
	}
	counts, err := pager.CountIncidents()
	if err != nil {
		t.Fatalf("CountIncidents: %v", err)
	}
	// Open: a (ai), c (webhook), e (ai), f (webhook) → ai=2, webhook=2, total=4.
	if counts.AIDetect != 2 || counts.Webhook != 2 || counts.Total != 4 {
		t.Fatalf("counts = %+v, want {AIDetect:2 Webhook:2 Total:4}", counts)
	}
}

func TestMemoryUnresolvedCount(t *testing.T) {
	runUnresolvedCount(t, storage.NewMemory())
}

func TestFileUnresolvedCount(t *testing.T) {
	p, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	defer p.Close()
	runUnresolvedCount(t, p)
}

func TestPostgresUnresolvedCount(t *testing.T) {
	p := newTestPostgres(t) // skips when TEST_POSTGRES_DSN is unset
	runUnresolvedCount(t, p)
}

// scriptBackfillUpdate extracts the single UPDATE statement from the operator
// migration script so the test exercises the real SQL an operator runs (rather
// than a hand-copied duplicate that could drift).
func scriptBackfillUpdate(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../scripts/postgres/migrate_incident_columns.sql")
	if err != nil {
		t.Fatalf("read migration script: %v", err)
	}
	// Strip `--` comment lines first: an inline comment may contain a `;`
	// (e.g. "extract as text; NULL when …") which would otherwise split a
	// statement mid-way.
	var code strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	for _, stmt := range strings.Split(code.String(), ";") {
		if strings.Contains(strings.ToUpper(stmt), "UPDATE VS_INCIDENTS SET") {
			return stmt
		}
	}
	t.Fatal("no UPDATE statement found in migration script")
	return ""
}

// TestPostgresManualBackfillFromData proves the operator migration script's
// backfill copies every property out of a legacy `data` blob into the promoted
// columns, including the effective-origin derivation. It seeds a legacy row
// (data populated, columns empty), runs the script's UPDATE, then reads the row
// back through GetIncident. Gated on TEST_POSTGRES_DSN.
func TestPostgresManualBackfillFromData(t *testing.T) {
	p := newTestPostgres(t) // skips when TEST_POSTGRES_DSN is unset
	accessor, ok := p.(storage.SQLAccessor)
	if !ok {
		t.Fatal("postgres backend must implement storage.SQLAccessor")
	}
	db := accessor.DB()

	// Insert a pre-upgrade row: only id/created_at and the whole-record `data`
	// blob are set; the promoted columns are left at their defaults/NULL. The
	// blob has no explicit origin, so the backfill must derive ai_detect from
	// the "agent:detect" source.
	if _, err := db.Exec(`
		INSERT INTO vs_incidents (id, created_at, data) VALUES (
			'legacy-1',
			now(),
			jsonb_build_object(
				'id',      'legacy-1',
				'title',   'old incident',
				'source',  'agent:detect',
				'service', 'checkout',
				'resolved', true,
				'resolved_at', to_jsonb(now()),
				'oncall_triggered', true,
				'notify_status', 'sent',
				'channels_enabled', jsonb_build_array('slack', 'email'),
				'assigned_member_ids', jsonb_build_array('u1', 'u2'),
				'content', jsonb_build_object('summary', 'legacy body')
			)
		)`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	if _, err := db.Exec(scriptBackfillUpdate(t)); err != nil {
		t.Fatalf("run backfill UPDATE: %v", err)
	}

	got, err := p.GetIncident("legacy-1")
	if err != nil {
		t.Fatalf("GetIncident after backfill: %v", err)
	}
	if got.Title != "old incident" || got.Source != "agent:detect" || got.Service != "checkout" {
		t.Fatalf("scalar backfill mismatch: %+v", got)
	}
	if got.Origin != storage.OriginAIDetect {
		t.Fatalf("origin = %q, want %q (derived from source)", got.Origin, storage.OriginAIDetect)
	}
	if !got.Resolved {
		t.Fatal("Resolved should be true after backfill")
	}
	if got.ResolvedAt == nil {
		t.Fatal("ResolvedAt should be set after backfill")
	}
	if !got.OnCallTriggered {
		t.Fatal("OnCallTriggered should be true after backfill")
	}
	if got.NotifyStatus != "sent" {
		t.Fatalf("NotifyStatus = %q, want sent", got.NotifyStatus)
	}
	if !reflect.DeepEqual(got.ChannelsEnabled, []string{"slack", "email"}) {
		t.Fatalf("ChannelsEnabled = %v, want [slack email]", got.ChannelsEnabled)
	}
	if !reflect.DeepEqual(got.AssignedMemberIDs, []string{"u1", "u2"}) {
		t.Fatalf("AssignedMemberIDs = %v, want [u1 u2]", got.AssignedMemberIDs)
	}
	if !reflect.DeepEqual(got.Content, map[string]interface{}{"summary": "legacy body"}) {
		t.Fatalf("Content = %v, want {summary: legacy body}", got.Content)
	}
}
