package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// Only UTF-16 surrogate escapes, U+D800 to U+DFFF, are rewritten. Text from
// U+D000 to U+D7FF, such as Hangul, keeps its exact bytes, so its version hash
// does not change and saving it again appends nothing.
func TestEvidenceNormalizationKeepsHangulEscapesByteForByte(t *testing.T) {
	// Not parallel: it writes to the shared test database, whose journal
	// sequence other tests compare exactly.
	for _, payload := range []string{
		`{"message":"\uD55C\uAD6D\uC5B4"}`,
		`{"message":"\ud55c\ud7a3\ud000"}`,
		`{"message":"plain text \u00e9 \u4e2d"}`,
	} {
		if got := normalizeEvidencePayload(json.RawMessage(payload)); !bytes.Equal(got, []byte(payload)) {
			t.Fatalf("payload %s was rewritten to %s", payload, got)
		}
	}
	for _, payload := range []string{`{"message":"\ud800"}`, `{"message":"\uDFFF"}`, `{"message":"\uD83D\uDE00"}`} {
		if got := normalizeEvidencePayload(json.RawMessage(payload)); bytes.Equal(got, []byte(payload)) {
			t.Fatalf("surrogate escape payload %s was not normalized", payload)
		}
	}
	for name, open := range gapsApartBackends(t) {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			scope := EvidenceScope{OrgID: "default", ClusterID: fmt.Sprintf("hangul-%s-%d", name, time.Now().UnixNano()), ClusterUID: "physical"}
			at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
			record := worstChange("change:hangul", "1", at, at, 0)
			record.Payload = json.RawMessage(`{"cluster":"worst","type":"created","value":"\uD55C\uAD6D\uC5B4"}`)
			before := evidenceVersion(record)
			normalized := normalizeEvidenceBatch(EvidenceBatch{Records: []EvidenceRecord{record}}).Records[0]
			if after := evidenceVersion(normalized); after != before {
				t.Fatalf("normalization changed the version of a Hangul record: %s -> %s", before, after)
			}
			for cycle := 0; cycle < 3; cycle++ {
				observed := at.Add(time.Duration(cycle) * time.Minute)
				again := record
				again.ObservedAt = observed
				if err := worstCommit(t.Context(), store, scope, fmt.Sprintf("cycle-%d", cycle), observed, []EvidenceRecord{again}); err != nil {
					t.Fatal(err)
				}
			}
			page, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Type: "change", Since: at.Add(-time.Hour), Until: at.Add(time.Hour), Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if page.Total != 1 || len(page.Items) != 1 {
				t.Fatalf("an unchanged Hangul record saved three times listed %d rows", page.Total)
			}
			var value struct{ Value string }
			if err := json.Unmarshal(page.Items[0].Payload, &value); err != nil || value.Value != "\uD55C\uAD6D\uC5B4" {
				t.Fatalf("saved Hangul text %q (%v)", value.Value, err)
			}
			if p, ok := store.(*postgresProvider); ok {
				if counts := worstCounts(t, p, scope); counts.rows != 1 {
					t.Fatalf("an unchanged Hangul record appended %d journal rows", counts.rows)
				}
			}
		})
	}
}
