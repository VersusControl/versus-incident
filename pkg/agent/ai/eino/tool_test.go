package eino

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
)

type adapterTool struct {
	invoke func(context.Context, json.RawMessage) (*core.ToolResult, error)
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
