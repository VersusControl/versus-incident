// Package aws exposes the AWS connector to the agents as read-only tools.
// Every tool calls the same pkg/aws service method as the matching admin
// route with an agent principal and reads stored state only: no tool starts
// a probe or calls AWS. Writes (connect, verify, rename, remove, recheck,
// options, privacy) stay in the admin UI.
package aws

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/settings"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// MaxModelViewBytes bounds one tool result for the model.
const MaxModelViewBytes = 6 << 10

// ServiceFunc returns the AWS connector service, or nil when the connector
// is not set up.
type ServiceFunc func() *settings.Service

// Option adjusts the tools.
type Option func(*tool)

// WithClock injects the clock used for freshness ages.
func WithClock(now func() time.Time) Option {
	return func(t *tool) {
		if now != nil {
			t.now = now
		}
	}
}

// New builds every AWS tool over one service source.
func New(service ServiceFunc, options ...Option) []core.Tool {
	if service == nil {
		return nil
	}
	out := make([]core.Tool, 0, len(toolNames))
	for _, name := range toolNames {
		t := &tool{name: name, service: service, now: time.Now}
		for _, option := range options {
			option(t)
		}
		out = append(out, t)
	}
	return out
}

type tool struct {
	name    string
	service ServiceFunc
	now     func() time.Time
}

func (t *tool) Name() string        { return t.name }
func (t *tool) DisplayName() string { return displayNames[t.name] }
func (t *tool) Description() string { return descriptions[t.name] }

func (t *tool) ArgsSchema() map[string]any {
	props := make(map[string]any, len(schemas[t.name]))
	for k, v := range schemas[t.name] {
		props[k] = v
	}
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if r := requiredArguments[t.name]; len(r) > 0 {
		schema["required"] = append([]string(nil), r...)
	}
	return schema
}

type arguments struct {
	AccountID string `json:"account_id"`
	Offset    int    `json:"offset"`
}

// Invoke answers from stored state through the one service method behind
// the matching admin route.
func (t *tool) Invoke(ctx context.Context, raw json.RawMessage) (*core.ToolResult, error) {
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return core.UnavailableToolResult(t.name, "infrastructure:view permission is required"), nil
	}
	var args arguments
	if len(raw) != 0 && string(raw) != "null" {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&args); err != nil || (args.AccountID != "" && !settings.ValidAccountID(args.AccountID)) || args.Offset < 0 {
			return nil, core.NewToolError(core.ToolErrorInvalidArguments, "invalid AWS tool arguments", err)
		}
	}
	svc := t.service()
	if svc == nil {
		return core.UnavailableToolResult(t.name, "the AWS connector is not set up"), nil
	}
	view, err := svc.Connection(ctx, svc.AgentPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	data := ConnectionView(view, args.AccountID, args.Offset, t.now())
	total, _ := data["accounts_total"].(int)
	found := view.Enabled && total > 0
	return &core.ToolResult{Tool: t.name, Found: found, Data: data}, nil
}
