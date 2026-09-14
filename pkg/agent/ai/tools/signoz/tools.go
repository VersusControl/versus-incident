// Package signoz exposes bounded SigNoz reads as model tools.
package signoz

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	signozapp "github.com/VersusControl/versus-incident/pkg/signoz"
)

var toolNames = []string{
	"discover_log_fields", "read_log_records",
	"discover_trace_fields", "read_trace_spans",
	"discover_metrics", "read_metric_series",
}

// Source binds one trusted configured source name and kind to its service.
type Source struct {
	Name    string
	Kind    signozapp.Signal
	Service *signozapp.Service
}

// New constructs only the tools supported by the supplied trusted source kinds.
func New(sources []Source) []core.Tool {
	byKind := map[signozapp.Signal]map[string]*signozapp.Service{}
	for _, source := range sources {
		if source.Name == "" || source.Service == nil {
			continue
		}
		if source.Kind != signozapp.SignalLogs && source.Kind != signozapp.SignalTraces && source.Kind != signozapp.SignalMetrics {
			continue
		}
		if byKind[source.Kind] == nil {
			byKind[source.Kind] = map[string]*signozapp.Service{}
		}
		byKind[source.Kind][source.Name] = source.Service
	}
	var tools []core.Tool
	for _, definition := range definitions {
		services := byKind[definition.kind]
		if definition.discovery {
			services = discoveryServices(services)
		}
		if len(services) == 0 {
			continue
		}
		names := make([]string, 0, len(services))
		for name := range services {
			names = append(names, name)
		}
		sort.Strings(names)
		tools = append(tools, &tool{definition: definition, services: services, sourceNames: names})
	}
	return tools
}

// FilterAuthorized removes SigNoz tools from unauthorized model catalogs.
func FilterAuthorized(ctx context.Context, tools []core.Tool) []core.Tool {
	if core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return tools
	}
	result := make([]core.Tool, 0, len(tools))
	for _, candidate := range tools {
		if candidate == nil || !isTool(candidate.Name()) {
			result = append(result, candidate)
		}
	}
	return result
}

type action int

const (
	actionFields action = iota
	actionSearch
	actionMetrics
	actionMetricQuery
)

type definition struct {
	name, display, description string
	kind                       signozapp.Signal
	action                     action
	discovery                  bool
}

var definitions = []definition{
	{name: "discover_log_fields", display: "Discover log fields", description: "Discover bounded source-wide log fields on an unscoped configured source.", kind: signozapp.SignalLogs, action: actionFields, discovery: true},
	{name: "read_log_records", display: "Read log records", description: "Read bounded log records using service, severity, text, and time filters.", kind: signozapp.SignalLogs, action: actionSearch},
	{name: "discover_trace_fields", display: "Discover trace fields", description: "Discover bounded source-wide span fields on an unscoped configured source.", kind: signozapp.SignalTraces, action: actionFields, discovery: true},
	{name: "read_trace_spans", display: "Read trace spans", description: "Read bounded spans using service, operation, error, and time filters.", kind: signozapp.SignalTraces, action: actionSearch},
	{name: "discover_metrics", display: "Discover metrics", description: "Discover bounded source-wide metric metadata on an unscoped configured source.", kind: signozapp.SignalMetrics, action: actionMetrics, discovery: true},
	{name: "read_metric_series", display: "Read metric series", description: "Read one bounded metric time series for a service and time window.", kind: signozapp.SignalMetrics, action: actionMetricQuery},
}

type tool struct {
	definition  definition
	services    map[string]*signozapp.Service
	sourceNames []string
}

func (tool *tool) Name() string        { return tool.definition.name }
func (tool *tool) DisplayName() string { return tool.definition.display }
func (tool *tool) Description() string { return tool.definition.description }
func (tool *tool) AvailabilityCapability() (string, string, int) {
	return tool.definition.name, string(tool.definition.kind), len(tool.sourceNames)
}
func (tool *tool) ArgsSchema() map[string]any {
	properties := map[string]any{
		"source": map[string]any{"type": "string", "enum": append([]string(nil), tool.sourceNames...), "description": "Configured SigNoz source. Valid choices: " + strings.Join(tool.sourceNames, ", ") + "."},
		"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": signozapp.MaximumLimit},
	}
	switch tool.definition.action {
	case actionFields:
		properties["search"] = stringProperty("Optional field-name substring.")
		properties["field_context"] = map[string]any{"type": "string", "enum": []string{"resource", "attribute", "scope", "log", "span", "metric", "body"}}
	case actionSearch:
		properties["service"] = stringProperty("Required service name that is ANDed with the configured source scope.")
		properties["lookback_minutes"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 360}
		properties["offset"] = map[string]any{"type": "integer", "minimum": 0, "maximum": signozapp.MaximumOffset}
		if tool.definition.kind == signozapp.SignalLogs {
			properties["search"] = stringProperty("Optional text contained in the log body.")
			properties["severity"] = stringProperty("Optional exact severity_text value.")
		} else {
			properties["operation"] = stringProperty("Optional exact span operation name.")
			properties["error"] = map[string]any{"type": "boolean"}
		}
	case actionMetrics:
		properties["search"] = stringProperty("Optional metric-name substring.")
	case actionMetricQuery:
		properties["service"] = stringProperty("Required service name that is ANDed with the configured source scope.")
		properties["metric_name"] = stringProperty("Exact metric name returned by discover_metrics.")
		properties["lookback_minutes"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 360}
		properties["step_seconds"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 3600}
	}
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	required := []string{}
	if len(tool.sourceNames) > 1 {
		required = append(required, "source")
	}
	if tool.definition.action == actionSearch {
		required = append(required, "service")
	}
	if tool.definition.action == actionMetricQuery {
		required = append(required, "service", "metric_name")
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func (tool *tool) Invoke(ctx context.Context, raw json.RawMessage) (*core.ToolResult, error) {
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return core.UnavailableToolResult(tool.Name(), "infrastructure:view permission is required"), nil
	}
	var args arguments
	if len(raw) != 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidError(err)
		}
	}
	service, source, err := tool.resolve(args.Source)
	if err != nil {
		return nil, invalidError(err)
	}
	var result signozapp.Result
	switch tool.definition.action {
	case actionFields:
		result, err = service.FieldKeys(ctx, tool.definition.kind, signozapp.FieldRequest{SearchText: args.Search, Context: args.FieldContext})
	case actionSearch:
		if strings.TrimSpace(args.Service) == "" {
			return nil, invalidError(signozapp.ErrInvalidArgument)
		}
		end := time.Now().UTC()
		lookback := boundedLookback(args.LookbackMinutes)
		request := signozapp.SearchRequest{Start: end.Add(-lookback), End: end, Service: args.Service, SearchText: args.Search, Severity: args.Severity, Operation: args.Operation, Offset: args.Offset, Limit: args.Limit}
		if args.Error != nil {
			request.ErrorOnly = args.Error
		}
		if tool.definition.kind == signozapp.SignalLogs {
			result, err = service.SearchLogs(ctx, request)
		} else {
			result, err = service.SearchTraces(ctx, request)
		}
	case actionMetrics:
		result, err = service.ListMetrics(ctx, args.Search, args.Limit)
	case actionMetricQuery:
		if strings.TrimSpace(args.Service) == "" || strings.TrimSpace(args.MetricName) == "" {
			return nil, invalidError(signozapp.ErrInvalidArgument)
		}
		end := time.Now().UTC()
		step := time.Duration(args.StepSeconds) * time.Second
		if step == 0 {
			step = time.Minute
		}
		result, err = service.QueryMetrics(ctx, signozapp.MetricRequest{Start: end.Add(-boundedLookback(args.LookbackMinutes)), End: end, Service: args.Service, MetricName: args.MetricName, TimeAggregation: "avg", SpaceAggregation: "avg", Step: step, Limit: args.Limit})
	}
	if err != nil {
		return nil, safeError(err)
	}
	data := map[string]any{"source": source, "signal": string(tool.definition.kind), "count": result.Count, "truncated": result.Truncated, "limit": result.Limit, "result": result.Data}
	if len(result.Truncation) > 0 {
		data["truncation"] = result.Truncation
	}
	if result.Offset > 0 {
		data["offset"] = result.Offset
	}
	return &core.ToolResult{Tool: tool.Name(), Found: result.Count > 0, Data: data}, nil
}

func discoveryServices(services map[string]*signozapp.Service) map[string]*signozapp.Service {
	result := make(map[string]*signozapp.Service)
	for name, service := range services {
		if service.DiscoveryAvailable() {
			result[name] = service
		}
	}
	return result
}

func (tool *tool) resolve(name string) (*signozapp.Service, string, error) {
	if name == "" {
		if len(tool.sourceNames) != 1 {
			return nil, "", signozapp.ErrInvalidArgument
		}
		name = tool.sourceNames[0]
	}
	service, ok := tool.services[name]
	if !ok {
		return nil, "", signozapp.ErrInvalidArgument
	}
	return service, name, nil
}

type arguments struct {
	Source          string `json:"source"`
	Service         string `json:"service"`
	Search          string `json:"search"`
	Severity        string `json:"severity"`
	Operation       string `json:"operation"`
	Error           *bool  `json:"error"`
	FieldContext    string `json:"field_context"`
	MetricName      string `json:"metric_name"`
	LookbackMinutes int    `json:"lookback_minutes"`
	StepSeconds     int    `json:"step_seconds"`
	Offset          int    `json:"offset"`
	Limit           int    `json:"limit"`
}

func boundedLookback(minutes int) time.Duration {
	if minutes <= 0 {
		minutes = 60
	}
	if minutes > 360 {
		minutes = 360
	}
	return time.Duration(minutes) * time.Minute
}
func stringProperty(description string) map[string]any {
	return map[string]any{"type": "string", "maxLength": 512, "description": description}
}
func isTool(name string) bool {
	for _, candidate := range toolNames {
		if candidate == name {
			return true
		}
	}
	return false
}
func invalidError(err error) error {
	return core.NewToolError(core.ToolErrorInvalidArguments, "invalid SigNoz tool arguments", err)
}
func safeError(err error) error {
	if errors.Is(err, signozapp.ErrInvalidArgument) || errors.Is(err, signozapp.ErrInvalidConfig) {
		return invalidError(err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return core.NewToolError(core.ToolErrorTimeout, "SigNoz read exceeded its time bound", err)
	}
	if errors.Is(err, signozapp.ErrResponseTooLarge) {
		return core.NewToolError(core.ToolErrorBackend, "SigNoz response exceeded its safe bound; narrow the service or time window", err)
	}
	return core.NewToolError(core.ToolErrorBackend, "SigNoz read failed", err)
}
