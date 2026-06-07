package common

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
)

func TestServiceNowProvider_TriggerOnCall_HappyPath(t *testing.T) {
	var gotPath string
	var gotAuthOK bool
	var gotBody map[string]string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path

		username, password, ok := r.BasicAuth()
		gotAuthOK = ok && username == "admin" && password == "s3cret"

		_ = json.NewDecoder(r.Body).Decode(&gotBody)

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"result":{}}`))
	}))
	defer server.Close()

	provider := NewServiceNowProvider(server.URL, "admin", "s3cret", "")

	if err := provider.TriggerOnCall(context.Background(), "INC-123", nil); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if gotPath != "/api/now/table/incident" {
		t.Errorf("expected path /api/now/table/incident, got %s", gotPath)
	}
	if !gotAuthOK {
		t.Errorf("expected valid HTTP Basic auth header")
	}
	if gotBody["short_description"] != "Incident INC-123" {
		t.Errorf("expected short_description mapped from incidentID, got %q", gotBody["short_description"])
	}
	if gotBody["correlation_id"] != "INC-123" {
		t.Errorf("expected correlation_id mapped from incidentID, got %q", gotBody["correlation_id"])
	}
}

func TestServiceNowProvider_TriggerOnCall_CustomTable(t *testing.T) {
	var gotPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	provider := NewServiceNowProvider(server.URL, "admin", "s3cret", "sn_si_incident")

	if err := provider.TriggerOnCall(context.Background(), "INC-999", nil); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if gotPath != "/api/now/table/sn_si_incident" {
		t.Errorf("expected custom table path, got %s", gotPath)
	}
}

func TestServiceNowProvider_TriggerOnCall_NonSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	provider := NewServiceNowProvider(server.URL, "admin", "wrong", "")

	err := provider.TriggerOnCall(context.Background(), "INC-456", nil)
	if err == nil {
		t.Fatalf("expected an error for non-2xx response, got nil")
	}
	if !strings.Contains(err.Error(), "non-success status") {
		t.Errorf("expected non-success status error, got: %v", err)
	}
}

func TestServiceNowProvider_TriggerOnCall_ConfigOverride(t *testing.T) {
	var gotPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	// Provider defaults point at an unreachable instance; the per-request
	// config override must redirect to the test server.
	provider := NewServiceNowProvider("https://unused.example", "default", "default", "")

	cfg := &config.OnCallConfig{
		ServiceNow: config.ServiceNowConfig{
			InstanceURL: server.URL,
			Username:    "admin",
			Password:    "s3cret",
			Table:       "incident",
		},
	}

	if err := provider.TriggerOnCall(context.Background(), "INC-777", cfg); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if gotPath != "/api/now/table/incident" {
		t.Errorf("expected override path, got %s", gotPath)
	}
}
