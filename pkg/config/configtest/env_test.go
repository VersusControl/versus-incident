package configtest

import (
	"os"
	"testing"
)

func TestIsConfigEnvCoversLoaderVariablesOnly(t *testing.T) {
	for _, name := range []string{
		"AGENT_AI_ENABLE", "AGENT_AI_BASE_URL", "AGENT_MODE", "GATEWAY_SECRET",
		"STORAGE_TYPE", "POSTGRES_DSN", "REDIS_TLS", "SLACK_ENABLE", "ONCALL_PROVIDER",
		"VERSUS_CONNECTORS_AWS_ENABLE", "KUBERNETES_EVIDENCE_SAVE_CHUNK", "DEBUG_BODY", "PORT",
	} {
		if !IsConfigEnv(name) {
			t.Errorf("IsConfigEnv(%q) = false, want true", name)
		}
	}
	for _, name := range []string{
		"AWS_ACCESS_KEY_ID", "HARNESS_RESULTS_DIR", "VERSUS_DEVOPS_BIN",
		"KUBERNETES_SERVICE_HOST", "PATH", "HOME", "GOFLAGS",
	} {
		if IsConfigEnv(name) {
			t.Errorf("IsConfigEnv(%q) = true, want false", name)
		}
	}
}

func TestIsolateUnsetsAndRestoresConfigVariables(t *testing.T) {
	t.Setenv("AGENT_AI_ENABLE", "true")
	t.Setenv("AGENT_AI_BASE_URL", "")
	t.Setenv("HARNESS_TASK", "kept")

	t.Run("isolated", func(t *testing.T) {
		Isolate(t)
		for _, name := range []string{"AGENT_AI_ENABLE", "AGENT_AI_BASE_URL"} {
			if value, ok := os.LookupEnv(name); ok {
				t.Errorf("%s still set to %q", name, value)
			}
		}
		if got := os.Getenv("HARNESS_TASK"); got != "kept" {
			t.Errorf("HARNESS_TASK = %q, want kept", got)
		}
	})

	if got := os.Getenv("AGENT_AI_ENABLE"); got != "true" {
		t.Errorf("AGENT_AI_ENABLE after cleanup = %q, want true", got)
	}
	if _, ok := os.LookupEnv("AGENT_AI_BASE_URL"); !ok {
		t.Error("AGENT_AI_BASE_URL was not restored after cleanup")
	}
}
