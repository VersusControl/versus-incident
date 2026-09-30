package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	lokiapp "github.com/VersusControl/versus-incident/pkg/loki"
)

func TestLiveLokiAdapter(t *testing.T) {
	address := os.Getenv("QA_DS3_LOKI_URL")
	if address == "" {
		t.Skip("QA_DS3_LOKI_URL is not set")
	}
	service, err := lokiapp.NewService(config.AgentLokiSourceConfig{
		Address: address, Query: `{app="qa_ds3_20260928"}`, ServiceLabel: "app",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := New([]Source{{Name: "qa-ds3-loki", Service: service}})
	if len(tools) != 2 {
		t.Fatalf("Loki tools=%d", len(tools))
	}
	ctx, cancel := context.WithTimeout(core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{
		Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true},
	}), 20*time.Second)
	defer cancel()
	for _, invocation := range []struct {
		tool core.Tool
		args string
	}{
		{tools[0], `{"label":"app","limit":10}`},
		{tools[1], `{"service":"qa_ds3_20260928","lookback_minutes":30,"limit":10}`},
	} {
		result, err := invocation.tool.Invoke(ctx, json.RawMessage(invocation.args))
		if err != nil || result == nil || !result.Found {
			t.Fatalf("%s found=%v err=%v", invocation.tool.Name(), result != nil && result.Found, err)
		}
		switch value := result.Data["result"].(type) {
		case lokiapp.DiscoveryResult:
			t.Logf("%s found=%v count=%d", invocation.tool.Name(), result.Found, value.Count)
		case lokiapp.ReadResult:
			t.Logf("%s found=%v count=%d", invocation.tool.Name(), result.Found, value.Count)
		default:
			t.Fatalf("%s returned unexpected result type %T", invocation.tool.Name(), value)
		}
	}
}

func TestScopedToolsRequirePermissionAndExactSource(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		fmt.Fprint(writer, `{"status":"success","data":{"resultType":"streams","result":[]}}`)
	}))
	defer server.Close()
	service, err := lokiapp.NewService(config.AgentLokiSourceConfig{Address: server.URL, Query: `{service="api"}`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := New([]Source{{Name: "one", Service: service}, {Name: "two", Service: service}})
	if len(tools) != 2 || tools[0].Name() != "discover_log_fields" || tools[1].Name() != "read_log_records" {
		t.Fatalf("scoped tools = %v", tools)
	}
	read := tools[1]
	if got := read.ArgsSchema()["properties"].(map[string]any)["offset"].(map[string]any)["maximum"]; got != 99 {
		t.Fatalf("read offset maximum = %v, want 99", got)
	}
	if got := tools[0].ArgsSchema()["properties"].(map[string]any)["offset"].(map[string]any)["maximum"]; got != lokiapp.MaximumOffset {
		t.Fatalf("discovery offset maximum = %v", got)
	}
	if read.(core.ContextAuthorizedTool).AuthorizedTool(context.Background()) != nil {
		t.Fatal("catalog exposed tool without permission")
	}
	denied, err := read.Invoke(context.Background(), json.RawMessage(`{"source":"one"}`))
	if err != nil || denied.IsAvailable() || requests != 0 {
		t.Fatalf("denied = %+v, %v; requests=%d", denied, err, requests)
	}
	authorized := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	if read.(core.ContextAuthorizedTool).AuthorizedTool(authorized) == nil {
		t.Fatal("authorized catalog omitted tool")
	}
	for _, args := range []string{`{}`, `{"source":"missing"}`, `{"source":"one","label":"service"}`, `{"source":"one","labels":{"a|b":"x"}}`} {
		if _, err := read.Invoke(authorized, json.RawMessage(args)); err == nil || requests != 0 {
			t.Fatalf("accepted %s: %v; requests=%d", args, err, requests)
		}
	}
	result, err := read.Invoke(authorized, json.RawMessage(`{"source":"one","service":"api"}`))
	if err != nil || result.Data["source"] != "one" || requests != 1 {
		t.Fatalf("result = %+v, %v; requests=%d", result, err, requests)
	}
}

func TestUnmappedServiceReturnsInvalidToolArguments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(writer, `{"status":"success","data":{"resultType":"streams","result":[]}}`)
	}))
	defer server.Close()
	service, err := lokiapp.NewService(config.AgentLokiSourceConfig{Address: server.URL, Query: `{app="api"}`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	read := New([]Source{{Name: "logs", Service: service}})[1]
	authorized := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	if result, err := read.Invoke(authorized, json.RawMessage(`{"service":"api"}`)); err == nil || result != nil {
		t.Fatalf("unmapped service yielded no_data: %+v, %v", result, err)
	}
}
