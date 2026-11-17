package finops

// toolSpec is one FinOps tool. This table is the single source of the tool
// names, descriptions, argument schemas and display names; the registry
// catalog and the toolset must list exactly these names.
type toolSpec struct {
	Name        string
	Display     string
	Description string
	Properties  map[string]any
	Required    []string
}

func stringProperty() map[string]any { return map[string]any{"type": "string", "maxLength": 256} }

func scopeProperties(extra map[string]any) map[string]any {
	out := map[string]any{
		"period":   map[string]any{"type": "string", "description": "YYYY-MM, mtd or last_30d (default mtd)", "maxLength": 16},
		"compare":  map[string]any{"type": "string", "enum": []string{"prev_period", "same_period_last_year"}},
		"accounts": map[string]any{"type": "string", "description": "comma-separated account IDs", "maxLength": 512},
		"regions":  map[string]any{"type": "string", "description": "comma-separated regions", "maxLength": 512},
		"teams":    map[string]any{"type": "string", "description": "comma-separated team owners", "maxLength": 512},
		"depth":    map[string]any{"type": "string", "enum": []string{"summary", "details"}},
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

var specs = []toolSpec{
	{Name: "cost_breakdown", Display: "Cost breakdown", Description: "Read cached cloud cost for a period: total, change, forecast and spend by service, account, account group, region or team, in net amortized cost.",
		Properties: scopeProperties(map[string]any{"group_by": map[string]any{"type": "string", "enum": []string{"service", "account", "account_group", "region", "team"}}})},
	{Name: "cost_drivers", Display: "Cost drivers", Description: "Explain what moved cloud cost between two periods: drivers by cause (new, removed, used more or less, price, discount, one-time, new account) with owners; they sum to the change.",
		Properties: scopeProperties(nil)},
	{Name: "cost_increases", Display: "Cost increases", Description: "List detected cost increases (a line that stepped up and stayed up) with start day, monthly impact, owner and status.",
		Properties: scopeProperties(map[string]any{"status": map[string]any{"type": "string", "enum": []string{"open", "expected", "resolved", "all"}}})},
	{Name: "cost_increase_detail", Display: "Cost increase detail", Description: "Explain one cost increase: cause tree, the change that likely started it, owner rule and related savings.",
		Properties: scopeProperties(map[string]any{"id": stringProperty()}), Required: []string{"id"}},
	{Name: "savings_opportunities", Display: "Savings opportunities", Description: "List cloud savings found (idle, rightsize, network path, commitment, storage class) with monthly saving, effort, risk and owner. Read-only guides.",
		Properties: scopeProperties(nil)},
	{Name: "budget_status", Display: "Budget status", Description: "Compare month-to-date cost and the month-end forecast with each budget: on track, at risk or over budget.",
		Properties: scopeProperties(nil)},
	{Name: "cost_accounts", Display: "Cost by account", Description: "List the accounts behind the cost source with status, group, cost, change, top driver and top team; names unreadable accounts.",
		Properties: scopeProperties(nil)},
}

var (
	toolNames         []string
	descriptions      = make(map[string]string, len(specs))
	schemas           = make(map[string]map[string]any, len(specs))
	requiredArguments = make(map[string][]string, len(specs))
	displayNames      = make(map[string]string, len(specs))
)

func init() {
	for _, spec := range specs {
		toolNames = append(toolNames, spec.Name)
		descriptions[spec.Name] = spec.Description
		schemas[spec.Name] = spec.Properties
		displayNames[spec.Name] = spec.Display
		if len(spec.Required) > 0 {
			requiredArguments[spec.Name] = spec.Required
		}
	}
}

// ToolNames lists the FinOps tools in order.
func ToolNames() []string { return append([]string(nil), toolNames...) }
