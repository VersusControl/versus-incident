package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBaselineResultJSONPreservesNullableCurrentAndMetadata(t *testing.T) {
	result := BaselineResult{
		Availability: HealthReady,
		Found:        true,
		Truncated:    true,
		Omitted:      2,
		Records: []BaselineRecord{{
			Service: "api", Signal: "logs", Family: "log_pattern", SourceType: "catalog",
			PatternID: "pattern-1", ExpectedMean: 3, ExpectedStd: 2, CurrentValue: nil,
			Unit: "events_per_second", ObservationCount: 12, Confident: false,
			LastTrainedAt: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC), Availability: HealthReady,
			ReasonCode: "current_value_unavailable", Provenance: []string{"learned_log_pattern"},
		}},
		Coverage: []BaselineCoverage{{Family: "logs", SourceType: "catalog", Availability: HealthReady}},
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, fragment := range []string{`"current_value":null`, `"found":true`, `"truncated":true`, `"omitted_records":2`, `"confident":false`} {
		if !strings.Contains(text, fragment) {
			t.Errorf("JSON %s does not contain %s", text, fragment)
		}
	}
	if strings.Contains(text, "org_id") || strings.Contains(text, "query") || strings.Contains(text, "credential") {
		t.Fatalf("provider-only or unsafe fields leaked into JSON: %s", text)
	}
}

func TestBaselineRequestIsProviderOnly(t *testing.T) {
	request := BaselineRequest{OrgID: "trusted", Service: "api", Signal: "logs", PatternID: "pattern-1", Window: 15 * time.Minute, Limit: 50}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "{}" {
		t.Fatalf("BaselineRequest JSON = %s, want no model-visible fields", encoded)
	}
}
