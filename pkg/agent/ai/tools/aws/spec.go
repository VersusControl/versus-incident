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
}

var specs = []toolSpec{
	{Name: "get_aws_connection", Display: "AWS connection", Description: "Read the AWS connection status from stored state: each account's status, last check and sync freshness, denied services with the IAM actions that fix them, regions not enabled and sync gaps. Never calls AWS.",
		Properties: map[string]any{
			"account_id": map[string]any{"type": "string", "description": "12-digit account ID; default all accounts", "pattern": "^[0-9]{12}$"},
			"offset":     map[string]any{"type": "integer", "minimum": 0, "description": "skip this many accounts; use the next.offset of a cut-short answer"},
		}},
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

// ToolNames lists the AWS tools in order.
func ToolNames() []string { return append([]string(nil), toolNames...) }
