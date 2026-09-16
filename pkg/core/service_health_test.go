package core_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
)

func TestLogHealthObservationCannotCarryRawSignalData(t *testing.T) {
	typeOf := reflect.TypeOf(core.LogHealthObservation{})
	for _, forbidden := range []string{"Message", "Samples", "Raw", "Fields", "Credentials", "Cursor"} {
		if _, ok := typeOf.FieldByName(forbidden); ok {
			t.Fatalf("log health observation exposes forbidden field %q", forbidden)
		}
	}
}

func TestHealthStatesAreCanonicalAndDistinct(t *testing.T) {
	states := []core.HealthState{core.HealthReady, core.HealthPartial, core.HealthNotConfigured, core.HealthCollecting, core.HealthNoData, core.HealthUnsupported, core.HealthError, core.HealthStale, core.HealthRestricted}
	seen := map[core.HealthState]bool{}
	for _, state := range states {
		if state == "" || seen[state] {
			t.Fatalf("invalid canonical state %q", state)
		}
		seen[state] = true
	}
}

func TestHealthProviderRequestHasOnlyNeutralBoundedInputs(t *testing.T) {
	typeOf := reflect.TypeOf(core.HealthCollectRequest{})
	for _, forbidden := range []string{"Provider", "Credentials", "Query", "Cursor", "TieBreakerField", "SearchAfter"} {
		if _, ok := typeOf.FieldByName(forbidden); ok {
			t.Fatalf("health provider request exposes adapter-private field %q", forbidden)
		}
	}
	if _, ok := typeOf.FieldByName("Budget"); !ok {
		t.Fatal("health provider request has no server budget")
	}
}

func TestHealthAssessmentPreservesAssessedZeroAndWithheldValues(t *testing.T) {
	zero := 0.0
	assessed, err := json.Marshal(core.HealthAssessment{RegressionScore: &zero})
	if err != nil {
		t.Fatal(err)
	}
	withheld, err := json.Marshal(core.HealthAssessment{ReasonCode: "insufficient_baseline"})
	if err != nil {
		t.Fatal(err)
	}
	if string(assessed) == string(withheld) || !strings.Contains(string(assessed), `"regression_score":0`) || !strings.Contains(string(withheld), `"regression_score":null`) || !strings.Contains(string(withheld), `"silent":null`) {
		t.Fatalf("assessed=%s withheld=%s", assessed, withheld)
	}
}

func TestSignalEvidenceCarriesTrustedAttribution(t *testing.T) {
	typeOf := reflect.TypeOf(core.SignalEvidence{})
	for _, required := range []string{"OrgID", "Service", "Operation", "Family", "Measure", "SourceRef", "SignalRef"} {
		if _, ok := typeOf.FieldByName(required); !ok {
			t.Fatalf("signal evidence has no %s attribution", required)
		}
	}
}
