package core

// Agent tool access groups. An auth adapter limits a caller to some of them
// with CallerAuthorization.ToolGroups; every built-in tool belongs to exactly
// one.
const (
	ToolGroupFinOps     = "finops"
	ToolGroupIncidents  = "incidents"
	ToolGroupServices   = "services"
	ToolGroupPatterns   = "patterns"
	ToolGroupOverview   = "overview"
	ToolGroupCommon     = "common"
	ToolGroupLogs       = "logs"
	ToolGroupMetrics    = "metrics"
	ToolGroupTraces     = "traces"
	ToolGroupRunbooks   = "runbooks"
	ToolGroupChanges    = "changes"
	ToolGroupKubernetes = "kubernetes"
	ToolGroupAWS        = "aws"
	ToolGroupAWSChanges = "aws-changes"
)

// ToolGrouper is implemented by a tool that names its own access group, such
// as a tool an extension registers outside the built-in catalog.
type ToolGrouper interface {
	ToolGroup() string
}
