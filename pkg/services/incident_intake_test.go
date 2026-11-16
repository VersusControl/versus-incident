package services

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// enableFailingChannel turns on one alert channel whose send always fails
// without touching the network: Lark with a template file that does not exist.
func enableFailingChannel(t *testing.T) {
	t.Helper()
	loadAgentTestConfig(t)
	cfg := config.GetConfig()
	prev := cfg.Alert
	cfg.Alert.Lark = config.LarkConfig{
		Enable:       true,
		WebhookURL:   "http://127.0.0.1:1/unused",
		TemplatePath: filepath.Join(t.TempDir(), "missing.tmpl"),
	}
	t.Cleanup(func() { cfg.Alert = prev })
}

func useMemoryStore(t *testing.T) storage.Provider {
	t.Helper()
	mem := storage.NewMemory()
	prev := Storage()
	SetStorage(mem)
	t.Cleanup(func() { SetStorage(prev) })
	return mem
}

func onlyStoredIncident(t *testing.T, mem storage.Provider) *storage.IncidentRecord {
	t.Helper()
	recs, err := mem.ListIncidents(0)
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("stored incidents = %d, want 1", len(recs))
	}
	return recs[0]
}

// A stored incident whose channel send fails is reported as stored with the
// failure, not as an error, so a sender that retries on error cannot create a
// second incident for the same alert.
func TestIngestIncident_StoredIncidentWithFailedChannelIsNotAnError(t *testing.T) {
	enableFailingChannel(t)
	mem := useMemoryStore(t)

	content := map[string]interface{}{"title": "HighErrorRate"}
	outcome := IngestIncident("", &content)
	if outcome.Err != nil {
		t.Fatalf("Err = %v, want nil once the incident is stored", outcome.Err)
	}
	if !outcome.Stored || outcome.IncidentID == "" {
		t.Fatalf("outcome = %+v, want stored with an id", outcome)
	}
	if outcome.NotifyStatus != "failed" || !strings.HasPrefix(outcome.NotifyError, "lark: ") {
		t.Fatalf("notify = %q / %q, want failed with the lark reason", outcome.NotifyStatus, outcome.NotifyError)
	}

	content2 := map[string]interface{}{"title": "HighErrorRate"}
	if err := CreateIncident("", &content2); err != nil {
		t.Fatalf("CreateIncident = %v, want nil once the incident is stored", err)
	}

	recs, err := mem.ListIncidents(0)
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("stored incidents = %d, want 2 (one per call)", len(recs))
	}
	for _, rec := range recs {
		if rec.NotifyStatus != "failed" || !strings.Contains(rec.NotifyError, "lark") {
			t.Fatalf("record notify = %q / %q, want failed with the reason", rec.NotifyStatus, rec.NotifyError)
		}
		if !reflect.DeepEqual(rec.ChannelsEnabled, []string{"lark"}) || len(rec.ChannelsNotified) != 0 {
			t.Fatalf("channels enabled=%v notified=%v, want [lark] and none", rec.ChannelsEnabled, rec.ChannelsNotified)
		}
	}
}

// Without storage nothing is recorded, so a channel failure is still returned
// and the sender's retry is the only way the alert gets delivered.
func TestIngestIncident_WithoutStorageChannelFailureIsAnError(t *testing.T) {
	enableFailingChannel(t)
	prev := Storage()
	SetStorage(nil)
	t.Cleanup(func() { SetStorage(prev) })

	content := map[string]interface{}{"title": "HighErrorRate"}
	outcome := IngestIncident("", &content)
	if outcome.Stored || outcome.Err == nil {
		t.Fatalf("outcome = %+v, want not stored with an error", outcome)
	}
}

// With no channel enabled the incident says nothing was sent instead of "sent".
func TestIngestIncident_NoChannelRecordsNone(t *testing.T) {
	loadAgentTestConfig(t)
	mem := useMemoryStore(t)

	content := map[string]interface{}{"title": "HighErrorRate"}
	outcome := IngestIncident("", &content)
	if outcome.Err != nil || outcome.NotifyStatus != "none" {
		t.Fatalf("outcome = %+v, want notify none and no error", outcome)
	}
	rec := onlyStoredIncident(t, mem)
	if rec.NotifyStatus != "none" || len(rec.ChannelsEnabled) != 0 {
		t.Fatalf("record notify=%q channels=%v, want none and no channels", rec.NotifyStatus, rec.ChannelsEnabled)
	}
}

// A detection whose channel fails keeps the channel it tried on the incident,
// so the incident can show which channel failed.
func TestCreateIncidentFromFinding_FailedChannelIsRecordedAsEnabled(t *testing.T) {
	enableFailingChannel(t)
	mem := useMemoryStore(t)

	finding := &core.AIFinding{Title: "db-pool connection refused", Summary: "burst", Severity: "medium"}
	result := core.AgentResult{Verdict: core.VerdictSpike, PatternID: "p-db-pool", Frequency: 20, DetectionEmission: &core.DetectionEmissionResult{}}
	if err := CreateIncidentFromFinding(finding, result, "agent:file:app", "db-pool"); err == nil {
		t.Fatal("detection emit should still report the delivery failure to the agent")
	}
	rec := onlyStoredIncident(t, mem)
	if !reflect.DeepEqual(rec.ChannelsEnabled, []string{"lark"}) || rec.NotifyStatus != "failed" {
		t.Fatalf("record channels=%v notify=%q, want [lark] failed", rec.ChannelsEnabled, rec.NotifyStatus)
	}
	if result.DetectionEmission.NotificationOutcome != "failed" {
		t.Fatalf("notification outcome = %q, want failed", result.DetectionEmission.NotificationOutcome)
	}
}

// Critical and high detections follow the global on-call switch: with on-call
// disabled no escalation is attempted, even when a workflow exists.
func TestCreateIncidentFromFinding_CriticalRespectsOnCallDisabled(t *testing.T) {
	loadAgentTestConfig(t)
	mem := useMemoryStore(t)
	cfg := config.GetConfig()
	prev := cfg.OnCall
	cfg.OnCall.Enable = false
	t.Cleanup(func() { cfg.OnCall = prev })
	core.SetOnCallWorkflow(core.NewOnCallWorkflow(nil, nil))
	t.Cleanup(func() { core.SetOnCallWorkflow(nil) })

	for _, severity := range []string{"critical", "high"} {
		finding := &core.AIFinding{Title: "db-pool " + severity, Summary: "burst", Severity: severity}
		result := core.AgentResult{Verdict: core.VerdictSpike, PatternID: "p-" + severity}
		if err := CreateIncidentFromFinding(finding, result, "agent:file:app", "db-pool"); err != nil {
			t.Fatalf("%s: emit error = %v, want none with on-call disabled", severity, err)
		}
	}
	recs, err := mem.ListIncidents(0)
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	for _, rec := range recs {
		if rec.OnCallTriggered || rec.OnCallError != "" {
			t.Fatalf("on-call triggered=%v error=%q, want untouched", rec.OnCallTriggered, rec.OnCallError)
		}
	}
}

// With on-call enabled a critical detection still escalates.
func TestCreateIncidentFromFinding_CriticalEscalatesWhenOnCallEnabled(t *testing.T) {
	loadAgentTestConfig(t)
	useMemoryStore(t)
	cfg := config.GetConfig()
	prev := cfg.OnCall
	cfg.OnCall.Enable = true
	t.Cleanup(func() { cfg.OnCall = prev })
	core.SetOnCallWorkflow(core.NewOnCallWorkflow(nil, nil))
	t.Cleanup(func() { core.SetOnCallWorkflow(nil) })

	finding := &core.AIFinding{Title: "db-pool down", Summary: "burst", Severity: "critical"}
	result := core.AgentResult{Verdict: core.VerdictSpike, PatternID: "p-critical-on"}
	err := CreateIncidentFromFinding(finding, result, "agent:file:app", "db-pool")
	if err == nil || !strings.Contains(err.Error(), "on-call workflow") {
		t.Fatalf("emit error = %v, want the on-call start attempt to be reported", err)
	}
}
