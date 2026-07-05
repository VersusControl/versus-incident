package controllers

import (
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

// TestGetPattern_CarriesSamples_ListStrips proves the sample-ring API contract:
//   - GET /api/agent/patterns/:id returns the bounded `samples` ring, and its
//     entries are redacted (the re-scrub caught a planted secret).
//   - GET /api/agent/patterns strips `samples` from every list row so the
//     (potentially huge) list stays lean.
func TestGetPattern_CarriesSamples_ListStrips(t *testing.T) {
	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	red, errs := agent.NewRedactor(false, nil)
	if len(errs) != 0 {
		t.Fatalf("NewRedactor: %v", errs)
	}
	cat.Upsert("p-samp", "payment declined <*>", "es:prod", 5, 0.2, "default", "payments")
	// A secret in the recorded line must be re-scrubbed at the storage boundary
	// so it never reaches the API detail response.
	cat.RecordSample("p-samp", "payment declined password=hunter2 for order", red)
	cat.RecordSample("p-samp", "payment declined for order 42", red)

	app := patternsApp(t, cat, config.AgentCatalogConfig{AutoPromoteAfter: 100}, 30*time.Second)

	// Detail read carries the ring.
	code, body := getJSON(t, app, "/api/agent/patterns/p-samp")
	if code != fiber.StatusOK {
		t.Fatalf("detail status = %d, want 200; body=%v", code, body)
	}
	rawSamples, ok := body["samples"].([]any)
	if !ok || len(rawSamples) != 2 {
		t.Fatalf("detail samples = %v, want a 2-entry ring", body["samples"])
	}
	for _, s := range rawSamples {
		line, _ := s.(string)
		if strings.Contains(line, "hunter2") {
			t.Fatalf("secret leaked into API detail sample: %q", line)
		}
	}
	// Latest entry is last (oldest→newest ordering).
	if latest, _ := rawSamples[len(rawSamples)-1].(string); latest != "payment declined for order 42" {
		t.Errorf("latest sample = %q, want the most-recently recorded line", latest)
	}

	// List rows strip the ring entirely.
	code, listBody := getJSON(t, app, "/api/agent/patterns")
	if code != fiber.StatusOK {
		t.Fatalf("list status = %d, want 200", code)
	}
	rows, _ := listBody["patterns"].([]any)
	if len(rows) != 1 {
		t.Fatalf("list rows = %d, want 1", len(rows))
	}
	row, _ := rows[0].(map[string]any)
	if _, present := row["samples"]; present {
		t.Errorf("list rows must NOT carry samples, got %v", row["samples"])
	}
	// The stored ring is untouched by the list read (strip is on the copy).
	if p := cat.Get("p-samp"); p == nil || len(p.Samples) != 2 {
		t.Errorf("listPatterns must not mutate the stored ring, got %v", p)
	}
}
