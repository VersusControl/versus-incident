package services

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	m "github.com/VersusControl/versus-incident/pkg/models"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// TestResolveSource covers the Source-label decision for every ingress
// path: agent-originated incidents are self-describing, SNS/SQS supply a
// hint, and the plain webhook falls back to "webhook".
func TestResolveSource(t *testing.T) {
	tests := []struct {
		name    string
		content map[string]interface{}
		hint    string
		want    string
	}{
		{
			name:    "plain webhook defaults to webhook",
			content: map[string]interface{}{"title": "disk full"},
			hint:    "",
			want:    "webhook",
		},
		{
			name:    "sns hint",
			content: map[string]interface{}{"title": "disk full"},
			hint:    "sns",
			want:    "sns",
		},
		{
			name:    "sqs hint",
			content: map[string]interface{}{"title": "disk full"},
			hint:    "sqs",
			want:    "sqs",
		},
		{
			name: "agent via Source prefix wins over hint",
			content: map[string]interface{}{
				"Source": "agent:elasticsearch:prod-app",
			},
			hint: "sqs",
			want: "agent:elasticsearch:prod-app",
		},
		{
			name: "agent via PatternID with empty Source falls back to agent",
			content: map[string]interface{}{
				"PatternID": "p-123",
			},
			hint: "",
			want: "agent",
		},
		{
			name: "agent via PatternID ignores hint",
			content: map[string]interface{}{
				"PatternID": "p-123",
				"Source":    "agent:loki:billing",
			},
			hint: "sns",
			want: "agent:loki:billing",
		},
		{
			name:    "nil content with hint",
			content: nil,
			hint:    "sqs",
			want:    "sqs",
		},
		{
			name:    "nil content no hint",
			content: nil,
			hint:    "",
			want:    "webhook",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveSource(tc.content, tc.hint); got != tc.want {
				t.Fatalf("resolveSource(%v, %q) = %q, want %q", tc.content, tc.hint, got, tc.want)
			}
		})
	}
}

// TestSourceHint verifies the reserved params key is read, trimmed, and
// safely returns empty for the no-params and nil-map cases.
func TestSourceHint(t *testing.T) {
	t.Run("no params", func(t *testing.T) {
		if got := sourceHint(); got != "" {
			t.Fatalf("sourceHint() = %q, want empty", got)
		}
	})
	t.Run("nil map", func(t *testing.T) {
		if got := sourceHint(nil); got != "" {
			t.Fatalf("sourceHint(nil) = %q, want empty", got)
		}
	})
	t.Run("missing key", func(t *testing.T) {
		p := map[string]string{"slack_channel_id": "C123"}
		if got := sourceHint(&p); got != "" {
			t.Fatalf("sourceHint() = %q, want empty", got)
		}
	})
	t.Run("present key trimmed", func(t *testing.T) {
		p := map[string]string{sourceHintKey: "  sqs  "}
		if got := sourceHint(&p); got != "sqs" {
			t.Fatalf("sourceHint() = %q, want sqs", got)
		}
	})
}

// TestBuildIncidentRecord_Source asserts the persisted record's Source
// is wired from the resolver for each ingress path.
func TestBuildIncidentRecord_Source(t *testing.T) {
	cfg := &config.Config{}

	tests := []struct {
		name    string
		content map[string]interface{}
		hint    string
		want    string
	}{
		{"webhook", map[string]interface{}{"title": "t"}, "", "webhook"},
		{"sns", map[string]interface{}{"title": "t"}, "sns", "sns"},
		{"sqs", map[string]interface{}{"title": "t"}, "sqs", "sqs"},
		{
			"agent",
			map[string]interface{}{"Source": "agent:splunk:web", "PatternID": "p1"},
			"sqs",
			"agent:splunk:web",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inc := m.NewIncident("", &tc.content, false)
			rec := buildIncidentRecord(inc, cfg, tc.content, false, tc.hint)
			if rec.Source != tc.want {
				t.Fatalf("Source = %q, want %q", rec.Source, tc.want)
			}
		})
	}
}

// TestBuildIncidentRecord_Origin asserts the coarse origin classifier is
// stamped at creation: agent-originated payloads (a Source prefix or a
// PatternID) classify as ai_detect; every ingress path — the plain
// webhook and the SNS/SQS hints — classifies as webhook.
func TestBuildIncidentRecord_Origin(t *testing.T) {
	cfg := &config.Config{}

	tests := []struct {
		name    string
		content map[string]interface{}
		hint    string
		want    string
	}{
		{"plain webhook", map[string]interface{}{"title": "disk full"}, "", storage.OriginWebhook},
		{"sns hint stays webhook origin", map[string]interface{}{"title": "t"}, "sns", storage.OriginWebhook},
		{"sqs hint stays webhook origin", map[string]interface{}{"title": "t"}, "sqs", storage.OriginWebhook},
		{
			"agent via Source prefix",
			map[string]interface{}{"Source": "agent:elasticsearch:prod-app"},
			"sqs",
			storage.OriginAIDetect,
		},
		{
			"agent via PatternID",
			map[string]interface{}{"PatternID": "p-123"},
			"",
			storage.OriginAIDetect,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inc := m.NewIncident("", &tc.content, false)
			rec := buildIncidentRecord(inc, cfg, tc.content, false, tc.hint)
			if rec.Origin != tc.want {
				t.Fatalf("Origin = %q, want %q", rec.Origin, tc.want)
			}
		})
	}
}
