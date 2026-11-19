package suspect

import "github.com/VersusControl/versus-incident/pkg/aws/changes"

// Key is the form two canonical ARNs are compared in: changes.ResourceKey,
// the one folding the change list, its resource filter and the suspect
// engine share. It returns "" for anything that is not a well-formed ARN,
// so a short name never matches.
func Key(arn string) string { return changes.ResourceKey(arn) }

// costShaping are change events that typically move cost: scaling,
// instance type, storage class, route table, endpoint removal, new NAT,
// log retention, replicas and deployments that change what runs. Names are
// compared without the API date some services append (Lambda logs
// UpdateFunctionConfiguration as UpdateFunctionConfiguration20150331v2).
var costShaping = map[string]bool{
	"UpdateService": true, "CreateService": true, "RegisterTaskDefinition": true,
	"UpdateAutoScalingGroup": true, "SetDesiredCapacity": true, "PutScalingPolicy": true,
	"ModifyInstanceAttribute": true, "RunInstances": true, "ModifyDBInstance": true,
	"ModifyDBCluster": true, "CreateDBInstanceReadReplica": true, "CreateDBInstance": true,
	"CreateReplicationGroup": true, "ModifyReplicationGroup": true,
	"PutBucketLifecycleConfiguration": true, "DeleteBucketLifecycle": true,
	"CreateRoute": true, "ReplaceRoute": true, "DeleteRoute": true, "CreateRouteTable": true,
	"AssociateRouteTable": true, "ReplaceRouteTableAssociation": true,
	"DeleteVpcEndpoints": true, "CreateNatGateway": true,
	"PutRetentionPolicy": true, "DeleteRetentionPolicy": true,
	"UpdateFunctionConfiguration20150331v2": true, "PutProvisionedConcurrencyConfig": true,
	"PutFunctionConcurrency20171031": true,
	"UpdateTable":                    true, "CreateNodegroup": true, "UpdateNodegroupConfig": true,
}

// CostShaping reports whether an event name typically moves cost.
func CostShaping(eventName string) bool {
	return costShaping[eventName] || costShaping[changes.BaseEventName(eventName)]
}
