package fakeaws

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Imported findings and service-side status of the estate: GuardDuty with
// an EKS attack sequence in the shared-services account, Security Hub's
// cross-Region duplicate, Inspector on in one account only, no Access
// Analyzer anywhere, RDS events, ACM certificates, Lambda runtimes and
// function URLs (mostly denied), and Service Quotas.

func init() {
	registerEstate(map[string]estateHandler{
		"guardduty:ListDetectors":            (*Server).estateDetectors,
		"guardduty:ListFindings":             (*Server).estateGuardDutyFindingIDs,
		"guardduty:GetFindings":              (*Server).estateGuardDutyFindings,
		"securityhub:GetFindings":            (*Server).estateSecurityHubFindings,
		"securityhub:ListFindingAggregators": (*Server).estateFindingAggregators,
		"inspector2:BatchGetAccountStatus":   (*Server).estateInspectorStatus,
		"inspector2:ListFindings":            (*Server).estateInspectorFindings,
		"access-analyzer:ListAnalyzers":      (*Server).estateAnalyzers,
		"rds:DescribeEvents":                 (*Server).estateDBEvents,
		"acm:ListCertificates":               (*Server).estateCertificates,
		"acm:DescribeCertificate":            (*Server).estateCertificate,
		"lambda:ListFunctions":               (*Server).estateFunctions,
		"lambda:GetFunctionUrlConfig":        (*Server).estateFunctionURL,
		"lambda:ListFunctionUrlConfigs":      (*Server).estateFunctionURLs,
		"servicequotas:GetServiceQuota":      (*Server).estateServiceQuota,
	})
}

// GuardDuty finding identifiers in the shared-services account.
const (
	EstateAttackSequenceFinding = "0f0e0d0c0b0a09080706050403020100"
	EstateRuntimeFinding        = "1f1e1d1c1b1a19181716151413121110"
	estateWorkloadsLowFinding   = "2f2e2d2c2b2a29282726252423222120"
)

// estateDetector: one detector in the home and the baseline Region, none
// in us-east-1, where ListDetectors answers an empty list.
func estateDetector(call estateCall) string {
	if call.region != EstateHomeRegion && call.region != EstateAggregationRegion {
		return ""
	}
	return strings.Repeat(call.account[11:]+call.region[len(call.region)-1:], 16)
}

func (s *Server) estateDetectors(writer http.ResponseWriter, call estateCall) {
	ids := []string{}
	if id := estateDetector(call); id != "" {
		ids = append(ids, id)
	}
	writeRESTJSONBody(writer, mustJSON(map[string]any{"detectorIds": ids}))
}

func (c estateCall) guardDutyIDs() []string {
	if !c.home() {
		return []string{}
	}
	switch c.account {
	case EstateShared:
		return []string{EstateAttackSequenceFinding, EstateRuntimeFinding}
	case EstateWorkloads:
		return []string{estateWorkloadsLowFinding}
	}
	return []string{}
}

func (s *Server) estateGuardDutyFindingIDs(writer http.ResponseWriter, call estateCall) {
	if !strings.Contains(call.request.path, estateDetector(call)) || estateDetector(call) == "" {
		estateError(writer, call, http.StatusBadRequest, "BadRequestException", "The request is rejected because the input detectorId is not owned by the current account.")
		return
	}
	// maxResults pages the IDs; the last page carries an empty token.
	body := jsonBody(call.request)
	ids := call.guardDutyIDs()
	page, next := estatePage(len(ids), jsonString(body, "nextToken"), jsonInt(body, "maxResults"))
	out := []string{}
	for _, index := range page {
		out = append(out, ids[index])
	}
	writeRESTJSONBody(writer, mustJSON(map[string]any{"findingIds": out, "nextToken": next}))
}

func (c estateCall) guardDutyFinding(id string) map[string]any {
	cluster := estateClusterName(c.account)
	clusterARN := c.arn("eks", "cluster/"+cluster)
	base := map[string]any{
		"accountId": c.account, "arn": c.arn("guardduty", "detector/"+estateDetector(c)+"/finding/"+id), "id": id,
		"partition": "aws", "region": c.region, "schemaVersion": "2.0",
		"createdAt": isoMillis(c.now.Add(-days(20))), "updatedAt": isoMillis(c.now.Add(-5 * time.Hour)),
	}
	eksResource := map[string]any{
		"resourceType": "EKSCluster",
		"eksClusterDetails": map[string]any{"name": cluster, "arn": clusterARN, "vpcId": estateVPC(c.account), "status": "ACTIVE",
			"tags": []any{map[string]any{"key": "karpenter.sh/discovery", "value": cluster}}, "createdAt": isoMillis(c.now.Add(-days(400)))},
	}
	switch id {
	case EstateAttackSequenceFinding:
		// An attack sequence has no resource details of its own: the
		// resource is the sequence, and the finding points at itself.
		base["type"], base["severity"], base["title"] = "AttackSequence:EKS/CompromisedCluster", 9.0, "Potential cluster compromise of "+cluster+" indicated by a sequence of actions."
		base["description"] = "A sequence of actions involving 2 signals indicating a possible cluster compromise."
		base["resource"] = map[string]any{"resourceType": "AttackSequence"}
		base["associatedAttackSequenceArn"] = base["arn"]
		base["service"] = map[string]any{
			"serviceName": "guardduty", "detectorId": estateDetector(c), "archived": false, "count": 2, "resourceRole": "TARGET",
			"eventFirstSeen": isoMillis(c.now.Add(-days(4))), "eventLastSeen": isoMillis(c.now.Add(-5 * time.Hour)),
			"additionalInfo": map[string]any{"value": "{}", "type": "default"},
			"detection":      map[string]any{"sequence": c.attackSequence(clusterARN, cluster)},
		}
	case EstateRuntimeFinding:
		base["type"], base["severity"], base["title"] = "Execution:Runtime/SuspiciousTool", 5.0, "A suspicious tool was executed in a container."
		base["description"] = "A process ran a binary associated with offensive security tooling."
		resource := eksResource
		resource["instanceDetails"] = map[string]any{"instanceId": estateNodeID(c.account, 0), "instanceType": "m6i.large",
			"availabilityZone": c.region + "a", "instanceState": "running", "imageId": "ami-0a1b2c3d4e5f6a7b8",
			"launchTime": isoMillis(c.now.Add(-days(8))), "networkInterfaces": []any{map[string]any{
				"networkInterfaceId": "eni-0" + strings.Repeat(c.account[11:], 16), "privateIpAddress": "10." + c.account[11:] + ".42.5",
				"privateIpAddresses": []any{map[string]string{"privateIpAddress": "10." + c.account[11:] + ".42.5"},
					map[string]string{"privateIpAddress": estatePodIP(c.account, 0)}}}}}
		resource["kubernetesDetails"] = map[string]any{"kubernetesWorkloadDetails": map[string]any{
			"name": "build-runner-7f9c", "type": "pods", "namespace": "ci", "uid": "0c0c0c0c-1d1d-4e4e-8f8f-000000000001",
			"containers": []any{map[string]any{"name": "runner", "image": EstateShared + ".dkr.ecr." + c.region + ".amazonaws.com/tools/scratch:latest"}},
		}}
		base["resource"] = resource
		base["service"] = map[string]any{
			"serviceName": "guardduty", "detectorId": estateDetector(c), "archived": false, "count": 141, "resourceRole": "TARGET",
			"featureName": "RuntimeMonitoring", "eventFirstSeen": isoMillis(c.now.Add(-days(4))), "eventLastSeen": isoMillis(c.now.Add(-5 * time.Hour)),
			"runtimeDetails": map[string]any{
				"process": map[string]any{"name": "nsenter", "executablePath": "/usr/bin/nsenter", "pid": 4242, "namespacePid": 383,
					"executableSha256": strings.Repeat("ab", 32), "startTime": epochSeconds(c.now.Add(-5 * time.Hour)), "userId": 1001, "euid": 1001,
					"lineage": []any{map[string]any{"name": "sh", "executablePath": "/bin/sh", "pid": 4240, "namespacePid": 372, "euid": 0, "userId": 1001}}},
				"context": map[string]any{"toolName": "nsenter", "toolCategory": "Container escape tool"},
			},
		}
	default:
		base["type"], base["severity"], base["title"] = "Recon:EC2/PortProbeUnprotectedPort", 2.0, "Unprotected port on EC2 instance is being probed."
		base["description"] = "An unprotected port on an instance is being probed by a known scanner."
		base["resource"] = map[string]any{"resourceType": "Instance", "instanceDetails": map[string]any{"instanceId": "i-0" + strings.Repeat(c.account[11:], 16)}}
		base["service"] = map[string]any{"serviceName": "guardduty", "detectorId": estateDetector(c), "archived": false, "count": 3, "resourceRole": "TARGET"}
	}
	return base
}

func estateNodeID(account string, index int) string {
	return "i-0" + strings.Repeat(account[11:], 15) + itoa(index)
}

// attackSequence is the detection block of an attack sequence. The wire
// names are camelCase; the sequence ID is not an ARN; actors are processes
// keyed by hash; resources are typed EKS_CLUSTER, EC2_INSTANCE and
// CONTAINER, each with its own data block; indicators hold MITRE tactics,
// techniques and, for processes, JSON-encoded strings.
func (c estateCall) attackSequence(clusterARN, cluster string) map[string]any {
	actor := "process:" + strings.Repeat("ab", 32)
	node := estateNodeID(c.account, 0)
	container := "container:" + strings.Repeat("cd", 32)
	image := c.account + ".dkr.ecr." + c.region + ".amazonaws.com/tools/scratch:latest"
	signal := func(id, name, description string, created, last time.Duration, count int, indicators []any) map[string]any {
		return map[string]any{
			"uid":  "arn:aws:guardduty:" + c.region + ":" + c.account + ":detector/" + estateDetector(c) + "/finding/" + id,
			"type": "FINDING", "name": name, "description": description,
			"createdAt": epochSeconds(c.now.Add(-created)), "updatedAt": epochSeconds(c.now.Add(-last)),
			"firstSeenAt": epochSeconds(c.now.Add(-created)), "lastSeenAt": epochSeconds(c.now.Add(-last)),
			"severity": 5.0, "count": count, "resourceUids": []string{clusterARN, node, container},
			"actorIds": []string{actor}, "endpointIds": []string{}, "signalIndicators": indicators,
		}
	}
	return map[string]any{
		"uid":         "1794838096318-0a1b2c3d-" + c.account[8:12] + "-4000-8000-000000000a5e",
		"description": "A sequence of actions involving 2 signals indicating a possible cluster compromise.",
		"actors":      []any{map[string]any{"id": actor, "process": map[string]any{"name": "nsenter", "path": "/usr/bin/nsenter", "sha256": strings.Repeat("ab", 32)}}},
		"resources": []any{
			map[string]any{"uid": clusterARN, "name": cluster, "accountId": c.account, "resourceType": "EKS_CLUSTER", "region": c.region,
				"cloudPartition": "aws", "tags": []any{}, "data": map[string]any{"eksCluster": map[string]any{
					"createdAt": epochSeconds(c.now.Add(-days(400))), "status": "ACTIVE", "vpcId": estateVPC(c.account)}}},
			map[string]any{"uid": node, "name": node, "accountId": c.account, "resourceType": "EC2_INSTANCE", "region": c.region,
				"cloudPartition": "aws", "tags": []any{map[string]string{"key": "eks:cluster-name", "value": cluster}},
				"data": map[string]any{"ec2Instance": map[string]any{"availabilityZone": c.region + "a", "instanceType": "m6i.large",
					"instanceState": "running", "ec2NetworkInterfaceUids": []string{"ip-10-" + c.account[11:] + "-42-5." + c.region + ".compute.internal"}}}},
			map[string]any{"uid": container, "name": "runner", "accountId": c.account, "resourceType": "CONTAINER", "region": c.region,
				"cloudPartition": "aws", "tags": []any{}, "data": map[string]any{
					"container":          map[string]any{"image": image, "imageUid": "sha256:" + strings.Repeat("0c", 32)},
					"kubernetesWorkload": map[string]any{"containerUids": []string{container}, "namespace": "ci", "kubernetesResourcesTypes": "Pods"}}},
		},
		"endpoints": []any{},
		"signals": []any{
			signal(EstateRuntimeFinding, "Execution:Runtime/SuspiciousTool", "A container ran a tool commonly used for offensive security.",
				days(4), 5*time.Hour, 141, []any{
					map[string]any{"key": "ATTACK_TACTIC", "values": []string{"Execution"}},
					map[string]any{"key": "SUSPICIOUS_PROCESS", "values": []string{"/usr/bin/nsenter"}},
				}),
			signal("3f3e3d3c3b3a39383736353433323130", "PrivilegeEscalation:Runtime/DockerSocketAccessed", "A container accessed the Docker socket on its node.",
				days(3), days(1), 12, []any{map[string]any{"key": "ATTACK_TACTIC", "values": []string{"Privilege Escalation"}}}),
		},
		"sequenceIndicators": []any{
			map[string]any{"key": "ATTACK_TACTIC", "values": []string{"Execution", "Privilege Escalation"}},
			map[string]any{"key": "SUSPICIOUS_PROCESS", "values": []string{`{"/usr/bin/nsenter":["ESCAPE_TO_HOST"]}`, `{"/bin/sh":["SH_C"]}`}},
			map[string]any{"key": "ATTACK_TECHNIQUE", "values": []string{"T1611 - Escape to Host", "T1059 - Command and Scripting Interpreter"}},
		},
		"additionalSequenceTypes": []string{"AttackSequence:EC2/CompromisedInstance"},
	}
}

func (s *Server) estateGuardDutyFindings(writer http.ResponseWriter, call estateCall) {
	body := jsonBody(call.request)
	requested, _ := body["findingIds"].([]any)
	known := map[string]bool{}
	for _, id := range call.guardDutyIDs() {
		known[id] = true
	}
	findings := []any{}
	for _, value := range requested {
		if id, _ := value.(string); known[id] {
			findings = append(findings, call.guardDutyFinding(id))
		}
	}
	writeRESTJSONBody(writer, mustJSON(map[string]any{"findings": findings}))
}

// ---- Security Hub ----

// estateStandard is a Security Hub standard the estate subscribes to.
type estateStandard struct{ path, requirement string }

var (
	estateFoundational = estateStandard{path: "aws-foundational-security-best-practices/v/1.0.0"}
	estatePCI          = estateStandard{path: "pci-dss/v/4.0.1", requirement: "PCI DSS v4.0.1/1.3.1"}
	estateCIS          = estateStandard{path: "cis-aws-foundations-benchmark/v/3.0.0", requirement: "CIS AWS Foundations Benchmark v3.0.0/5.3"}
)

// estateSecurityHubID is a finding ID with consolidated control findings
// off, as in the surveyed estate: the standard subscription is part of the
// ID, so one failing control yields one finding per enabled standard.
func estateSecurityHubID(region, account string, standard estateStandard, control, uuid string) string {
	return "arn:aws:securityhub:" + region + ":" + account + ":subscription/" + standard.path + "/" + control + "/finding/" + uuid
}

// securityHubFinding is one control finding in the AWS Security Finding
// Format, standard-specific. origin is the Region the resource is in; the
// finding keeps it when the aggregation Region serves it too.
func (c estateCall) securityHubFinding(standard estateStandard, control, uuid, title, severity string, resource map[string]any, origin string) map[string]any {
	normalized := map[string]int{"CRITICAL": 90, "HIGH": 70, "MEDIUM": 40, "LOW": 1}[severity]
	id := estateSecurityHubID(origin, c.account, standard, control, uuid)
	resourceID, _ := resource["Id"].(string)
	resource["Partition"], resource["Region"], resource["Provider"] = "aws", origin, "AWS"
	resource["Owner"] = map[string]any{"Account": map[string]string{"Id": c.account}}
	compliance := map[string]any{"Status": "FAILED", "SecurityControlId": control,
		"AssociatedStandards": []any{map[string]any{"StandardsId": "standards/" + standard.path}}}
	if standard.requirement != "" {
		compliance["RelatedRequirements"] = []string{standard.requirement}
	}
	recommendation := "https://docs.aws.amazon.com/console/securityhub/" + control + "/remediation"
	return map[string]any{
		"SchemaVersion": "2018-10-08", "Id": id,
		"ProductArn": "arn:aws:securityhub:" + origin + "::product/aws/securityhub", "ProductName": "Security Hub", "CompanyName": "AWS",
		"Region": origin, "GeneratorId": standard.path + "/" + control, "AwsAccountId": c.account, "AwsAccountName": "estate-" + strings.ToLower(estateShort(c.account)),
		"Types":           []string{"Software and Configuration Checks/Industry and Regulatory Standards"},
		"FirstObservedAt": isoMillis(c.now.Add(-days(300))), "LastObservedAt": isoMillis(c.now.Add(-3 * time.Hour)),
		"CreatedAt": isoMillis(c.now.Add(-days(300))), "UpdatedAt": isoMillis(c.now.Add(-3 * time.Hour)),
		"Severity": map[string]any{"Product": float64(normalized), "Label": severity, "Normalized": normalized, "Original": severity},
		"Title":    control + " " + title, "Description": "This control checks " + strings.ToLower(title[:1]) + title[1:] + ".",
		"Remediation": map[string]any{"Recommendation": map[string]any{"Text": "For information on how to correct this issue, consult the AWS Security Hub controls documentation.",
			"Url": recommendation}},
		"ProductFields": map[string]string{
			"StandardsArn":             "arn:aws:securityhub:::standards/" + standard.path,
			"StandardsSubscriptionArn": "arn:aws:securityhub:" + origin + ":" + c.account + ":subscription/" + standard.path,
			"ControlId":                control, "RecommendationUrl": recommendation,
			"StandardsControlArn":         "arn:aws:securityhub:" + origin + ":" + c.account + ":control/" + standard.path + "/" + control,
			"RelatedAWSResources:0/name":  "securityhub-" + strings.ToLower(strings.ReplaceAll(control, ".", "-")),
			"RelatedAWSResources:0/type":  "AWS::Config::ConfigRule",
			"aws/securityhub/ProductName": "Security Hub", "aws/securityhub/CompanyName": "AWS", "Resources:0/Id": resourceID,
			"aws/securityhub/FindingId": "arn:aws:securityhub:" + origin + "::product/aws/securityhub/" + id, "PreviousComplianceStatus": "FAILED",
		},
		"Resources": []any{resource}, "Compliance": compliance,
		"WorkflowState": "NEW", "Workflow": map[string]string{"Status": "NEW"}, "RecordState": "ACTIVE",
		"FindingProviderFields": map[string]any{"Severity": map[string]string{"Label": severity, "Original": severity},
			"Types": []string{"Software and Configuration Checks/Industry and Regulatory Standards"}},
		"ProcessedAt": isoMillis(c.now.Add(-3 * time.Hour)),
	}
}

// homeSecurityHubFindings are the findings about resources in the home
// Region: the EKS endpoint control under the foundational standard, and
// one security group control failing under two standards, so the same
// resource and control appear under two finding IDs.
func (c estateCall) homeSecurityHubFindings() []map[string]any {
	cluster := "arn:aws:eks:" + EstateHomeRegion + ":" + c.account + ":cluster/" + estateClusterName(c.account)
	group := "sg-0" + strings.Repeat(c.account[11:], 16)
	groupARN := "arn:aws:ec2:" + EstateHomeRegion + ":" + c.account + ":security-group/" + group
	securityGroup := func() map[string]any {
		return map[string]any{"Type": "AwsEc2SecurityGroup", "Id": groupARN, "Details": map[string]any{"AwsEc2SecurityGroup": map[string]any{
			"GroupName": "bastion-sg", "GroupId": group, "OwnerId": c.account, "VpcId": estateVPC(c.account),
			"IpPermissions":       []any{map[string]any{"IpProtocol": "tcp", "FromPort": 0, "ToPort": 65535, "IpRanges": []any{map[string]string{"CidrIp": "0.0.0.0/0"}}}},
			"IpPermissionsEgress": []any{map[string]any{"IpProtocol": "-1", "IpRanges": []any{map[string]string{"CidrIp": "0.0.0.0/0"}}}},
		}}}
	}
	title := "Security groups should not allow ingress from 0.0.0.0/0 or ::/0 to port 3389"
	return []map[string]any{
		c.securityHubFinding(estateFoundational, "EKS.1", estateSecurityHubUUID(c.account, "eks1"), "EKS cluster endpoints should not be publicly accessible", "HIGH",
			map[string]any{"Type": "AwsEksCluster", "Id": cluster}, EstateHomeRegion),
		c.securityHubFinding(estatePCI, "EC2.14", estateSecurityHubUUID(c.account, "pci14"), title, "HIGH", securityGroup(), EstateHomeRegion),
		c.securityHubFinding(estateCIS, "EC2.14", estateSecurityHubUUID(c.account, "cis14"), title, "HIGH", securityGroup(), EstateHomeRegion),
	}
}

func estateSecurityHubUUID(account, suffix string) string {
	return "0b1c2d3e-" + account[8:12] + "-4000-8000-" + strings.Repeat("0", 12-len(suffix)) + suffix
}

// estateSecurityHubFindings: the home Region's findings are served from the
// home Region and again from the aggregation Region, with the same Id and
// Region; the aggregation Region adds its own finding (Config not
// recording there). Summing per Region double-counts, and so does
// counting per finding ID across standards. MaxResults and NextToken page
// the list.
func (s *Server) estateSecurityHubFindings(writer http.ResponseWriter, call estateCall) {
	findings := []map[string]any{}
	switch call.region {
	case EstateHomeRegion:
		findings = call.homeSecurityHubFindings()
	case EstateAggregationRegion:
		findings = append(findings, call.securityHubFinding(estateFoundational, "Config.1", estateSecurityHubUUID(call.account, "cfg1"),
			"AWS Config should be enabled and use the service-linked role for resource recording", "CRITICAL",
			map[string]any{"Type": "AwsAccount", "Id": "AWS::::Account:" + call.account}, call.region))
		findings = append(findings, call.homeSecurityHubFindings()...)
	default:
		estateError(writer, call, http.StatusBadRequest, "InvalidAccessException",
			"Account "+call.account+" is not subscribed to AWS Security Hub")
		return
	}
	body := jsonBody(call.request)
	if filters, _ := body["Filters"].(map[string]any); filters != nil {
		if ids, _ := filters["Id"].([]any); len(ids) > 0 {
			want, _ := ids[0].(map[string]any)
			kept := []map[string]any{}
			for _, finding := range findings {
				if finding["Id"] == want["Value"] {
					kept = append(kept, finding)
				}
			}
			findings = kept
		}
	}
	page, next := estatePage(len(findings), jsonString(body, "NextToken"), jsonInt(body, "MaxResults"))
	out := []any{}
	for _, index := range page {
		out = append(out, findings[index])
	}
	reply := map[string]any{"Findings": out}
	if next != "" {
		reply["NextToken"] = next
	}
	writeRESTJSONBody(writer, mustJSON(reply))
}

// estatePage pages count items by an opaque offset token.
func estatePage(count int, token string, limit int) ([]int, string) {
	indexes := make([]string, count)
	for index := range indexes {
		indexes[index] = itoa(index)
	}
	if decoded, err := base64.StdEncoding.DecodeString(token); err == nil {
		token = strings.TrimPrefix(string(decoded), "estate:")
	}
	page, next := pageOf(indexes, token, limit)
	out := make([]int, 0, len(page))
	for _, value := range page {
		index, _ := strconv.Atoi(value)
		out = append(out, index)
	}
	if next != "" {
		next = base64.StdEncoding.EncodeToString([]byte("estate:" + next))
	}
	return out, next
}

// estateFindingAggregators: every member sees the one aggregator, in the
// aggregation Region, from whichever Region it asks.
func (s *Server) estateFindingAggregators(writer http.ResponseWriter, call estateCall) {
	writeRESTJSONBody(writer, mustJSON(map[string]any{"FindingAggregators": []any{map[string]any{
		"FindingAggregatorArn": "arn:aws:securityhub:" + EstateAggregationRegion + ":" + call.account + ":finding-aggregator/0a1b2c3d-0000-4000-8000-00000000a99e",
	}}}))
}

// ---- Inspector ----

// inspectorOn: Inspector scans only in the shared-services account's home
// Region; elsewhere it is not enabled.
func (c estateCall) inspectorOn() bool { return c.account == EstateShared && c.home() }

func (s *Server) estateInspectorStatus(writer http.ResponseWriter, call estateCall) {
	state := func(on bool) map[string]any {
		if on {
			return map[string]any{"status": "ENABLED"}
		}
		return map[string]any{"status": "DISABLED"}
	}
	on := call.inspectorOn()
	writeRESTJSONBody(writer, mustJSON(map[string]any{"accounts": []any{map[string]any{
		"accountId": call.account, "state": state(on),
		"resourceState": map[string]any{"ec2": state(on), "ecr": state(on), "lambda": state(on), "lambdaCode": state(false), "codeRepository": state(false)},
	}}, "failedAccounts": []any{}}))
}

func (s *Server) estateInspectorFindings(writer http.ResponseWriter, call estateCall) {
	if !call.inspectorOn() {
		estateError(writer, call, http.StatusForbidden, "AccessDeniedException", "Invoking account is not enabled.")
		return
	}
	criteria, _ := jsonBody(call.request)["filterCriteria"].(map[string]any)
	findings := []any{}
	for _, finding := range []map[string]any{call.inspectorPackageFinding(), call.inspectorNetworkFinding()} {
		if inspectorMatches(finding, criteria) {
			findings = append(findings, finding)
		}
	}
	writeRESTJSONBody(writer, mustJSON(map[string]any{"findings": findings}))
}

// inspectorFilterFields maps the string filters the estate honours to the
// finding field each one reads. A finding without the field (a network
// finding has no exploitAvailable) never matches a filter on it.
var inspectorFilterFields = map[string]string{
	"findingType": "type", "findingStatus": "status", "severity": "severity", "exploitAvailable": "exploitAvailable", "fixAvailable": "fixAvailable",
}

func inspectorMatches(finding map[string]any, criteria map[string]any) bool {
	for filter, field := range inspectorFilterFields {
		conditions, _ := criteria[filter].([]any)
		if len(conditions) == 0 {
			continue
		}
		value, _ := finding[field].(string)
		matched := false
		for _, item := range conditions {
			condition, _ := item.(map[string]any)
			want, _ := condition["value"].(string)
			switch condition["comparison"] {
			case "EQUALS":
				matched = matched || (value != "" && value == want)
			case "NOT_EQUALS":
				matched = matched || (value != "" && value != want)
			case "PREFIX":
				matched = matched || (value != "" && strings.HasPrefix(value, want))
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func (c estateCall) inspectorPackageFinding() map[string]any {
	digest := "sha256:" + strings.Repeat("0c", 32)
	return map[string]any{
		"awsAccountId": c.account, "findingArn": c.arn("inspector2", "finding/"+strings.Repeat("4a", 16)),
		"type": "PACKAGE_VULNERABILITY", "status": "ACTIVE", "severity": "CRITICAL", "title": "CVE-2024-0001 - examplelib",
		"description": "A heap overflow in examplelib allows remote code execution.", "inspectorScore": 9.8,
		"exploitAvailable": "YES", "fixAvailable": "YES", "exploitabilityDetails": map[string]any{"lastKnownExploitAt": epochSeconds(c.now.Add(-days(30)))},
		"epss":            map[string]any{"score": 0.91},
		"firstObservedAt": epochSeconds(c.now.Add(-days(200))), "lastObservedAt": epochSeconds(c.now.Add(-days(1))), "updatedAt": epochSeconds(c.now.Add(-days(1))),
		"remediation": map[string]any{"recommendation": map[string]any{"text": "None Provided"}},
		"packageVulnerabilityDetails": map[string]any{
			"vulnerabilityId": "CVE-2024-0001", "source": "NVD", "vendorSeverity": "CRITICAL", "sourceUrl": "https://nvd.nist.gov/vuln/detail/CVE-2024-0001",
			"cvss":               []any{map[string]any{"baseScore": 9.8, "scoringVector": "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", "source": "NVD", "version": "3.1"}},
			"vulnerablePackages": []any{map[string]any{"name": "examplelib", "version": "1.2.3", "fixedInVersion": "1.2.4", "packageManager": "OS", "arch": "X86_64", "epoch": 0, "release": "1.amzn2023"}},
			"referenceUrls":      []string{"https://example.com/advisories/0001"}, "relatedVulnerabilities": []string{},
		},
		"resources": []any{map[string]any{
			"type": "AWS_ECR_CONTAINER_IMAGE", "id": c.arn("ecr", "repository/platform/payments-api/"+digest), "partition": "aws", "region": c.region,
			"details": map[string]any{"awsEcrContainerImage": map[string]any{
				"repositoryName": "platform/payments-api", "imageHash": digest, "imageTags": []string{"v1.41.0"}, "registry": c.account,
				"pushedAt": epochSeconds(c.now.Add(-days(200))), "lastInUseAt": epochSeconds(c.now.Add(-days(2))), "inUseCount": 6,
				"platform": "AMAZON_LINUX_2023", "architecture": "amd64",
			}},
		}},
	}
}

// inspectorNetworkFinding has no exploitAvailable field: network
// reachability findings carry a path instead.
func (c estateCall) inspectorNetworkFinding() map[string]any {
	instance := "i-0" + strings.Repeat(c.account[11:], 16)
	return map[string]any{
		"awsAccountId": c.account, "findingArn": c.arn("inspector2", "finding/"+strings.Repeat("5b", 16)),
		"type": "NETWORK_REACHABILITY", "status": "ACTIVE", "severity": "MEDIUM", "title": "Port 22 is reachable from a Transit Gateway - TCP",
		"description":     "On the instance, the port range 22 - 22 is reachable from a transit gateway attachment.",
		"firstObservedAt": epochSeconds(c.now.Add(-days(60))), "lastObservedAt": epochSeconds(c.now.Add(-days(1))), "updatedAt": epochSeconds(c.now.Add(-days(1))),
		"fixAvailable": "YES", "remediation": map[string]any{"recommendation": map[string]any{"text": "Edit the security group to remove access from the transit gateway."}},
		"networkReachabilityDetails": map[string]any{
			"protocol": "TCP", "openPortRange": map[string]any{"begin": 22, "end": 22},
			"networkPath": map[string]any{"steps": []any{
				map[string]any{"componentId": EstateTransitGateway, "componentType": "AWS::EC2::TransitGateway"},
				map[string]any{"componentId": "tgw-attach-0" + strings.Repeat(c.account[11:], 16), "componentType": "AWS::EC2::TransitGatewayAttachment"},
				map[string]any{"componentId": "acl-0" + strings.Repeat(c.account[11:], 16), "componentType": "AWS::EC2::NetworkAcl"},
				map[string]any{"componentId": "sg-0" + strings.Repeat(c.account[11:], 16), "componentType": "AWS::EC2::SecurityGroup"},
				map[string]any{"componentId": "eni-0" + strings.Repeat(c.account[11:], 16), "componentType": "AWS::EC2::NetworkInterface"},
			}},
		},
		"resources": []any{map[string]any{"type": "AWS_EC2_INSTANCE", "id": instance, "partition": "aws", "region": c.region,
			"details": map[string]any{"awsEc2Instance": map[string]any{"type": "m6i.large", "vpcId": estateVPC(c.account), "platform": "AMAZON_LINUX_2023"}}}},
	}
}

// estateAnalyzers: no account has an IAM Access Analyzer, so there are no
// external-access findings to read.
func (s *Server) estateAnalyzers(writer http.ResponseWriter, _ estateCall) {
	writeRESTJSONBody(writer, mustJSON(map[string]any{"analyzers": []any{}}))
}

// ---- RDS events ----

type estateDBEvent struct {
	source, sourceType, message string
	categories                  []string
	ago                         time.Duration
}

// estateDBEvents: workloads sees backups and a creation; shared services
// had a failover, a restart and a recovery last week.
func estateDBEvents(account string) []estateDBEvent {
	switch account {
	case EstateWorkloads:
		return []estateDBEvent{
			{source: "orders-db", sourceType: "db-instance", message: "Finished DB Instance backup", categories: []string{"backup"}, ago: 20 * time.Hour},
			{source: "payments-db", sourceType: "db-cluster", message: "DB cluster created", categories: []string{"creation"}, ago: days(5)},
			{source: "rds:payments-db-2026-11-21-02-05", sourceType: "db-cluster-snapshot", message: "Creating automated cluster snapshot",
				categories: []string{"backup"}, ago: days(1)},
		}
	case EstateShared:
		return []estateDBEvent{
			{source: "ci-db", sourceType: "db-instance", message: "Multi-AZ instance failover started.", categories: []string{"failover"}, ago: days(3)},
			{source: "ci-db", sourceType: "db-instance", message: "DB instance restarted", categories: []string{"availability"}, ago: days(3) - 4*time.Minute},
			{source: "ci-db", sourceType: "db-instance",
				message:    "Recovery of the DB instance has started. Recovery time will vary with the amount of data to be recovered.",
				categories: []string{"recovery"}, ago: days(3) - 6*time.Minute},
			{source: "ci-db", sourceType: "db-instance", message: "Recovery of the DB instance is complete.", categories: []string{"recovery"}, ago: days(3) - 19*time.Minute},
		}
	}
	return nil
}

func (s *Server) estateDBEvents(writer http.ResponseWriter, call estateCall) {
	members := ""
	if call.home() {
		for _, event := range estateDBEvents(call.account) {
			kind := map[string]string{"db-instance": "db", "db-cluster": "cluster", "db-cluster-snapshot": "cluster-snapshot", "db-snapshot": "snapshot"}[event.sourceType]
			members += "<Event>" + xmlText("SourceIdentifier", event.source) + xmlText("SourceType", event.sourceType) + xmlText("Message", event.message) +
				"<EventCategories>"
			for _, category := range event.categories {
				members += xmlText("EventCategory", category)
			}
			members += "</EventCategories>" + xmlText("Date", isoMillis(call.now.Add(-event.ago))) +
				xmlText("SourceArn", call.arn("rds", kind+":"+event.source)) + "</Event>"
		}
	}
	writeQuery(writer, "DescribeEvents", "<Events>"+members+"</Events>")
}

// ---- ACM ----

func estateCertificateID(account string, index int) string {
	return "0a0a0a0a-" + account[8:12] + "-4000-8000-00000000000" + itoa(index)
}

type estateCert struct {
	index          int
	region, domain string
	// wildcard adds *.domain as a second name.
	wildcard bool
	status   string
	notAfter time.Duration // from now; negative means expired
	inUseBy  func(estateCall) []string
}

func estateCertificates(account string) []estateCert {
	switch account {
	case EstateHub:
		return []estateCert{
			{index: 1, region: EstateGlobalRegion, domain: "cdn.shop.example.com", status: "ISSUED", notAfter: days(200),
				inUseBy: func(c estateCall) []string {
					return []string{"arn:aws:cloudfront::" + EstateHub + ":distribution/E1ESTATEEXAMPLE"}
				}},
			{index: 2, region: EstateHomeRegion, domain: "shop.example.com", wildcard: true, status: "ISSUED", notAfter: days(150),
				inUseBy: func(c estateCall) []string {
					return []string{c.arn("elasticloadbalancing", "loadbalancer/app/edge-public/0a1b2c3d4e5f6a7b")}
				}},
			{index: 3, region: EstateGlobalRegion, domain: "old.shop.example.com", status: "EXPIRED", notAfter: -days(60)},
		}
	case EstateData:
		return []estateCert{{index: 1, region: EstateHomeRegion, domain: "ingest.shop.example.com", status: "EXPIRED", notAfter: -days(12)}}
	}
	return nil
}

func (cert estateCert) names() []string {
	if cert.wildcard {
		return []string{cert.domain, "*." + cert.domain}
	}
	return []string{cert.domain}
}

func (c estateCall) certificateARN(cert estateCert) string {
	return "arn:aws:acm:" + cert.region + ":" + c.account + ":certificate/" + estateCertificateID(c.account, cert.index)
}

func (s *Server) estateCertificates(writer http.ResponseWriter, call estateCall) {
	summaries := []any{}
	for _, cert := range estateCertificates(call.account) {
		if cert.region != call.region {
			continue
		}
		inUse := cert.inUseBy != nil
		summaries = append(summaries, map[string]any{
			"CertificateArn": call.certificateARN(cert), "DomainName": cert.domain, "SubjectAlternativeNameSummaries": cert.names(),
			"ExportOption": "DISABLED", "Exported": false,
			"HasAdditionalSubjectAlternativeNames": false, "Status": cert.status, "Type": "AMAZON_ISSUED", "KeyAlgorithm": "RSA-2048",
			"KeyUsages": []string{"DIGITAL_SIGNATURE", "KEY_ENCIPHERMENT"}, "ExtendedKeyUsages": []string{"TLS_WEB_SERVER_AUTHENTICATION"},
			"InUse": inUse, "RenewalEligibility": map[bool]string{true: "ELIGIBLE", false: "INELIGIBLE"}[inUse],
			"NotBefore": epochSeconds(call.now.Add(cert.notAfter - days(395))), "NotAfter": epochSeconds(call.now.Add(cert.notAfter)),
			"CreatedAt": epochSeconds(call.now.Add(cert.notAfter - days(400))), "IssuedAt": epochSeconds(call.now.Add(cert.notAfter - days(395))),
		})
	}
	writeJSONBody(writer, mustJSON(map[string]any{"CertificateSummaryList": summaries}))
}

func (s *Server) estateCertificate(writer http.ResponseWriter, call estateCall) {
	arn := jsonString(jsonBody(call.request), "CertificateArn")
	for _, cert := range estateCertificates(call.account) {
		if cert.region != call.region || call.certificateARN(cert) != arn {
			continue
		}
		inUseBy := []string{}
		if cert.inUseBy != nil {
			inUseBy = cert.inUseBy(call)
		}
		validations := []any{}
		for _, name := range cert.names() {
			validations = append(validations, map[string]any{"DomainName": name, "ValidationDomain": name, "ValidationStatus": "SUCCESS",
				"ResourceRecord":   map[string]any{"Name": "_0a1b2c3d4e5f." + cert.domain + ".", "Type": "CNAME", "Value": "_6a7b8c9d0e1f.mhbtsbpdnt.acm-validations.aws."},
				"ValidationMethod": "DNS"})
		}
		writeJSONBody(writer, mustJSON(map[string]any{"Certificate": map[string]any{
			"CertificateArn": arn, "DomainName": cert.domain, "SubjectAlternativeNames": cert.names(),
			"DomainValidationOptions": validations,
			"Serial":                  "0a:1b:2c:3d:4e:5f:6a:7b:8c:9d:0e:1f:2a:3b:4c:5d", "Subject": "CN=" + cert.domain, "Issuer": "Amazon",
			"CreatedAt": epochSeconds(call.now.Add(cert.notAfter - days(400))), "IssuedAt": epochSeconds(call.now.Add(cert.notAfter - days(395))),
			"Status": cert.status, "NotBefore": epochSeconds(call.now.Add(cert.notAfter - days(395))), "NotAfter": epochSeconds(call.now.Add(cert.notAfter)),
			"KeyAlgorithm": "RSA-2048", "SignatureAlgorithm": "SHA256WITHRSA", "InUseBy": inUseBy, "Type": "AMAZON_ISSUED",
			"KeyUsages":          []any{map[string]string{"Name": "DIGITAL_SIGNATURE"}, map[string]string{"Name": "KEY_ENCIPHERMENT"}},
			"ExtendedKeyUsages":  []any{map[string]string{"Name": "TLS_WEB_SERVER_AUTHENTICATION", "OID": "1.3.6.1.5.5.7.3.1"}},
			"RenewalEligibility": map[bool]string{true: "ELIGIBLE", false: "INELIGIBLE"}[len(inUseBy) > 0],
			"Options":            map[string]string{"CertificateTransparencyLoggingPreference": "ENABLED", "Export": "DISABLED"},
		}}))
		return
	}
	estateError(writer, call, http.StatusBadRequest, "ResourceNotFoundException", "Could not find certificate "+arn+".")
}

// ---- Lambda ----

type estateFunction struct {
	name, runtime string
	// url is the function URL's auth type; empty means no URL. A function
	// in denied refuses both URL reads.
	url    string
	denied bool
}

// estateFunctions: deprecated runtimes in workloads, a container-image
// function with no Runtime field, and URL reads denied for most functions.
func estateFunctions(account string) []estateFunction {
	switch account {
	case EstateHub:
		return []estateFunction{{name: "lz-guardrail-notify", runtime: "python3.13"}, {name: "lz-tag-audit", runtime: "python3.13", denied: true}}
	case EstateWorkloads:
		return []estateFunction{
			{name: "payments-webhook", runtime: "nodejs16.x", denied: true},
			{name: "orders-export", runtime: "python3.8", denied: true},
			{name: "image-resize", runtime: "", denied: true},
			{name: "status-page", runtime: "nodejs20.x", url: "AWS_IAM"},
		}
	case EstateShared:
		return []estateFunction{{name: "ci-cleanup", runtime: "provided.al2023", denied: true}}
	case EstateData:
		return []estateFunction{{name: "etl-trigger", runtime: "python3.12"}, {name: "etl-notify", runtime: "python3.12", denied: true}}
	}
	return nil
}

// EstateDeniedFunctionURLs counts the functions whose URL configuration the
// reader cannot read, per account.
func EstateDeniedFunctionURLs(account string) int {
	count := 0
	for _, function := range estateFunctions(account) {
		if function.denied {
			count++
		}
	}
	return count
}

func (s *Server) estateFunctions(writer http.ResponseWriter, call estateCall) {
	functions := []any{}
	if call.home() {
		for _, function := range estateFunctions(call.account) {
			entry := map[string]any{
				"FunctionName": function.name, "FunctionArn": call.arn("lambda", "function:"+function.name),
				"Role": "arn:aws:iam::" + call.account + ":role/lambda-exec", "CodeSize": 2048, "Description": "",
				"Timeout": 30, "MemorySize": 256, "LastModified": lambdaTime(call.now.Add(-days(90))), "Version": "$LATEST",
				"TracingConfig": map[string]string{"Mode": "PassThrough"}, "Architectures": []string{"x86_64"},
				"EphemeralStorage": map[string]int{"Size": 512}, "LoggingConfig": map[string]string{"LogFormat": "Text", "LogGroup": "/aws/lambda/" + function.name},
				"CodeSha256": "q1w2e3r4t5y6u7i8o9p0a1s2d3f4g5h6j7k8l9z0x1c=", "RevisionId": "0c0c0c0c-" + call.account[8:12] + "-4000-8000-00000000a1b2",
				"SnapStart": map[string]string{"ApplyOn": "None", "OptimizationStatus": "Off"},
			}
			if function.runtime == "" {
				entry["PackageType"] = "Image"
			} else {
				entry["PackageType"], entry["Runtime"], entry["Handler"] = "Zip", function.runtime, "index.handler"
			}
			functions = append(functions, entry)
		}
	}
	writeRESTJSONBody(writer, mustJSON(map[string]any{"Functions": functions}))
}

func (c estateCall) function(name string) (estateFunction, bool) {
	for _, function := range estateFunctions(c.account) {
		if function.name == name {
			return function, true
		}
	}
	return estateFunction{}, false
}

// estateLambdaURLDenied is the message Lambda's front end gives for a
// denied function-URL read: it names neither the action nor the caller.
const estateLambdaURLDenied = "Unable to determine service/operation name to be authorized"

func (s *Server) estateFunctionURL(writer http.ResponseWriter, call estateCall) {
	name := strings.Split(strings.Trim(call.request.path, "/"), "/")[2]
	function, ok := call.function(name)
	arn := call.arn("lambda", "function:"+name)
	switch {
	case !ok:
		estateError(writer, call, http.StatusNotFound, "ResourceNotFoundException", "Function not found: "+arn)
	case function.denied:
		estateError(writer, call, http.StatusForbidden, "AccessDeniedException", estateLambdaURLDenied)
	case function.url == "":
		estateError(writer, call, http.StatusNotFound, "ResourceNotFoundException", "The resource you requested does not exist.")
	default:
		writeRESTJSONBody(writer, mustJSON(call.functionURL(function)))
	}
}

func (c estateCall) functionURL(function estateFunction) map[string]any {
	return map[string]any{
		"FunctionUrl": "https://" + strings.Repeat("a1b2c3", 5) + "ab.lambda-url." + c.region + ".on.aws/", "FunctionArn": c.arn("lambda", "function:"+function.name),
		"AuthType": function.url, "CreationTime": isoMillis(c.now.Add(-days(100))), "LastModifiedTime": isoMillis(c.now.Add(-days(100))),
		"InvokeMode": "BUFFERED", "Cors": map[string]any{"AllowOrigins": []string{"*"}, "AllowMethods": []string{"GET"}, "MaxAge": 0},
	}
}

func (s *Server) estateFunctionURLs(writer http.ResponseWriter, call estateCall) {
	name := strings.Split(strings.Trim(call.request.path, "/"), "/")[2]
	function, ok := call.function(name)
	arn := call.arn("lambda", "function:"+name)
	switch {
	case !ok:
		estateError(writer, call, http.StatusNotFound, "ResourceNotFoundException", "Function not found: "+arn)
	case function.denied:
		estateError(writer, call, http.StatusForbidden, "AccessDeniedException", estateLambdaURLDenied)
	case function.url == "":
		writeRESTJSONBody(writer, `{"FunctionUrlConfigs":[]}`)
	default:
		writeRESTJSONBody(writer, mustJSON(map[string]any{"FunctionUrlConfigs": []any{call.functionURL(function)}}))
	}
}

// ---- Service Quotas ----

func (s *Server) estateServiceQuota(writer http.ResponseWriter, call estateCall) {
	body := jsonBody(call.request)
	service, code := jsonString(body, "ServiceCode"), jsonString(body, "QuotaCode")
	var quota map[string]any
	switch service + "/" + code {
	case "lambda/L-B99A9384":
		// The hub was never raised from the new-account default.
		value := 400.0
		if call.account == EstateHub {
			value = 10
		}
		quota = map[string]any{"ServiceName": "AWS Lambda", "QuotaName": "Concurrent executions", "Value": value, "Unit": "Count",
			"UsageMetric": map[string]any{"MetricNamespace": "AWS/Lambda", "MetricName": "ConcurrentExecutions",
				"MetricDimensions": map[string]string{}, "MetricStatisticRecommendation": "Maximum"},
			"Description": "The maximum number of events that functions can process simultaneously in the current Region."}
	case "vpc/L-F678F1CE":
		quota = map[string]any{"ServiceName": "Amazon Virtual Private Cloud (Amazon VPC)", "QuotaName": "VPCs per Region", "Value": 5.0, "Unit": "None",
			"UsageMetric": map[string]any{"MetricNamespace": "AWS/Usage", "MetricName": "ResourceCount",
				"MetricDimensions":              map[string]string{"Class": "None", "Resource": "vpcsPerRegion", "Service": "EC2", "Type": "Resource"},
				"MetricStatisticRecommendation": "Maximum"},
			"Period":      map[string]any{"PeriodValue": 5, "PeriodUnit": "MINUTE"},
			"Description": "The maximum number of VPCs per Region."}
	default:
		estateError(writer, call, http.StatusBadRequest, "NoSuchResourceException", "The request failed because the specified service quota does not exist.")
		return
	}
	quota["ServiceCode"], quota["QuotaCode"] = service, code
	quota["QuotaArn"] = "arn:aws:servicequotas:" + call.region + ":" + call.account + ":" + service + "/" + code
	quota["Adjustable"], quota["GlobalQuota"], quota["QuotaAppliedAtLevel"] = true, false, "ACCOUNT"
	writeJSONBody(writer, mustJSON(map[string]any{"Quota": quota}))
}
