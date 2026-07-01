package agent

import (
	"strconv"
	"strings"
	"testing"
)

func TestMiner_ClustersSimilarMessages(t *testing.T) {
	m := NewMiner(0.4, 4, 100)

	id1, tmpl1, isNew1 := m.Cluster("connection refused to database server db-01 port 5432")
	if !isNew1 {
		t.Fatalf("first message should be new")
	}
	if id1 == "" || tmpl1 == "" {
		t.Fatalf("expected id+template, got %q %q", id1, tmpl1)
	}

	id2, _, isNew2 := m.Cluster("connection refused to database server db-02 port 5432")
	if isNew2 {
		t.Errorf("second similar message should NOT be a new cluster")
	}
	if id2 != id1 {
		t.Errorf("similar messages should share cluster id, got %s vs %s", id1, id2)
	}

	id3, _, _ := m.Cluster("connection refused to database server db-03 port 5432")
	if id3 != id1 {
		t.Errorf("third similar message should share cluster id, got %s vs %s", id1, id3)
	}

	// Wildly different message → new cluster
	id4, _, isNew4 := m.Cluster("user alice signed in successfully from web client")
	if !isNew4 || id4 == id1 {
		t.Errorf("dissimilar message should produce a new cluster")
	}
}

func TestMiner_TokenizeIgnoresRedactedAsVariable(t *testing.T) {
	m := NewMiner(0.4, 4, 100)
	id1, _, _ := m.Cluster("user <REDACTED:email> failed login from <REDACTED:ipv4>")
	id2, _, isNew := m.Cluster("user <REDACTED:email> failed login from <REDACTED:ipv4>")
	if isNew {
		t.Errorf("identical redacted messages must share a cluster")
	}
	if id1 != id2 {
		t.Errorf("expected same id, got %s vs %s", id1, id2)
	}
}

// TestMiner_Reset proves Reset forgets every learned cluster and the bucket
// tree, so a line the miner had already learned is re-discovered as new (isNew
// again) after the reset — mining truly restarts from scratch.
func TestMiner_Reset(t *testing.T) {
	m := NewMiner(0.4, 4, 100)
	msg := "connection refused to database server db-01 port 5432"

	if _, _, isNew := m.Cluster(msg); !isNew {
		t.Fatalf("first sighting should be new")
	}
	if _, _, isNew := m.Cluster(msg); isNew {
		t.Fatalf("second sighting should not be new before reset")
	}
	if n := len(m.Snapshot()); n != 1 {
		t.Fatalf("miner should hold 1 cluster before reset, got %d", n)
	}

	m.Reset()

	if n := len(m.Snapshot()); n != 0 {
		t.Fatalf("miner should hold 0 clusters after reset, got %d", n)
	}
	if _, _, isNew := m.Cluster(msg); !isNew {
		t.Fatalf("after reset the same line must be re-discovered as new")
	}
}

// TestIsVariable_TimestampsBlank proves every timestamp shape the founder can
// hit is recognised as a variable (so tokenize replaces it with <*>), while a
// near-miss set of ordinary tokens is left exactly as it was (no over-blanking).
func TestIsVariable_TimestampsBlank(t *testing.T) {
	blank := []struct{ name, tok string }{
		{"iso date", "2026-07-01"},
		{"clock hh:mm:ss", "11:21:55"},
		{"clock colon millis (founder)", "11:21:55:471"},
		{"clock dot millis", "11:21:55.471"},
		{"clock comma millis", "11:21:55,471"},
		{"iso8601 combined", "2026-07-01T11:21:55"},
		{"iso8601 frac + Z", "2026-07-01T11:21:55.471Z"},
		{"iso8601 frac + offset", "2026-07-01T11:21:55.471+07:00"},
	}
	for _, tc := range blank {
		if !isVariable(tc.tok) {
			t.Errorf("%s: isVariable(%q) = false, want true (timestamp must blank)", tc.name, tc.tok)
		}
	}

	// Near-miss: ordinary tokens keep whatever classification they had before —
	// the timestamp rules must not newly change any of them. A plain hyphenated
	// word / SCREAMING_CASE constant stays a literal; a bare number, a UUID and
	// a version-ish alnum keep their existing (pre-change) variable treatment.
	notTimestamp := []struct {
		name string
		tok  string
		want bool
	}{
		{"service name", "account-service", false},
		{"screaming constant", "ACCOUNT_NOT_FOUND", false},
		{"dotted version", "v1.2.3", true}, // alnum-with-letter, unchanged
		{"bare year", "2026", true},        // pure number, unchanged
		{"plain word", "exception", false}, // no digits
		{"bad date (2 digit year)", "26-7-1", false},
		{"bad clock (single field)", "11:21", false},
	}
	for _, tc := range notTimestamp {
		if got := isVariable(tc.tok); got != tc.want {
			t.Errorf("%s: isVariable(%q) = %v, want %v (timestamp rules over-/under-reached)", tc.name, tc.tok, got, tc.want)
		}
	}
}

// founderLine is the exact production line the founder hit. Only the timestamp
// (date + clock) varies from copy to copy; everything else is identical.
func founderLine(date, clock string) string {
	return "[ " + date + " " + clock + " ] [ ERROR ] [ account-service , requestID = , traceID = <*> , spanID = <*> ] [ ERROR ] [ <*> ] <*> - Client side exception. [ code = ACCOUNT_NOT_FOUND , message = Not found account ]"
}

// TestMiner_TimestampsCollapseToOneTemplate feeds 50 copies of the founder's
// line that differ ONLY in the timestamp and asserts they collapse into a
// SINGLE drain template — the fix for the 2000+-pattern explosion. Before the
// timestamp masking each distinct date/time routed to its own drain-tree branch
// (the timestamp tokens were literal keys), minting one pattern per line.
func TestMiner_TimestampsCollapseToOneTemplate(t *testing.T) {
	m := NewMiner(0.4, 4, 100)

	firstID := ""
	for i := 0; i < 50; i++ {
		// Vary both the date and every clock field, including the colon-millis.
		date := "2026-07-" + pad2(1+i%28)
		clock := pad2(i%24) + ":" + pad2(i%60) + ":" + pad2((i*7)%60) + ":" + strconv.Itoa(100+i)
		id, tmpl, _ := m.Cluster(founderLine(date, clock))
		if id == "" {
			t.Fatalf("line %d produced no cluster", i)
		}
		if firstID == "" {
			firstID = id
			// The mined template must carry a wildcard where the timestamp was.
			if !strings.Contains(tmpl, "<*>") {
				t.Fatalf("template has no wildcard: %q", tmpl)
			}
		} else if id != firstID {
			t.Fatalf("line %d minted a new pattern %q (want the shared %q) — timestamps not masked", i, id, firstID)
		}
	}

	if n := len(m.Snapshot()); n != 1 {
		t.Fatalf("founder line collapsed to %d templates, want exactly 1", n)
	}
}

func pad2(n int) string {
	s := strconv.Itoa(n)
	if len(s) == 1 {
		return "0" + s
	}
	return s
}
