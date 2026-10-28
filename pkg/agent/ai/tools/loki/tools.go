package loki

// Package loki adapts configured Loki read services to existing log capabilities.

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	lokiapp "github.com/VersusControl/versus-incident/pkg/loki"
)

type Source struct {
	Name    string
	Service *lokiapp.Service
}

type tool struct {
	name     string
	services map[string]*lokiapp.Service
	sources  []string
}

func New(sources []Source) []core.Tool {
	reads := map[string]*lokiapp.Service{}
	discovery := map[string]*lokiapp.Service{}
	for _, source := range sources {
		if source.Name == "" || source.Service == nil {
			continue
		}
		reads[source.Name] = source.Service
		if source.Service.DiscoveryAvailable() {
			discovery[source.Name] = source.Service
		}
	}
	result := []core.Tool{}
	for _, definition := range []struct {
		name     string
		services map[string]*lokiapp.Service
	}{{"discover_log_fields", discovery}, {"read_log_records", reads}} {
		if len(definition.services) == 0 {
			continue
		}
		names := make([]string, 0, len(definition.services))
		for name := range definition.services {
			names = append(names, name)
		}
		sort.Strings(names)
		result = append(result, &tool{name: definition.name, services: definition.services, sources: names})
	}
	return result
}

func (current *tool) Name() string { return current.name }
func (current *tool) Description() string {
	if current.name == "discover_log_fields" {
		return "Discover bounded Loki label names or values within the configured selector scope (or source-wide on an unscoped source)."
	}
	return "Read bounded Loki log records using trusted source scope and validated label, service, and text filters."
}
func (current *tool) SourceNames() []string { return append([]string(nil), current.sources...) }
func (current *tool) AvailabilityCapability() (string, string, int) {
	return current.name, "logs", len(current.sources)
}
func (current *tool) AuthorizedTool(ctx context.Context) core.Tool {
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return nil
	}
	return current
}
func (current *tool) ArgsSchema() map[string]any {
	properties := map[string]any{
		"source": map[string]any{"type": "string", "enum": current.SourceNames()},
		"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": lokiapp.MaximumLimit},
		"offset": map[string]any{"type": "integer", "minimum": 0, "maximum": lokiapp.MaximumOffset},
	}
	if current.name == "read_log_records" {
		properties["offset"] = map[string]any{"type": "integer", "minimum": 0, "maximum": lokiapp.ToolPolicy().MaxRows - 1}
	}
	required := []string{}
	if len(current.sources) > 1 {
		required = append(required, "source")
	}
	if current.name == "discover_log_fields" {
		properties["label"] = map[string]any{"type": "string", "description": "Optional exact label name; returns its values instead of label names."}
		properties["search"] = map[string]any{"type": "string", "description": "Optional case-insensitive substring filter."}
	} else {
		properties["labels"] = map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Exact Loki stream label filters."}
		properties["service"] = map[string]any{"type": "string", "description": "Optional exact service label."}
		properties["search"] = map[string]any{"type": "string", "description": "Optional literal substring in log body."}
		properties["lookback_minutes"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 360}
	}
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}
func (current *tool) Invoke(ctx context.Context, raw json.RawMessage) (*core.ToolResult, error) {
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return core.UnavailableToolResult(current.name, "infrastructure:view permission is required"), nil
	}
	var args struct {
		Source   string            `json:"source"`
		Label    string            `json:"label"`
		Labels   map[string]string `json:"labels"`
		Service  string            `json:"service"`
		Search   string            `json:"search"`
		Lookback int               `json:"lookback_minutes"`
		Limit    int               `json:"limit"`
		Offset   int               `json:"offset"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&args); err != nil {
			return nil, invalid(err)
		}
	}
	if args.Source == "" && len(current.sources) == 1 {
		args.Source = current.sources[0]
	}
	service := current.services[args.Source]
	if service == nil {
		return nil, invalid(lokiapp.ErrInvalidArgument)
	}
	var data any
	var count int
	if current.name == "discover_log_fields" {
		if args.Labels != nil || args.Service != "" || args.Lookback != 0 {
			return nil, invalid(lokiapp.ErrInvalidArgument)
		}
		result, err := service.Discover(ctx, lokiapp.DiscoveryRequest{Label: args.Label, Search: args.Search, Limit: args.Limit, Offset: args.Offset})
		if err != nil {
			return nil, safe(err)
		}
		data, count = result, result.Count
	} else {
		if args.Label != "" || args.Lookback < 0 || args.Lookback > 360 {
			return nil, invalid(lokiapp.ErrInvalidArgument)
		}
		result, err := service.Read(ctx, lokiapp.ReadRequest{Labels: args.Labels, Service: args.Service, Search: args.Search, Lookback: time.Duration(args.Lookback) * time.Minute, Limit: args.Limit, Offset: args.Offset})
		if err != nil {
			return nil, safe(err)
		}
		data, count = result, result.Count
	}
	return &core.ToolResult{Tool: current.name, Found: count > 0, Data: map[string]any{"source": args.Source, "signal": "logs", "result": data}}, nil
}
func invalid(err error) error {
	return core.NewToolError(core.ToolErrorInvalidArguments, "invalid Loki tool arguments", err)
}
func safe(err error) error {
	if errors.Is(err, lokiapp.ErrInvalidArgument) || errors.Is(err, lokiapp.ErrUnsupportedScope) {
		return invalid(err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return core.NewToolError(core.ToolErrorTimeout, "Loki read exceeded its time bound", err)
	}
	if errors.Is(err, lokiapp.ErrResponseTooLarge) {
		return core.NewToolError(core.ToolErrorBackend, "Loki response exceeded its safe bound; narrow the time window", err)
	}
	return core.NewToolError(core.ToolErrorBackend, "Loki read failed", err)
}
