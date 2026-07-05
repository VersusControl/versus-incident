package storage

import "testing"

// TestOriginFromSource covers the back-compat derivation used for records
// persisted before the Origin field existed: agent-emitted incidents carry
// an "agent" / "agent:<...>" Source and classify as ai_detect; every other
// Source (inbound ingestion) classifies as webhook.
func TestOriginFromSource(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{"empty defaults to webhook", "", OriginWebhook},
		{"plain webhook", "webhook", OriginWebhook},
		{"sns transport is inbound", "sns", OriginWebhook},
		{"sqs transport is inbound", "sqs", OriginWebhook},
		{"bare agent", "agent", OriginAIDetect},
		{"agent with source suffix", "agent:elasticsearch:prod-app", OriginAIDetect},
		{"whitespace agent still classifies", "  agent:loki:billing  ", OriginAIDetect},
		{"lookalike is not agent", "agenting", OriginWebhook},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := OriginFromSource(tc.source); got != tc.want {
				t.Fatalf("OriginFromSource(%q) = %q, want %q", tc.source, got, tc.want)
			}
		})
	}
}

// TestEffectiveOrigin proves the explicit Origin wins when set, and that a
// legacy record with no Origin is classified from its Source rather than
// dropped into an empty bucket.
func TestEffectiveOrigin(t *testing.T) {
	tests := []struct {
		name string
		rec  IncidentRecord
		want string
	}{
		{
			name: "explicit ai_detect wins",
			rec:  IncidentRecord{Origin: OriginAIDetect, Source: "webhook"},
			want: OriginAIDetect,
		},
		{
			name: "explicit webhook wins",
			rec:  IncidentRecord{Origin: OriginWebhook, Source: "agent:loki:x"},
			want: OriginWebhook,
		},
		{
			name: "legacy agent record derives ai_detect",
			rec:  IncidentRecord{Source: "agent:elasticsearch:prod-app"},
			want: OriginAIDetect,
		},
		{
			name: "legacy webhook record derives webhook",
			rec:  IncidentRecord{Source: "sns"},
			want: OriginWebhook,
		},
		{
			name: "legacy record with no source defaults to webhook",
			rec:  IncidentRecord{},
			want: OriginWebhook,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rec.EffectiveOrigin(); got != tc.want {
				t.Fatalf("EffectiveOrigin() = %q, want %q", got, tc.want)
			}
		})
	}
}
