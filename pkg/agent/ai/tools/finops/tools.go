// Package finops exposes the FinOps service to the agents as read-only
// tools. Every tool calls the same finops.Service method as the matching
// admin route and reads the cost cache only: no tool calls a cloud billing
// API or spends a paid request. Writes (mark expected, dismiss, budgets,
// refresh, reports, export) stay in the admin UI.
package finops

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/core"
	core_finops "github.com/VersusControl/versus-incident/pkg/finops"
)

// MaxModelViewBytes bounds one tool result for the model.
const MaxModelViewBytes = 6 << 10

// ServiceFunc returns the read-only FinOps service over the cost cache, or
// nil when cost data is not set up. Tools hold only the Reader, so they
// cannot reach a write method.
type ServiceFunc func() core_finops.Reader

// New builds every FinOps tool over one service source.
func New(service ServiceFunc) []core.Tool {
	if service == nil {
		return nil
	}
	out := make([]core.Tool, 0, len(toolNames))
	for _, name := range toolNames {
		out = append(out, &tool{name: name, service: service})
	}
	return out
}

type tool struct {
	name    string
	service ServiceFunc
}

func (t *tool) Name() string        { return t.name }
func (t *tool) DisplayName() string { return displayNames[t.name] }
func (t *tool) Description() string { return descriptions[t.name] }

func (t *tool) ArgsSchema() map[string]any {
	props := make(map[string]any, len(schemas[t.name]))
	for k, v := range schemas[t.name] {
		props[k] = v
	}
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if r := requiredArguments[t.name]; len(r) > 0 {
		schema["required"] = append([]string(nil), r...)
	}
	return schema
}

type arguments struct {
	Period   string `json:"period"`
	Compare  string `json:"compare"`
	Accounts string `json:"accounts"`
	Regions  string `json:"regions"`
	Teams    string `json:"teams"`
	Depth    string `json:"depth"`
	GroupBy  string `json:"group_by"`
	Status   string `json:"status"`
	ID       string `json:"id"`
}

func (a arguments) query() core_finops.Query {
	list := func(s string) []string {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	return core_finops.Query{Period: a.Period, Compare: a.Compare, Accounts: list(a.Accounts), Regions: list(a.Regions),
		Teams: list(a.Teams), Depth: a.Depth, GroupBy: a.GroupBy, Status: a.Status}
}

// Invoke answers from the cache through the one service method behind the
// matching admin route.
func (t *tool) Invoke(ctx context.Context, raw json.RawMessage) (*core.ToolResult, error) {
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return core.UnavailableToolResult(t.name, "infrastructure:view permission is required"), nil
	}
	var args arguments
	if len(raw) != 0 && string(raw) != "null" {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&args); err != nil {
			return nil, core.NewToolError(core.ToolErrorInvalidArguments, "invalid FinOps tool arguments", err)
		}
	}
	svc := t.service()
	if svc == nil {
		return core.UnavailableToolResult(t.name, "cloud cost data is not set up"), nil
	}
	view, header, found, err := t.view(ctx, svc, args)
	if err != nil {
		if errorIsBadArgs(err) {
			return nil, core.NewToolError(core.ToolErrorInvalidArguments, "invalid FinOps tool arguments", err)
		}
		return nil, err
	}
	if !header.Enabled {
		res := core.UnavailableToolResult(t.name, "cloud cost data is off ("+header.Reason+")")
		res.Data = map[string]any{"enabled": false, "reason": header.Reason, "links": map[string]any{"ui": header.Links.UI}}
		return res, nil
	}
	for k, v := range headerView(header) {
		if _, set := view[k]; !set {
			view[k] = v
		}
	}
	view["ref"] = ref(t.name, args)
	return &core.ToolResult{Tool: t.name, Found: found, Data: fit(view)}, nil
}

func errorIsBadArgs(err error) bool {
	return err != nil && (strings.Contains(err.Error(), core_finops.ErrBadQuery.Error()) || err == core_finops.ErrNotFound)
}

// view calls the service method for the tool and compacts its result.
func (t *tool) view(ctx context.Context, s core_finops.Reader, a arguments) (map[string]any, core_finops.Header, bool, error) {
	q := a.query()
	switch t.name {
	case "cost_breakdown":
		sum, err := s.Summary(ctx, q)
		if err != nil || !sum.Enabled {
			return nil, sum.Header, false, err
		}
		bd, err := s.Breakdown(ctx, q)
		if err != nil {
			return nil, bd.Header, false, err
		}
		return breakdownView(sum, bd), bd.Header, len(bd.Rows) > 0, nil
	case "cost_drivers":
		v, err := s.Drivers(ctx, q)
		return driversView(v), v.Header, len(v.Rows) > 0 || v.Steady, err
	case "cost_increases":
		v, err := s.Increases(ctx, q)
		return increasesView(v), v.Header, len(v.Items) > 0, err
	case "cost_increase_detail":
		if strings.TrimSpace(a.ID) == "" {
			return nil, core_finops.Header{}, false, core_finops.ErrNotFound
		}
		v, err := s.Increase(ctx, q, a.ID)
		return increaseDetailView(v), v.Header, true, err
	case "savings_opportunities":
		v, err := s.Savings(ctx, q)
		return savingsView(v), v.Header, len(v.Items) > 0, err
	case "budget_status":
		v, err := s.Budget(ctx, q)
		return budgetView(v), v.Header, len(v.Budgets) > 0, err
	case "cost_accounts":
		v, err := s.Accounts(ctx, q)
		return accountsView(v), v.Header, len(v.Accounts) > 0, err
	}
	return nil, core_finops.Header{}, false, core_finops.ErrNotFound
}

func ref(name string, a arguments) string {
	v := url.Values{}
	if a.Period != "" {
		v.Set("period", a.Period)
	}
	if a.ID != "" {
		v.Set("id", a.ID)
	}
	if a.GroupBy != "" {
		v.Set("group_by", a.GroupBy)
	}
	r := "finops://" + name
	if enc := v.Encode(); enc != "" {
		r += "?" + enc
	}
	return r
}
