package services

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// autoResolveTestConfig ensures the global config exists (on-call ON so ack-URL
// injection is observable) and forces EVERY alert channel off so the fan-out
// builds zero providers and never touches the network. It reuses the
// sync.Once-guarded loadAgentTestConfig, then mutates + restores the global.
func autoResolveTestConfig(t *testing.T) {
	t.Helper()
	loadAgentTestConfig(t)
	cfg := config.GetConfig()

	prev := *cfg // shallow snapshot is enough for the scalar flags we touch
	cfg.OnCall.Enable = true
	// A positive wait window is what makes the ack link meaningful (and what
	// gates its injection); keep it non-zero so AckURL is emitted.
	cfg.OnCall.WaitMinutes = 3
	cfg.Alert.Slack.Enable = false
	cfg.Alert.Telegram.Enable = false
	cfg.Alert.Viber.Enable = false
	cfg.Alert.Email.Enable = false
	cfg.Alert.MSTeams.Enable = false
	cfg.Alert.Lark.Enable = false
	t.Cleanup(func() {
		cfg.OnCall = prev.OnCall
		cfg.Alert = prev.Alert
	})
}

// onlyIncident runs CreateIncident against a fresh memory store and returns the
// single persisted record.
func onlyIncident(t *testing.T, content map[string]interface{}, params ...*map[string]string) *storage.IncidentRecord {
	t.Helper()
	mem := storage.NewMemory()
	prev := Storage()
	SetStorage(mem)
	t.Cleanup(func() { SetStorage(prev) })

	if err := CreateIncident("", &content, params...); err != nil {
		t.Fatalf("CreateIncident: %v", err)
	}
	recs, err := mem.ListIncidents(0)
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected exactly 1 persisted incident, got %d", len(recs))
	}
	return recs[0]
}

// TestCreateIncident_WebhookAutoResolve exercises the webhook auto-resolve gate
// through the real emit path: the incident runs the FULL normal unresolved flow
// (AckURL injected when on-call is enabled, alert fans out, on-call escalation
// started), and is THEN stamped resolved in storage. The only delta versus a
// normal webhook incident is the persisted resolved / resolved_at — and it is
// scoped STRICTLY to the webhook origin.
func TestCreateIncident_WebhookAutoResolve(t *testing.T) {
	autoResolveTestConfig(t)

	t.Run("default on auto-resolves a webhook incident", func(t *testing.T) {
		// No intake blob stored → default ON.
		rec := onlyIncident(t, map[string]interface{}{"title": "spammy webhook"})
		if !rec.Resolved {
			t.Fatal("webhook incident should be auto-resolved (Resolved=true)")
		}
		if rec.ResolvedAt == nil {
			t.Fatal("auto-resolved incident should carry a ResolvedAt timestamp")
		}
		// AckURL IS injected. AckURL and on-call escalation share the exact
		// same !resolved && OnCall.Enable gate, so a present AckURL proves the
		// full unresolved flow ran and the on-call branch was entered — the
		// auto-resolve stamp is applied only AFTER, as a follow-up write.
		if _, ok := rec.Content["AckURL"]; !ok {
			t.Fatal("auto-resolved incident with on-call enabled MUST still get an AckURL")
		}
		if rec.Origin != storage.OriginWebhook {
			t.Fatalf("origin = %q, want webhook", rec.Origin)
		}
		// The alert still fanned out: the fan-out stage ran and stamped the
		// terminal status (no channels enabled → "sent").
		if rec.NotifyStatus != "sent" {
			t.Fatalf("notify_status = %q, want sent (alert must still fan out)", rec.NotifyStatus)
		}
	})

	t.Run("auto-resolved incident matches a normal open incident except resolved", func(t *testing.T) {
		content := func() map[string]interface{} {
			return map[string]interface{}{"title": "identical webhook payload"}
		}

		// Auto-resolve ON (default): full flow + resolved stamp.
		on := onlyIncident(t, content())

		// Auto-resolve OFF: the plain normal open webhook incident.
		off := func() *storage.IncidentRecord {
			mem := storage.NewMemory()
			prev := Storage()
			SetStorage(mem)
			defer SetStorage(prev)
			if err := SaveIntakeSettings(mem, IntakeSettings{AutoResolveWebhook: false}); err != nil {
				t.Fatalf("SaveIntakeSettings: %v", err)
			}
			c := content()
			if err := CreateIncident("", &c); err != nil {
				t.Fatalf("CreateIncident: %v", err)
			}
			recs, _ := mem.ListIncidents(0)
			if len(recs) != 1 {
				t.Fatalf("expected 1 incident, got %d", len(recs))
			}
			return recs[0]
		}()

		// The net delta must be ONLY the persisted resolved / resolved_at.
		if !on.Resolved || off.Resolved {
			t.Fatalf("resolved: on=%v off=%v, want on=true off=false", on.Resolved, off.Resolved)
		}
		if on.ResolvedAt == nil || off.ResolvedAt != nil {
			t.Fatal("resolved_at should be set only on the auto-resolved record")
		}
		if _, on := on.Content["AckURL"]; !on {
			t.Fatal("auto-resolved record must carry the same AckURL as a normal incident")
		}
		if _, off := off.Content["AckURL"]; !off {
			t.Fatal("normal open record must carry an AckURL")
		}
		if on.Origin != off.Origin || on.Source != off.Source {
			t.Fatalf("origin/source differ: on=(%q,%q) off=(%q,%q)", on.Origin, on.Source, off.Origin, off.Source)
		}
		if on.NotifyStatus != off.NotifyStatus {
			t.Fatalf("notify_status differ: on=%q off=%q", on.NotifyStatus, off.NotifyStatus)
		}
		if on.OnCallTriggered != off.OnCallTriggered {
			t.Fatalf("on_call_triggered differ: on=%v off=%v", on.OnCallTriggered, off.OnCallTriggered)
		}
	})

	t.Run("disabled leaves a webhook incident open with ack + oncall", func(t *testing.T) {
		mem := storage.NewMemory()
		prev := Storage()
		SetStorage(mem)
		t.Cleanup(func() { SetStorage(prev) })
		if err := SaveIntakeSettings(mem, IntakeSettings{AutoResolveWebhook: false}); err != nil {
			t.Fatalf("SaveIntakeSettings: %v", err)
		}

		content := map[string]interface{}{"title": "real webhook alert"}
		if err := CreateIncident("", &content); err != nil {
			t.Fatalf("CreateIncident: %v", err)
		}
		recs, _ := mem.ListIncidents(0)
		if len(recs) != 1 {
			t.Fatalf("expected 1 incident, got %d", len(recs))
		}
		rec := recs[0]
		if rec.Resolved {
			t.Fatal("with auto-resolve OFF a webhook incident must stay open")
		}
		if rec.ResolvedAt != nil {
			t.Fatal("open incident must not carry a ResolvedAt")
		}
		if _, ok := rec.Content["AckURL"]; !ok {
			t.Fatal("open webhook incident with on-call enabled should get an AckURL")
		}
	})

	t.Run("sns transport is never auto-resolved", func(t *testing.T) {
		params := map[string]string{sourceHintKey: "sns"}
		rec := onlyIncident(t, map[string]interface{}{"title": "from sns"}, &params)
		if rec.Resolved {
			t.Fatal("an SNS-transported incident must NOT be auto-resolved")
		}
		if rec.Source != "sns" {
			t.Fatalf("source = %q, want sns", rec.Source)
		}
		if _, ok := rec.Content["AckURL"]; !ok {
			t.Fatal("non-auto-resolved incident with on-call enabled should get an AckURL")
		}
	})

	t.Run("agent-emitted incident is never auto-resolved", func(t *testing.T) {
		rec := onlyIncident(t, map[string]interface{}{
			"AlertName": "pool exhausted",
			"PatternID": "p-1",
			"Source":    "agent:elasticsearch:app",
		})
		if rec.Resolved {
			t.Fatal("an agent-emitted incident must NOT be auto-resolved")
		}
		if rec.Origin != storage.OriginAIDetect {
			t.Fatalf("origin = %q, want ai_detect", rec.Origin)
		}
	})

	t.Run("already-resolved payload path is unchanged", func(t *testing.T) {
		rec := onlyIncident(t, map[string]interface{}{"title": "cleared", "status": "resolved"})
		if !rec.Resolved {
			t.Fatal("an already-resolved payload must stay resolved")
		}
		// The existing already-resolved path does NOT synthesize a ResolvedAt;
		// auto-resolve only stamps it for the webhook auto-resolve branch.
		if rec.ResolvedAt != nil {
			t.Fatal("already-resolved-from-payload must keep its existing shape (no synthesized ResolvedAt)")
		}
	})
}
