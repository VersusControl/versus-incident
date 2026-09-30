// Package prometheus exposes bounded Prometheus reads as model tools.
package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	prometheusapp "github.com/VersusControl/versus-incident/pkg/prometheus"
)

var toolNames = map[string]bool{"discover_metrics": true, "read_metric_series": true}

type Source struct {
	Name    string
	Service *prometheusapp.Service
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
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	return []core.Tool{
		&tool{name: "discover_metrics", display: "Discover metrics", description: "Discover bounded metric metadata and service evidence from a configured Prometheus source.", services: services, sourceNames: names},
		&tool{name: "read_metric_series", display: "Read metric series", description: "Read one bounded Prometheus metric for a required service and time window.", services: services, sourceNames: names},
	}
}

func FilterAuthorized(ctx context.Context, tools []core.Tool) []core.Tool {
	result := make([]core.Tool, 0, len(tools))
	for _, candidate := range tools {
		if candidate == nil || !toolNames[candidate.Name()] {
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
	name, display, description string
	services                   map[string]Source
	sourceNames                []string
}

func (current *tool) AuthorizedTool(ctx context.Context) core.Tool {
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return nil
	}
	services := make(map[string]Source, len(current.services))
	names := make([]string, 0, len(current.sourceNames))
	for _, name := range current.sourceNames {
		source := current.services[name]
		if source.Guard(ctx) {
			services[name] = source
			names = append(names, name)
		}
	}
	if len(services) == 0 {
		return nil
	}
	return &tool{name: current.name, display: current.display, description: current.description, services: services, sourceNames: names}
}

func (tool *tool) Name() string        { return tool.name }
func (tool *tool) DisplayName() string { return tool.display }
func (tool *tool) Description() string { return tool.description }
func (tool *tool) SourceNames() []string {
	return append([]string(nil), tool.sourceNames...)
}
func (tool *tool) AvailabilityCapability() (string, string, int) {
	return tool.name, "metrics", len(tool.sourceNames)
}
func (tool *tool) ArgsSchema() map[string]any {
	properties := map[string]any{"source": map[string]any{"type": "string", "enum": append([]string(nil), tool.sourceNames...), "description": "Configured Prometheus source. Valid choices: " + strings.Join(tool.sourceNames, ", ") + "."}}
	required := []string{}
	if len(tool.sourceNames) > 1 {
		required = append(required, "source")
	}
	if tool.name == "discover_metrics" {
		properties["search"] = map[string]any{"type": "string", "maxLength": 256}
		properties["offset"] = map[string]any{"type": "integer", "minimum": 0, "maximum": prometheusapp.MaximumOffset}
		properties["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": prometheusapp.MaximumLimit}
	} else {
		properties["service"] = map[string]any{"type": "string", "maxLength": 256}
		properties["metric_name"] = map[string]any{"type": "string", "maxLength": 256}
		properties["lookback_minutes"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 360}
		properties["step_seconds"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 3600}
		required = append(required, "service", "metric_name")
	}
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

type arguments struct {
	Source          string `json:"source"`
	Search          string `json:"search"`
	Offset          int    `json:"offset"`
	Limit           int    `json:"limit"`
	Service         string `json:"service"`
	MetricName      string `json:"metric_name"`
	LookbackMinutes int    `json:"lookback_minutes"`
	StepSeconds     int    `json:"step_seconds"`
}

func (tool *tool) Invoke(ctx context.Context, raw json.RawMessage) (*core.ToolResult, error) {
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return core.UnavailableToolResult(tool.name, "infrastructure:view permission is required"), nil
	}
	var args arguments
	if len(raw) != 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidError(err)
		}
	}
	service, sourceName, err := tool.resolve(args.Source)
	if err != nil {
		return nil, invalidError(err)
	}
	if !service.Guard(ctx) {
		return core.UnavailableToolResult(tool.name, "Prometheus source is no longer available"), nil
	}
	data := map[string]any{"source": sourceName, "signal": "metrics"}
	found := false
	if tool.name == "discover_metrics" {
		result, readErr := service.Service.Discover(ctx, prometheusapp.DiscoveryRequest{Search: args.Search, Offset: args.Offset, Limit: args.Limit})
		if readErr != nil {
			return nil, safeError(readErr)
		}
		data["count"], data["offset"], data["limit"], data["truncated"], data["result"] = result.Count, result.Offset, result.Limit, result.Truncated, result.Metrics
		if result.NextOffset > 0 {
			data["next_offset"] = result.NextOffset
		}
		if len(result.Truncation) > 0 {
			data["truncation"] = result.Truncation
		}
		found = result.Count > 0
	} else {
		result, readErr := service.Service.Read(ctx, prometheusapp.ReadRequest{MetricName: args.MetricName, Service: args.Service, Lookback: time.Duration(args.LookbackMinutes) * time.Minute, Step: time.Duration(args.StepSeconds) * time.Second})
		if readErr != nil {
			return nil, safeError(readErr)
		}
		data["count"], data["datapoints"], data["step"], data["truncated"], data["result"] = result.Count, result.Datapoints, result.Step, result.Truncated, result.Series
		if len(result.Truncation) > 0 {
			data["truncation"] = result.Truncation
		}
		found = result.Count > 0
	}
	return &core.ToolResult{Tool: tool.name, Found: found, Data: data}, nil
}

func (tool *tool) resolve(name string) (Source, string, error) {
	if name == "" {
		if len(tool.sourceNames) != 1 {
			return Source{}, "", prometheusapp.ErrInvalidArgument
		}
		name = tool.sourceNames[0]
	}
	service, ok := tool.services[name]
	if !ok {
		return Source{}, "", prometheusapp.ErrInvalidArgument
	}
	return service, name, nil
}

func invalidError(err error) error {
	return core.NewToolError(core.ToolErrorInvalidArguments, "invalid Prometheus tool arguments", err)
}
func safeError(err error) error {
	if errors.Is(err, prometheusapp.ErrInvalidArgument) || errors.Is(err, prometheusapp.ErrInvalidConfig) {
		return invalidError(err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return core.NewToolError(core.ToolErrorTimeout, "Prometheus read exceeded its time bound", err)
	}
	if errors.Is(err, context.Canceled) {
		return core.NewToolError(core.ToolErrorCancelled, "Prometheus read was cancelled", err)
	}
	if errors.Is(err, prometheusapp.ErrResponseTooLarge) {
		return core.NewToolError(core.ToolErrorBackend, "Prometheus response exceeded its safe bound; narrow the service or time window", err)
	}
	return core.NewToolError(core.ToolErrorBackend, "Prometheus read failed", err)
}
