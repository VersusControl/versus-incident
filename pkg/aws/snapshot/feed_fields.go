package snapshot

import (
	"context"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// fieldAttributes maps a change template's field onto the allowlisted
// snapshot attribute that holds the same value. Only these fields can show
// a before value; every other field shows its new value only.
var fieldAttributes = map[string]string{
	"task_definition": model.AttrTaskDef,
	"desired":         model.AttrDesired,
	"instance_type":   model.AttrSize,
	"instance_class":  model.AttrSize,
	"size":            model.AttrSize,
	"runtime":         model.AttrRuntime,
	"engine":          model.AttrEngine,
}

// hiddenField reports a field whose value is never shown: a secret-shaped
// name, or a tag a call set (its value is not kept at ingest). The rule is
// the one ingest applies, so the two can never disagree.
func hiddenField(name string) bool {
	return strings.HasPrefix(name, changes.TagFieldPrefix) || changes.SecretFieldName(name)
}

// shortValue reduces an ARN-shaped value to its last path segment, so a
// snapshot's task definition ARN and a change's family:revision compare.
func shortValue(field, value string) string {
	if field == "task_definition" {
		if index := strings.LastIndex(value, "/"); index >= 0 {
			return value[index+1:]
		}
	}
	return value
}

// arnServiceUnits maps an ARN's service to the unit service that reads it,
// where the two names differ.
var arnServiceUnits = map[string]string{
	"autoscaling":          "auto-scaling",
	"elasticloadbalancing": "elbv2",
	"states":               "step-functions",
	"apigateway":           "api-gateway",
	"execute-api":          "api-gateway",
	"events":               "eventbridge",
	"monitoring":           "cloudwatch",
}

func arnParts(arn string) (service, region, account string, ok bool) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 || parts[0] != "arn" {
		return "", "", "", false
	}
	return parts[2], parts[3], parts[4], true
}

// storeFields fills before values from the snapshot just before a change.
type storeFields struct {
	store *Store
}

// Fields shows each field's new value, hides secret-shaped and
// unallowlisted tag values, and adds the before value where a snapshot
// before the change holds it:
//   - the snapshot read is the generation current just before the event;
//   - an earlier change to the same field after that snapshot is the
//     before value, so changes between two snapshots chain (41 → 42 → 43);
//   - a snapshot already holding the new value was read during or after
//     the change, so the before value is ambiguous;
//   - a resource whose unit could not be read because a permission is
//     missing shows the new value only and names the permission.
func (resolver storeFields) Fields(ctx context.Context, change model.Change) []FeedField {
	out := newOnlyFields(change.Fields)
	for index := range out {
		if hiddenField(out[index].Field) {
			out[index].After, out[index].BeforeStatus = nil, BeforeHidden
		}
	}
	if len(change.Resources) == 0 {
		return out
	}
	arn := change.Resources[0]
	asOf := change.Time.Add(-time.Microsecond)
	var resource model.Resource
	var status UnitStatus
	found := false
	var permission *string
	needed := false
	for _, field := range out {
		if _, mapped := fieldAttributes[field.Field]; mapped && field.BeforeStatus != BeforeHidden {
			needed = true
		}
	}
	if !needed {
		return out
	}
	var err error
	resource, status, found, err = resolver.store.Resource(ctx, arn, asOf)
	if err != nil {
		return out
	}
	if !found {
		permission = resolver.missingPermission(ctx, arn, asOf)
	}
	for index := range out {
		field := &out[index]
		attribute, mapped := fieldAttributes[field.Field]
		if !mapped || field.BeforeStatus == BeforeHidden || field.After == nil {
			continue
		}
		if !found {
			field.MissingPermission = permission
			continue
		}
		before := shortValue(field.Field, resource.Attributes[attribute])
		previous, ok, err := resolver.previousValue(ctx, arn, field.Field, status.CommittedAt, change)
		if err != nil {
			// The changes since the snapshot could not be read: an earlier
			// change may sit between, so the snapshot value is not known
			// to be the before value.
			field.BeforeStatus = BeforeAmbiguous
			continue
		}
		if ok {
			before = previous
		}
		if before == "" {
			continue
		}
		value := before
		field.Before = &value
		if before == shortValue(field.Field, *field.After) {
			field.BeforeStatus = BeforeAmbiguous
			continue
		}
		field.BeforeStatus = BeforeKnown
	}
	return out
}

// previousValue is the after value of the newest earlier change to the
// same resource and field made after the snapshot was taken.
func (resolver storeFields) previousValue(ctx context.Context, arn, field string, snapshotAt time.Time, change model.Change) (string, bool, error) {
	if snapshotAt.IsZero() || !snapshotAt.Before(change.Time) {
		return "", false, nil
	}
	_, _, account, ok := arnParts(arn)
	if !ok {
		account = change.Account
	}
	result, err := resolver.store.Changes(ctx, Query{Account: account, ARN: arn}, snapshotAt, change.Time.Add(time.Microsecond))
	if err != nil {
		return "", false, err
	}
	value, found := "", false
	for _, earlier := range result.Changes {
		if earlier.ID == change.ID || earlier.ErrorCode != "" || earlier.Time.After(change.Time) ||
			earlier.Time.Equal(change.Time) && earlier.ID > change.ID {
			continue
		}
		if after := earlier.Fields[field]; after != "" {
			value, found = shortValue(field, after), true
		}
	}
	return value, found, nil
}

// missingPermission names the permission whose denial leaves the
// resource's unit unread, if that is why it is missing. A historical read
// cannot know an attempt's state at that time, so the unit's current state
// answers: a permission denied now is the one to grant.
func (resolver storeFields) missingPermission(ctx context.Context, arn string, _ time.Time) *string {
	service, region, account, ok := arnParts(arn)
	if !ok {
		return nil
	}
	unit := service
	if mapped, ok := arnServiceUnits[service]; ok {
		unit = mapped
	}
	result, err := resolver.store.Resources(ctx, Query{Account: account, Service: unit})
	if err != nil {
		return nil
	}
	for _, gap := range result.Gaps {
		if gap.Reason != model.GapDenied || region != "" && gap.Region != region && gap.Region != model.GlobalRegion {
			continue
		}
		operation := gap.Operation
		if operation == "" {
			operation = "Describe*"
		}
		permission := service + ":" + operation
		return &permission
	}
	return nil
}
