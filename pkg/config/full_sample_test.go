package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadFullSampleConfig proves the FULL documented sample config
// (config/config.yaml) loads-and-unmarshals cleanly through the exact same
// merge+unmarshal pipeline that LoadConfig uses. It calls the unexported
// loadConfigFromPath helper directly so it does not consume the package's
// sync.Once-guarded global load — the helper is the unguarded core that
// LoadConfig wraps.
func TestLoadFullSampleConfig(t *testing.T) {
	c, err := loadConfigFromPath("../../config/config.yaml")
	if err != nil {
		t.Fatalf("loadConfigFromPath(sample): %v", err)
	}
	if c == nil {
		t.Fatal("loadConfigFromPath returned nil *Config")
	}

	if c.Name != "versus" {
		t.Errorf("Name = %q, want versus", c.Name)
	}
	if c.Port != 3000 {
		t.Errorf("Port = %d, want 3000", c.Port)
	}
	if c.Storage.Type != "file" {
		t.Errorf("Storage.Type = %q, want file", c.Storage.Type)
	}
	if c.OnCall.Provider != "aws_incident_manager" {
		t.Errorf("OnCall.Provider = %q, want aws_incident_manager", c.OnCall.Provider)
	}
	if c.Agent.Mode != "training" {
		t.Errorf("Agent.Mode = %q, want training", c.Agent.Mode)
	}

	// Override-map fields must unmarshal as maps, not collapse to scalars.
	if c.OnCall.AwsIncidentManager.OtherResponsePlanArns == nil {
		t.Error("OnCall.AwsIncidentManager.OtherResponsePlanArns = nil, want non-nil map")
	}
	for _, key := range []string{"infra", "app", "db"} {
		if _, ok := c.OnCall.AwsIncidentManager.OtherResponsePlanArns[key]; !ok {
			t.Errorf("OnCall.AwsIncidentManager.OtherResponsePlanArns missing key %q", key)
		}
	}
}

// TestLoadSparseConfigKeepsDefaults mirrors the deep-merge feature through the
// unguarded helper: a sparse user config overrides only the keys it sets while
// omitted keys keep the embedded best-practice defaults. Because it goes
// through loadConfigFromPath rather than LoadConfig, it no longer needs the
// sync.Once-guarded global and can run alongside the other load tests.
func TestLoadSparseConfigKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	userYAML := `
port: 9090
alert:
  slack:
    enable: true
    channel_id: "C999"
`
	if err := os.WriteFile(path, []byte(userYAML), 0o600); err != nil {
		t.Fatalf("write user config: %v", err)
	}

	c, err := loadConfigFromPath(path)
	if err != nil {
		t.Fatalf("loadConfigFromPath(sparse): %v", err)
	}

	// User-set keys win.
	if c.Port != 9090 {
		t.Errorf("Port = %d, want 9090 (user override)", c.Port)
	}
	if !c.Alert.Slack.Enable {
		t.Error("Alert.Slack.Enable = false, want true (user override)")
	}
	if c.Alert.Slack.ChannelID != "C999" {
		t.Errorf("Alert.Slack.ChannelID = %q, want C999 (user override)", c.Alert.Slack.ChannelID)
	}

	// Omitted keys keep their embedded defaults.
	if c.Name != "versus" {
		t.Errorf("Name = %q, want versus (default)", c.Name)
	}
	if c.Host != "0.0.0.0" {
		t.Errorf("Host = %q, want 0.0.0.0 (default)", c.Host)
	}
	if c.Storage.Type != "file" {
		t.Errorf("Storage.Type = %q, want file (default)", c.Storage.Type)
	}
	if c.OnCall.Provider != "aws_incident_manager" {
		t.Errorf("OnCall.Provider = %q, want aws_incident_manager (default)", c.OnCall.Provider)
	}
	if c.Agent.Mode != "training" {
		t.Errorf("Agent.Mode = %q, want training (default)", c.Agent.Mode)
	}
}
