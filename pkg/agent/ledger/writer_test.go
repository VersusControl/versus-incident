package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestBlobWriterAppendsDenseImmutableEntriesAndScrubsPayload(t *testing.T) {
	provider := storage.NewMemory()
	writer := NewBlobWriter(provider, "org-a")
	ctx := context.Background()
	if err := writer.Begin(ctx, Trigger{RunID: "run-a", Kind: TriggerUser}); err != nil {
		t.Fatal(err)
	}
	secret := "sk-012345678901234567890123"
	entry, err := writer.Append(ctx, Entry{RunID: "run-a", Kind: EntryToolCall, State: "intent", Payload: json.RawMessage(`{"value":"` + secret + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Seq != 1 || strings.Contains(string(entry.Payload), secret) {
		t.Fatalf("entry was not assigned/scrubbed: %+v", entry)
	}
	second, err := writer.Append(ctx, Entry{RunID: "run-a", Kind: EntryToolCall, State: "done"})
	if err != nil || second.Seq != 2 {
		t.Fatalf("second entry=%+v err=%v", second, err)
	}
	if err := writer.Begin(ctx, Trigger{RunID: "run-a", Kind: TriggerUser}); !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("duplicate Begin error=%v", err)
	}
}

func TestBlobWriterRequiresRecordedTrigger(t *testing.T) {
	writer := NewBlobWriter(storage.NewMemory(), "org-a")
	if _, err := writer.Append(context.Background(), Entry{RunID: "missing", Kind: EntryToolCall}); !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("Append error=%v, want ledger unavailable", err)
	}
}

type completionFailureWriter struct{ appends int }

func (writer *completionFailureWriter) Begin(context.Context, Trigger) error            { return nil }
func (writer *completionFailureWriter) Close(context.Context, string, RunOutcome) error { return nil }
func (writer *completionFailureWriter) Append(_ context.Context, entry Entry) (Entry, error) {
	writer.appends++
	if writer.appends == 2 {
		return Entry{}, ErrLedgerUnavailable
	}
	entry.Seq = int64(writer.appends)
	return entry, nil
}

type invokedTool struct{ calls int }

func (*invokedTool) Name() string               { return "read_status" }
func (*invokedTool) Description() string        { return "read status" }
func (*invokedTool) ArgsSchema() map[string]any { return map[string]any{"type": "object"} }
func (tool *invokedTool) Invoke(context.Context, json.RawMessage) (*core.ToolResult, error) {
	tool.calls++
	return &core.ToolResult{Tool: tool.Name(), Found: true}, nil
}

func TestLedgeredToolReturnsCompletionFailure(t *testing.T) {
	writer := &completionFailureWriter{}
	tool := &invokedTool{}
	ctx := WithRun(context.Background(), writer, "run-a")
	_, err := (LedgeredTool{Tool: tool}).Invoke(ctx, json.RawMessage(`{"q":"status"}`))
	if tool.calls != 1 || !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("calls=%d error=%v, want invoked tool and visible ledger failure", tool.calls, err)
	}
	var classified *core.ToolError
	if !errors.As(err, &classified) || classified.Code != core.ToolErrorInternal || classified.Message != "audit unavailable" {
		t.Fatalf("tool error = %#v, want safe internal audit error", classified)
	}
}
