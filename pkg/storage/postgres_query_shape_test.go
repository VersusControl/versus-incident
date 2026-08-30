package storage

import (
	"strings"
	"testing"
)

func TestEscapeLikePatternTreatsMetacharactersLiterally(t *testing.T) {
	if got, want := escapeLikePattern(`cpu_%\host`), `cpu\_\%\\host`; got != want {
		t.Fatalf("escapeLikePattern = %q, want %q", got, want)
	}
}

func TestAnalysisSearchSQLUsesIndexedIncidentIDAndServiceLabel(t *testing.T) {
	query := analysisSearchFilteredSQL()
	for _, required := range []string{"i.id = a.incident_id", "a.incident_id = $2", "i.service = $3", "ESCAPE '\\'"} {
		if !strings.Contains(query, required) {
			t.Fatalf("analysis search SQL missing %q:\n%s", required, query)
		}
	}
	if strings.Contains(query, "a.data->>'incident_id'") || strings.Contains(query, "i.content") || strings.Contains(query, "LOWER(i.service)") {
		t.Fatalf("analysis search SQL bypasses indexed incident_id:\n%s", query)
	}
}
