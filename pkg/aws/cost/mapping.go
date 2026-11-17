package cost

import (
	"strings"

	"github.com/VersusControl/versus-incident/pkg/finops"
)

// Record types Cost Explorer reports. The three usage kinds carry the cost
// of running things (on demand, reserved-covered and Savings Plan-covered);
// every other kind becomes its own line so credits, refunds, tax, fees and
// unused commitments are never hidden inside a service.
var usageRecordTypes = []string{"Usage", "DiscountedUsage", "SavingsPlanCoveredUsage"}

func isUsageRecord(rt string) bool {
	for _, u := range usageRecordTypes {
		if rt == u {
			return true
		}
	}
	return false
}

// recordLine maps a non-usage record type to the neutral record type and
// the neutral service key of its own line.
func recordLine(rt string) (record, service string) {
	switch rt {
	case "Credit":
		return finops.RecordCredit, "billing.credit"
	case "Refund":
		return finops.RecordRefund, "billing.refund"
	case "Tax":
		return finops.RecordTax, "billing.tax"
	case "RIFee", "SavingsPlanRecurringFee":
		// Under amortized cost these carry the commitment nobody used.
		return finops.RecordDiscount, "commitment.unused"
	case "SavingsPlanNegation":
		return finops.RecordDiscount, "commitment.savings"
	case "Fee", "SavingsPlanUpfrontFee":
		return finops.RecordFee, "billing.fee"
	}
	if strings.Contains(strings.ToLower(rt), "discount") {
		return finops.RecordDiscount, "billing.discount"
	}
	return finops.RecordFee, "billing.fee"
}

// serviceKeys maps Cost Explorer SERVICE values to neutral keys. Unknown
// services keep their AWS name as the key; the page then shows the AWS name.
var serviceKeys = map[string]string{
	"Amazon Elastic Compute Cloud - Compute":          "compute.vm",
	"Amazon EC2 Container Service":                    "compute.container",
	"Amazon Elastic Container Service":                "compute.container",
	"Amazon Elastic Container Service for Kubernetes": "compute.kubernetes",
	"Amazon Elastic Kubernetes Service":               "compute.kubernetes",
	"AWS Lambda":                                      "compute.function",
	"Amazon Simple Storage Service":                   "storage.object",
	"Amazon Elastic File System":                      "storage.file",
	"AWS Backup":                                      "storage.backup",
	"Amazon Relational Database Service":              "database.relational",
	"Amazon Aurora":                                   "database.relational",
	"Amazon DynamoDB":                                 "database.keyvalue",
	"Amazon ElastiCache":                              "database.cache",
	"Amazon OpenSearch Service":                       "database.search",
	"Amazon Elasticsearch Service":                    "database.search",
	"Amazon Elastic Load Balancing":                   "network.loadbalancer",
	"Elastic Load Balancing":                          "network.loadbalancer",
	"Amazon CloudFront":                               "network.cdn",
	"Amazon Route 53":                                 "network.dns",
	"Amazon Virtual Private Cloud":                    "network.endpoint",
	"AWS Data Transfer":                               "network.transfer",
	"AmazonCloudWatch":                                "observability.metrics",
	"Amazon CloudWatch":                               "observability.metrics",
	"Amazon Simple Queue Service":                     "messaging.queue",
	"Amazon Kinesis":                                  "messaging.stream",
	"Amazon Redshift":                                 "analytics.warehouse",
	"Amazon Athena":                                   "analytics.query",
	"Amazon Bedrock":                                  "ai.model",
	"AWS Key Management Service":                      "security.keys",
	"AWS Secrets Manager":                             "security.secrets",
	"Amazon EC2 Container Registry (ECR)":             "registry.container",
	"Amazon Elastic Container Registry":               "registry.container",
	"Savings Plans for AWS Compute usage":             "commitment.savings",
	"AWS Marketplace":                                 "billing.marketplace",
	"Tax":                                             "billing.tax",
}

// usageGroups refines "EC2 - Other" and similar mixed services by usage type.
var usageGroups = []struct {
	match   string
	service string
	group   string
}{
	{"NatGateway", "network.nat", "NAT data processing"},
	{"EBS:", "storage.block", "disk volumes"},
	{"EBSOptimized", "compute.vm", "instances"},
	{"ElasticIP", "network.ip", "public IP addresses"},
	{"PublicIPv4", "network.ip", "public IP addresses"},
	{"DataTransfer", "network.transfer", "data transfer"},
	{"-Out-Bytes", "network.transfer", "data transfer"},
	{"-In-Bytes", "network.transfer", "data transfer"},
	{"VpcEndpoint", "network.endpoint", "private endpoints"},
	{"BoxUsage", "compute.vm", "instances"},
	{"SpotUsage", "compute.vm", "instances"},
	{"TimedStorage", "storage.object", "storage"},
	{"Requests-", "storage.object", "requests"},
	{"DataProcessing-Bytes", "observability.logs", "log ingestion"},
	{"TimedStorage-ByteHrs", "storage.object", "storage"},
}

// ServiceKey maps an AWS service name (and, when known, a usage type) to a
// neutral service key and a plain usage group.
func ServiceKey(awsService, usageType string) (key, group string) {
	key = serviceKeys[awsService]
	if strings.HasPrefix(awsService, "AWS Support") {
		key = "billing.support"
	}
	if usageType != "" {
		for _, g := range usageGroups {
			if strings.Contains(usageType, g.match) {
				group = g.group
				if key == "" || awsService == "EC2 - Other" {
					key = g.service
				}
				break
			}
		}
	}
	if key == "" {
		key = awsService
	}
	return key, group
}

// globalServices have no region.
var globalServices = map[string]bool{
	"Amazon CloudFront":                   true,
	"Amazon Route 53":                     true,
	"AWS Identity and Access Management":  true,
	"AWS Organizations":                   true,
	"AWS Cost Explorer":                   true,
	"AWS Marketplace":                     true,
	"Savings Plans for AWS Compute usage": true,
	"Tax":                                 true,
}

func isGlobalService(awsService string) bool {
	return globalServices[awsService] || strings.HasPrefix(awsService, "AWS Support")
}

// regionPrefixes maps usage-type prefixes to regions. A usage type without a
// known prefix on a regional service is in us-east-1, as AWS bills it.
var regionPrefixes = map[string]string{
	"USE1": "us-east-1", "USE2": "us-east-2", "USW1": "us-west-1", "USW2": "us-west-2",
	"CAN1": "ca-central-1", "CAN2": "ca-west-1", "MXC1": "mx-central-1",
	"EUC1": "eu-central-1", "EUC2": "eu-central-2", "EUW1": "eu-west-1", "EUW2": "eu-west-2",
	"EUW3": "eu-west-3", "EUN1": "eu-north-1", "EUS1": "eu-south-1", "EUS2": "eu-south-2",
	"APE1": "ap-east-1", "APE2": "ap-east-2", "APN1": "ap-northeast-1", "APN2": "ap-northeast-2",
	"APN3": "ap-northeast-3", "APS1": "ap-southeast-1", "APS2": "ap-southeast-2",
	"APS3": "ap-south-1", "APS4": "ap-southeast-3", "APS5": "ap-south-2", "APS6": "ap-southeast-4",
	"APS7": "ap-southeast-5", "APS9": "ap-southeast-7", "SAE1": "sa-east-1", "MES1": "me-south-1",
	"MEC1": "me-central-1", "AFS1": "af-south-1", "ILC1": "il-central-1",
	"UGW1": "us-gov-west-1", "UGE1": "us-gov-east-1",
}

// RegionOf reads the region from the usage type prefix ("USE1-…").
func RegionOf(awsService, usageType string) string {
	if isGlobalService(awsService) {
		return finops.GlobalRegion
	}
	if usageType == "" {
		return finops.UnknownRegion
	}
	if prefix, _, ok := strings.Cut(usageType, "-"); ok {
		if r, ok := regionPrefixes[prefix]; ok {
			return r
		}
	}
	return "us-east-1"
}

// TagValue reads a Cost Explorer tag group key ("team$checkout"); untagged
// cost ("team$") returns "".
func TagValue(groupKey string) string {
	_, v, ok := strings.Cut(groupKey, "$")
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}
