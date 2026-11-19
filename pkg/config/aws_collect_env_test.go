package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAWSCollectEnvironmentOverridesEveryKnob: each interval and history
// bound in connectors.aws.collect has its own VERSUS_CONNECTORS_AWS_COLLECT_*
// override that wins over the file, and an out-of-range value fails the load
// without echoing it.
func TestAWSCollectEnvironmentOverridesEveryKnob(t *testing.T) {
	directory := t.TempDir()
	main := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(main, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file := "connectors:\n  aws:\n    enable: true\n    collect:\n      intervals:\n        inventory: 20m\n        changes: 10m\n      history:\n        change_retention: 72h\n"
	if err := os.WriteFile(filepath.Join(directory, "connectors.yaml"), []byte(file), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfigFromPath(main)
	if err != nil {
		t.Fatal(err)
	}
	collect := loaded.Connectors.AWS.Collect
	if collect.InventoryInterval() != 20*time.Minute || collect.ChangesInterval() != 10*time.Minute || collect.ChangeRetention() != 72*time.Hour {
		t.Fatalf("file values = %+v", collect)
	}

	t.Setenv("VERSUS_CONNECTORS_AWS_COLLECT_INVENTORY_INTERVAL", "45m")
	t.Setenv("VERSUS_CONNECTORS_AWS_COLLECT_CHANGES_INTERVAL", "2m")
	t.Setenv("VERSUS_CONNECTORS_AWS_COLLECT_HISTORY_CHANGE_RETENTION", "240h")
	loaded, err = loadConfigFromPath(main)
	if err != nil {
		t.Fatal(err)
	}
	collect = loaded.Connectors.AWS.Collect
	if collect.InventoryInterval() != 45*time.Minute || collect.ChangesInterval() != 2*time.Minute || collect.ChangeRetention() != 240*time.Hour {
		t.Fatalf("env overrides = %+v", collect)
	}

	for name, value := range map[string]string{
		"VERSUS_CONNECTORS_AWS_COLLECT_INVENTORY_INTERVAL":       "sensitive-value",
		"VERSUS_CONNECTORS_AWS_COLLECT_CHANGES_INTERVAL":         "25h",
		"VERSUS_CONNECTORS_AWS_COLLECT_HISTORY_CHANGE_RETENTION": "30m",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, value)
			_, err := loadConfigFromPath(main)
			if err == nil || strings.Contains(err.Error(), "sensitive-value") {
				t.Fatalf("invalid override accepted or echoed: %v", err)
			}
		})
	}
}

// TestAWSChangesSettingsFileEnvAndBounds: every change poller setting has a
// YAML key and an environment override, and each is bounded.
func TestAWSChangesSettingsFileEnvAndBounds(t *testing.T) {
	directory := t.TempDir()
	main := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(main, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file := "connectors:\n  aws:\n    enable: true\n    collect:\n      changes:\n        lookback: 12h\n        overlap: 30m\n        page_budget: 100\n" +
		"        routine_events: [\"ssm:UpdateInstanceInformation\"]\n        burst_window: 5m\n        burst_min: 3\n        failure_keep: 10\n        read_cap: 5000\n"
	if err := os.WriteFile(filepath.Join(directory, "connectors.yaml"), []byte(file), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfigFromPath(main)
	if err != nil {
		t.Fatal(err)
	}
	collect := loaded.Connectors.AWS.Collect
	if collect.ChangeLookback() != 12*time.Hour || collect.ChangeOverlap() != 30*time.Minute || collect.Changes.PageBudget != 100 ||
		len(collect.ChangeRoutineEvents()) != 1 || collect.ChangeBurstWindow() != 5*time.Minute || collect.ChangeBurstMin() != 3 ||
		collect.ChangeFailureKeep() != 10 || collect.ChangeReadCap() != 5000 {
		t.Fatalf("file values = %+v", collect.Changes)
	}

	t.Setenv("VERSUS_CONNECTORS_AWS_COLLECT_CHANGES_OVERLAP", "45m")
	t.Setenv("VERSUS_CONNECTORS_AWS_COLLECT_CHANGES_READ_CAP", "8000")
	t.Setenv("VERSUS_CONNECTORS_AWS_COLLECT_CHANGES_ROUTINE_EVENTS", "")
	loaded, err = loadConfigFromPath(main)
	if err != nil {
		t.Fatal(err)
	}
	collect = loaded.Connectors.AWS.Collect
	if collect.ChangeOverlap() != 45*time.Minute || collect.ChangeReadCap() != 8000 || len(collect.ChangeRoutineEvents()) != 0 {
		t.Fatalf("env overrides = %+v", collect.Changes)
	}

	for name, value := range map[string]string{
		"VERSUS_CONNECTORS_AWS_COLLECT_CHANGES_LOOKBACK":       "1000h",
		"VERSUS_CONNECTORS_AWS_COLLECT_CHANGES_OVERLAP":        "1m",
		"VERSUS_CONNECTORS_AWS_COLLECT_CHANGES_PAGE_BUDGET":    "5000",
		"VERSUS_CONNECTORS_AWS_COLLECT_CHANGES_ROUTINE_EVENTS": "not an event",
		"VERSUS_CONNECTORS_AWS_COLLECT_CHANGES_BURST_WINDOW":   "2h",
		"VERSUS_CONNECTORS_AWS_COLLECT_CHANGES_BURST_MIN":      "1",
		"VERSUS_CONNECTORS_AWS_COLLECT_CHANGES_FAILURE_KEEP":   "2",
		"VERSUS_CONNECTORS_AWS_COLLECT_CHANGES_READ_CAP":       "10",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, value)
			if _, err := loadConfigFromPath(main); err == nil {
				t.Fatal("out-of-range value accepted")
			}
		})
	}
}

// TestHistoryHorizonWarning: history shorter than the 24 h compare and
// alarm-line horizon warns; the default does not.
func TestHistoryHorizonWarning(t *testing.T) {
	if warning := (AWSCollectConfig{}).HistoryHorizonWarning(); warning != "" {
		t.Fatalf("default warns: %s", warning)
	}
	short := AWSCollectConfig{History: AWSCollectHistoryConfig{MaxAge: "12h"}}
	if short.HistoryHorizonWarning() == "" {
		t.Fatal("12h of history did not warn")
	}
}
