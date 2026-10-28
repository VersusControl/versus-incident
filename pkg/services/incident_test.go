package services

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	m "github.com/VersusControl/versus-incident/pkg/models"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/utils"
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

// TestBuildIncidentRecord_Service asserts the persisted Service column is
// derived through the shared key set that the incident detail and report read,
// so the incidents LIST (which renders the column) agrees with the DETAIL. The
// regression was the one-word "ServiceName" key being omitted, leaving the
// column blank while the detail showed a service.
func TestBuildIncidentRecord_Service(t *testing.T) {
	cfg := &config.Config{}

	tests := []struct {
		name    string
		content map[string]interface{}
		want    string
	}{
		{"one-word ServiceName only (the reported bug)", map[string]interface{}{"ServiceName": "checkout"}, "checkout"},
		{"lowercased servicename", map[string]interface{}{"servicename": "billing"}, "billing"},
		{"agent sets ServiceName and Service", map[string]interface{}{"ServiceName": "payments", "Service": "payments"}, "payments"},
		{"underscored service_name", map[string]interface{}{"service_name": "orders"}, "orders"},
		{"exact Service", map[string]interface{}{"Service": "web"}, "web"},
		{"lower service", map[string]interface{}{"service": "api"}, "api"},
		{"app fallback", map[string]interface{}{"app": "worker"}, "worker"},
		{"component fallback", map[string]interface{}{"component": "scheduler"}, "scheduler"},
		{"no service keys stays blank", map[string]interface{}{"title": "disk full"}, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inc := m.NewIncident("", &tc.content, false)
			rec := buildIncidentRecord(inc, cfg, tc.content, false, "")
			if rec.Service != tc.want {
				t.Fatalf("Service = %q, want %q", rec.Service, tc.want)
			}
		})
	}
}

// TestBuildIncidentRecord_Title asserts the persisted Title is derived through
// the shared title key set. The regression was a CloudWatch alarm, whose title
// key is AlarmName, persisting a blank or unrelated title.
func TestBuildIncidentRecord_Title(t *testing.T) {
	cfg := &config.Config{}

	tests := []struct {
		name    string
		content map[string]interface{}
		want    string
	}{
		{"cloudwatch AlarmName (the reported bug)", map[string]interface{}{
			"AlarmName":      "checkout-5xx",
			"NewStateValue":  "ALARM",
			"NewStateReason": "threshold crossed",
		}, "checkout-5xx"},
		{"title", map[string]interface{}{"title": "disk full"}, "disk full"},
		{"alertname", map[string]interface{}{"alertname": "HighErrorRate"}, "HighErrorRate"},
		{"summary", map[string]interface{}{"summary": "cpu saturated"}, "cpu saturated"},
		{"subject", map[string]interface{}{"subject": "nightly job failed"}, "nightly job failed"},
		{"name", map[string]interface{}{"name": "checkout-latency"}, "checkout-latency"},
		{"nested alertmanager labels", map[string]interface{}{
			"labels": map[string]interface{}{"alertname": "PostgresqlDown"},
		}, "PostgresqlDown"},
		{"no title keys stays blank", map[string]interface{}{"ServiceName": "checkout"}, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inc := m.NewIncident("", &tc.content, false)
			rec := buildIncidentRecord(inc, cfg, tc.content, false, "")
			if rec.Title != tc.want {
				t.Fatalf("Title = %q, want %q", rec.Title, tc.want)
			}
		})
	}
}

// TestServiceLabel_ListEqualsDetail proves the list value (ServiceLabel) and
// the detail value agree. The detail derives its service from content via
// pickString(ServiceName, Service, service); ServiceLabel returns the durable
// column when present and otherwise falls back to the same content, so a fresh
// record and a LEGACY row (blank column, service only in content) both display
// what the detail shows.
func TestServiceLabel_ListEqualsDetail(t *testing.T) {
	// detailService mirrors ui IncidentDetailPage's
	// pickString(content, "ServiceName", "Service", "service").
	detailService := func(content map[string]interface{}) string {
		return utils.PayloadString(content, "ServiceName", "Service", "service")
	}

	cfg := &config.Config{}
	contents := []map[string]interface{}{
		{"ServiceName": "checkout"}, // agent / detail key
		{"ServiceName": "payments", "Service": "payments"},
		{"Service": "web"},
		{"service": "api"},
	}
	for _, content := range contents {
		inc := m.NewIncident("", &content, false)
		rec := buildIncidentRecord(inc, cfg, content, false, "")
		if ServiceLabel(rec) != detailService(content) {
			t.Fatalf("fresh record: list %q != detail %q (content %v)", ServiceLabel(rec), detailService(content), content)
		}

		// Legacy row: same content, but the column was persisted blank before
		// the fix. ServiceLabel must fall back to content and match the detail.
		legacy := &storage.IncidentRecord{Content: content}
		if ServiceLabel(legacy) != detailService(content) {
			t.Fatalf("legacy row: list %q != detail %q (content %v)", ServiceLabel(legacy), detailService(content), content)
		}
	}

	// A durable column always wins over content for the list, so sorting /
	// filtering that keys on the stored column is never regressed.
	pinned := &storage.IncidentRecord{Service: "stored-svc", Content: map[string]interface{}{"ServiceName": "content-svc"}}
	if got := ServiceLabel(pinned); got != "stored-svc" {
		t.Fatalf("stored column must win: ServiceLabel = %q, want %q", got, "stored-svc")
	}
}
