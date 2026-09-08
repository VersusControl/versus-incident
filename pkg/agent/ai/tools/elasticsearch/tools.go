// Package elasticsearch exposes bounded log-source operations as model tools.
package elasticsearch

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/core"
	elasticsearchapp "github.com/VersusControl/versus-incident/pkg/elasticsearch"
)

var toolNames = []string{"list_log_indices", "get_log_mappings", "search_logs", "get_log_shards"}

// Source binds a stable configured source name to its application service.
type Source struct {
	Name    string
	Service *elasticsearchapp.Service
}

// New constructs the complete tool bundle over healthy, constructible sources.
func New(sources []Source) []core.Tool {
	services := make(map[string]*elasticsearchapp.Service, len(sources))
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
	result := make([]core.Tool, 0, len(toolNames))
	for _, name := range toolNames {
		result = append(result, &tool{name: name, services: services, sourceNames: names})
	}
	return result
}

// FilterAuthorized removes Elasticsearch tools from unauthorized model catalogs.
func FilterAuthorized(ctx context.Context, tools []core.Tool) []core.Tool {
	if core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return tools
	}
	result := make([]core.Tool, 0, len(tools))
	for _, candidate := range tools {
		if candidate == nil || !isElasticsearchTool(candidate.Name()) {
			result = append(result, candidate)
		}
	}
	return result
}

func isElasticsearchTool(name string) bool {
	for _, candidate := range toolNames {
		if candidate == name {
			return true
		}
	}
	return false
}

type tool struct {
	name        string
	services    map[string]*elasticsearchapp.Service
	sourceNames []string
}

func (tool *tool) Name() string { return tool.name }

func (tool *tool) DisplayName() string { return displayNames[tool.name] }

func (tool *tool) Description() string { return descriptions[tool.name] }

func (tool *tool) ArgsSchema() map[string]any {
	properties := map[string]any{
		"source": map[string]any{"type": "string", "maxLength": 128, "enum": append([]string(nil), tool.sourceNames...), "description": "Configured Elasticsearch source. Valid choices: " + strings.Join(tool.sourceNames, ", ") + "."},
		"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": elasticsearchapp.MaximumLimit, "description": limitDescription(tool.name)},
	}
	if tool.name == "search_logs" {
		properties["query_body"] = map[string]any{"type": "object", "description": "Bounded Query DSL using allowlisted fields in query, sort, and _source only. Expensive query families are rejected."}
		properties["fields"] = map[string]any{"type": "array", "maxItems": elasticsearchapp.MaximumFields, "description": "Allowlisted source fields to return.", "items": map[string]any{"type": "string", "maxLength": 512}}
	}
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(tool.sourceNames) > 1 {
		schema["required"] = []string{"source"}
	}
	return schema
}

func (tool *tool) Invoke(ctx context.Context, raw json.RawMessage) (*core.ToolResult, error) {
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return core.UnavailableToolResult(tool.name, "infrastructure:view permission is required"), nil
	}
	var args arguments
	if len(raw) != 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, core.NewToolError(core.ToolErrorInvalidArguments, "invalid log tool arguments", err)
		}
	}
	service, err := tool.resolve(args.Source)
	if err != nil {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, "log source is unknown or ambiguous", err)
	}
	var value any
	switch tool.name {
	case "list_log_indices":
		value, err = service.ListIndices(ctx, args.Limit)
	case "get_log_mappings":
		value, err = service.Mappings(ctx, args.Limit)
	case "search_logs":
		value, err = service.Search(ctx, elasticsearchapp.SearchOptions{QueryBody: args.QueryBody, Fields: args.Fields, Limit: args.Limit})
	case "get_log_shards":
		value, err = service.Shards(ctx, args.Limit)
	default:
		return nil, core.NewToolError(core.ToolErrorInternal, "unknown log tool", nil)
	}
	if err != nil {
		return nil, safeToolError(err)
	}
	data, err := resultMap(value)
	if err != nil {
		return nil, core.NewToolError(core.ToolErrorInternal, "log result unavailable", err)
	}
	found := resultCount(value) > 0
	data["source"] = resolvedSource(args.Source, tool.sourceNames)
	data["count"] = resultCount(value)
	return &core.ToolResult{Tool: tool.name, Found: found, Data: data}, nil
}

func (tool *tool) resolve(name string) (*elasticsearchapp.Service, error) {
	if name == "" {
		if len(tool.sourceNames) != 1 {
			return nil, elasticsearchapp.ErrInvalidArguments
		}
		name = tool.sourceNames[0]
	}
	service, ok := tool.services[name]
	if !ok {
		return nil, elasticsearchapp.ErrInvalidArguments
	}
	return service, nil
}

type arguments struct {
	Source    string          `json:"source"`
	Limit     int             `json:"limit"`
	QueryBody json.RawMessage `json:"query_body"`
	Fields    []string        `json:"fields"`
}

func resultMap(value any) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var data any
	if err := json.Unmarshal(encoded, &data); err != nil {
		return nil, err
	}
	if object, ok := data.(map[string]any); ok {
		return object, nil
	}
	return map[string]any{"items": data}, nil
}

func resultCount(value any) int {
	switch typed := value.(type) {
	case elasticsearchapp.IndexResult:
		return len(typed.Items)
	case elasticsearchapp.MappingResult:
		return len(typed.Items)
	case elasticsearchapp.ShardResult:
		return len(typed.Items)
	case elasticsearchapp.SearchResult:
		return len(typed.Hits)
	default:
		return 0
	}
}

func resolvedSource(requested string, names []string) string {
	if requested != "" {
		return requested
	}
	if len(names) == 1 {
		return names[0]
	}
	return ""
}

func safeToolError(err error) error {
	if errors.Is(err, elasticsearchapp.ErrInvalidArguments) || errors.Is(err, elasticsearchapp.ErrInvalidConfig) {
		return core.NewToolError(core.ToolErrorInvalidArguments, "invalid log tool arguments", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return core.NewToolError(core.ToolErrorTimeout, "log read exceeded its bound", err)
	}
	if errors.Is(err, elasticsearchapp.ErrResponseTooLarge) {
		return core.NewToolError(core.ToolErrorBackend, "log response exceeded its safe bound; narrow the configured index scope or query", err)
	}
	return core.NewToolError(core.ToolErrorBackend, "log read failed", err)
}

func limitDescription(name string) string {
	if name == "get_log_mappings" {
		return "Maximum indices to return. Each index has a separate fixed field budget."
	}
	return "Maximum items to return; result metadata reports truncation."
}

var descriptions = map[string]string{
	"list_log_indices": "List bounded log indices matching a configured log source.",
	"get_log_mappings": "Inspect mappings with bounded index and per-index field budgets; totals and truncation are reported.",
	"search_logs":      "Search allowlisted fields with bounded read-only Query DSL; total relation and truncation are reported.",
	"get_log_shards":   "Inspect bounded shard health for a configured log source.",
}

var displayNames = map[string]string{
	"list_log_indices": "Listing Elasticsearch log indices",
	"get_log_mappings": "Inspecting Elasticsearch log mappings",
	"search_logs":      "Searching Elasticsearch logs",
	"get_log_shards":   "Inspecting Elasticsearch shard health",
}
