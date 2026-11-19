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
	if got := collect.InventoryMaxGenerations(); got != 192 {
		t.Fatalf("inventory every 15 min keeps %d generations, want 192", got)
	}
	// 24 h of flapping on a 2-minute pass is 720 generations: all kept.
	if passes := int(24 * time.Hour / DefaultAWSHealthInterval); collect.HealthMaxGenerations() < passes {
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
