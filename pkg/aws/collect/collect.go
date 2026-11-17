// Package collect reads a connected AWS account into committed snapshots:
// inventory, typed relations, applications, health, security flags and the
// CloudTrail change history. It is read-only: every call is a Describe, List,
// Get (metadata) or LookupEvents operation, and every call waits on the same
// per-account, per-region limiter the settings probes use.
package collect

import (
	"context"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/catalog"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
)

// Kind groups units by how often they run.
type Kind string

const (
	// KindInventory units describe what exists.
	KindInventory Kind = "inventory"
	// KindHealth units describe what is unhealthy now.
	KindHealth Kind = "health"
	// KindChanges is the CloudTrail poller.
	KindChanges Kind = "changes"
)

// Detail fan-out caps. Beyond them a unit still commits its list and names
// the unread remainder as a gap.
const (
	maxTargetGroupHealth = 100
	maxStacksDescribed   = 50
	maxBucketDetails     = 200
	maxQueueDetails      = 200
	maxRuleTargets       = 100
	maxClusters          = 50
	maxServicesPerCall   = 10
	maxHealthEvents      = 20
	maxApplications      = 50
	maxNodegroups        = 100
)

// spec describes one unit service.
type spec struct {
	service string
	global  bool
	kind    Kind
	run     func(context.Context, *call) (result, error)
}

// specs is the full unit list in collection order. Health-kind units run
// first so a first sync shows what is broken early.
var specs = []spec{
	{service: "cloudwatch", kind: KindHealth, run: collectCloudWatch},
	{service: "ecs", kind: KindHealth, run: collectECS},
	{service: "elbv2", kind: KindHealth, run: collectELB},
	{service: "ec2", kind: KindHealth, run: collectEC2},
	{service: "rds", kind: KindHealth, run: collectRDS},
	{service: "cloudformation", kind: KindHealth, run: collectCloudFormation},
	{service: "aws-health", global: true, kind: KindHealth, run: collectAWSHealth},
	{service: "vpc", kind: KindInventory, run: collectVPC},
	{service: "auto-scaling", kind: KindInventory, run: collectAutoScaling},
	{service: "lambda", kind: KindInventory, run: collectLambda},
	{service: "eks", kind: KindInventory, run: collectEKS},
	{service: "dynamodb", kind: KindInventory, run: collectDynamoDB},
	{service: "elasticache", kind: KindInventory, run: collectElastiCache},
	{service: "sqs", kind: KindInventory, run: collectSQS},
	{service: "sns", kind: KindInventory, run: collectSNS},
	{service: "eventbridge", kind: KindInventory, run: collectEventBridge},
	{service: "step-functions", kind: KindInventory, run: collectStepFunctions},
	{service: "kinesis", kind: KindInventory, run: collectKinesis},
	{service: "api-gateway", kind: KindInventory, run: collectAPIGateway},
	{service: snapshot.TagService, kind: KindInventory, run: collectTags},
	{service: snapshot.AppRegistryService, kind: KindInventory, run: collectAppRegistry},
	{service: "s3", global: true, kind: KindInventory, run: collectS3},
	{service: "cloudfront", global: true, kind: KindInventory, run: collectCloudFront},
	{service: "route53", global: true, kind: KindInventory, run: collectRoute53},
	{service: "iam", global: true, kind: KindInventory, run: collectIAM},
}

// Services lists every unit service the collectors produce, in collection
// order, including the change poller.
func Services() []string {
	out := make([]string, 0, len(specs)+1)
	for _, entry := range specs {
		out = append(out, entry.service)
	}
	return append(out, snapshot.ChangeService)
}

// fixActions returns the IAM actions that clear a denial for a unit service,
// from the catalog when the service is in it.
func fixActions(service string) []string {
	switch service {
	case "vpc":
		service = "ec2"
	case snapshot.TagService:
		return []string{"tag:GetResources"}
	case snapshot.AppRegistryService:
		return []string{"servicecatalog:ListApplications", "servicecatalog:ListAssociatedResources"}
	}
	if entry, ok := catalog.ByID(service); ok {
		return entry.FixActions
	}
	return nil
}

// result is one unit's candidate before it is committed.
type result struct {
	resources []model.Resource
	health    []model.Health
	flags     []model.SecurityFlag
	gaps      []model.Gap
}

// call is the context one unit runs with.
type call struct {
	env       env
	unit      model.Unit
	partition string
	// endpointRegion is the region a regional client signs for; for a global
	// unit it is the limiter region.
	endpointRegion string
	allowlist      map[string]bool
	appTagKeys     []string
	now            time.Time
	// writer commits for this sync and refuses writes once the account or
	// region was purged.
	writer *snapshot.Writer
	// transientCode is the error code of the first detail call that failed
	// for a passing reason. A unit with one keeps its last good generation.
	transientCode string
}

func (c *call) arn(service, resource string) string {
	region := c.unit.Region
	if region == model.GlobalRegion {
		region = ""
	}
	return model.ARN(c.partition, service, region, c.unit.Account, resource)
}

func (c *call) regionalARN(service, region, resource string) string {
	return model.ARN(c.partition, service, region, c.unit.Account, resource)
}

// newResource starts a resource owned by this unit.
func (c *call) newResource(arn, typ, name, state string) model.Resource {
	return model.Resource{
		ARN:     arn,
		Service: c.unit.Service,
		Type:    typ,
		Account: c.unit.Account,
		Region:  c.unit.Region,
		Name:    name,
		State:   state,
	}
}

// applyTags resolves the tag-based application from the first configured
// tag key that is allowlisted and set, then redacts values outside the
// allowlist: the application is a tag value too, kept only when its tag key
// is allowlisted.
func (c *call) applyTags(resource *model.Resource, tags map[string]string) {
	if len(tags) == 0 {
		return
	}
	for _, key := range c.appTagKeys {
		if !c.allowlist[key] {
			continue
		}
		if value := strings.TrimSpace(tags[key]); value != "" && resource.App == "" {
			resource.App = clip(value, 128)
			resource.AppSource = snapshot.AppSourceTags
			resource.AppTag = key
		}
	}
	resource.Tags = tags
	model.RedactName(resource, c.allowlist)
}

func (c *call) healthItem(resource, source string, status model.HealthStatus, summary string, since time.Time, ref string) model.Health {
	return model.Health{
		Resource: resource,
		Account:  c.unit.Account,
		Region:   c.unit.Region,
		Source:   source,
		Status:   status,
		Summary:  summary,
		Since:    since.UTC(),
		Ref:      ref,
	}
}

func (c *call) flag(resource string, kind model.SecurityFlagKind, summary string) model.SecurityFlag {
	return model.SecurityFlag{Resource: resource, Account: c.unit.Account, Region: c.unit.Region, Kind: kind, Summary: summary}
}

// detailGap names one detail call that could not be read. A failure that
// may pass on the next attempt (throttling, a timeout, the network) is
// remembered so the unit keeps its last good generation instead of
// replacing detail with nothing.
func (c *call) detailGap(operation string, err error) model.Gap {
	code := errorCode(err)
	if transientCode(code) && c.transientCode == "" {
		c.transientCode = code
	}
	return model.Gap{
		Account:   c.unit.Account,
		Region:    c.unit.Region,
		Service:   c.unit.Service,
		Reason:    model.GapReasonForCode(code),
		Operation: operation,
	}
}

// transientCode reports whether a failure may clear by itself. A denial or
// a missing subscription stays until someone changes the account.
func transientCode(code string) bool {
	switch code {
	case "", CodeDenied, CodeNotOnPlan, CodeNotInRegion:
		return false
	}
	return true
}

// capGap names detail calls skipped because a fan-out cap was reached.
func (c *call) capGap(operation string) model.Gap {
	return model.Gap{Account: c.unit.Account, Region: c.unit.Region, Service: c.unit.Service, Reason: model.GapNotChecked, Operation: operation}
}

func clip(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func derefInt32(value *int32) int32 {
	if value == nil {
		return 0
	}
	return *value
}

func derefBool(value *bool) bool {
	return value != nil && *value
}

func derefTime(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

func lastSegment(value, separator string) string {
	if index := strings.LastIndex(value, separator); index >= 0 {
		return value[index+len(separator):]
	}
	return value
}

// unknownSince marks a health signal whose start time AWS does not report.
// A zero time keeps two identical syncs byte-identical, so an unchanged
// account does not grow a new generation on every pass.
var unknownSince time.Time

// databaseEventWindow is how far back database events count as current.
const databaseEventWindow = time.Hour

func (c *call) eventWindow() time.Duration { return databaseEventWindow }
