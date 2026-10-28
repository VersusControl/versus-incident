package graylog

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
	graylogapp "github.com/VersusControl/versus-incident/pkg/graylog"
)

func TestLiveGraylogAdapter(t *testing.T) {
	address := os.Getenv("QA_DS5_GRAYLOG_URL")
	password := os.Getenv("QA_DS5_GRAYLOG_PASSWORD")
	if address == "" || password == "" {
		t.Skip("QA_DS5_GRAYLOG_URL and QA_DS5_GRAYLOG_PASSWORD are required")
	}
	service, err := graylogapp.NewService(config.AgentGraylogSourceConfig{
		Address: address, Username: "admin", Password: password, Query: "source:qa_ds5_20260928",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := New([]Source{{Name: "qa-ds5-graylog", Service: service}})
	if len(tools) != 2 {
		t.Fatalf("Graylog tools=%d", len(tools))
	}
	ctx, cancel := context.WithTimeout(core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{
		Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true},
	}), 20*time.Second)
	defer cancel()
	for _, invocation := range []struct {
		tool core.Tool
		args string
	}{
		{tools[0], `{"limit":10}`},
		{tools[1], `{"lookback_minutes":30,"limit":10}`},
	} {
		result, err := invocation.tool.Invoke(ctx, json.RawMessage(invocation.args))
		if err != nil || result == nil || !result.Found {
			t.Fatalf("%s found=%v err=%v", invocation.tool.Name(), result != nil && result.Found, err)
		}
		switch value := result.Data["result"].(type) {
		case graylogapp.DiscoveryResult:
			t.Logf("%s found=%v count=%d", invocation.tool.Name(), result.Found, value.Count)
		case graylogapp.ReadResult:
			t.Logf("%s found=%v count=%d", invocation.tool.Name(), result.Found, value.Count)
		default:
			t.Fatalf("%s returned unexpected result type %T", invocation.tool.Name(), value)
		}
	}
}

func TestToolRequiresPermissionAndExactSource(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		fmt.Fprint(writer, `{"total_results":0,"messages":[]}`)
	}))
	defer server.Close()
	service, err := graylogapp.NewService(config.AgentGraylogSourceConfig{Address: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := New([]Source{{Name: "one", Service: service}, {Name: "two", Service: service}})
	if len(tools) != 2 || tools[0].Name() != "discover_log_fields" || tools[1].Name() != "read_log_records" {
		t.Fatalf("tools = %v", tools)
	}
	read := tools[1]
	if read.(core.ContextAuthorizedTool).AuthorizedTool(context.Background()) != nil {
		t.Fatal("catalog exposed unauthorized tool")
	}
	for _, candidate := range tools {
		if candidate.(core.ContextAuthorizedTool).AuthorizedTool(context.Background()) != nil {
			t.Fatalf("catalog exposed unauthorized %s", candidate.Name())
		}
		denied, err := candidate.Invoke(context.Background(), json.RawMessage(`{"source":"one"}`))
		if err != nil || denied.IsAvailable() || requests != 0 {
			t.Fatalf("%s denied = %+v, %v; requests=%d", candidate.Name(), denied, err, requests)
		}
	}
	allowed := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	for _, candidate := range tools {
		for _, input := range []string{`{"source":"one"}{"query":"*"}`, `{"source":"one"}garbage`} {
			_, err := candidate.Invoke(allowed, json.RawMessage(input))
			code, _ := core.ClassifyToolError(err)
			if code != core.ToolErrorInvalidArguments || requests != 0 {
				t.Fatalf("%s accepted %s: %v; requests=%d", candidate.Name(), input, err, requests)
			}
		}
	}
	for _, input := range []string{`{}`, `{"source":"absent"}`, `{"source":"one","query":"*"}`, `{"source":"one","service":"api"}`, `{"source":"one","filters":{"a|b":"api"}}`} {
		if _, err := read.Invoke(allowed, json.RawMessage(input)); err == nil || requests != 0 {
			t.Fatalf("accepted %s: %v; requests=%d", input, err, requests)
		}
	}
	result, err := read.Invoke(allowed, json.RawMessage(`{"source":"one","filters":{"service":"api"}}`))
	if err != nil || result.Data["source"] != "one" || requests != 1 {
		t.Fatalf("result = %+v, %v; requests=%d", result, err, requests)
	}
	result, err = tools[0].Invoke(allowed, json.RawMessage(`{"source":"two"}`))
	if err != nil || result.Data["source"] != "two" || requests != 2 {
		t.Fatalf("discovery = %+v, %v; requests=%d", result, err, requests)
	}
}
