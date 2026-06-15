package services

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// loadAgentTestConfig writes a minimal config to a temp dir and loads it
// into the global singleton. On-call is globally disabled and the workflow
// is never initialized — exactly the default that QA-003 crashes under.
func loadAgentTestConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `name: test
host: 0.0.0.0
port: 3000
public_host: http://localhost:3000
alert:
  slack:
    enable: false
oncall:
  enable: false
  initialized_only: false
redis:
  host: localhost
  port: 6379
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
}

// TestCreateIncidentFromFinding_HighSeverityNoOnCall reproduces QA-003: a
// high-severity AI finding force-sets oncall_enable=true, but with the
// on-call workflow uninitialized the emit path must persist the incident
// and return without panicking (on-call simply skipped).
func TestCreateIncidentFromFinding_HighSeverityNoOnCall(t *testing.T) {
	loadAgentTestConfig(t)

	mem := storage.NewMemory()
	prev := Storage()
	SetStorage(mem)
	t.Cleanup(func() { SetStorage(prev) })

	f := &core.AIFinding{
		Title:      "EC2 CPU saturation",
		Summary:    "Sustained CPU spike on ec2-i-0abcd1234",
		Severity:   "high",
		Category:   "compute",
		Confidence: 0.92,
	}
	r := core.AgentResult{Verdict: core.VerdictSpike, PatternID: "p-ec2-cpu"}

	// Would panic before the fix (GetOnCallWorkflow on a nil singleton).
	if err := CreateIncidentFromFinding(f, r, "agent:metrics:ec2-i-0abcd1234", "ec2-i-0abcd1234"); err != nil {
		t.Fatalf("CreateIncidentFromFinding returned error: %v", err)
	}

	got, err := mem.ListIncidents(10)
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 persisted incident, got %d", len(got))
	}
	if got[0].Source != "agent:metrics:ec2-i-0abcd1234" {
		t.Fatalf("Source = %q, want agent:metrics:ec2-i-0abcd1234", got[0].Source)
	}
	if got[0].OnCallTriggered {
		t.Fatal("OnCallTriggered should be false when the on-call workflow is not initialized")
	}
}
