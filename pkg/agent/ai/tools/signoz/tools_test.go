package signoz

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
	signozapp "github.com/VersusControl/versus-incident/pkg/signoz"
)

func TestNewKeepsSourceKindsSeparate(t *testing.T) {
	service := testService(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(writer, `{"data":{"data":{"results":[]}}}`)
	}))
	logs := New([]Source{{Name: "logs", Kind: signozapp.SignalLogs, Service: service}})
	if len(logs) != 2 || logs[0].Name() != "discover_log_fields" || logs[1].Name() != "read_log_records" {
		t.Fatalf("log tools = %v", names(logs))
	}
	for _, forbidden := range []string{"read_metric_series", "read_trace_spans"} {
		if contains(logs, forbidden) {
			t.Fatalf("logs source unlocked %s", forbidden)
		}
	}
	all := New([]Source{{Name: "logs", Kind: signozapp.SignalLogs, Service: service}, {Name: "metrics", Kind: signozapp.SignalMetrics, Service: service}, {Name: "traces", Kind: signozapp.SignalTraces, Service: service}})
	if len(all) != 6 {
		t.Fatalf("all tools = %v", names(all))
	}
}

func TestNewOmitsSourceWideDiscoveryForScopedBindings(t *testing.T) {
	server, rootCAs := newTLSTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	service, err := signozapp.NewService(signozapp.Config{Address: server.URL, APIKey: "test-api-key", AllowLoopback: true, RootCAs: rootCAs, ScopeFilter: "service.name = 'checkout'"}, signozapp.ToolPolicy())
	if err != nil {
		t.Fatal(err)
	}
	tools := New([]Source{{Name: "scoped", Kind: signozapp.SignalLogs, Service: service}})
	if len(tools) != 1 || tools[0].Name() != "read_log_records" {
		t.Fatalf("scoped tools = %v", names(tools))
	}
}

func TestInvokeRequiresAuthorizationAndExactSource(t *testing.T) {
	service := testService(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(writer, `{"data":{"data":{"results":[]}}}`)
	}))
	tool := New([]Source{{Name: "one", Kind: signozapp.SignalLogs, Service: service}, {Name: "two", Kind: signozapp.SignalLogs, Service: service}})[1]
	denied, err := tool.Invoke(context.Background(), json.RawMessage(`{"source":"one","service":"api"}`))
	if err != nil || denied.IsAvailable() {
		t.Fatalf("denied = %+v err=%v", denied, err)
	}
	ctx := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	if _, err := tool.Invoke(ctx, json.RawMessage(`{"service":"api"}`)); err == nil {
		t.Fatal("ambiguous source accepted")
	}
	if _, err := tool.Invoke(ctx, json.RawMessage(`{"source":"missing","service":"api"}`)); err == nil {
		t.Fatal("unknown source accepted")
	}
	if _, err := tool.Invoke(ctx, json.RawMessage(`{"source":"one","service":"api"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestFilterAuthorizedRemovesSigNozTools(t *testing.T) {
	service := testService(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { fmt.Fprint(writer, `{}`) }))
	tools := New([]Source{{Name: "logs", Kind: signozapp.SignalLogs, Service: service}})
	if got := FilterAuthorized(context.Background(), tools); len(got) != 0 {
		t.Fatalf("denied tools = %v", names(got))
	}
	ctx := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	if got := FilterAuthorized(ctx, tools); len(got) != 2 {
		t.Fatalf("authorized tools = %v", names(got))
	}
}

func TestInvokeOmitsUpstreamErrorBodyAndAPIKey(t *testing.T) {
	const apiKeyCanary = "SIGNOZ_TOOL_API_KEY_CANARY_29af"
	const bodyCanary = "SIGNOZ_TOOL_BODY_CANARY_64d1"
	server, rootCAs := newTLSTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(writer, "%s %s", bodyCanary, apiKeyCanary)
	}))
	service, err := signozapp.NewService(signozapp.Config{Address: server.URL, APIKey: apiKeyCanary, AllowLoopback: true, RootCAs: rootCAs}, signozapp.ToolPolicy())
	if err != nil {
		t.Fatal(err)
	}
	readTool := New([]Source{{Name: "logs", Kind: signozapp.SignalLogs, Service: service}})[1]
	ctx := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	_, invokeErr := readTool.Invoke(ctx, json.RawMessage(`{"service":"api"}`))
	var toolErr *core.ToolError
	if invokeErr == nil || !errors.As(invokeErr, &toolErr) || containsAny(invokeErr.Error(), apiKeyCanary, bodyCanary) {
		t.Fatalf("tool error = %v", invokeErr)
	}
}

func TestAuthorizedInvokeScrubsExactAPIKeyFromSerializedResult(t *testing.T) {
	const apiKey = "SIGNOZ_SUCCESS_API_KEY_CANARY_29af"
	server, rootCAs := newTLSTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(writer, `{"data":{"data":{"results":[{"rows":[{"%s":"key","nested":["%s",{"value":"prefix %s suffix"}]}]}]}}}`, apiKey, apiKey, apiKey)
	}))
	service, err := signozapp.NewService(signozapp.Config{Address: server.URL, APIKey: apiKey, AllowLoopback: true, RootCAs: rootCAs}, signozapp.ToolPolicy())
	if err != nil {
		t.Fatal(err)
	}
	readTool := New([]Source{{Name: "logs", Kind: signozapp.SignalLogs, Service: service}})[1]
	ctx := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	result, err := readTool.Invoke(ctx, json.RawMessage(`{"source":"logs","service":"api"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), apiKey) {
		t.Fatalf("serialized ToolResult contains exact API key: %s", encoded)
	}
}

func testService(t *testing.T, handler http.Handler) *signozapp.Service {
	t.Helper()
	server, rootCAs := newTLSTestServer(t, handler)
	service, err := signozapp.NewService(signozapp.Config{Address: server.URL, APIKey: "test-api-key", AllowLoopback: true, RootCAs: rootCAs}, signozapp.ToolPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func newTLSTestServer(t *testing.T, handler http.Handler) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	rootCAs := x509.NewCertPool()
	rootCAs.AddCert(server.Certificate())
	t.Cleanup(server.Close)
	return server, rootCAs
}
func names(tools []core.Tool) []string {
	result := make([]string, 0, len(tools))
	for _, tool := range tools {
		result = append(result, tool.Name())
	}
	return result
}
func contains(tools []core.Tool, name string) bool {
	for _, tool := range tools {
		if tool.Name() == name {
			return true
		}
	}
	return false
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
