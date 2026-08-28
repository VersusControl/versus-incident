package tools

import (
	"encoding/json"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
)

func assertUnavailable(t *testing.T, result *core.ToolResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result == nil || result.IsAvailable() || result.Reason == "" {
		t.Fatalf("result = %+v, want unavailable with reason", result)
	}
}

func mustArgs(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return encoded
}
