package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadConfigMergesDefaults proves the deep-merge baseline: a partial user
// config overrides only the keys it sets, while every key it omits keeps the
// embedded best-practice default instead of falling back to an empty value.
//
// LoadConfig is guarded by a sync.Once, so this exercises the single allowed
// load for the package's test process.
func TestLoadConfigMergesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// A deliberately sparse user config: it sets a handful of keys and omits
	// everything else. Without the default merge, the omitted keys would be
	// empty zero values.
	userYAML := `
port: 8080
alert:
  slack:
    enable: true
    channel_id: "C123"
oncall:
  wait_minutes: 7
`
	if err := os.WriteFile(path, []byte(userYAML), 0o600); err != nil {
		t.Fatalf("write user config: %v", err)
	}

	if err := LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	c := GetConfig()

	// User-set keys win.
	if c.Port != 8080 {
		t.Errorf("Port = %d, want 8080 (user override)", c.Port)
	}
	if !c.Alert.Slack.Enable {
		t.Error("Alert.Slack.Enable = false, want true (user override)")
	}
	if c.Alert.Slack.ChannelID != "C123" {
		t.Errorf("Alert.Slack.ChannelID = %q, want C123 (user override)", c.Alert.Slack.ChannelID)
	}
	if c.OnCall.WaitMinutes != 7 {
		t.Errorf("OnCall.WaitMinutes = %d, want 7 (user override)", c.OnCall.WaitMinutes)
	}

	// Keys the user omitted keep their embedded defaults.
	if c.Name != "versus" {
		t.Errorf("Name = %q, want versus (default)", c.Name)
	}
	if c.Host != "0.0.0.0" {
		t.Errorf("Host = %q, want 0.0.0.0 (default)", c.Host)
	}
	if c.Alert.Slack.TemplatePath != "config/slack_message.tmpl" {
		t.Errorf("Alert.Slack.TemplatePath = %q, want config/slack_message.tmpl (default)", c.Alert.Slack.TemplatePath)
	}
	if !c.Queue.Enable {
		t.Error("Queue.Enable = false, want true (default)")
	}
	if c.OnCall.Provider != "aws_incident_manager" {
		t.Errorf("OnCall.Provider = %q, want aws_incident_manager (default)", c.OnCall.Provider)
	}
	if c.Storage.Type != "file" {
		t.Errorf("Storage.Type = %q, want file (default)", c.Storage.Type)
	}
	if c.Storage.File.MaxIncidents != 1000 {
		t.Errorf("Storage.File.MaxIncidents = %d, want 1000 (default)", c.Storage.File.MaxIncidents)
	}
	if c.Agent.Mode != "training" {
		t.Errorf("Agent.Mode = %q, want training (default)", c.Agent.Mode)
	}
}
