package eino

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
)

type adapterTool struct {
	invoke func(context.Context, json.RawMessage) (*core.ToolResult, error)
}

type failingMarshaler struct{}

func (failingMarshaler) MarshalJSON() ([]byte, error) {
	return nil, errors.New("encode failed for password=hunter2")
}

func (adapterTool) Name() string               { return "probe" }
func (adapterTool) Description() string        { return "probe tool" }
func (adapterTool) ArgsSchema() map[string]any { return map[string]any{"type": "object"} }
func (value adapterTool) Invoke(ctx context.Context, args json.RawMessage) (*core.ToolResult, error) {
	return value.invoke(ctx, args)
}

func TestNewToolPreservesMetadataAndResult(t *testing.T) {
	adapter, err := NewTool(adapterTool{invoke: func(context.Context, json.RawMessage) (*core.ToolResult, error) {
		return &core.ToolResult{Tool: "probe", Found: true, Data: map[string]any{"ok": true}}, nil
	}}, 0, 8192)
	if err != nil {
		t.Fatalf("NewTool: %v", err)
	}
	info, err := adapter.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != "probe" || info.Desc != "probe tool" {
		t.Fatalf("Info = %#v", info)
	}
	output, err := adapter.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if !strings.Contains(output, `"ok":true`) {
		t.Fatalf("output = %q", output)
	}
}

func TestNewToolReturnsTimeoutAsStructuredResult(t *testing.T) {
	adapter, err := NewTool(adapterTool{invoke: func(context.Context, json.RawMessage) (*core.ToolResult, error) {
		time.Sleep(20 * time.Millisecond)
		return &core.ToolResult{Tool: "probe", Found: true}, nil
	}}, time.Millisecond, 8192)
	if err != nil {
		t.Fatalf("NewTool: %v", err)
	}
	output, err := adapter.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if !strings.Contains(output, `tool \"probe\" timed out`) {
		t.Fatalf("output = %q", output)
	}
	if !strings.Contains(output, `"code":"timeout"`) {
		t.Fatalf("output = %q", output)
	}
}

func TestNewToolReturnsParentCancellationAsStructuredResult(t *testing.T) {
	adapter, err := NewTool(adapterTool{invoke: func(ctx context.Context, _ json.RawMessage) (*core.ToolResult, error) {
		<-ctx.Done()
		return nil, errors.New("provider cancelled: postgres://admin:hunter2@db.internal/catalog")
	}}, time.Minute, 8192)
	if err != nil {
		t.Fatalf("NewTool: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	output, err := adapter.InvokableRun(ctx, `{}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if output != `{"error":{"code":"cancelled","message":"tool run was cancelled"}}` {
		t.Fatalf("output = %q", output)
	}
	if strings.Contains(output, "retry") || strings.Contains(output, "hunter2") || strings.Contains(output, "db.internal") {
		t.Fatalf("output encouraged retry or leaked cancellation cause: %q", output)
	}
}

func TestNewToolHidesBackendErrorDetails(t *testing.T) {
	secret := "postgres://admin:hunter2@db.internal/catalog?sslkey=/private/key.pem"
	adapter, err := NewTool(adapterTool{invoke: func(context.Context, json.RawMessage) (*core.ToolResult, error) {
		return nil, errors.New("catalog query failed: " + secret)
	}}, 0, 8192)
	if err != nil {
		t.Fatalf("NewTool: %v", err)
	}
	output, err := adapter.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if strings.Contains(output, secret) || strings.Contains(output, "hunter2") || strings.Contains(output, "/private/key.pem") {
		t.Fatalf("output leaked backend error: %q", output)
	}
	if output != `{"error":{"code":"backend_error","message":"tool backend failed"}}` {
		t.Fatalf("output = %q", output)
	}
}

func TestNewToolPreservesSafeValidationError(t *testing.T) {
	adapter, err := NewTool(adapterTool{invoke: func(context.Context, json.RawMessage) (*core.ToolResult, error) {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, "service is required", errors.New("unsafe parser detail"))
	}}, 0, 8192)
	if err != nil {
		t.Fatalf("NewTool: %v", err)
	}
	output, err := adapter.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if output != `{"error":{"code":"invalid_arguments","message":"service is required"}}` {
		t.Fatalf("output = %q", output)
	}
	if strings.Contains(output, "unsafe parser detail") {
		t.Fatalf("output leaked cause: %q", output)
	}
}

func TestNewToolHidesMarshalErrorDetails(t *testing.T) {
	adapter, err := NewTool(adapterTool{invoke: func(context.Context, json.RawMessage) (*core.ToolResult, error) {
		return &core.ToolResult{Tool: "probe", Found: true, Data: map[string]any{"value": failingMarshaler{}}}, nil
	}}, 0, 8192)
	if err != nil {
		t.Fatalf("NewTool: %v", err)
	}
	output, err := adapter.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if output != `{"error":{"code":"internal_error","message":"tool result could not be encoded"}}` {
		t.Fatalf("output = %q", output)
	}
	if strings.Contains(output, "hunter2") {
		t.Fatalf("output leaked marshal error: %q", output)
	}
}

func TestNewToolCapsOutput(t *testing.T) {
	adapter, err := NewTool(adapterTool{invoke: func(context.Context, json.RawMessage) (*core.ToolResult, error) {
		return &core.ToolResult{Tool: "probe", Found: true, Data: map[string]any{"value": strings.Repeat("x", 100)}}, nil
	}}, 0, 20)
	if err != nil {
		t.Fatalf("NewTool: %v", err)
	}
	output, err := adapter.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if len(output) <= 20 || !strings.HasSuffix(output, `..."truncated"`) {
		t.Fatalf("output = %q", output)
	}
}
