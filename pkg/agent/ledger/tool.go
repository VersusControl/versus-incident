package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/google/uuid"
)

type LedgeredTool struct {
	Tool core.Tool
}

func (tool LedgeredTool) Name() string               { return tool.Tool.Name() }
func (tool LedgeredTool) Description() string        { return tool.Tool.Description() }
func (tool LedgeredTool) ArgsSchema() map[string]any { return tool.Tool.ArgsSchema() }

func (tool LedgeredTool) Invoke(ctx context.Context, args json.RawMessage) (*core.ToolResult, error) {
	run, ok := RunFromContext(ctx)
	if !ok {
		return tool.Tool.Invoke(ctx, args)
	}
	callID := uuid.NewString()
	payload, err := json.Marshal(struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	}{Name: tool.Name(), Args: args})
	if err != nil {
		return nil, core.NewToolError(core.ToolErrorInternal, "audit unavailable", ErrLedgerUnavailable)
	}
	intent, err := run.Writer.Append(ctx, Entry{RunID: run.RunID, Kind: EntryToolCall, State: "intent", Ref: callID, Payload: payload})
	if err != nil {
		return nil, core.NewToolError(core.ToolErrorInternal, "audit unavailable", ErrLedgerUnavailable)
	}
	result, invokeErr := tool.Tool.Invoke(ctx, args)
	projection, projectionErr := toolResultProjection(tool.Name(), result, invokeErr)
	if projectionErr != nil {
		return nil, core.NewToolError(core.ToolErrorInternal, "audit unavailable", projectionErr)
	}
	intent.State = "done"
	if invokeErr != nil {
		intent.State = "failed"
	}
	intent.Payload = projection
	if _, appendErr := run.Writer.Append(ctx, intent); appendErr != nil {
		return nil, core.NewToolError(core.ToolErrorInternal, "audit unavailable", errors.Join(ErrLedgerUnavailable, appendErr))
	}
	return result, invokeErr
}

func toolResultProjection(name string, result *core.ToolResult, invokeErr error) (json.RawMessage, error) {
	hasher := sha256.New()
	counter := &hashCounter{Hash: hasher}
	if result != nil {
		if err := json.NewEncoder(counter).Encode(result); err != nil {
			return nil, err
		}
	}
	state := "done"
	errorCode := ""
	if invokeErr != nil {
		state = "failed"
		code, _ := core.ClassifyToolError(invokeErr)
		errorCode = string(code)
	}
	payload, err := json.Marshal(struct {
		Name      string `json:"name"`
		ResultSHA string `json:"result_sha256"`
		Size      int64  `json:"result_bytes"`
		State     string `json:"state"`
		ErrorCode string `json:"error_code,omitempty"`
	}{Name: name, ResultSHA: hex.EncodeToString(hasher.Sum(nil)), Size: counter.Count, State: state, ErrorCode: string(errorCode)})
	return payload, err
}

type hashCounter struct {
	Hash  hash.Hash
	Count int64
}

func (counter *hashCounter) Write(value []byte) (int, error) {
	count, err := counter.Hash.Write(value)
	counter.Count += int64(count)
	return count, err
}

var _ io.Writer = (*hashCounter)(nil)
var _ core.Tool = LedgeredTool{}
