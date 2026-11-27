package config

import (
	"testing"
	"time"
)

// TestGenerationCapIsSizedFromTheInterval: each collection kind keeps at
// least two days' worth of passes, so a health unit that changes on every
// 2-minute pass (a flapping alarm) still keeps 24 h of alarm history.
func TestGenerationCapIsSizedFromTheInterval(t *testing.T) {
	var collect AWSCollectConfig
	if got := collect.HealthMaxGenerations(); got != 1440 {
		t.Fatalf("health every 2 min keeps %d generations, want 1440", got)
	}
	// Inventory is read in full every 6 h, but the change feed re-reads a
	// changed unit at the 5-minute change poll: the cap keeps two days of
	// those re-reads.
	if got := collect.InventoryMaxGenerations(); got != 576 {
		t.Fatalf("inventory re-read every 5 min keeps %d generations, want 576", got)
	}
	collect.Intervals.Inventory, collect.Intervals.Changes = "15m", "30m"
	if got := collect.InventoryMaxGenerations(); got != 192 {
		t.Fatalf("inventory every 15 min keeps %d generations, want 192", got)
	}
	collect.Intervals.Changes = ""
	collect.Intervals.Inventory = ""
	// 24 h of flapping on the 2-minute fast cadence is 720 generations:
	// all kept.
	if passes := int(24 * time.Hour / DefaultAWSHealthHotInterval); collect.HealthMaxGenerations() < passes {
		t.Fatalf("a day of health passes (%d) does not fit", passes)
	}
	collect.History.MaxGenerations = 5000
	if got := collect.HealthMaxGenerations(); got != 5000 {
		t.Fatalf("a larger max_generations is kept: %d", got)
	}
	collect.History.MaxGenerations = 0
	collect.Intervals.Health = "30s"
	if got := collect.HealthMaxGenerations(); got != 5760 {
		t.Fatalf("health every 30 s keeps %d generations, want 5760", got)
	}
}
