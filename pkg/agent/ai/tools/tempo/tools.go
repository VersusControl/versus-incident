// Package tempo exposes bounded Tempo reads under existing trace capabilities.
package tempo

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	tempoapp "github.com/VersusControl/versus-incident/pkg/tempo"
)

type Source struct {
	Name    string
	Service *tempoapp.Service
	Guard   func(context.Context) bool
}

func New(sources []Source) []core.Tool {
	services := map[string]Source{}
	for _, source := range sources {
		if source.Name != "" && source.Service != nil && source.Guard != nil {
			services[source.Name] = source
		}
	}
	if len(services) == 0 {
		return nil
	}
	names := make([]string, 0, len(services))
	discoveries := map[string]Source{}
	for name, source := range services {
		names = append(names, name)
		if source.Service.DiscoveryAvailable() {
			discoveries[name] = source
		}
	}
	sort.Strings(names)
	result := []core.Tool{}
	if len(discoveries) != 0 {
		discoveryNames := make([]string, 0, len(discoveries))
		for name := range discoveries {
			discoveryNames = append(discoveryNames, name)
		}
		sort.Strings(discoveryNames)
		result = append(result, &tool{name: "discover_trace_fields", description: "Discover bounded Tempo trace fields.", services: discoveries, sourceNames: discoveryNames})
	}
	return append(result, &tool{name: "read_trace_spans", description: "Read bounded Tempo spans using fixed selectors.", services: services, sourceNames: names})
}

func FilterAuthorized(ctx context.Context, tools []core.Tool) []core.Tool {
	result := make([]core.Tool, 0, len(tools))
	for _, candidate := range tools {
		if candidate == nil {
			continue
		}
		if candidate.Name() != "discover_trace_fields" && candidate.Name() != "read_trace_spans" {
			result = append(result, candidate)
			continue
		}
		if filter, ok := candidate.(core.ContextAuthorizedTool); ok {
			candidate = filter.AuthorizedTool(ctx)
		} else if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
			candidate = nil
		}
		if candidate != nil {
			result = append(result, candidate)
		}
	}
	return result
}

type tool struct {
	name, description string
	services          map[string]Source
	sourceNames       []string
}

func (current *tool) AuthorizedTool(ctx context.Context) core.Tool {
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return nil
	}
	services := map[string]Source{}
	names := []string{}
	for _, name := range current.sourceNames {
		if source := current.services[name]; source.Guard(ctx) {
			services[name] = source
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return &tool{name: current.name, description: current.description, services: services, sourceNames: names}
}

func (current *tool) Name() string { return current.name }
func (current *tool) DisplayName() string {
	if current.name == "discover_trace_fields" {
		return "Discover trace fields"
	}
	return "Read trace spans"
}
func (current *tool) Description() string   { return current.description }
func (current *tool) SourceNames() []string { return append([]string(nil), current.sourceNames...) }
func (current *tool) AvailabilityCapability() (string, string, int) {
	return current.name, "traces", len(current.sourceNames)
}
func (current *tool) ArgsSchema() map[string]any {
	properties := map[string]any{
		"source": map[string]any{"type": "string", "enum": append([]string(nil), current.sourceNames...), "description": "Configured Tempo source: " + strings.Join(current.sourceNames, ", ")},
		"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": tempoapp.MaximumLimit},
		"offset": map[string]any{"type": "integer", "minimum": 0, "maximum": tempoapp.MaximumOffset},
	}
	required := []string{}
	if len(current.sourceNames) > 1 {
		required = append(required, "source")
	}
	if current.name == "discover_trace_fields" {
		properties["search"] = map[string]any{"type": "string", "maxLength": 256}
	} else {
		properties["service"] = map[string]any{"type": "string", "maxLength": 256}
		properties["trace_id"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 32, "description": "Optional exact trace ID for stable trace-local pagination."}
		properties["operation"] = map[string]any{"type": "string", "maxLength": 256}
		properties["error"] = map[string]any{"type": "boolean"}
		properties["lookback_minutes"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 360}
		required = append(required, "service")
	}
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

type arguments struct {
	Source          string `json:"source"`
	Service         string `json:"service"`
	TraceID         string `json:"trace_id"`
	Operation       string `json:"operation"`
	Search          string `json:"search"`
	Error           *bool  `json:"error"`
	Offset          int    `json:"offset"`
	Limit           int    `json:"limit"`
	LookbackMinutes int    `json:"lookback_minutes"`
}

func (current *tool) Invoke(ctx context.Context, raw json.RawMessage) (*core.ToolResult, error) {
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return core.UnavailableToolResult(current.name, "infrastructure:view permission is required"), nil
	}
	var args arguments
	if len(raw) != 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidError(err)
		}
	}
	name := args.Source
	if name == "" && len(current.sourceNames) == 1 {
		name = current.sourceNames[0]
	}
	source, ok := current.services[name]
	if !ok {
		return nil, invalidError(tempoapp.ErrInvalidArgument)
	}
	if !source.Guard(ctx) {
		return core.UnavailableToolResult(current.name, "Tempo source is no longer available"), nil
	}
	data := map[string]any{"source": name, "signal": "traces"}
	found := false
	if current.name == "discover_trace_fields" {
		result, err := source.Service.Discover(ctx, tempoapp.FieldRequest{Search: args.Search, Offset: args.Offset, Limit: args.Limit})
		if err != nil {
			return nil, safeError(err)
		}
		data["result"], data["count"], data["offset"], data["limit"], data["truncated"] = result.Fields, result.Count, result.Offset, result.Limit, result.Truncated
		if result.NextOffset > 0 {
			data["next_offset"] = result.NextOffset
		}
		if len(result.Truncation) > 0 {
			data["truncation"] = result.Truncation
		}
		found = result.Count > 0
	} else {
		if args.LookbackMinutes < 0 || args.LookbackMinutes > 360 {
			return nil, invalidError(tempoapp.ErrInvalidArgument)
		}
		result, err := source.Service.Read(ctx, tempoapp.ReadRequest{Service: args.Service, TraceID: args.TraceID, Operation: args.Operation, ErrorOnly: args.Error, Lookback: time.Duration(args.LookbackMinutes) * time.Minute, Offset: args.Offset, Limit: args.Limit})
		if err != nil {
			return nil, safeError(err)
		}
		data["result"], data["count"], data["traces"], data["offset"], data["limit"], data["truncated"] = result.Spans, result.Count, result.Traces, result.Offset, result.Limit, result.Truncated
		if result.NextOffset > 0 {
			data["next_offset"] = result.NextOffset
		}
		if len(result.Truncation) > 0 {
			data["truncation"] = result.Truncation
		}
		found = result.Count > 0
	}
	return &core.ToolResult{Tool: current.name, Found: found, Data: data}, nil
}

func invalidError(err error) error {
	return core.NewToolError(core.ToolErrorInvalidArguments, "invalid Tempo tool arguments", err)
}
func safeError(err error) error {
	if errors.Is(err, tempoapp.ErrInvalidArgument) || errors.Is(err, tempoapp.ErrInvalidConfig) {
		return invalidError(err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return core.NewToolError(core.ToolErrorTimeout, "Tempo read exceeded its time bound", err)
	}
	if errors.Is(err, context.Canceled) {
		return core.NewToolError(core.ToolErrorCancelled, "Tempo read was cancelled", err)
	}
	if errors.Is(err, tempoapp.ErrUnsupported) {
		return core.NewToolError(core.ToolErrorBackend, "Tempo read is unsupported for this source or response", err)
	}
	if errors.Is(err, tempoapp.ErrResponseTooLarge) {
		return core.NewToolError(core.ToolErrorBackend, "Tempo response exceeded its safe bound", err)
	}
	return core.NewToolError(core.ToolErrorBackend, "Tempo read failed", err)
}
