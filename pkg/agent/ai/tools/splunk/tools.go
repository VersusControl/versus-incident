package splunk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	splunkapp "github.com/VersusControl/versus-incident/pkg/splunk"
)

type Source struct {
	Name    string
	Service *splunkapp.Service
}

type tool struct {
	name     string
	services map[string]*splunkapp.Service
	sources  []string
}

func New(sources []Source) []core.Tool {
	services := map[string]*splunkapp.Service{}
	for _, source := range sources {
		if source.Name != "" && source.Service != nil {
			services[source.Name] = source.Service
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
		&tool{name: "discover_log_fields", services: services, sources: names},
		&tool{name: "read_log_records", services: services, sources: names},
	}
}

func (current *tool) Name() string { return current.name }
func (current *tool) Description() string {
	if current.name == "discover_log_fields" {
		return "Discover fields sampled from bounded recent Splunk records within the configured index."
	}
	return "Read bounded Splunk records within the configured index with exact field filters."
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
		"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": splunkapp.MaximumLimit},
	}
	if current.name == "discover_log_fields" {
		properties["search"] = map[string]any{"type": "string", "description": "Optional field-name substring."}
	} else {
		properties["filters"] = map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Exact allowlisted field filters; index is fixed by the operator."}
		properties["lookback_minutes"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 360}
	}
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(current.sources) > 1 {
		schema["required"] = []string{"source"}
	}
	return schema
}

func (current *tool) Invoke(ctx context.Context, raw json.RawMessage) (*core.ToolResult, error) {
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return core.UnavailableToolResult(current.name, "infrastructure:view permission is required"), nil
	}
	var args struct {
		Source   string            `json:"source"`
		Filters  map[string]string `json:"filters"`
		Search   string            `json:"search"`
		Lookback int               `json:"lookback_minutes"`
		Limit    int               `json:"limit"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&args) != nil {
			return nil, invalid()
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return nil, invalid()
		}
	}
	if args.Source == "" && len(current.sources) == 1 {
		args.Source = current.sources[0]
	}
	service := current.services[args.Source]
	if service == nil || args.Lookback < 0 || args.Lookback > 360 ||
		(current.name == "discover_log_fields" && (args.Filters != nil || args.Lookback != 0)) ||
		(current.name == "read_log_records" && args.Search != "") {
		return nil, invalid()
	}
	var data any
	var count int
	if current.name == "discover_log_fields" {
		result, err := service.Discover(ctx, splunkapp.DiscoveryRequest{Search: args.Search, Limit: args.Limit})
		if err != nil {
			return nil, safe(err)
		}
		data, count = result, result.Count
	} else {
		result, err := service.Read(ctx, splunkapp.ReadRequest{Filters: args.Filters, Lookback: time.Duration(args.Lookback) * time.Minute, Limit: args.Limit})
		if err != nil {
			return nil, safe(err)
		}
		data, count = result, result.Count
	}
	return &core.ToolResult{Tool: current.name, Found: count > 0, Data: map[string]any{"source": args.Source, "signal": "logs", "result": data}}, nil
}

func invalid() error {
	return core.NewToolError(core.ToolErrorInvalidArguments, "invalid Splunk tool arguments", splunkapp.ErrInvalidArgument)
}
func safe(err error) error {
	if errors.Is(err, splunkapp.ErrInvalidArgument) {
		return invalid()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return core.NewToolError(core.ToolErrorTimeout, "Splunk read exceeded its time bound", nil)
	}
	return core.NewToolError(core.ToolErrorBackend, "Splunk read failed", nil)
}
