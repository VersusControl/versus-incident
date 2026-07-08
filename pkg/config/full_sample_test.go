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
	// The AI block's `provider` key must round-trip into AgentAIConfig.Provider
	// (the field the eino registry selects on). The documented sample carries
	// the openai default.
	if c.Agent.AI.Provider != "openai" {
		t.Errorf("Agent.AI.Provider = %q, want openai", c.Agent.AI.Provider)
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

// TestAgentAIProviderSelection proves the `agent.ai.provider` selection
// round-trips through the loader: the YAML key maps into
// AgentAIConfig.Provider, an omitted block keeps the embedded openai default,
// and the AGENT_AI_PROVIDER env var overrides the YAML value (env wins).
func TestAgentAIProviderSelection(t *testing.T) {
	writeConfig := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		return path
	}

	t.Run("yaml provider round-trips", func(t *testing.T) {
		path := writeConfig(t, `
agent:
  ai:
    provider: qwen
    model: qwen-plus
`)
		c, err := loadConfigFromPath(path)
		if err != nil {
			t.Fatalf("loadConfigFromPath: %v", err)
		}
		if c.Agent.AI.Provider != "qwen" {
			t.Errorf("Agent.AI.Provider = %q, want qwen (from YAML)", c.Agent.AI.Provider)
		}
	})

	t.Run("omitted provider keeps openai default", func(t *testing.T) {
		// A sparse config that touches the agent block but not ai.provider must
		// keep the embedded default of openai.
		path := writeConfig(t, `
agent:
  mode: training
`)
		c, err := loadConfigFromPath(path)
		if err != nil {
			t.Fatalf("loadConfigFromPath: %v", err)
		}
		if c.Agent.AI.Provider != "openai" {
			t.Errorf("Agent.AI.Provider = %q, want openai (default)", c.Agent.AI.Provider)
		}
	})

	t.Run("env overrides yaml", func(t *testing.T) {
		path := writeConfig(t, `
agent:
  ai:
    provider: qwen
    model: qwen-plus
`)
		t.Setenv("AGENT_AI_PROVIDER", "deepseek")
		c, err := loadConfigFromPath(path)
		if err != nil {
			t.Fatalf("loadConfigFromPath: %v", err)
		}
		if c.Agent.AI.Provider != "deepseek" {
			t.Errorf("Agent.AI.Provider = %q, want deepseek (AGENT_AI_PROVIDER overrides YAML)", c.Agent.AI.Provider)
		}
	})

	t.Run("empty env does not clobber yaml", func(t *testing.T) {
		path := writeConfig(t, `
agent:
  ai:
    provider: ollama
    model: llama3
`)
		t.Setenv("AGENT_AI_PROVIDER", "")
		c, err := loadConfigFromPath(path)
		if err != nil {
			t.Fatalf("loadConfigFromPath: %v", err)
		}
		if c.Agent.AI.Provider != "ollama" {
			t.Errorf("Agent.AI.Provider = %q, want ollama (empty env must not clobber YAML)", c.Agent.AI.Provider)
		}
	})
}

// TestAutoPromoteAfterNormalization proves the load chokepoint folds any
// non-positive auto_promote_after up to the default: an explicit 0, a negative
// value, and an omitted key all arrive as the default, while a positive value
// passes through untouched. This is the single guard that stops a present-but-
// empty key or a ${VAR} that expands to empty from silently becoming a
// "promotion disabled" state.
func TestAutoPromoteAfterNormalization(t *testing.T) {
	writeConfig := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		return path
	}

	cases := []struct {
		name string
		yaml string
		want int
	}{
		{
			name: "explicit zero normalizes to default",
			yaml: "agent:\n  catalog:\n    auto_promote_after: 0\n",
			want: DefaultAutoPromoteAfter,
		},
		{
			name: "negative normalizes to default",
			yaml: "agent:\n  catalog:\n    auto_promote_after: -7\n",
			want: DefaultAutoPromoteAfter,
		},
		{
			name: "omitted keeps default",
			yaml: "agent:\n  mode: training\n",
			want: DefaultAutoPromoteAfter,
		},
		{
			name: "positive passes through unchanged",
			yaml: "agent:\n  catalog:\n    auto_promote_after: 42\n",
			want: 42,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.yaml)
			c, err := loadConfigFromPath(path)
			if err != nil {
				t.Fatalf("loadConfigFromPath: %v", err)
			}
			if c.Agent.Catalog.AutoPromoteAfter != tc.want {
				t.Errorf("AutoPromoteAfter = %d, want %d", c.Agent.Catalog.AutoPromoteAfter, tc.want)
			}
		})
	}
}
