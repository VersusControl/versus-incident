package tempo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
	tempoapp "github.com/VersusControl/versus-incident/pkg/tempo"
)

func allowed() context.Context {
	return core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
}

func TestToolsFilterRoutesAndRecheckGuards(t *testing.T) {
	const token = "TEMPO_TOOL_SECRET_CANARY"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/search" {
			fmt.Fprint(writer, `{"traces":[]}`)
			return
		}
		writer.WriteHeader(500)
		fmt.Fprint(writer, token)
	}))
	defer server.Close()
	service, err := tempoapp.NewService(tempoapp.Config{Address: server.URL, BearerToken: token, AllowLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	active := true
	tools := New([]Source{{Name: "z", Service: service, Guard: func(context.Context) bool { return false }}, {Name: "a", Service: service, Guard: func(context.Context) bool { return active }}})
	if len(tools) != 2 || tools[0].Name() != "discover_trace_fields" || tools[1].Name() != "read_trace_spans" {
		t.Fatalf("tools=%v", tools)
	}
	traceIDSchema := tools[1].ArgsSchema()["properties"].(map[string]any)["trace_id"].(map[string]any)
	if traceIDSchema["minLength"] != 1 {
		t.Fatalf("short Tempo trace IDs excluded by schema: %v", traceIDSchema)
	}
	if len(FilterAuthorized(context.Background(), tools)) != 0 {
		t.Fatal("unauthorized catalog")
	}
	filtered := FilterAuthorized(allowed(), tools)
	if len(filtered) != 2 {
		t.Fatalf("filtered=%v", filtered)
	}
	for _, candidate := range filtered {
		if names := candidate.(core.SourceRoutedTool).SourceNames(); len(names) != 1 || names[0] != "a" {
			t.Fatalf("source names=%v", names)
		}
	}
	if _, err := tools[1].Invoke(allowed(), json.RawMessage(`{"service":"api"}`)); err == nil {
		t.Fatal("ambiguous source accepted")
	}
	if _, err := tools[1].Invoke(allowed(), json.RawMessage(`{"source":"unknown","service":"api"}`)); err == nil {
		t.Fatal("unknown source accepted")
	}
	result, err := filtered[1].Invoke(allowed(), json.RawMessage(`{"source":"a","service":"api"}`))
	if err != nil || !result.IsAvailable() {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	_, err = filtered[0].Invoke(allowed(), json.RawMessage(`{"source":"a"}`))
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("unsafe error=%v", err)
	}
	active = false
	result, err = filtered[1].Invoke(allowed(), json.RawMessage(`{"source":"a","service":"api"}`))
	if err != nil || result.IsAvailable() {
		t.Fatalf("revoked result=%+v err=%v", result, err)
	}
}

func TestScopedSourceDoesNotExposeDiscovery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	service, err := tempoapp.NewService(tempoapp.Config{Address: server.URL, AllowLoopback: true, ScopeService: "api"})
	if err != nil {
		t.Fatal(err)
	}
	tools := New([]Source{{Name: "scoped", Service: service, Guard: func(context.Context) bool { return true }}})
	if len(tools) != 1 || tools[0].Name() != "read_trace_spans" {
		t.Fatalf("scoped tools=%v", tools)
	}
}
