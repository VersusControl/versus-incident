// Package model holds the normalized shapes every AWS collector produces and
// every reader consumes: resources, health, changes, security flags and the
// sync state that says how much of a view can be trusted. It is a leaf
// package: it imports only the standard library, so collectors, the snapshot
// store, the change pipeline and later read surfaces can share it without a
// cycle.
package model

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// GlobalRegion is the region label for services that have one endpoint for
// the whole account (S3 listing, CloudFront, Route 53, IAM, AWS Health).
const GlobalRegion = "global"

// ResourcePolicyType is the type of a policy entry: the grants of one
// resource's policy, stored beside the resource (as "<ARN>#policy") rather
// than on it, because they are read on another cadence. It is not a
// resource of its own, so listings, counts and compares leave it out.
const ResourcePolicyType = "resource-policy"

// PolicyEntryService is the unit that holds only policy entries: bucket
// policies.
const PolicyEntryService = "s3-policy"

// RedactedValue replaces a tag value whose key is not on the allowlist. The
// key stays visible so a reader can still see which tags exist.
const RedactedValue = "[redacted]"

// Unit is the smallest independently committed piece of a snapshot: one
// service in one region of one account. A failure in one unit never touches
// another.
type Unit struct {
	Account string `json:"account"`
	Region  string `json:"region"`
	Service string `json:"service"`
}

// Key is the stable identifier of a unit inside one organization.
func (unit Unit) Key() string {
	return unit.Account + "/" + unit.Region + "/" + unit.Service
}

// RelationType names a typed edge read from configuration.
type RelationType string

const (
	// RelationInVPC links a resource to the VPC it lives in.
	RelationInVPC RelationType = "in_vpc"
	// RelationInSubnet links a resource to a subnet it uses.
	RelationInSubnet RelationType = "in_subnet"
	// RelationSecurityGroup links a resource to a security group it uses.
	RelationSecurityGroup RelationType = "uses_security_group"
	// RelationRoutesTo links a load balancer to a target group, or a target
	// group to a registered target.
	RelationRoutesTo RelationType = "routes_to"
	// RelationTriggers links an event source (queue, stream, table) to the
	// function it invokes.
	RelationTriggers RelationType = "triggers"
	// RelationSubscribes links a topic to a subscribed endpoint.
	RelationSubscribes RelationType = "subscribes"
	// RelationTargets links an EventBridge rule to one of its targets.
	RelationTargets RelationType = "targets"
	// RelationPartOfStack links a resource to the CloudFormation stack that
	// created it.
	RelationPartOfStack RelationType = "part_of_stack"
	// RelationMemberOf links a member to its group: a service to its
	// cluster, a node group to its cluster, an instance to its group.
	RelationMemberOf RelationType = "member_of"
	// RelationAttachedTo links a network interface or gateway to what it
	// is attached to.
	RelationAttachedTo RelationType = "attached_to"
	// RelationDeadLetter links a queue or function to its dead-letter target.
	RelationDeadLetter RelationType = "dead_letter"
	// RelationOrigin links a distribution to an origin it serves from.
	RelationOrigin RelationType = "origin"
	// RelationMonitors links an alarm to the resource its metric describes.
	RelationMonitors RelationType = "monitors"
	// RelationAssociated links an application or a stack to a resource that
	// belongs to it.
	RelationAssociated RelationType = "associated"
	// RelationContains links a group to a member it manages, such as an Auto
	// Scaling group to its instances.
	RelationContains RelationType = "contains"
	// RelationTriggeredBy links a function to an event source that invokes it.
	RelationTriggeredBy RelationType = "triggered_by"
	// RelationAttachedToTransit links a transit gateway attachment to the
	// transit gateway it attaches to, and to the network it attaches.
	RelationAttachedToTransit RelationType = "attached_to_transit"
	// RelationSharedVia links a resource share to an account, organization
	// or organizational unit it is shared with.
	RelationSharedVia RelationType = "shared_via"
	// RelationAliasOf links a DNS record to the load balancer or
	// distribution host name it points at.
	RelationAliasOf RelationType = "alias_of"
	// RelationTrusts links a role to a principal its trust policy lets
	// assume it.
	RelationTrusts RelationType = "trusts"
	// RelationMayAccess links a resource to a principal its resource policy
	// grants access.
	RelationMayAccess RelationType = "may_access"
)

// RelationKind says how a relation is known.
type RelationKind string

const (
	// RelationConfigured is a relation AWS states in configuration: a
	// listener's target group, a DNS alias, an attachment. A relation
	// stored before kinds existed reads as configured.
	RelationConfigured RelationKind = "configured"
	// RelationKindMayAccess is a relation a policy allows: a role trust or a
	// resource policy grant. It says what is permitted, not what happens.
	RelationKindMayAccess RelationKind = "may_access"
	// RelationInferred is a relation matched by a name or an address: a
	// CNAME whose host name is a load balancer's.
	RelationInferred RelationKind = "inferred"
)

// Access classes a policy grant is reduced to.
const (
	AccessRead  = "read"
	AccessWrite = "write"
	AccessAdmin = "admin"
)

// Relation targets that are not ARNs. A target may also be a bare 12-digit
// account ID, an ARN in another account, or a DNS host name.
const (
	// OrgTargetPrefix starts a target naming a whole AWS Organization:
	// "org:o-a1b2c3d4e5".
	OrgTargetPrefix = "org:"
	// AnyPrincipal is the target of a grant to every principal.
	AnyPrincipal = "*"
)

// OrgTarget is the relation target that names an organization.
func OrgTarget(id string) string { return OrgTargetPrefix + id }

// Relation is one typed edge. Target is an ARN when one can be built, else
// the provider identifier the configuration names: an account ID, an
// organization (OrgTarget), or a host name. Kind says how the relation is
// known and Source names the AWS operation it was read from. A policy grant
// carries its access class and resource pattern; Count is how many grants
// the one relation stands for when wide grants were collapsed.
type Relation struct {
	Type    RelationType `json:"type"`
	Target  string       `json:"target"`
	Kind    RelationKind `json:"kind,omitempty"`
	Source  string       `json:"source,omitempty"`
	Access  string       `json:"access,omitempty"`
	Pattern string       `json:"pattern,omitempty"`
	Count   int          `json:"count,omitempty"`
}

// UnmarshalJSON reads a relation; one stored before kinds existed has no
// kind and reads as configured.
func (relation *Relation) UnmarshalJSON(data []byte) error {
	type plain Relation
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*relation = Relation(decoded)
	if relation.Kind == "" {
		relation.Kind = RelationConfigured
	}
	return nil
}

// relationKey is what makes two relations the same link: the source and
// the count are merged, not compared.
type relationKey struct {
	kind            RelationType
	target          string
	how             RelationKind
	access, pattern string
}

func (relation Relation) key() relationKey {
	return relationKey{relation.Type, relation.Target, relation.Kind, relation.Access, relation.Pattern}
}

// NormalizeDNSName is the form host names are matched in: lower case, no
// trailing dot, and no "dualstack." prefix, which Route 53 adds to an alias
// of a load balancer. "DualStack.Web-1.us-east-1.elb.amazonaws.com." and
// "web-1.us-east-1.elb.amazonaws.com" are the same name.
func NormalizeDNSName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimSuffix(name, ".")
	return strings.TrimPrefix(name, "dualstack.")
}

// Resource is one normalized AWS resource. Tags carry every key; values are
// replaced with RedactedValue unless the key is allowlisted. Attributes hold
// a closed set of metadata keys (identifiers, sizes, zones), never bodies,
// environment values or user data.
type Resource struct {
	ARN       string `json:"arn"`
	Service   string `json:"service"`
	Type      string `json:"type"`
	Account   string `json:"account"`
	Region    string `json:"region"`
	App       string `json:"app,omitempty"`
	AppSource string `json:"app_source,omitempty"`
	// AppTag is the tag key App was read from when AppSource is
	// AppSourceTags. App is then a tag value, shown only while that key is
	// on the tag-value allowlist (RedactName).
	AppTag     string            `json:"app_tag,omitempty"`
	Name       string            `json:"name"`
	State      string            `json:"state,omitempty"`
	Tags       map[string]string `json:"tags,omitempty"`
	Relations  []Relation        `json:"relations,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// Attribute keys collectors may set. Anything else is dropped by Sanitize.
const (
	AttrID            = "id"
	AttrVPC           = "vpc_id"
	AttrZone          = "availability_zone"
	AttrSize          = "size"
	AttrEngine        = "engine"
	AttrRuntime       = "runtime"
	AttrScheme        = "scheme"
	AttrTaskDef       = "task_definition"
	AttrDesired       = "desired"
	AttrRunning       = "running"
	AttrDNSName       = "dns_name"
	AttrRollout       = "rollout_state"
	AttrPublicIP      = "public_ip"
	AttrStackName     = "stack_name"
	AttrBucketRegion  = "bucket_region"
	AttrApplicationID = "application_id"
	AttrResourceID    = "physical_id"
	// AttrOrganizationID and AttrManagementAccount are the organization an
	// account belongs to and its management account, the only organization
	// facts a member account can read.
	AttrOrganizationID    = "organization_id"
	AttrManagementAccount = "management_account"
	// AttrOwnerAccount is the account that owns a resource another account
	// can see: a shared transit gateway, a received resource share.
	AttrOwnerAccount = "owner_account"
	// AttrSubject is the ARN of the resource a resource-policy entry
	// describes.
	AttrSubject = "subject"
)

var allowedAttributes = map[string]bool{
	AttrID: true, AttrVPC: true, AttrZone: true, AttrSize: true, AttrEngine: true,
	AttrRuntime: true, AttrScheme: true, AttrTaskDef: true, AttrDesired: true,
	AttrRunning: true, AttrDNSName: true, AttrRollout: true, AttrPublicIP: true,
	AttrStackName: true, AttrBucketRegion: true, AttrApplicationID: true,
	AttrResourceID: true, AttrOrganizationID: true, AttrManagementAccount: true,
	AttrOwnerAccount: true, AttrSubject: true,
}

// AllowedAttribute reports whether key is part of the closed attribute set.
func AllowedAttribute(key string) bool { return allowedAttributes[key] }

// HealthStatus is a closed vocabulary rendered as a word plus an icon.
type HealthStatus string

const (
	HealthOK       HealthStatus = "ok"
	HealthWarning  HealthStatus = "warning"
	HealthCritical HealthStatus = "critical"
	HealthUnknown  HealthStatus = "unknown"
)

// Health sources.
const (
	SourceAlarm          = "alarm"
	SourceInstanceStatus = "instance_status"
	SourceTargetHealth   = "target_health"
	SourceRollout        = "rollout"
	SourceDatabaseEvent  = "database_event"
	SourceAWSHealth      = "aws_health"
	SourceStackStatus    = "stack_status"
)

// Health is one health signal about one resource. Resource is the ARN of the
// affected resource when the signal names one; Ref is the alarm, event or
// stack the signal came from.
type Health struct {
	Resource string       `json:"resource,omitempty"`
	Account  string       `json:"account"`
	Region   string       `json:"region"`
	Source   string       `json:"source"`
	Status   HealthStatus `json:"status"`
	Summary  string       `json:"summary"`
	Since    time.Time    `json:"since,omitzero"`
	Ref      string       `json:"ref,omitempty"`
	Children []string     `json:"children,omitempty"`
}

// ActorClass says who made a change, coarsely, without keeping the person's
// session name.
type ActorClass string

const (
	ActorPipeline   ActorClass = "pipeline"
	ActorPerson     ActorClass = "person"
	ActorAWSService ActorClass = "aws_service"
	ActorUnknown    ActorClass = "unknown"
)

// Actor is the classified principal of a change. PrincipalARN is the user or
// role ARN, never an assumed-role session ARN, so a session name (often an
// email address) is not stored.
type Actor struct {
	Class         ActorClass `json:"class"`
	PrincipalType string     `json:"principal_type,omitempty"`
	PrincipalARN  string     `json:"principal_arn,omitempty"`
	SessionClass  string     `json:"session_class,omitempty"`
	// Via names the AWS service that made the call on the principal's
	// behalf (cloudformation, servicecatalog). The change is still the
	// principal's.
	Via string `json:"via,omitempty"`
}

// Change is one write event in plain language. Fields holds only the
// identifier fields the event's template allowlists; request and response
// bodies are never kept.
type Change struct {
	ID          string    `json:"id"`
	Time        time.Time `json:"time"`
	Account     string    `json:"account"`
	Region      string    `json:"region"`
	Service     string    `json:"service"`
	EventSource string    `json:"event_source"`
	EventName   string    `json:"event_name"`
	Template    string    `json:"template"`
	Summary     string    `json:"summary"`
	// Resources are canonical ARNs only, resolved at ingest from the
	// resource type, account and Region. A name that could not be resolved
	// is kept in Unresolved instead, never guessed into an ARN.
	Resources  []string          `json:"resources,omitempty"`
	Unresolved []string          `json:"unresolved,omitempty"`
	Actor      Actor             `json:"actor"`
	ErrorCode  string            `json:"error_code,omitempty"`
	Fields     map[string]string `json:"fields,omitempty"`
}

// SecurityFlagKind is the closed list of metadata-only checks.
type SecurityFlagKind string

const (
	FlagPublicExposure SecurityFlagKind = "public_exposure"
	FlagEncryptionOff  SecurityFlagKind = "encryption_off"
	FlagAdminPortOpen  SecurityFlagKind = "admin_port_open"
	FlagNoAlarms       SecurityFlagKind = "no_alarms"
	FlagNoTags         SecurityFlagKind = "no_tags"
)

// SecurityFlag is one metadata finding on one resource.
type SecurityFlag struct {
	Resource string           `json:"resource"`
	Account  string           `json:"account"`
	Region   string           `json:"region"`
	Kind     SecurityFlagKind `json:"kind"`
	Summary  string           `json:"summary"`
}

// GapReason names why data is missing. A gap is never rendered as zero.
type GapReason string

const (
	GapDenied     GapReason = "denied"
	GapNotEnabled GapReason = "not_enabled"
	GapThrottled  GapReason = "throttled"
	GapNotChecked GapReason = "not_checked"
	// GapNotInRegion means the service has no endpoint in the region: there
	// is nothing to read there, which is not a failure.
	GapNotInRegion GapReason = "not_available_in_region"
	// GapNotOnPlan means the account's support plan does not offer the
	// service (AWS Health on Basic support).
	GapNotOnPlan GapReason = "not_on_plan"
	// GapCapped means a per-cycle fan-out cap stopped the read: Seen were
	// read and the rest were not. With Total unknown the count reads as
	// "more than Seen".
	GapCapped GapReason = "capped"
)

// ReadOutcome is how a read ended, as the coverage view groups it. Every
// gap reason maps to exactly one outcome. A "not set" answer (no bucket
// policy, no tag set) is a successful read and never a gap, so it has no
// outcome here.
type ReadOutcome string

const (
	// OutcomeNotRead is a read that has no answer: denied, throttled, not
	// on the support plan, or not checked. It is a gap.
	OutcomeNotRead ReadOutcome = "not_read"
	// OutcomeCapped is a read stopped by a per-cycle cap. It is a gap.
	OutcomeCapped ReadOutcome = "capped"
	// OutcomeOffInAWS is a feature the account has not turned on (a
	// detector, an analyzer, a log). It is the account's choice, not a gap.
	OutcomeOffInAWS ReadOutcome = "off_in_aws"
	// OutcomeNotOffered is a service with no endpoint in the Region. There
	// is nothing to read, so it is not a gap.
	OutcomeNotOffered ReadOutcome = "not_offered"
)

// OutcomeForReason maps a gap reason onto its read outcome. An unknown
// reason reads as not read, never as clear.
func OutcomeForReason(reason GapReason) ReadOutcome {
	switch reason {
	case GapCapped:
		return OutcomeCapped
	case GapNotEnabled:
		return OutcomeOffInAWS
	case GapNotInRegion:
		return OutcomeNotOffered
	default:
		return OutcomeNotRead
	}
}

// IsGap reports whether an outcome leaves data missing: only not read and
// capped do.
func (outcome ReadOutcome) IsGap() bool {
	return outcome == OutcomeNotRead || outcome == OutcomeCapped
}

// Gap is one named hole in a view: the unit (or one detail call inside it)
// that has no current answer, why, and the last time it had one.
type Gap struct {
	Account    string    `json:"account"`
	Region     string    `json:"region"`
	Service    string    `json:"service"`
	Reason     GapReason `json:"reason"`
	Operation  string    `json:"operation,omitempty"`
	LastGoodAt time.Time `json:"last_good_at,omitzero"`
	// Seen and Total count the items a per-item detail call covered: Seen
	// were read, Total were attempted or known ("28 of 35 functions"). A
	// capped gap with Total zero means more than Seen exist. Both are zero
	// for a single call.
	Seen  int `json:"seen,omitempty"`
	Total int `json:"total,omitempty"`
}

// Outcome is the read outcome the gap's reason maps to.
func (gap Gap) Outcome() ReadOutcome { return OutcomeForReason(gap.Reason) }

// SyncStatus is the state of one unit.
type SyncStatus string

const (
	// SyncPending means the unit is expected but has never been attempted.
	SyncPending SyncStatus = "pending"
	// SyncOK means the last attempt committed or confirmed a snapshot.
	SyncOK SyncStatus = "ok"
	// SyncFailed means the last attempt failed; any committed snapshot is
	// kept and served as of LastGoodAt.
	SyncFailed SyncStatus = "failed"
)

// SyncState is the honest status of one unit.
type SyncState struct {
	State       SyncStatus `json:"state"`
	LastGoodAt  time.Time  `json:"last_good_at,omitzero"`
	AttemptedAt time.Time  `json:"attempted_at,omitzero"`
	ErrorCode   string     `json:"error_code,omitempty"`
	Gaps        []Gap      `json:"gaps,omitempty"`
}

// GapReasonForCode maps the closed error vocabulary onto a gap reason.
// Anything unrecognized is not_checked, never zero and never a denial.
func GapReasonForCode(code string) GapReason {
	switch code {
	case "denied", "trust_denied", "external_id_mismatch", "role_not_found":
		return GapDenied
	case "not_on_plan":
		return GapNotOnPlan
	case "region_disabled", "not_enabled":
		return GapNotEnabled
	case "throttled":
		return GapThrottled
	case string(GapNotInRegion):
		return GapNotInRegion
	default:
		return GapNotChecked
	}
}

// PartitionForRegion returns the ARN partition for a region.
func PartitionForRegion(region string) string {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "aws-cn"
	case strings.HasPrefix(region, "us-gov-"):
		return "aws-us-gov"
	default:
		return "aws"
	}
}

// ARN builds an ARN from its parts.
func ARN(partition, service, region, account, resource string) string {
	return "arn:" + partition + ":" + service + ":" + region + ":" + account + ":" + resource
}

// Sanitize returns a copy of resource with attributes outside the closed set
// removed, relations de-duplicated and sorted, and empty maps dropped, so two
// equal inventories always encode to the same bytes.
func Sanitize(resource Resource) Resource {
	out := resource
	if len(resource.Attributes) > 0 {
		out.Attributes = make(map[string]string, len(resource.Attributes))
		for key, value := range resource.Attributes {
			if allowedAttributes[key] && value != "" {
				out.Attributes[key] = value
			}
		}
		if len(out.Attributes) == 0 {
			out.Attributes = nil
		}
	}
	if len(resource.Tags) > 0 {
		out.Tags = make(map[string]string, len(resource.Tags))
		for key, value := range resource.Tags {
			out.Tags[key] = value
		}
	} else {
		out.Tags = nil
	}
	if len(resource.Relations) > 0 {
		seen := make(map[relationKey]int, len(resource.Relations))
		relations := make([]Relation, 0, len(resource.Relations))
		for _, relation := range resource.Relations {
			if relation.Target == "" {
				continue
			}
			if relation.Kind == "" {
				relation.Kind = RelationConfigured
			}
			if relation.Count < 0 {
				relation.Count = 0
			}
			if index, ok := seen[relation.key()]; ok {
				// The same link read twice (an A and an AAAA alias, two
				// statements granting the same) stays one link: the larger
				// count and the first source in sort order win, so the
				// result does not depend on read order.
				kept := &relations[index]
				kept.Count = max(kept.Count, relation.Count)
				if kept.Source == "" || (relation.Source != "" && relation.Source < kept.Source) {
					kept.Source = relation.Source
				}
				continue
			}
			seen[relation.key()] = len(relations)
			relations = append(relations, relation)
		}
		sort.Slice(relations, func(left, right int) bool {
			a, b := relations[left], relations[right]
			switch {
			case a.Type != b.Type:
				return a.Type < b.Type
			case a.Target != b.Target:
				return a.Target < b.Target
			case a.Kind != b.Kind:
				return a.Kind < b.Kind
			case a.Access != b.Access:
				return a.Access < b.Access
			}
			return a.Pattern < b.Pattern
		})
		out.Relations = relations
		if len(out.Relations) == 0 {
			out.Relations = nil
		}
	} else {
		out.Relations = nil
	}
	return out
}

// NameTagKey is the tag AWS consoles show as a resource's name. Its value is
// a tag value like any other: shown only while the key is allowlisted.
const NameTagKey = "Name"

// AppSourceTags marks an application read from a configured tag key.
const AppSourceTags = "tags"

// nameTagTypes are the resource types whose display name a collector takes
// from the Name tag (EC2 and VPC resources have no name of their own).
var nameTagTypes = map[string]bool{
	"instance": true, "vpc": true, "subnet": true, "route-table": true,
	"internet-gateway": true, "nat-gateway": true,
}

// NameFromTag reports that a resource type takes its display name from the
// Name tag.
func NameFromTag(resourceType string) bool { return nameTagTypes[resourceType] }

// ResourceID is the resource's own ID from an ARN: the last segment of the
// resource part ("arn:aws:ec2:us-east-1:111122223333:instance/i-0abc" →
// "i-0abc", "arn:aws:lambda:…:function:checkout" → "checkout"), so the
// account ID never travels with it. A value that is not an ARN is returned
// unchanged. It fails closed: anything that looks like an ARN (any case,
// surrounding spaces) but is not a well-formed one, or leaves no resource
// ID, answers RedactedValue, never the raw string.
func ResourceID(arn string) string {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(arn)), "arn:") {
		return arn
	}
	if !strings.HasPrefix(arn, "arn:") {
		return RedactedValue
	}
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 || parts[1] == "" || parts[2] == "" || parts[5] == "" {
		return RedactedValue
	}
	resource := parts[5]
	if index := strings.LastIndex(resource, "/"); index >= 0 {
		resource = resource[index+1:]
	} else if _, name, ok := strings.Cut(resource, ":"); ok {
		resource = name
	}
	if resource == "" || strings.ContainsAny(resource, " \t\r\n") {
		return RedactedValue
	}
	return resource
}

// RedactName applies the tag-value allowlist to a resource's display name,
// application and tags. Unless the Name tag is allowlisted (exact key), a
// name taken from the Name tag is replaced by the resource ID. An application
// read from a tag is a tag value too: unless the tag key it came from is
// allowlisted (exact, case-sensitive key), it is left empty. Every tag value
// whose key is not allowlisted is redacted. Collectors call it before a
// resource is stored and readers call it again on every read, so narrowing
// the allowlist hides values stored while it was wider.
func RedactName(resource *Resource, allowlist map[string]bool) {
	if !allowlist[NameTagKey] {
		value, tagged := resource.Tags[NameTagKey]
		if tagged && (NameFromTag(resource.Type) || (value != "" && resource.Name == value)) {
			resource.Name = ResourceID(resource.ARN)
		}
	}
	if resource.AppSource == AppSourceTags && (resource.AppTag == "" || !allowlist[resource.AppTag]) {
		resource.App, resource.AppSource = "", ""
	}
	if len(resource.Tags) > 0 {
		resource.Tags = RedactTags(resource.Tags, allowlist)
	}
}

// RedactTags keeps every key and replaces each value whose key is not in
// allowlist. Key matching is exact, like AWS tag keys.
func RedactTags(tags map[string]string, allowlist map[string]bool) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	out := make(map[string]string, len(tags))
	for key, value := range tags {
		if allowlist[key] {
			out[key] = value
		} else {
			out[key] = RedactedValue
		}
	}
	return out
}
