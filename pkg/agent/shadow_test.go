package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShadowLog_RecordCoalescesByPattern(t *testing.T) {
	s, err := LoadShadowLog("", 0)
	if err != nil {
		t.Fatalf("LoadShadowLog: %v", err)
	}

	s.Record("src-a", "p1", "tmpl one", "msg one", "oom", "unknown", 3)
	s.Record("src-a", "p1", "tmpl one v2", "msg two", "oom", "unknown", 2)
	s.Record("src-a", "p2", "tmpl two", "other", "", "unknown", 1)

	if got := s.Len(); got != 2 {
		t.Fatalf("Len=%d want 2 (p1 should coalesce)", got)
	}

	all := s.All()
	var p1 *ShadowEvent
	for _, e := range all {
		if e.PatternID == "p1" {
			p1 = e
			break
		}
	}
	if p1 == nil {
		t.Fatal("p1 not found")
	}
	if p1.Count != 5 {
		t.Errorf("Count=%d want 5", p1.Count)
	}
	if p1.Occurrences != 2 {
		t.Errorf("Occurrences=%d want 2", p1.Occurrences)
	}
	if p1.Template != "tmpl one v2" {
		t.Errorf("Template not refreshed: %q", p1.Template)
	}
	if p1.SampleMessage != "msg two" {
		t.Errorf("SampleMessage not refreshed: %q", p1.SampleMessage)
	}
}

func TestShadowLog_RecordEmptyPatternIgnored(t *testing.T) {
	s, _ := LoadShadowLog("", 0)
	s.Record("src", "", "tmpl", "msg", "", "unknown", 1)
	if s.Len() != 0 {
		t.Fatalf("expected empty, got %d", s.Len())
	}
}

func TestShadowLog_EvictsOldestWhenFull(t *testing.T) {
	s, _ := LoadShadowLog("", 3)
	// Insert with monotonically advancing LastSeen so eviction is deterministic.
	for i := 0; i < 5; i++ {
		s.Record("src", "p"+string(rune('a'+i)), "tmpl", "msg", "", "unknown", 1)
		// Force a measurable LastSeen difference.
		time.Sleep(2 * time.Millisecond)
	}
	if s.Len() != 3 {
		t.Fatalf("Len=%d want 3 (cap)", s.Len())
	}
	// The two earliest (pa, pb) should have been evicted.
	for _, gone := range []string{"pa", "pb"} {
		for _, e := range s.All() {
			if e.PatternID == gone {
				t.Errorf("expected %s evicted, still present", gone)
			}
		}
	}
}

func TestShadowLog_SampleMessageTruncated(t *testing.T) {
	s, _ := LoadShadowLog("", 0)
	huge := strings.Repeat("x", shadowSampleMaxBytes+200)
	s.Record("src", "p1", "tmpl", huge, "", "unknown", 1)
	got := s.All()[0].SampleMessage
	if len(got) > shadowSampleMaxBytes+10 { // +ellipsis bytes
		t.Errorf("SampleMessage not truncated: len=%d", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected truncation ellipsis, got tail %q", got[len(got)-5:])
	}
}

func TestShadowLog_PersistAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shadow.json")

	s, err := LoadShadowLog(path, 0)
	if err != nil {
		t.Fatalf("LoadShadowLog: %v", err)
	}
	s.Record("src", "p1", "tmpl one", "msg one", "oom", "unknown", 4)
	s.Record("src", "p2", "tmpl two", "msg two", "", "spike", 7)
	if !s.Dirty() {
		t.Fatal("expected dirty after Record")
	}
	if err := s.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if s.Dirty() {
		t.Error("expected clean after Persist")
	}

	// Reload from disk.
	s2, err := LoadShadowLog(path, 0)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if s2.Len() != 2 {
		t.Fatalf("reloaded Len=%d want 2", s2.Len())
	}
	stats := s2.Stats()
	if stats["verdict_unknown"] != 1 || stats["verdict_spike"] != 1 {
		t.Errorf("verdict stats = %+v", stats)
	}
	if stats["total_signals"] != 11 {
		t.Errorf("total_signals=%d want 11", stats["total_signals"])
	}
}

func TestShadowLog_ClearPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shadow.json")
	s, _ := LoadShadowLog(path, 0)
	s.Record("src", "p1", "t", "m", "", "unknown", 1)
	if err := s.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if cleared := s.Clear(); cleared != 1 {
		t.Errorf("Clear=%d want 1", cleared)
	}
	if !s.Dirty() {
		t.Error("expected dirty after Clear")
	}
	if err := s.Persist(); err != nil {
		t.Fatalf("Persist after clear: %v", err)
	}

	// File should now contain zero events.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f shadowFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(f.Events) != 0 {
		t.Errorf("file events=%d want 0", len(f.Events))
	}
}

func TestShadowLog_CorruptFileStartsFresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shadow.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, err := LoadShadowLog(path, 0)
	if err == nil {
		t.Log("LoadShadowLog returned nil err (acceptable: warning-only path)")
	}
	if s == nil || s.Len() != 0 {
		t.Fatalf("expected fresh empty log on corrupt file")
	}
	// Should be writable from a clean state.
	s.Record("src", "p1", "t", "m", "", "unknown", 1)
	if err := s.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
}
