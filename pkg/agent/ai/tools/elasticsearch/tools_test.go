package elasticsearch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	elasticsearchapp "github.com/VersusControl/versus-incident/pkg/elasticsearch"
)

func TestNewReturnsExactToolBundleAndSchemas(t *testing.T) {
	service := newTestService(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { _, _ = writer.Write([]byte(`[]`)) }))
	tools := New([]Source{{Name: "primary", Service: service}})
	if len(tools) != len(toolNames) {
		t.Fatalf("tools = %d, want %d", len(tools), len(toolNames))
	}
	for index, candidate := range tools {
		if candidate.Name() != toolNames[index] {
			t.Errorf("tool[%d] = %q, want %q", index, candidate.Name(), toolNames[index])
		}
		schema := candidate.ArgsSchema()
		if schema["additionalProperties"] != false || schema["type"] != "object" {
			t.Errorf("%s schema = %#v", candidate.Name(), schema)
		}
		properties := schema["properties"].(map[string]any)
		source := properties["source"].(map[string]any)
		if !reflect.DeepEqual(source["enum"], []string{"primary"}) {
			t.Errorf("%s source enum = %#v", candidate.Name(), source["enum"])
		}
		if _, required := schema["required"]; required {
			t.Errorf("%s single-source schema unexpectedly requires source", candidate.Name())
		}
		_, hasQuery := properties["query_body"]
		if hasQuery != (candidate.Name() == "search_logs") {
			t.Errorf("%s query_body presence = %t", candidate.Name(), hasQuery)
		}
	}
	if New(nil) != nil {
		t.Fatal("empty sources should omit tools")
	}
}

func TestInvocationResolvesOneSourceAndRequiresSelectionForMany(t *testing.T) {
	serverA := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`[{"index":"a","status":"green","docs.count":"1"}]`))
	}))
	defer serverA.Close()
	serverB := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`[{"index":"b","status":"yellow","docs.count":"2"}]`))
	}))
	defer serverB.Close()
	serviceA, _ := elasticsearchapp.NewService(config.AgentElasticsearchSourceConfig{Addresses: []string{serverA.URL}, AllowLoopback: true, Index: "a-*"})
	serviceB, _ := elasticsearchapp.NewService(config.AgentElasticsearchSourceConfig{Addresses: []string{serverB.URL}, AllowLoopback: true, Index: "b-*"})

	one := New([]Source{{Name: "alpha", Service: serviceA}})[0]
	result, err := one.Invoke(authorizedContext(), nil)
	if err != nil || !result.Found || result.Data["source"] != "alpha" || result.Data["count"] != 1 {
		t.Fatalf("single result = %#v, err = %v", result, err)
	}

	many := New([]Source{{Name: "zeta", Service: serviceB}, {Name: "alpha", Service: serviceA}})[0]
	manySchema := many.ArgsSchema()
	if !reflect.DeepEqual(manySchema["required"], []string{"source"}) {
		t.Fatalf("multi-source required = %#v", manySchema["required"])
	}
	manySource := manySchema["properties"].(map[string]any)["source"].(map[string]any)
	if !reflect.DeepEqual(manySource["enum"], []string{"alpha", "zeta"}) {
		t.Fatalf("multi-source enum = %#v", manySource["enum"])
	}
	_, err = many.Invoke(authorizedContext(), nil)
	code, message := core.ClassifyToolError(err)
	if code != core.ToolErrorInvalidArguments || message != "log source is unknown or ambiguous" {
		t.Fatalf("ambiguous error = %q %q", code, message)
	}
	selected, err := many.Invoke(authorizedContext(), json.RawMessage(`{"source":"zeta","limit":1}`))
	if err != nil || selected.Data["source"] != "zeta" {
		t.Fatalf("selected result = %#v, err = %v", selected, err)
	}
	_, err = many.Invoke(authorizedContext(), json.RawMessage(`{"source":"unknown"}`))
	if code, _ := core.ClassifyToolError(err); code != core.ToolErrorInvalidArguments {
		t.Fatalf("unknown source code = %q", code)
	}
}

func TestEveryToolInvokesTheSourceScopedBackend(t *testing.T) {
	var paths []string
	service := newTestService(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		switch {
		case strings.HasPrefix(request.URL.Path, "/_cat/indices/"):
			_, _ = writer.Write([]byte(`[{"index":"logs-2026","status":"green","docs.count":"1"}]`))
		case strings.HasSuffix(request.URL.Path, "/_mapping"):
			_, _ = writer.Write([]byte(`{"logs-2026":{"mappings":{"properties":{"message":{"type":"text"}}}}}`))
		case strings.HasSuffix(request.URL.Path, "/_search"):
			_, _ = writer.Write([]byte(`{"hits":{"total":{"value":1,"relation":"eq"},"hits":[{"_id":"one","_source":{"message":"hello"}}]}}`))
		case strings.HasPrefix(request.URL.Path, "/_cat/shards/"):
			_, _ = writer.Write([]byte(`[{"index":"logs-2026","shard":"0","prirep":"p","state":"STARTED","docs":"1","store":"1kb","node":"node-a"}]`))
		default:
			http.NotFound(writer, request)
		}
	}))

	tools := New([]Source{{Name: "primary", Service: service}})
	for _, candidate := range tools {
		arguments := json.RawMessage(`{"limit":1}`)
		if candidate.Name() == "search_logs" {
			arguments = json.RawMessage(`{"limit":1,"fields":["message"]}`)
		}
		result, err := candidate.Invoke(authorizedContext(), arguments)
		if err != nil || result.Tool != candidate.Name() || !result.Found || result.Data["source"] != "primary" || result.Data["count"] != 1 {
			t.Fatalf("%s result = %#v, err = %v", candidate.Name(), result, err)
		}
	}

	wantPaths := []string{"/_cat/indices/logs-*", "/logs-*/_mapping", "/logs-*/_search", "/_cat/shards/logs-*"}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("paths = %v, want %v", paths, wantPaths)
	}
}

func TestSearchInvocationReturnsCoreEnvelopeAndSafeBackendError(t *testing.T) {
	service := newTestService(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{"hits":{"total":{"value":1,"relation":"eq"},"hits":[{"_id":"one","_source":{"message":{"text":"hello secret-token","nested":["secret-token"]},"password":"hidden"}}]}}`))
	}))
	service.SetScrubber(secretScrubber{})
	search := New([]Source{{Name: "primary", Service: service}})[2]
	result, err := search.Invoke(authorizedContext(), json.RawMessage(`{"query_body":{"query":{"match":{"message":"hello"}}},"fields":["message"]}`))
	if err != nil || result.Tool != "search_logs" || !result.Found || result.Data["count"] != 1 {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	encoded, _ := json.Marshal(result)
	if string(encoded) == "" || reflect.DeepEqual(result.Data, map[string]any{}) || strings.Contains(string(encoded), "secret-token") {
		t.Fatalf("empty result envelope: %s", encoded)
	}

	failing := newTestService(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, "secret-provider-error", http.StatusInternalServerError)
	}))
	_, err = New([]Source{{Name: "primary", Service: failing}})[0].Invoke(authorizedContext(), nil)
	code, message := core.ClassifyToolError(err)
	if code != core.ToolErrorBackend || message != "log read failed" || errors.Is(err, nil) {
		t.Fatalf("safe error = %q %q (%v)", code, message, err)
	}
}

func TestResponseTooLargeIsBackendBoundsError(t *testing.T) {
	err := safeToolError(elasticsearchapp.ErrResponseTooLarge)
	code, message := core.ClassifyToolError(err)
	if code != core.ToolErrorBackend || message != "log response exceeded its safe bound; narrow the configured index scope or query" {
		t.Fatalf("response-too-large classification = %q %q", code, message)
	}
}

func TestToolsFailClosedWithoutInfrastructurePermission(t *testing.T) {
	service := newTestService(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Fatal("unauthorized invocation reached Elasticsearch")
	}))
	tools := New([]Source{{Name: "primary", Service: service}})
	if got := FilterAuthorized(context.Background(), tools); len(got) != 0 {
		t.Fatalf("unauthorized catalog retained %d tools", len(got))
	}
	if got := FilterAuthorized(authorizedContext(), tools); len(got) != len(toolNames) {
		t.Fatalf("authorized catalog has %d tools", len(got))
	}
	for _, candidate := range tools {
		result, err := candidate.Invoke(context.Background(), nil)
		if err != nil || result.IsAvailable() || result.Reason != "infrastructure:view permission is required" {
			t.Fatalf("%s unauthorized result = %#v, err = %v", candidate.Name(), result, err)
		}
	}
}

func authorizedContext() context.Context {
	return core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
}

type secretScrubber struct{}

func (secretScrubber) Scrub(value string) string {
	return strings.ReplaceAll(value, "secret-token", "<redacted>")
}

func newTestService(t *testing.T, handler http.Handler) *elasticsearchapp.Service {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	service, err := elasticsearchapp.NewService(config.AgentElasticsearchSourceConfig{Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", MessageField: "message"})
	if err != nil {
		t.Fatal(err)
	}
	return service
}
