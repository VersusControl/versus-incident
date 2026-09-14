package agent

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/ai/detect"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
)

func TestSigNozExactSecretScrubSurvivesWorkerBoundaries(t *testing.T) {
	const secret = "SIGNOZ-SOURCE-CANARY-4f68c2"
	const redacted = "[REDACTED:SIGNOZ_API_KEY]"
	timestamp := time.Now().UTC().Add(-time.Second)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != signalsources.SigNozQueryRangePath {
			t.Errorf("SigNoz request = %s %s", request.Method, request.URL.Path)
		}
		if got := request.Header.Get("SIGNOZ-API-KEY"); got != secret {
			t.Errorf("SIGNOZ-API-KEY = %q, want configured key", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		row := map[string]any{
			"timestamp": timestamp.Format(time.RFC3339Nano),
			"data": map[string]any{
				"id":              secret,
				"body":            "service=api request failed token=" + secret,
				"severity_text":   "critical-" + secret,
				core.FieldService: "api-" + secret,
				secret + "-field": []any{
					"prefix " + secret,
					map[string]any{secret + "-nested": secret},
				},
			},
		}
		response := map[string]any{
			"status": "success",
			"data": map[string]any{"type": "raw", "data": map[string]any{
				"results": []any{map[string]any{"queryName": "A", "rows": []any{row}}},
			}},
		}
		if err := json.NewEncoder(writer).Encode(response); err != nil {
			t.Errorf("encode SigNoz response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	rootCAs := x509.NewCertPool()
	rootCAs.AddCert(server.Certificate())
	source, err := signalsources.NewSigNozSource("prod", config.AgentSignozSourceConfig{
		Address:       server.URL,
		APIKey:        secret,
		AllowLoopback: true,
		RootCAs:       rootCAs,
		ExtraFields:   []string{core.FieldService, secret + "-field"},
		PageSize:      10,
	})
	if err != nil {
		t.Fatalf("NewSigNozSource: %v", err)
	}

	ctx := context.Background()
	signals, _, err := source.Pull(ctx, time.Time{})
	if err != nil {
		t.Fatalf("pull production SigNoz source: %v", err)
	}
	if len(signals) != 1 {
		t.Fatalf("signals = %#v, want one", signals)
	}
	cleanSignal := signals[0]
	assertSigNozCanaryAbsent(t, secret, "serialized source signal before worker", cleanSignal)
	if cleanSignal.Message != "service=api request failed token="+redacted || cleanSignal.Severity != "critical-"+redacted {
		t.Fatalf("source text fields were not preserved and scrubbed: %#v", cleanSignal)
	}
	if cleanSignal.Fields[core.FieldService] != "api-"+redacted {
		t.Fatalf("configured service field = %#v", cleanSignal.Fields[core.FieldService])
	}
	redactedField := redacted + "-field"
	if _, ok := cleanSignal.Fields[redactedField]; !ok {
		t.Fatalf("configured secret-bearing field missing after scrub: %#v", cleanSignal.Fields)
	}
	if cleanSignal.Raw["id"] != redacted {
		t.Fatalf("raw ID = %#v, want scrubbed marker", cleanSignal.Raw["id"])
	}
	if _, ok := cleanSignal.Raw[redactedField]; !ok {
		t.Fatalf("nested raw field missing after scrub: %#v", cleanSignal.Raw)
	}
	if err := source.Rewind(ctx); err != nil {
		t.Fatalf("rewind source after boundary assertion: %v", err)
	}
	worker := newSeamWorker(t, "training", source, AIBundle{}, nil)
	worker.tickSource(ctx, source, "training")

	patterns := worker.catalog.All()
	if len(patterns) != 1 || len(patterns[0].Samples) != 1 {
		t.Fatalf("worker catalog = %#v, want one pattern with one sample", patterns)
	}
	_, prompt := detect.BuildPrompt(core.AgentResult{
		Verdict:       core.VerdictUnknown,
		PatternID:     patterns[0].ID,
		Template:      patterns[0].Template,
		SampleSignals: []core.Signal{cleanSignal},
		Frequency:     1,
	}, cleanSignal.Source, "api", sampleMessages([]core.Signal{cleanSignal}, 3))

	store := worker.catalog.store
	if err := worker.catalog.Persist(); err != nil {
		t.Fatalf("persist catalog: %v", err)
	}
	shadow, err := LoadShadowLog(store, 10)
	if err != nil {
		t.Fatalf("load shadow log: %v", err)
	}
	shadow.Record(cleanSignal.Source, patterns[0].ID, "api", patterns[0].Template, cleanSignal.Message, "", "unknown", 1)
	if err := shadow.Persist(); err != nil {
		t.Fatalf("persist shadow log: %v", err)
	}
	detectLog, err := LoadDetectLog(store, 10)
	if err != nil {
		t.Fatalf("load detect log: %v", err)
	}
	detectLog.Record(&DetectEvent{
		Source: cleanSignal.Source, PatternID: patterns[0].ID, Template: patterns[0].Template,
		Service: "api", Verdict: "unknown", Frequency: 1,
		Samples: sampleMessages([]core.Signal{cleanSignal}, 3), UserPrompt: prompt, Outcome: "cached",
	})
	if err := detectLog.Persist(); err != nil {
		t.Fatalf("persist detect log: %v", err)
	}

	assertSigNozCanaryAbsent(t, secret, "admin and model projections", map[string]any{
		"signals": cleanSignal,
		"catalog": patterns,
		"shadow":  shadow.All(),
		"detect":  detectLog.All(),
		"prompt":  prompt,
	})
	for _, blob := range []string{"patterns", "shadow", "detect"} {
		data, readErr := store.ReadBlob(blob)
		if readErr != nil {
			t.Fatalf("read %s blob: %v", blob, readErr)
		}
		if strings.Contains(string(data), secret) {
			t.Fatalf("persisted %s blob contains configured API key: %s", blob, data)
		}
	}
}

func assertSigNozCanaryAbsent(t *testing.T, secret, boundary string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s: %v", boundary, err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("%s contains configured API key: %s", boundary, encoded)
	}
}
