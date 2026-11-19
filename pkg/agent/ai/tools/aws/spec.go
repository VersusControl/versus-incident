package aws

// toolSpec is one AWS tool. This table is the single source of the tool
// names, descriptions, argument schemas and display names; the registry
// catalog and the aws toolset must list exactly these names.
type toolSpec struct {
	Name        string
	Display     string
	Description string
	Properties  map[string]any
	Required    []string
	// UIPath is the admin page the tool's answer mirrors.
	UIPath string
}

var specs = []toolSpec{
	{Name: "get_aws_connection", Display: "AWS connection", Description: "Read the AWS connection status from stored state: each account's status, last check and sync freshness, denied services with the IAM actions that fix them, regions not enabled and sync gaps. Never calls AWS.",
		Properties: map[string]any{
			"account_id": map[string]any{"type": "string", "description": "12-digit account ID; default all accounts", "pattern": "^[0-9]{12}$"},
			"offset":     map[string]any{"type": "integer", "minimum": 0, "description": "skip this many accounts; use the next.offset of a cut-short answer"},
		}, UIPath: settingsUI},
	{Name: ChangesToolName, Display: "AWS changes", Description: "Read recorded AWS changes (CloudTrail writes) from stored state: who changed what, error bursts, gaps and the likely cause of an alarm (the suspect field; tied candidates are equally likely). Never calls AWS. Say \"no changes\" only when complete is true; otherwise counts are lower bounds.",
		Properties: map[string]any{
			"window":       map[string]any{"type": "string", "enum": []string{"1h", "24h"}, "description": "default 24h"},
			"until":        map[string]any{"type": "string", "format": "date-time", "description": "window end; default now"},
			"account_id":   map[string]any{"type": "string", "pattern": "^[0-9]{12}$"},
			"region":       map[string]any{"type": "string", "description": "a Region, or global"},
			"actor_class":  map[string]any{"type": "string", "enum": []string{"pipeline", "person", "aws_service", "unknown"}},
			"service":      map[string]any{"type": "string"},
			"resource":     map[string]any{"type": "string", "description": "canonical ARN"},
			"q":            map[string]any{"type": "string", "description": "free text"},
			"show_routine": map[string]any{"type": "boolean"},
			"cursor":       map[string]any{"type": "string", "description": "next_cursor of the previous answer"},
			"limit":        map[string]any{"type": "integer", "minimum": 1, "maximum": changesMaxLimit},
		}, UIPath: changesUI},
}

var (
	toolNames         []string
	descriptions      = make(map[string]string, len(specs))
	schemas           = make(map[string]map[string]any, len(specs))
	requiredArguments = make(map[string][]string, len(specs))
	displayNames      = make(map[string]string, len(specs))
	uiPaths           = make(map[string]string, len(specs))
)

func init() {
	for _, spec := range specs {
		toolNames = append(toolNames, spec.Name)
		descriptions[spec.Name] = spec.Description
		schemas[spec.Name] = spec.Properties
		displayNames[spec.Name] = spec.Display
		uiPaths[spec.Name] = spec.UIPath
		if len(spec.Required) > 0 {
			requiredArguments[spec.Name] = spec.Required
		}
	}
}

// ToolNames lists the AWS tools in order.
func ToolNames() []string { return append([]string(nil), toolNames...) }

// ConnectionToolNames lists the tools New builds over the settings service.
func ConnectionToolNames() []string {
	var out []string
	for _, name := range toolNames {
		if name != ChangesToolName {
			out = append(out, name)
		}
	}
	return out
}
