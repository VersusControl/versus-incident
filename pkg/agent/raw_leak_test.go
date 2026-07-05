package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/agent/ai/detect"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// rawOnlySecret is a sentinel that lives ONLY in Signal.Raw — never in the
// redacted Message/Fields. No default redaction rule matches it, so ANY
// appearance downstream is an unambiguous leak: it means a builder started
// serializing Signal.Raw (the deliberately-unredacted original the worker
// keeps for admin-only debug) into a prompt/log/persisted payload, exposing
// the very secrets the redactor scrubs out of Message/Fields.
//
// These tests are the guard that a future snapshot / admin-dump /
// enterprise-brain path cannot silently start emitting Signal.Raw. They
// exercise the REAL builders (detect.BuildPrompt, ShadowLog, DetectLog) and
// their real serializers, not a hand-rolled marshal.
const rawOnlySecret = "RAW-ONLY-SENTINEL-DO-NOT-LEAK-42"

// leakSignal returns a Signal in the exact post-redaction shape the worker
// produces: Message/Fields carry no secret (already scrubbed), while Raw holds
// the original secret-bearing document (left alone for admin debug).
func leakSignal() core.Signal {
	return core.Signal{
		Source:  "es:prod",
		Message: "service=api request failed id=<REDACTED:uuid>",
		Fields: map[string]interface{}{
			core.FieldService: "api",
			core.FieldSignal:  "request failed",
		},
		Raw: map[string]interface{}{
			"raw_line": "service=api request failed token=" + rawOnlySecret,
			"headers":  map[string]interface{}{"authorization": "Bearer " + rawOnlySecret},
		},
	}
}

func TestSignalRaw_NeverLeaksIntoModelPrompt(t *testing.T) {
	sig := leakSignal()
	result := core.AgentResult{
		Verdict:       core.VerdictUnknown,
		PatternID:     "p1",
		Template:      "service=api request failed id=<*>",
		SampleSignals: []core.Signal{sig},
		Frequency:     1,
	}
	// The worker feeds BuildPrompt the samples produced by sampleMessages,
	// which reads .Message (redacted) — exercise that exact builder chain.
	samples := sampleMessages(result.SampleSignals, 3)
	system, user := detect.BuildPrompt(result, sig.Source, "api", samples)
	if strings.Contains(system+user, rawOnlySecret) {
		t.Fatalf("Signal.Raw leaked into the model prompt:\nsystem=%s\nuser=%s", system, user)
	}
}

func TestSignalRaw_NeverLeaksIntoShadowLog(t *testing.T) {
	sig := leakSignal()
	store := storage.NewMemory()
	sl, err := LoadShadowLog(store, 0)
	if err != nil {
		t.Fatalf("LoadShadowLog: %v", err)
	}
	// The worker records the redacted Message as the shadow sample.
	sl.Record(sig.Source, "p1", "api", "tmpl", sig.Message, "default", "unknown", 1)
	if err := sl.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	assertBlobHasNoRawSecret(t, store, "shadow")
	// The in-memory snapshot the admin API serves must be clean too.
	assertJSONHasNoRawSecret(t, "shadow.All", sl.All())
}

func TestSignalRaw_NeverLeaksIntoDetectLog(t *testing.T) {
	sig := leakSignal()
	store := storage.NewMemory()
	dl, err := LoadDetectLog(store, 0)
	if err != nil {
		t.Fatalf("LoadDetectLog: %v", err)
	}
	// Build the DetectEvent exactly as emitDetect does: Samples come from
	// sampleMessages (redacted Message), never from Raw.
	evt := &DetectEvent{
		Source:    sig.Source,
		PatternID: "p1",
		Template:  "tmpl",
		Service:   "api",
		Verdict:   "unknown",
		Frequency: 1,
		Samples:   sampleMessages([]core.Signal{sig}, 3),
		Outcome:   "emitted",
	}
	dl.Record(evt)
	if err := dl.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	assertBlobHasNoRawSecret(t, store, "detect")
	assertJSONHasNoRawSecret(t, "detect.All", dl.All())
}

func assertBlobHasNoRawSecret(t *testing.T, store storage.Provider, blob string) {
	t.Helper()
	data, err := store.ReadBlob(blob)
	if err != nil {
		t.Fatalf("ReadBlob(%s): %v", blob, err)
	}
	if strings.Contains(string(data), rawOnlySecret) {
		t.Fatalf("Signal.Raw leaked into persisted %q blob:\n%s", blob, data)
	}
}

func assertJSONHasNoRawSecret(t *testing.T, what string, v interface{}) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", what, err)
	}
	if strings.Contains(string(data), rawOnlySecret) {
		t.Fatalf("Signal.Raw leaked into %s payload:\n%s", what, data)
	}
}
