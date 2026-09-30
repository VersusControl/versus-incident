package cloudwatchlogs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"time"

	app "github.com/VersusControl/versus-incident/pkg/cloudwatchlogs"
	"github.com/VersusControl/versus-incident/pkg/core"
)

type Source struct {
	Name   string
	Reader *app.Reader
	Guard  func(context.Context) bool
}

type tool struct {
	name    string
	sources map[string]Source
	names   []string
}

func New(sources []Source) []core.Tool {
	configured := map[string]Source{}
	for _, source := range sources {
		if source.Name != "" && source.Reader != nil {
			configured[source.Name] = source
		}
	}
	if len(configured) == 0 {
		return nil
	}
	names := make([]string, 0, len(configured))
	for name := range configured {
		names = append(names, name)
	}
	sort.Strings(names)
	return []core.Tool{
		&tool{name: "discover_log_fields", sources: configured, names: names},
		&tool{name: "read_log_records", sources: configured, names: names},
	}
}

func (current *tool) Name() string { return current.name }
func (current *tool) Description() string {
	if current.name == "discover_log_fields" {
		return "Sample JSON field names from bounded recent CloudWatch Logs events within the configured group, stream prefix, and filter pattern; this is not full field metadata."
	}
	return "Read bounded CloudWatch Logs events within the configured group, stream prefix, and filter pattern. Search is a client-side literal."
}
func (current *tool) SourceNames() []string { return append([]string(nil), current.names...) }
func (current *tool) AvailabilityCapability() (string, string, int) {
	return current.name, "logs", len(current.names)
}
func (current *tool) AuthorizedTool(ctx context.Context) core.Tool {
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return nil
	}
	allowed := map[string]Source{}
	var names []string
	for _, name := range current.names {
		source := current.sources[name]
		if source.Guard == nil || source.Guard(ctx) {
			allowed[name] = source
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return &tool{name: current.name, sources: allowed, names: names}
}
func (current *tool) ArgsSchema() map[string]any {
	properties := map[string]any{
		"source":           map[string]any{"type": "string", "enum": current.SourceNames()},
		"limit":            map[string]any{"type": "integer", "minimum": 1, "maximum": app.MaxRows},
		"lookback_minutes": map[string]any{"type": "integer", "minimum": 1, "maximum": int(app.MaxLookback / time.Minute)},
	}
	if current.name == "read_log_records" {
		properties["search"] = map[string]any{"type": "string", "maxLength": 256, "description": "Literal substring of the redacted event message; applied locally after the scoped AWS query."}
	}
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(current.names) > 1 {
		schema["required"] = []string{"source"}
	}
	return schema
}

func (current *tool) Invoke(ctx context.Context, raw json.RawMessage) (*core.ToolResult, error) {
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return core.UnavailableToolResult(current.name, "infrastructure:view permission is required"), nil
	}
	var args struct {
		Source   string `json:"source"`
		Search   string `json:"search"`
		Lookback int    `json:"lookback_minutes"`
		Limit    int    `json:"limit"`
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, invalid()
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return nil, invalid()
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, invalid()
	}
	if args.Source == "" && len(current.names) == 1 {
		args.Source = current.names[0]
	}
	source, ok := current.sources[args.Source]
	if !ok || current.name == "discover_log_fields" && args.Search != "" {
		return nil, invalid()
	}
	if source.Guard != nil && !source.Guard(ctx) {
		return core.UnavailableToolResult(current.name, "CloudWatch Logs source is no longer available"), nil
	}
	request := app.Request{Search: args.Search, LookbackMinutes: args.Lookback, Limit: args.Limit}
	var result app.Result
	var err error
	if current.name == "discover_log_fields" {
		result, err = source.Reader.Discover(ctx, request)
	} else {
		result, err = source.Reader.Read(ctx, request)
	}
	if err != nil {
		return nil, safe(err)
	}
	found := result.Count > 0
	if current.name == "discover_log_fields" {
		found = len(result.Fields) > 0
	}
	return &core.ToolResult{Tool: current.name, Found: found, Data: map[string]any{"source": source.Reader.Safe(args.Source), "signal": "logs", "result": result}}, nil
}

func invalid() error {
	return core.NewToolError(core.ToolErrorInvalidArguments, "invalid CloudWatch Logs tool arguments", nil)
}

func safe(err error) error {
	if errors.Is(err, app.ErrScope) {
		return invalid()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return core.NewToolError(core.ToolErrorTimeout, "CloudWatch Logs read exceeded its deadline", nil)
	}
	if errors.Is(err, context.Canceled) {
		return core.NewToolError(core.ToolErrorCancelled, "CloudWatch Logs read was cancelled", nil)
	}
	return core.NewToolError(core.ToolErrorBackend, "CloudWatch Logs read failed", nil)
}
