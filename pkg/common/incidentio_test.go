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

func TestIncidentioProvider_TriggerOnCall_HappyPath(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotBody map[string]interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")

		_ = json.NewDecoder(r.Body).Decode(&gotBody)

		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	provider := NewIncidentioProvider("test-api-key", "src-123")
	provider.baseURL = server.URL

	if err := provider.TriggerOnCall(context.Background(), "INC-123", nil); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if gotPath != "/src-123" {
		t.Errorf("expected alert source config id in path, got %s", gotPath)
	}
	if gotAuth != "Bearer test-api-key" {
		t.Errorf("expected Bearer auth header, got %q", gotAuth)
	}
	if gotBody["deduplication_key"] != "INC-123" {
		t.Errorf("expected deduplication_key mapped from incidentID, got %v", gotBody["deduplication_key"])
	}
	if gotBody["title"] != "Incident INC-123" {
		t.Errorf("expected title mapped from incidentID, got %v", gotBody["title"])
	}
}

func TestIncidentioProvider_TriggerOnCall_NonSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	provider := NewIncidentioProvider("bad-key", "src-123")
	provider.baseURL = server.URL

	err := provider.TriggerOnCall(context.Background(), "INC-456", nil)
	if err == nil {
		t.Fatalf("expected an error for non-2xx response, got nil")
	}
	if !strings.Contains(err.Error(), "non-success status") {
		t.Errorf("expected non-success status error, got: %v", err)
	}
}

func TestIncidentioProvider_TriggerOnCall_ConfigOverride(t *testing.T) {
	var gotPath string
	var gotAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	provider := NewIncidentioProvider("default-key", "default-src")
	provider.baseURL = server.URL

	cfg := &config.OnCallConfig{
		Incidentio: config.IncidentioConfig{
			APIKey:              "override-key",
			AlertSourceConfigID: "override-src",
		},
	}

	if err := provider.TriggerOnCall(context.Background(), "INC-777", cfg); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if gotPath != "/override-src" {
		t.Errorf("expected override alert source id in path, got %s", gotPath)
	}
	if gotAuth != "Bearer override-key" {
		t.Errorf("expected override Bearer auth header, got %q", gotAuth)
	}
}
