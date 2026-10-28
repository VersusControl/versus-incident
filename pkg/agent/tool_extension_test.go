package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type extensionTool string

func (tool extensionTool) Name() string          { return string(tool) }
func (extensionTool) Description() string        { return "test" }
func (extensionTool) ArgsSchema() map[string]any { return map[string]any{"type": "object"} }
func (tool extensionTool) Invoke(context.Context, json.RawMessage) (*core.ToolResult, error) {
	return &core.ToolResult{Tool: tool.Name(), Found: true}, nil
}

type routedExtensionTool struct {
	name, source, signal string
}

func (tool routedExtensionTool) Name() string          { return tool.name }
func (routedExtensionTool) Description() string        { return "test routed provider" }
func (tool routedExtensionTool) SourceNames() []string { return []string{tool.source} }
func (tool routedExtensionTool) AvailabilityCapability() (string, string, int) {
	if tool.signal != "" {
		return tool.name, tool.signal, 1
	}
	return tool.name, "metrics", 1
}
func (tool routedExtensionTool) ArgsSchema() map[string]any {
	properties := map[string]any{"source": map[string]any{"type": "string", "enum": []string{tool.source}}, "limit": map[string]any{"type": "integer"}}
	if tool.signal == "traces" {
		if tool.source == "tempo-primary" {
			properties["trace_id"] = map[string]any{"type": "string"}
		} else {
			properties["field_context"] = map[string]any{"type": "string"}
		}
	}
	return map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
}
func (tool routedExtensionTool) Invoke(_ context.Context, raw json.RawMessage) (*core.ToolResult, error) {
	var args struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if args.Source != tool.source {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, "wrong source", nil)
	}
	return &core.ToolResult{Tool: tool.name, Found: true, Data: map[string]any{"source": tool.source}}, nil
}

type schemaRoutedTool struct {
	routedExtensionTool
	properties []string
	required   []string
	calls      *int
}

func (tool schemaRoutedTool) Description() string {
	if tool.source == "cloudwatch-primary" {
		return "CloudWatch metrics require namespace"
	}
	return tool.routedExtensionTool.Description()
}

func (tool schemaRoutedTool) ArgsSchema() map[string]any {
	properties := map[string]any{"source": map[string]any{"type": "string"}}
	for _, name := range tool.properties {
		properties[name] = map[string]any{"type": "string"}
	}
	return map[string]any{"type": "object", "properties": properties, "required": tool.required, "additionalProperties": false}
}

func (tool schemaRoutedTool) Invoke(ctx context.Context, raw json.RawMessage) (*core.ToolResult, error) {
	*tool.calls++
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	for _, name := range tool.required {
		value, _ := args[name].(string)
		if strings.TrimSpace(value) == "" {
			return nil, core.NewToolError(core.ToolErrorInvalidArguments, "required provider argument is missing", nil)
		}
	}
	return tool.routedExtensionTool.Invoke(ctx, raw)
}

func TestToolContributorsAreOrderedScopedAndRejectCollisions(t *testing.T) {
	RegisterToolContributor("z-test", func(scope tenancy.OrgScope, _ []config.AgentSourceConfig, _ core.Scrubber) ([]core.Tool, []error) {
		if scope.Write != "acme" {
			t.Fatalf("scope = %+v", scope)
		}
		return []core.Tool{extensionTool("shared")}, nil
	})
	RegisterToolContributor("a-test", func(tenancy.OrgScope, []config.AgentSourceConfig, core.Scrubber) ([]core.Tool, []error) {
		return []core.Tool{extensionTool("shared"), extensionTool("first")}, nil
	})
	t.Cleanup(func() { RegisterToolContributor("z-test", nil); RegisterToolContributor("a-test", nil) })
	tools, errs := contributedTools(tenancy.NewOrgScope("acme"), nil, nil)
	if len(tools) != 2 || tools[0].Name() != "shared" || tools[1].Name() != "first" {
		t.Fatalf("tools = %v", tools)
	}
	if len(errs) != 1 {
		t.Fatalf("errors = %v", errs)
	}
}

func TestToolContributorsAggregatePrometheusAndSigNozMetricsForChatAndAnalyze(t *testing.T) {
	capabilities := []string{"discover_metrics", "read_metric_series"}
	RegisterToolContributor("prometheus", func(tenancy.OrgScope, []config.AgentSourceConfig, core.Scrubber) ([]core.Tool, []error) {
		return []core.Tool{
			routedExtensionTool{name: capabilities[0], source: "prometheus-primary"},
			routedExtensionTool{name: capabilities[1], source: "prometheus-primary"},
		}, nil
	})
	RegisterToolContributor("signoz_metrics", func(tenancy.OrgScope, []config.AgentSourceConfig, core.Scrubber) ([]core.Tool, []error) {
		return []core.Tool{
			routedExtensionTool{name: capabilities[0], source: "signoz-primary"},
			routedExtensionTool{name: capabilities[1], source: "signoz-primary"},
		}, nil
	})
	t.Cleanup(func() {
		RegisterToolContributor("prometheus", nil)
		RegisterToolContributor("signoz_metrics", nil)
	})

	runtime, errs := contributedTools(tenancy.NewOrgScope("acme"), nil, nil)
	if len(errs) != 0 || len(runtime) != 2 {
		t.Fatalf("tools=%v errors=%v", runtime, errs)
	}
	snapshot := aitools.BindRuntimeCapabilities(aitools.Snapshot{DataSources: map[string]aitools.DependencyStatus{"metrics": {Configured: true, Count: 2}}}, runtime)
	manager := aitools.NewManager(storage.NewMemory())
	for _, agentKind := range []aitools.AgentKind{aitools.AgentChat, aitools.AgentAnalyze} {
		filtered, err := manager.Filter(tenancy.NewOrgScope("acme"), agentKind, runtime, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(filtered))
		for _, tool := range filtered {
			names = append(names, tool.Name())
		}
		if !slices.Equal(names, capabilities) {
			t.Fatalf("%s tools = %v", agentKind, names)
		}
	}
	for _, tool := range runtime {
		routed := tool.(core.SourceRoutedTool)
		if got := routed.SourceNames(); !slices.Equal(got, []string{"prometheus-primary", "signoz-primary"}) {
			t.Fatalf("%s sources = %v", tool.Name(), got)
		}
		for _, source := range routed.SourceNames() {
			result, err := tool.Invoke(context.Background(), json.RawMessage(`{"source":"`+source+`"}`))
			if err != nil || result.Data["source"] != source {
				t.Fatalf("%s source %q result=%+v err=%v", tool.Name(), source, result, err)
			}
		}
	}
}

func TestCombinedMetricSchemasAndProviderDispatch(t *testing.T) {
	for _, capability := range []string{"discover_metrics", "read_metric_series"} {
		t.Run(capability, func(t *testing.T) {
			prometheusCalls, signozCalls, cloudwatchCalls := 0, 0, 0
			prometheus := schemaRoutedTool{routedExtensionTool: routedExtensionTool{name: capability, source: "prometheus-primary"}, calls: &prometheusCalls}
			signoz := schemaRoutedTool{routedExtensionTool: routedExtensionTool{name: capability, source: "signoz-primary"}, calls: &signozCalls}
			cloudwatch := schemaRoutedTool{routedExtensionTool: routedExtensionTool{name: capability, source: "cloudwatch-primary"}, calls: &cloudwatchCalls}
			wantRequired := []string{"source"}
			if capability == "discover_metrics" {
				prometheus.properties = []string{"search", "offset", "limit"}
				signoz.properties = []string{"search", "limit"}
				cloudwatch.properties = []string{"namespace", "search", "limit"}
				cloudwatch.required = []string{"namespace"}
			} else {
				prometheus.properties = []string{"service", "metric_name", "lookback_minutes", "step_seconds"}
				signoz.properties = []string{"service", "metric_name", "lookback_minutes", "step_seconds", "limit"}
				cloudwatch.properties = []string{"namespace", "service", "metric_name", "lookback_minutes", "step_seconds", "limit"}
				prometheus.required = []string{"service", "metric_name"}
				signoz.required = []string{"service", "metric_name"}
				cloudwatch.required = []string{"namespace", "service", "metric_name"}
				wantRequired = []string{"metric_name", "service", "source"}
			}
			if got := cloudwatch.Description(); got != "CloudWatch metrics require namespace" {
				t.Fatalf("single-provider description = %q", got)
			}
			combined, err := CombineSourceRoutedTools(&cloudwatch, &prometheus)
			if err != nil {
				t.Fatal(err)
			}
			combined, err = CombineSourceRoutedTools(combined, &signoz)
			if err != nil {
				t.Fatal(err)
			}
			wantDescription := "Discover bounded metrics within the selected source's supported scope; available arguments depend on the source."
			if capability == "read_metric_series" {
				wantDescription = "Read bounded metric series from the selected source; available arguments depend on the source."
			}
			if got := combined.Description(); got != wantDescription || strings.Contains(strings.ToLower(got), "namespace") {
				t.Fatalf("combined description = %q, want %q", got, wantDescription)
			}
			schema := combined.ArgsSchema()
			if got, _ := schema["required"].([]string); !slices.Equal(got, wantRequired) {
				t.Fatalf("required = %v, want %v", got, wantRequired)
			}
			properties := schema["properties"].(map[string]any)
			for _, name := range []string{"namespace", "offset", "step_seconds", "limit"} {
				if name == "offset" && capability != "discover_metrics" || name == "step_seconds" && capability != "read_metric_series" {
					continue
				}
				if _, ok := properties[name]; !ok {
					t.Fatalf("merged schema missing %s", name)
				}
			}
			base := `"search":"cpu"`
			if capability == "read_metric_series" {
				base = `"service":"api","metric_name":"cpu"`
			}
			for _, source := range []string{"prometheus-primary", "signoz-primary"} {
				args := `{"source":"` + source + `",` + base + `}`
				if _, err := combined.Invoke(context.Background(), json.RawMessage(args)); err != nil {
					t.Fatalf("%s rejected %s: %v", capability, args, err)
				}
				args = `{"source":"` + source + `",` + base + `,"namespace":"AWS/EC2"}`
				if _, err := combined.Invoke(context.Background(), json.RawMessage(args)); err == nil {
					t.Fatalf("accepted inapplicable namespace for %s", source)
				} else if code, _ := core.ClassifyToolError(err); code != core.ToolErrorInvalidArguments {
					t.Fatalf("namespace rejection: %v", err)
				}
			}
			if prometheusCalls != 1 || signozCalls != 1 {
				t.Fatalf("provider calls prometheus=%d signoz=%d", prometheusCalls, signozCalls)
			}
			cloudwatchArgs := `{"source":"cloudwatch-primary",` + base + `}`
			if _, err := combined.Invoke(context.Background(), json.RawMessage(cloudwatchArgs)); err == nil {
				t.Fatal("CloudWatch accepted missing namespace")
			} else if code, _ := core.ClassifyToolError(err); code != core.ToolErrorInvalidArguments {
				t.Fatalf("namespace validation: %v", err)
			}
			if capability == "read_metric_series" {
				if _, err := combined.Invoke(context.Background(), json.RawMessage(`{"source":"cloudwatch-primary","namespace":"AWS/EC2","service":"api"}`)); err == nil {
					t.Fatal("CloudWatch accepted missing exact metric")
				}
			}
			cloudwatchArgs = `{"source":"cloudwatch-primary",` + base + `,"namespace":"AWS/EC2"}`
			if _, err := combined.Invoke(context.Background(), json.RawMessage(cloudwatchArgs)); err != nil {
				t.Fatalf("CloudWatch rejected valid request: %v", err)
			}
			if cloudwatchCalls != 2 && capability == "discover_metrics" || cloudwatchCalls != 3 && capability == "read_metric_series" {
				t.Fatalf("CloudWatch calls = %d", cloudwatchCalls)
			}
			providerArgs := []string{
				`{"source":"signoz-primary",` + base + `,"limit":1}`,
				`{"source":"cloudwatch-primary",` + base + `,"namespace":"AWS/EC2","limit":1}`,
			}
			if capability == "discover_metrics" {
				providerArgs = append(providerArgs, `{"source":"prometheus-primary",`+base+`,"offset":1}`)
			} else {
				providerArgs = append(providerArgs, `{"source":"prometheus-primary",`+base+`,"step_seconds":60}`)
			}
			for _, args := range providerArgs {
				if _, err := combined.Invoke(context.Background(), json.RawMessage(args)); err != nil {
					t.Fatalf("rejected applicable argument %s: %v", args, err)
				}
			}
			invalid := `{"source":"prometheus-primary",` + base + `,"limit":1}`
			if capability == "read_metric_series" {
				if _, err := combined.Invoke(context.Background(), json.RawMessage(invalid)); err == nil {
					t.Fatal("Prometheus accepted SigNoz-only limit")
				}
			} else {
				invalid = `{"source":"signoz-primary",` + base + `,"offset":1}`
				if _, err := combined.Invoke(context.Background(), json.RawMessage(invalid)); err == nil {
					t.Fatal("SigNoz accepted Prometheus-only offset")
				}
			}
		})
	}
}

func TestToolContributorsAggregateTempoAndSigNozTraces(t *testing.T) {
	capabilities := []string{"discover_trace_fields", "read_trace_spans"}
	for _, provider := range []struct{ key, source string }{{"tempo", "tempo-primary"}, {"signoz_traces", "signoz-primary"}} {
		provider := provider
		RegisterToolContributor(provider.key, func(tenancy.OrgScope, []config.AgentSourceConfig, core.Scrubber) ([]core.Tool, []error) {
			return []core.Tool{
				routedExtensionTool{name: capabilities[0], source: provider.source, signal: "traces"},
				routedExtensionTool{name: capabilities[1], source: provider.source, signal: "traces"},
			}, nil
		})
		t.Cleanup(func() { RegisterToolContributor(provider.key, nil) })
	}
	tools, errs := contributedTools(tenancy.NewOrgScope("acme"), nil, nil)
	if len(errs) != 0 || len(tools) != 2 {
		t.Fatalf("tools=%v errors=%v", tools, errs)
	}
	for _, tool := range tools {
		if got := tool.(core.SourceRoutedTool).SourceNames(); !slices.Equal(got, []string{"signoz-primary", "tempo-primary"}) {
			t.Fatalf("sources=%v", got)
		}
		for _, source := range []string{"signoz-primary", "tempo-primary"} {
			result, err := tool.Invoke(context.Background(), json.RawMessage(`{"source":"`+source+`"}`))
			if err != nil || result.Data["source"] != source {
				t.Fatalf("source=%s result=%+v err=%v", source, result, err)
			}
		}
		for _, invalid := range []string{`{"source":"signoz-primary","trace_id":"abcdef"}`, `{"source":"tempo-primary","field_context":"span"}`} {
			if _, err := tool.Invoke(context.Background(), json.RawMessage(invalid)); err == nil {
				t.Fatalf("accepted provider-inapplicable arguments %s: %v", invalid, err)
			} else if code, _ := core.ClassifyToolError(err); code != core.ToolErrorInvalidArguments {
				t.Fatalf("wrong rejection for %s: %v", invalid, err)
			}
		}
		for _, valid := range []string{`{"source":"signoz-primary","field_context":"span","limit":1}`, `{"source":"tempo-primary","trace_id":"abcdef","limit":1}`} {
			if _, err := tool.Invoke(context.Background(), json.RawMessage(valid)); err != nil {
				t.Fatalf("rejected provider arguments %s: %v", valid, err)
			}
		}
	}
}

func TestToolContributorRejectsDuplicateSourceWithinProvider(t *testing.T) {
	RegisterToolContributor("prometheus", func(tenancy.OrgScope, []config.AgentSourceConfig, core.Scrubber) ([]core.Tool, []error) {
		return []core.Tool{
			routedExtensionTool{name: "discover_metrics", source: "same"},
			routedExtensionTool{name: "discover_metrics", source: "same"},
		}, nil
	})
	t.Cleanup(func() { RegisterToolContributor("prometheus", nil) })
	tools, errs := contributedTools(tenancy.NewOrgScope("acme"), nil, nil)
	if len(tools) != 1 || len(errs) != 1 {
		t.Fatalf("tools=%v errors=%v", tools, errs)
	}
}
