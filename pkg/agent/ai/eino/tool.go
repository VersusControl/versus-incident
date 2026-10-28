package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// NewTool adapts a core.Tool to Eino's invokable tool contract.
func NewTool(value core.Tool, timeout time.Duration, maxOutputBytes int) (tool.InvokableTool, error) {
	info := &schema.ToolInfo{Name: value.Name(), Desc: value.Description()}
	if argsSchema := value.ArgsSchema(); len(argsSchema) > 0 {
		raw, err := json.Marshal(argsSchema)
		if err != nil {
			return nil, fmt.Errorf("eino: marshal schema for tool %q: %w", value.Name(), err)
		}
		parsed := &jsonschema.Schema{}
		if err := json.Unmarshal(raw, parsed); err != nil {
			return nil, fmt.Errorf("eino: parse schema for tool %q: %w", value.Name(), err)
		}
		info.ParamsOneOf = schema.NewParamsOneOfByJSONSchema(parsed)
	}
	return &invokableTool{impl: value, info: info, timeout: timeout, maxOutputBytes: maxOutputBytes}, nil
}

type invokableTool struct {
	impl           core.Tool
	info           *schema.ToolInfo
	timeout        time.Duration
	maxOutputBytes int
}

func (adapter *invokableTool) Info(context.Context) (*schema.ToolInfo, error) {
	return adapter.info, nil
}

func (adapter *invokableTool) InvokableRun(ctx context.Context, arguments string, _ ...tool.Option) (string, error) {
	result, err := adapter.invoke(ctx, json.RawMessage(arguments))
	if err != nil {
		// Only classified fields cross the model boundary; raw causes are intentionally discarded.
		code, message := core.ClassifyToolError(err)
		return marshalToolError(code, message), nil
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return marshalToolError(core.ToolErrorInternal, "tool result could not be encoded"), nil
	}
	return capToolOutput(string(encoded), adapter.maxOutputBytes), nil
}

type toolErrorEnvelope struct {
	Error struct {
		Code    core.ToolErrorCode `json:"code"`
		Message string             `json:"message"`
	} `json:"error"`
}

func marshalToolError(code core.ToolErrorCode, message string) string {
	var envelope toolErrorEnvelope
	envelope.Error.Code = code
	envelope.Error.Message = message
	encoded, _ := json.Marshal(envelope)
	return string(encoded)
}

func (adapter *invokableTool) invoke(ctx context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if adapter.timeout <= 0 {
		return adapter.impl.Invoke(ctx, args)
	}

	timedCtx, cancel := context.WithTimeout(ctx, adapter.timeout)
	defer cancel()

	type invocation struct {
		result *core.ToolResult
		err    error
	}
	done := make(chan invocation, 1)
	go func() {
		result, err := adapter.impl.Invoke(timedCtx, args)
		done <- invocation{result: result, err: err}
	}()

	select {
	case <-timedCtx.Done():
		if errors.Is(timedCtx.Err(), context.Canceled) {
			return nil, core.NewToolError(core.ToolErrorCancelled, "tool run was cancelled", timedCtx.Err())
		}
		message := fmt.Sprintf("tool %q timed out after %s", adapter.impl.Name(), adapter.timeout)
		return nil, core.NewToolError(core.ToolErrorTimeout, message, timedCtx.Err())
	case result := <-done:
		return result.result, result.err
	}
}

func capToolOutput(value string, maxBytes int) string {
	if maxBytes > 0 && len(value) > maxBytes {
		return value[:maxBytes] + `..."truncated"`
	}
	return value
}
