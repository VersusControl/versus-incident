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
