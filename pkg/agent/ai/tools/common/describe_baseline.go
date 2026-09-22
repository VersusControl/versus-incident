package tools

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/baseline"
	"github.com/VersusControl/versus-incident/pkg/core"
)

const (
	baselineMinWindow = 5 * time.Minute
	baselineMaxWindow = 24 * time.Hour
	baselineTextLimit = 128
)

var baselineSelector = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

// DescribeBaseline reads learned expectations from a trusted scoped provider.
type DescribeBaseline struct {
	Provider core.BaselineProvider
	OrgID    string
	Now      func() time.Time
}

func (DescribeBaseline) Name() string        { return "describe_baseline" }
func (DescribeBaseline) DisplayName() string { return "Describing baseline" }
func (DescribeBaseline) Description() string {
	return "Describe bounded learned baseline expectations for one service and signal without returning raw observations or provider queries."
}
func (DescribeBaseline) ArgsSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"service": map[string]any{"type": "string", "minLength": 1, "maxLength": baselineTextLimit},
			"signal":  map[string]any{"type": "string", "minLength": 1, "maxLength": baselineTextLimit, "description": "Signal family such as logs, metrics, or traces; an exact log pattern ID also selects one pattern."},
			"window":  map[string]any{"type": "string", "description": "Current observation window from 5m through 24h."},
		},
		"required": []string{"service", "signal", "window"},
	}
}

type describeBaselineArgs struct {
	Service string `json:"service"`
	Signal  string `json:"signal"`
	Window  string `json:"window"`
}

func (tool DescribeBaseline) Invoke(ctx context.Context, raw json.RawMessage) (*core.ToolResult, error) {
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return core.UnavailableToolResult(tool.Name(), "infrastructure:view permission is required"), nil
	}
	var args describeBaselineArgs
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &args) != nil {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, "arguments must be valid JSON", nil)
	}
	args.Service = strings.TrimSpace(args.Service)
	args.Signal = strings.TrimSpace(args.Signal)
	if !validBaselineSelector(args.Service) || !validBaselineSelector(args.Signal) {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, "service and signal must be bounded identifiers", nil)
	}
	window, err := time.ParseDuration(strings.TrimSpace(args.Window))
	if err != nil || window < baselineMinWindow || window > baselineMaxWindow {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, "window must be a duration from 5m through 24h", err)
	}
	if tool.Provider == nil {
		return core.UnavailableToolResult(tool.Name(), "baseline provider is not configured"), nil
	}
	now := time.Now
	if tool.Now != nil {
		now = tool.Now
	}
	request := core.BaselineRequest{OrgID: tool.OrgID, Service: args.Service, Signal: args.Signal, Window: window, Limit: baseline.MaxRecords, At: now().UTC()}
	if args.Signal != "logs" && args.Signal != "metrics" && args.Signal != "traces" {
		request.PatternID = args.Signal
		request.Signal = "logs"
	}
	result, err := tool.Provider.DescribeBaselines(ctx, request)
	if err != nil {
		return nil, core.NewToolError(core.ToolErrorBackend, "baseline provider failed", err)
	}
	return &core.ToolResult{Tool: tool.Name(), Found: result.Found, Data: map[string]any{"service": args.Service, "signal": args.Signal, "window": window.String(), "baseline": result}}, nil
}

func validBaselineSelector(value string) bool {
	return len(value) > 0 && len(value) <= baselineTextLimit && baselineSelector.MatchString(value)
}

// AvailabilityCapability reports the constructed baseline surface.
func (tool DescribeBaseline) AvailabilityCapability() (string, string, int) {
	if tool.Provider == nil {
		return "baseline_provider", "", 0
	}
	return "baseline_provider", "", 1
}

// FilterBaselineAuthorized removes the baseline tool from unauthorized catalogs.
func FilterBaselineAuthorized(ctx context.Context, tools []core.Tool) []core.Tool {
	if core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return tools
	}
	result := make([]core.Tool, 0, len(tools))
	for _, candidate := range tools {
		if candidate == nil || candidate.Name() != (DescribeBaseline{}).Name() {
			result = append(result, candidate)
		}
	}
	return result
}
