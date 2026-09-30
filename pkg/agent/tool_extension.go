package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// ToolContributor builds read-only runtime tools from trusted configuration.
// External modules use it to add licensed tools without importing private code
// into OSS. Implementations must enforce their own entitlement and org scope.
type ToolContributor func(tenancy.OrgScope, []config.AgentSourceConfig, core.Scrubber) ([]core.Tool, []error)

var toolContributors = struct {
	sync.RWMutex
	items map[string]ToolContributor
}{items: map[string]ToolContributor{}}

// RegisterToolContributor installs or removes one named runtime contributor.
func RegisterToolContributor(name string, contributor ToolContributor) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	toolContributors.Lock()
	defer toolContributors.Unlock()
	if contributor == nil {
		delete(toolContributors.items, name)
		return
	}
	toolContributors.items[name] = contributor
}

func contributedTools(scope tenancy.OrgScope, sources []config.AgentSourceConfig, scrubber core.Scrubber) ([]core.Tool, []error) {
	toolContributors.RLock()
	names := make([]string, 0, len(toolContributors.items))
	items := make(map[string]ToolContributor, len(toolContributors.items))
	for name, contributor := range toolContributors.items {
		names = append(names, name)
		items[name] = contributor
	}
	toolContributors.RUnlock()
	sort.Strings(names)
	var tools []core.Tool
	var errs []error
	seen := map[string]ownedTool{}
	for _, name := range names {
		built, buildErrs := items[name](scope.Normalized(), sources, scrubber)
		for _, err := range buildErrs {
			if err != nil {
				errs = append(errs, fmt.Errorf("tool contributor %s: %w", name, err))
			}
		}
		for _, candidate := range built {
			if candidate == nil {
				continue
			}
			if existing, duplicate := seen[candidate.Name()]; duplicate {
				if existing.owner == name {
					errs = append(errs, fmt.Errorf("tool %q registered more than once by %s", candidate.Name(), name))
					continue
				}
				combined, err := CombineSourceRoutedTools(existing.tool, candidate)
				if err != nil {
					errs = append(errs, fmt.Errorf("tool %q registered by both %s and %s: %w", candidate.Name(), existing.owner, name, err))
					continue
				}
				seen[candidate.Name()] = ownedTool{owner: existing.owner + "+" + name, tool: combined}
				for index := range tools {
					if tools[index].Name() == candidate.Name() {
						tools[index] = combined
						break
					}
				}
				continue
			}
			seen[candidate.Name()] = ownedTool{owner: name, tool: candidate}
			tools = append(tools, candidate)
		}
	}
	return tools, errs
}

type ownedTool struct {
	owner string
	tool  core.Tool
}

type sourceRoutedAggregate struct {
	name        string
	description string
	signal      string
	tools       map[string]core.Tool
	sources     []string
	schema      map[string]any
}

// CombineSourceRoutedTools combines two providers of one existing capability.
// Dispatch remains explicit through the model-provided source argument.
func CombineSourceRoutedTools(left, right core.Tool) (core.Tool, error) {
	children := []core.Tool{left, right}
	if aggregate, ok := left.(*sourceRoutedAggregate); ok {
		children = append([]core.Tool(nil), aggregate.uniqueTools()...)
		children = append(children, right)
	}
	owners := map[string]core.Tool{}
	for _, child := range children {
		routed, ok := child.(core.SourceRoutedTool)
		if !ok {
			return nil, fmt.Errorf("capability is not source-routable")
		}
		for _, source := range routed.SourceNames() {
			if _, duplicate := owners[source]; duplicate {
				return nil, fmt.Errorf("source %q is ambiguous", source)
			}
			owners[source] = child
		}
	}
	sources := make([]string, 0, len(owners))
	for source := range owners {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	signal := ""
	if reporter, ok := left.(interface {
		AvailabilityCapability() (string, string, int)
	}); ok {
		_, signal, _ = reporter.AvailabilityCapability()
	}
	schema := mergedSourceSchema(children, sources, signal == "logs")
	description := left.Description()
	switch signal {
	case "logs":
		switch left.Name() {
		case "discover_log_fields":
			description = "Discover bounded log fields within the selected source's supported scope; available arguments depend on the source."
		case "read_log_records":
			description = "Read bounded log records from the selected source; available filters depend on the source."
		}
	case "metrics":
		switch left.Name() {
		case "discover_metrics":
			description = "Discover bounded metrics within the selected source's supported scope; available arguments depend on the source."
		case "read_metric_series":
			description = "Read bounded metric series from the selected source; available arguments depend on the source."
		}
	}
	return &sourceRoutedAggregate{name: left.Name(), description: description, signal: signal, tools: owners, sources: sources, schema: schema}, nil
}

func (tool *sourceRoutedAggregate) Name() string        { return tool.name }
func (tool *sourceRoutedAggregate) Description() string { return tool.description }
func (tool *sourceRoutedAggregate) ArgsSchema() map[string]any {
	return tool.schema
}
func (tool *sourceRoutedAggregate) SourceNames() []string {
	return append([]string(nil), tool.sources...)
}
func (tool *sourceRoutedAggregate) AvailabilityCapability() (string, string, int) {
	return tool.name, tool.signal, len(tool.sources)
}
func (tool *sourceRoutedAggregate) AuthorizedTool(ctx context.Context) core.Tool {
	children := tool.uniqueTools()
	authorized := make([]core.Tool, 0, len(children))
	for _, child := range children {
		if filter, ok := child.(core.ContextAuthorizedTool); ok {
			child = filter.AuthorizedTool(ctx)
		}
		if child != nil {
			authorized = append(authorized, child)
		}
	}
	if len(authorized) == 0 {
		return nil
	}
	if len(authorized) == 1 {
		return authorized[0]
	}
	combined := authorized[0]
	for _, child := range authorized[1:] {
		var err error
		combined, err = CombineSourceRoutedTools(combined, child)
		if err != nil {
			return nil
		}
	}
	return combined
}
func (tool *sourceRoutedAggregate) Invoke(ctx context.Context, raw json.RawMessage) (*core.ToolResult, error) {
	var route struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(raw, &route); err != nil {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, "invalid tool arguments", err)
	}
	if route.Source == "" && len(tool.sources) == 1 {
		route.Source = tool.sources[0]
	}
	child := tool.tools[route.Source]
	if child == nil {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, "a valid source is required", nil)
	}
	var arguments map[string]json.RawMessage
	if err := json.Unmarshal(raw, &arguments); err != nil || arguments == nil {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, "invalid tool arguments", err)
	}
	properties, _ := child.ArgsSchema()["properties"].(map[string]any)
	for name := range arguments {
		if name == "source" {
			continue
		}
		if _, allowed := properties[name]; !allowed {
			return nil, core.NewToolError(core.ToolErrorInvalidArguments, "argument is not applicable to this source", nil)
		}
	}
	return child.Invoke(ctx, raw)
}

func (tool *sourceRoutedAggregate) uniqueTools() []core.Tool {
	seen := map[core.Tool]bool{}
	result := make([]core.Tool, 0, len(tool.tools))
	for _, source := range tool.sources {
		child := tool.tools[source]
		if !seen[child] {
			seen[child] = true
			result = append(result, child)
		}
	}
	return result
}

func mergedSourceSchema(tools []core.Tool, sources []string, mergeLogBounds bool) map[string]any {
	properties := map[string]any{}
	requiredCounts := map[string]int{}
	for _, tool := range tools {
		schema := tool.ArgsSchema()
		if childProperties, ok := schema["properties"].(map[string]any); ok {
			for name, value := range childProperties {
				if previous, exists := properties[name]; exists && mergeLogBounds && name != "source" {
					if first, ok := previous.(map[string]any); ok {
						if next, ok := value.(map[string]any); ok && first["type"] == next["type"] {
							merged := make(map[string]any, len(next)+1)
							for key, item := range next {
								merged[key] = item
							}
							for _, bound := range []string{"minimum", "maximum"} {
								left, leftOK := numericBound(first[bound])
								right, rightOK := numericBound(next[bound])
								if leftOK && rightOK && (bound == "minimum" && left < right || bound == "maximum" && left > right) {
									merged[bound] = first[bound]
								}
							}
							merged["description"] = "Interpretation and valid range depend on the selected source."
							value = merged
						}
					}
				}
				properties[name] = value
			}
		}
		if childRequired, ok := schema["required"].([]string); ok {
			for _, name := range childRequired {
				if name != "source" {
					requiredCounts[name]++
				}
			}
		}
	}
	properties["source"] = map[string]any{"type": "string", "enum": append([]string(nil), sources...), "description": "Configured source. Valid choices: " + strings.Join(sources, ", ") + "."}
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	var requiredNames []string
	if len(sources) > 1 {
		requiredNames = append(requiredNames, "source")
	}
	for name, count := range requiredCounts {
		if count == len(tools) {
			requiredNames = append(requiredNames, name)
		}
	}
	if len(requiredNames) > 0 {
		sort.Strings(requiredNames)
		schema["required"] = requiredNames
	}
	return schema
}

func numericBound(value any) (float64, bool) {
	switch number := value.(type) {
	case int:
		return float64(number), true
	case float64:
		return number, true
	default:
		return 0, false
	}
}
