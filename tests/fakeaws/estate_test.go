package fakeaws

import (
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type estateReply struct {
	status    int
	body      string
	errorType string
}

// estateDo sends one signed request in the estate scenario as account, the
// way the SDK shapes it: a form body for query actions, a target header for
// JSON services, a path for REST services.
func estateDo(t *testing.T, server *Server, account, region, service, method, path, target, body string) estateReply {
	t.Helper()
	request, err := http.NewRequest(method, "https://"+service+"."+region+".amazonaws.com"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=ASIAKEY/20261122/"+region+"/"+service+"/aws4_request, SignedHeaders=host, Signature=x")
	request.Header.Set(AccountHeader, account)
	request.Header.Set(ScenarioHeader, ScenarioEstateHubSpoke)
	switch {
	case target != "":
		request.Header.Set("X-Amz-Target", target)
		request.Header.Set("Content-Type", "application/x-amz-json-1.1")
	case strings.Contains(body, "Action="):
		request.Header.Set("Content-Type", formType)
	}
	response, err := server.HTTPClient().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return estateReply{status: response.StatusCode, body: string(data), errorType: response.Header.Get("X-Amzn-Errortype")}
}

func estateQuery(t *testing.T, server *Server, account, region, service string, values url.Values) estateReply {
	t.Helper()
	return estateDo(t, server, account, region, service, http.MethodPost, "/", "", values.Encode())
}

func newEstate(t *testing.T) *Server {
	server := New(t, "")
	server.SetClock(func() time.Time { return time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC) })
	return server
}

func TestEstateAliasRecordsKeepRoute53Forms(t *testing.T) {
	server := newEstate(t)
	reply := estateDo(t, server, EstateShared, EstateGlobalRegion, "route53", http.MethodGet, "/2013-04-01/hostedzone/"+EstatePublicZoneID+"/rrset", "", "")
	if reply.status != http.StatusOK {
		t.Fatalf("records: %d %s", reply.status, reply.body)
	}
	var page struct {
		Records []struct {
			Name   string
			Type   string
			Alias  *struct{ HostedZoneId, DNSName string } `xml:"AliasTarget"`
			Values []string                                `xml:"ResourceRecords>ResourceRecord>Value"`
		} `xml:"ResourceRecordSets>ResourceRecordSet"`
	}
	if err := xml.Unmarshal([]byte(reply.body), &page); err != nil {
		t.Fatal(err)
	}
	aliases := map[string][]string{}
	for _, record := range page.Records {
		if !strings.HasSuffix(record.Name, ".") {
			t.Fatalf("record name %q has no trailing dot", record.Name)
		}
		if record.Alias != nil {
			aliases[record.Alias.DNSName] = append(aliases[record.Alias.DNSName], record.Type)
		}
		if record.Type == "CNAME" && strings.HasPrefix(record.Name, "assets.") && strings.HasSuffix(record.Values[0], ".") {
			t.Fatalf("CNAME value %q gained a trailing dot", record.Values[0])
		}
	}
	// The application load balancer alias: dualstack prefix, trailing dot,
	// once for A and once for AAAA.
	if types := aliases["dualstack."+EstateHubALBDNS+"."]; len(types) != 2 {
		t.Fatalf("hub alias types = %v, want A and AAAA (aliases %v)", types, aliases)
	}
	// An application load balancer alias may also come without the prefix,
	// and a wildcard name can be an alias too.
	if _, ok := aliases[EstateHubALBDNS+"."]; !ok {
		t.Fatalf("ALB alias without dualstack missing: %v", aliases)
	}
	if types := aliases["dualstack."+EstateWorkloadsALBDNS+"."]; len(types) != 2 {
		t.Fatalf("aliases to the workloads load balancer (payments and the wildcard) = %v", types)
	}
	// The network load balancer alias has no dualstack prefix.
	if _, ok := aliases[EstateDataNLBDNS+"."]; !ok {
		t.Fatalf("NLB alias missing: %v", aliases)
	}
	if _, ok := aliases[EstateDistributionDNS+"."]; !ok {
		t.Fatalf("CloudFront alias missing: %v", aliases)
	}
	if !strings.Contains(reply.body, `<Name>\052.`+EstatePublicZone+`</Name>`) {
		t.Fatal("wildcard record name is not octal-escaped")
	}
	if !strings.Contains(reply.body, `<HostedZoneId>`+EstateCloudFrontZone+`</HostedZoneId>`) {
		t.Fatal("CloudFront alias zone missing")
	}

	// The private zone names VPCs of other accounts, by ID only.
	reply = estateDo(t, server, EstateShared, EstateGlobalRegion, "route53", http.MethodGet, "/2013-04-01/hostedzone/"+EstatePrivateZoneID, "", "")
	for _, account := range []string{EstateHub, EstateWorkloads, EstateData, EstateOutside} {
		if !strings.Contains(reply.body, "<VPCId>"+estateVPC(account)+"</VPCId>") {
			t.Fatalf("private zone lacks the VPC of %s: %s", account, reply.body)
		}
	}
	if strings.Contains(reply.body, EstateHub) || strings.Contains(reply.body, "Owner") {
		t.Fatal("a zone association must not name the VPC owner")
	}
	if !strings.Contains(reply.body, "<Features/>") {
		t.Fatalf("private zone lacks its empty Features element: %s", reply.body)
	}
	// Only the DNS account owns zones.
	if reply := estateDo(t, server, EstateWorkloads, EstateGlobalRegion, "route53", http.MethodGet, "/2013-04-01/hostedzone", "", ""); strings.Contains(reply.body, "<HostedZone>") {
		t.Fatalf("workloads lists zones: %s", reply.body)
	}
}

func TestEstateSecurityHubServesTheHomeFindingTwice(t *testing.T) {
	server := newEstate(t)
	type finding struct {
		Id, Region string
		Resources  []struct{ Id string }
		Compliance struct{ SecurityControlId string }
	}
	read := func(region, body string) []finding {
		reply := estateDo(t, server, EstateWorkloads, region, "securityhub", http.MethodPost, "/findings", "", body)
		if reply.status != http.StatusOK {
			t.Fatalf("%s: %d %s", region, reply.status, reply.body)
		}
		var page struct{ Findings []finding }
		if err := json.Unmarshal([]byte(reply.body), &page); err != nil {
			t.Fatal(err)
		}
		return page.Findings
	}
	home, aggregated := read(EstateHomeRegion, "{}"), read(EstateAggregationRegion, "{}")
	if len(home) != 3 || len(aggregated) != 4 {
		t.Fatalf("home %d, aggregation %d findings; want 3 and 4", len(home), len(aggregated))
	}
	// Consolidated control findings are off: the ID names the standard,
	// and one control on one security group fails under two standards.
	perResource := map[string]int{}
	for _, item := range home {
		if !strings.Contains(item.Id, ":subscription/") {
			t.Fatalf("finding ID %q does not name a standard subscription", item.Id)
		}
		for _, resource := range item.Resources {
			perResource[item.Compliance.SecurityControlId+" "+resource.Id]++
		}
	}
	twice := 0
	for _, count := range perResource {
		if count == 2 {
			twice++
		}
	}
	if twice != 1 {
		t.Fatalf("control/resource pairs = %v, want one served under two standards", perResource)
	}
	seen := 0
	for _, item := range append(home, aggregated...) {
		if item.Id == EstateSecurityFinding {
			seen++
			if item.Region != EstateHomeRegion {
				t.Fatalf("duplicate carries Region %q, want the origin %q", item.Region, EstateHomeRegion)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("finding %s served %d times across Regions, want 2", EstateSecurityFinding, seen)
	}
	byID := read(EstateAggregationRegion, `{"Filters":{"Id":[{"Value":"`+EstateSecurityFinding+`","Comparison":"EQUALS"}]}}`)
	if len(byID) != 1 || byID[0].Id != EstateSecurityFinding {
		t.Fatalf("Id filter = %v", byID)
	}
	// MaxResults pages the list with an opaque token.
	reply := estateDo(t, server, EstateWorkloads, EstateAggregationRegion, "securityhub", http.MethodPost, "/findings", "", `{"MaxResults":3}`)
	var first struct {
		Findings  []finding
		NextToken string
	}
	if err := json.Unmarshal([]byte(reply.body), &first); err != nil || len(first.Findings) != 3 || first.NextToken == "" {
		t.Fatalf("first page = %v %s", err, reply.body)
	}
	if rest := read(EstateAggregationRegion, `{"MaxResults":3,"NextToken":"`+first.NextToken+`"}`); len(rest) != 1 {
		t.Fatalf("second page has %d findings, want 1", len(rest))
	}
	reply = estateDo(t, server, EstateWorkloads, EstateHomeRegion, "securityhub", http.MethodGet, "/findingAggregator/list", "", "")
	if !strings.Contains(reply.body, `"FindingAggregatorArn":"arn:aws:securityhub:`+EstateAggregationRegion+`:`) {
		t.Fatalf("finding aggregators = %d %s", reply.status, reply.body)
	}
	if reply := estateDo(t, server, EstateWorkloads, EstateGlobalRegion, "securityhub", http.MethodPost, "/findings", "", "{}"); reply.errorType != "InvalidAccessException" {
		t.Fatalf("us-east-1 = %d %q", reply.status, reply.errorType)
	}
}

type estateRoleXML struct {
	RoleName string
	Trust    string `xml:"AssumeRolePolicyDocument"`
	LastUsed *struct {
		LastUsedDate string
	} `xml:"RoleLastUsed"`
}

func estateRolesOf(t *testing.T, server *Server, account string) map[string]estateRoleXML {
	t.Helper()
	reply := estateQuery(t, server, account, EstateGlobalRegion, "iam", url.Values{"Action": {"GetAccountAuthorizationDetails"}, "Filter.member.1": {"Role"}})
	var page struct {
		Roles []estateRoleXML `xml:"GetAccountAuthorizationDetailsResult>RoleDetailList>member"`
	}
	if err := xml.Unmarshal([]byte(reply.body), &page); err != nil {
		t.Fatalf("%v: %s", err, reply.body)
	}
	roles := map[string]estateRoleXML{}
	for _, role := range page.Roles {
		if strings.HasPrefix(role.Trust, "{") || !strings.HasPrefix(role.Trust, "%7B") {
			t.Fatalf("%s trust is not URL-encoded: %.40s", role.RoleName, role.Trust)
		}
		decoded, err := url.QueryUnescape(role.Trust)
		if err != nil || !json.Valid([]byte(decoded)) {
			t.Fatalf("%s trust does not decode to JSON: %v", role.RoleName, err)
		}
		role.Trust = decoded
		roles[role.RoleName] = role
	}
	return roles
}

func TestEstateTrustPoliciesKeepIAMForms(t *testing.T) {
	server := newEstate(t)
	workloads := estateRolesOf(t, server, EstateWorkloads)
	if trust := workloads["legacy-cross-deploy"].Trust; !strings.Contains(trust, `"`+EstateDeletedRoleID+`"`) ||
		!strings.Contains(trust, `{"Sid":"","Effect":"Allow","Principal":{"AWS":"`+EstateDeletedUserID+`"}`) {
		t.Fatalf("deleted principals not shown as unique IDs (a role in a list, a user alone with an empty Sid): %s", trust)
	}
	if trust := workloads["orders-worker-irsa"].Trust; !strings.Contains(trust, `:sub":["system:serviceaccount:orders:orders-worker",`) || strings.Contains(trust, ":aud") {
		t.Fatalf("multi-account service-account trust (a sub list, no aud) = %s", trust)
	}
	if trust := workloads["gha-payments-deploy"].Trust; !strings.Contains(trust, "repo:example-org/payments-api:ref:refs/heads/main") {
		t.Fatalf("pinned GitHub trust = %s", trust)
	}
	if trust := workloads["payments-api-irsa"].Trust; !strings.Contains(trust, "system:serviceaccount:payments:payments-api") {
		t.Fatalf("service-account trust = %s", trust)
	}
	if trust := workloads["lz-admin"].Trust; !strings.Contains(trust, "arn:aws:iam::"+EstateManagement+":root") || strings.Contains(trust, "Condition") {
		t.Fatalf("landing-zone admin trust = %s", trust)
	}
	// A role never used carries an empty RoleLastUsed element.
	if last := workloads["lz-admin"].LastUsed; last == nil || last.LastUsedDate != "" {
		t.Fatalf("never-used role last used = %+v, want an empty element", last)
	}
	if last := workloads["gha-payments-deploy"].LastUsed; last == nil || last.LastUsedDate == "" {
		t.Fatal("used role has no last-used date")
	}

	shared := estateRolesOf(t, server, EstateShared)
	if trust := shared["ci-runner"].Trust; !strings.Contains(trust, `"repo:example-org/*"`) || !strings.Contains(trust, `"Statement":{`) {
		t.Fatalf("unpinned GitHub trust (a single Statement object) = %s", trust)
	}
	hub := estateRolesOf(t, server, EstateHub)
	if trust := hub["scheduler-tag-audit"].Trust; !strings.Contains(trust, `"aws:SourceAccount":"`+EstateHub+`"`) || !strings.Contains(trust, `"ArnEquals":{"aws:SourceArn"`) {
		t.Fatalf("confused-deputy service trust = %s", trust)
	}
	data := estateRolesOf(t, server, EstateData)
	if trust := data["gha-etl-deploy"].Trust; !strings.Contains(trust, `:sub":"repo:example-org/etl-jobs:ref:refs/heads/main"`) ||
		!strings.Contains(trust, `:role/github-actions-runner-role"]`) {
		t.Fatalf("GitHub trust with a caller statement = %s", trust)
	}
	if trust := data["vendor-monitoring"].Trust; !strings.Contains(trust, `"sts:ExternalId"`) || !strings.Contains(trust, EstateVendor) {
		t.Fatalf("ExternalId trust = %s", trust)
	}
	if trust := data["etl-reader"].Trust; !strings.Contains(trust, `"aws:PrincipalOrgID":"`+EstateOrgID+`"`) || !strings.Contains(trust, "ForAnyValue:StringLike") {
		t.Fatalf("organization trust = %s", trust)
	}

	reply := estateQuery(t, server, EstateData, EstateGlobalRegion, "iam", url.Values{"Action": {"GetRole"}, "RoleName": {"etl-reader"}})
	if reply.status != http.StatusOK || !strings.Contains(reply.body, "<RoleLastUsed><LastUsedDate>") {
		t.Fatalf("GetRole = %d %s", reply.status, reply.body)
	}
}

func TestEstateCredentialReportForms(t *testing.T) {
	server := newEstate(t)
	reply := estateQuery(t, server, EstateWorkloads, EstateGlobalRegion, "iam", url.Values{"Action": {"GetCredentialReport"}})
	var page struct {
		Content string `xml:"GetCredentialReportResult>Content"`
		Format  string `xml:"GetCredentialReportResult>ReportFormat"`
	}
	if err := xml.Unmarshal([]byte(reply.body), &page); err != nil || page.Format != "text/csv" {
		t.Fatalf("report: %v %s", err, reply.body)
	}
	decoded, err := base64.StdEncoding.DecodeString(page.Content)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(decoded), "\n")
	if lines[0] != strings.Join(CredentialReportColumns(), ",") {
		t.Fatalf("header = %s", lines[0])
	}
	for index, line := range lines {
		if got := strings.Count(line, ",") + 1; got != len(CredentialReportColumns()) {
			t.Fatalf("line %d has %d cells", index, got)
		}
	}
	for _, form := range []string{"<root_account>", "not_supported", "no_information", ",N/A,"} {
		if !strings.Contains(string(decoded), form) {
			t.Fatalf("report lacks the %q form", form)
		}
	}
	// Cells are RFC 3339 with a Z; the root password is enabled; a
	// deactivated key still carries its last use.
	if strings.Contains(string(decoded), "+00:00") || !strings.Contains(string(decoded), ":00Z,") {
		t.Fatal("report timestamps are not in the Z form")
	}
	cells := map[string][]string{}
	for _, line := range lines[1:] {
		row := strings.Split(line, ",")
		cells[row[0]] = row
	}
	if root := cells["<root_account>"]; root[3] != "true" || root[6] != "not_supported" || !strings.HasSuffix(root[5], "Z") {
		t.Fatalf("root row = %v", root)
	}
	if user := cells["svc-s3-export"]; user[8] != "false" || user[10] == "N/A" {
		t.Fatalf("deactivated key row = %v", user)
	}
	// The hub has never generated a report.
	reply = estateQuery(t, server, EstateHub, EstateGlobalRegion, "iam", url.Values{"Action": {"GetCredentialReport"}})
	if reply.status != http.StatusGone || !strings.Contains(reply.body, "<Code>ReportNotPresent</Code>") || strings.Contains(reply.body, "<Message>") {
		t.Fatalf("hub report = %d %s", reply.status, reply.body)
	}
}

func TestEstateTargetsArePodAddresses(t *testing.T) {
	server := newEstate(t)
	group := estateTargetGroups(EstateWorkloads)[0]
	arn := "arn:aws:elasticloadbalancing:" + EstateHomeRegion + ":" + EstateWorkloads + ":" + group.id
	reply := estateQuery(t, server, EstateWorkloads, EstateHomeRegion, "elasticloadbalancing", url.Values{"Action": {"DescribeTargetHealth"}, "TargetGroupArn": {arn}})
	var page struct {
		Targets []struct {
			ID       string `xml:"Target>Id"`
			State    string `xml:"TargetHealth>State"`
			Reason   string `xml:"TargetHealth>Reason"`
			Override string `xml:"AdministrativeOverride>State"`
		} `xml:"DescribeTargetHealthResult>TargetHealthDescriptions>member"`
	}
	if err := xml.Unmarshal([]byte(reply.body), &page); err != nil || len(page.Targets) != 3 {
		t.Fatalf("targets: %v %s", err, reply.body)
	}
	unhealthy := 0
	for _, target := range page.Targets {
		if !strings.HasPrefix(target.ID, "10.2.42.") {
			t.Fatalf("target %q is not a pod address", target.ID)
		}
		if target.Override != "no_override" {
			t.Fatalf("target %q has no administrative override block", target.ID)
		}
		if target.State == "unhealthy" && target.Reason == "Target.ResponseCodeMismatch" {
			unhealthy++
		}
	}
	if unhealthy != 1 {
		t.Fatalf("unhealthy targets = %d, want 1", unhealthy)
	}
	reply = estateQuery(t, server, EstateData, EstateHomeRegion, "elasticloadbalancing", url.Values{"Action": {"DescribeTargetGroups"}})
	if !strings.Contains(reply.body, "<LoadBalancerArns/>") || !strings.Contains(reply.body, "<TargetType>ip</TargetType>") {
		t.Fatalf("data target groups lack an orphan or IP group: %s", reply.body)
	}
	// The network load balancer's group checks over TCP, with no matcher.
	if !strings.Contains(reply.body, "<Protocol>TCP</Protocol>") || !strings.Contains(reply.body, "<HealthCheckEnabled>true</HealthCheckEnabled>") {
		t.Fatalf("data target groups lack a TCP group: %s", reply.body)
	}
	// A group with no registered target answers an empty list, not an error.
	orphan := estateTargetGroups(EstateData)[1]
	reply = estateQuery(t, server, EstateData, EstateHomeRegion, "elasticloadbalancing", url.Values{"Action": {"DescribeTargetHealth"},
		"TargetGroupArn": {"arn:aws:elasticloadbalancing:" + EstateHomeRegion + ":" + EstateData + ":" + orphan.id}})
	if reply.status != http.StatusOK || !strings.Contains(reply.body, "<TargetHealthDescriptions></TargetHealthDescriptions>") {
		t.Fatalf("empty target group = %d %s", reply.status, reply.body)
	}
}

func TestEstateCrossAccountLinksAreOneSided(t *testing.T) {
	server := newEstate(t)
	attachments := func(account string) []struct{ Owner, Gateway, Type string } {
		reply := estateQuery(t, server, account, EstateHomeRegion, "ec2", url.Values{"Action": {"DescribeTransitGatewayAttachments"}})
		var page struct {
			Items []struct {
				Owner   string `xml:"resourceOwnerId"`
				Gateway string `xml:"transitGatewayOwnerId"`
				Type    string `xml:"resourceType"`
			} `xml:"transitGatewayAttachments>item"`
		}
		if err := xml.Unmarshal([]byte(reply.body), &page); err != nil {
			t.Fatal(err)
		}
		out := []struct{ Owner, Gateway, Type string }{}
		for _, item := range page.Items {
			out = append(out, struct{ Owner, Gateway, Type string }{item.Owner, item.Gateway, item.Type})
		}
		return out
	}
	if hub := attachments(EstateHub); len(hub) != 6 {
		t.Fatalf("hub sees %d attachments, want 6 (four VPCs, a VPN, a peering)", len(hub))
	}
	spoke := attachments(EstateData)
	if len(spoke) != 1 || spoke[0].Owner != EstateData || spoke[0].Gateway != EstateHub {
		t.Fatalf("spoke attachments = %+v", spoke)
	}

	// RAM: the hub's share reaches every spoke through the organization.
	reply := estateDo(t, server, EstateData, EstateHomeRegion, "ram", http.MethodPost, "/getresourceshares", "", `{"resourceOwner":"OTHER-ACCOUNTS"}`)
	if !strings.Contains(reply.body, `"owningAccountId":"`+EstateHub+`"`) {
		t.Fatalf("spoke inbound shares = %s", reply.body)
	}
	reply = estateDo(t, server, EstateHub, EstateHomeRegion, "ram", http.MethodPost, "/getresourceshareassociations", "", `{"associationType":"PRINCIPAL"}`)
	for _, unit := range EstateOrganizationalUnits() {
		if !strings.Contains(reply.body, `"associatedEntity":"`+unit+`"`) {
			t.Fatalf("hub share lacks the unit %s: %s", unit, reply.body)
		}
	}
	// The owner sees the share's tags and configuration; a receiver sees
	// neither key.
	reply = estateDo(t, server, EstateHub, EstateHomeRegion, "ram", http.MethodPost, "/getresourceshares", "", `{"resourceOwner":"SELF"}`)
	if !strings.Contains(reply.body, `"tags":[`) || !strings.Contains(reply.body, "retainSharingOnAccountLeaveOrganization") {
		t.Fatalf("owner share = %s", reply.body)
	}
	reply = estateDo(t, server, EstateData, EstateHomeRegion, "ram", http.MethodPost, "/getresourceshares", "", `{"resourceOwner":"OTHER-ACCOUNTS"}`)
	if strings.Contains(reply.body, `"tags"`) || strings.Contains(reply.body, "resourceShareConfiguration") {
		t.Fatalf("receiver share = %s", reply.body)
	}
	// A spoke sees the hub's transit gateway without the hub's tags.
	reply = estateQuery(t, server, EstateData, EstateHomeRegion, "ec2", url.Values{"Action": {"DescribeTransitGateways"}})
	if !strings.Contains(reply.body, "<ownerId>"+EstateHub+"</ownerId>") || !strings.Contains(reply.body, "<tagSet/>") {
		t.Fatalf("spoke transit gateway = %s", reply.body)
	}

	// Resource policies show only on the owner.
	reply = estateDo(t, server, EstateWorkloads, EstateHomeRegion, "s3", http.MethodGet, "/wkl-exports-0a1b2c?policy=", "", "")
	if reply.status != http.StatusOK || !strings.Contains(reply.body, "arn:aws:iam::"+EstateData+":root") || strings.HasPrefix(reply.body, "<") {
		t.Fatalf("bucket policy (a raw JSON body) = %d %s", reply.status, reply.body)
	}
	reply = estateDo(t, server, EstateData, EstateHomeRegion, "kms", http.MethodPost, "/", "TrentService.GetKeyPolicy", `{"KeyId":"`+estateDataKeyID+`","PolicyName":"default"}`)
	if !strings.Contains(reply.body, `kms:ViaService`) || !strings.Contains(reply.body, EstateWorkloads) {
		t.Fatalf("key policy = %s", reply.body)
	}
	reply = estateDo(t, server, EstateShared, EstateHomeRegion, "ecr", http.MethodPost, "/", "AmazonEC2ContainerRegistry_V20150921.GetRepositoryPolicy",
		`{"repositoryName":"platform/payments-api"}`)
	if !strings.Contains(reply.body, `aws:PrincipalOrgID`) || !strings.Contains(reply.body, `\n`) {
		t.Fatalf("org repository policy (indented text) = %s", reply.body)
	}

	// The cluster endpoint is open to the world and control-plane logging is off.
	reply = estateDo(t, server, EstateWorkloads, EstateHomeRegion, "eks", http.MethodGet, "/clusters/"+estateClusterName(EstateWorkloads), "", "")
	if !strings.Contains(reply.body, `"publicAccessCidrs":["0.0.0.0/0"]`) || !strings.Contains(reply.body, `"enabled":false`) || strings.Contains(reply.body, `"health"`) {
		t.Fatalf("cluster (open endpoint, logging off, no health element when healthy) = %s", reply.body)
	}
	reply = estateDo(t, server, EstateHub, EstateGlobalRegion, "acm", http.MethodPost, "/", "CertificateManager.DescribeCertificate",
		`{"CertificateArn":"arn:aws:acm:us-east-1:`+EstateHub+`:certificate/`+estateCertificateID(EstateHub, 1)+`"}`)
	if !strings.Contains(reply.body, `"InUseBy":["arn:aws:cloudfront::`+EstateHub+`:distribution/E1ESTATEEXAMPLE"]`) {
		t.Fatalf("certificate in use = %s", reply.body)
	}
}

func TestEstateNotConfiguredAndDeniedAnswers(t *testing.T) {
	server := newEstate(t)
	cases := []struct {
		name                       string
		account, region, service   string
		method, path, target, body string
		status                     int
		code                       string
	}{
		{"bucket without policy", EstateWorkloads, EstateHomeRegion, "s3", http.MethodGet, "/wkl-logs-6a7b8c?policy=", "", "", http.StatusNotFound, "<Code>NoSuchBucketPolicy</Code>"},
		{"repository without policy", EstateShared, EstateHomeRegion, "ecr", http.MethodPost, "/", "AmazonEC2ContainerRegistry_V20150921.GetRepositoryPolicy",
			`{"repositoryName":"tools/scratch"}`, http.StatusBadRequest, `"__type":"RepositoryPolicyNotFoundException"`},
		{"function URL denied", EstateWorkloads, EstateHomeRegion, "lambda", http.MethodGet, "/2021-10-31/functions/orders-export/url", "", "",
			http.StatusForbidden, "AccessDeniedException"},
		{"function without URL", EstateData, EstateHomeRegion, "lambda", http.MethodGet, "/2021-10-31/functions/etl-trigger/url", "", "",
			http.StatusNotFound, "ResourceNotFoundException"},
		{"organization listing denied", EstateHub, EstateGlobalRegion, "organizations", http.MethodPost, "/", "AWSOrganizationsV20161128.ListAccounts", "{}",
			http.StatusBadRequest, `"__type":"AccessDeniedException"`},
		{"inspector not enabled", EstateHub, EstateHomeRegion, "inspector2", http.MethodPost, "/findings/list", "", "{}",
			http.StatusForbidden, "AccessDeniedException"},
	}
	for _, item := range cases {
		reply := estateDo(t, server, item.account, item.region, item.service, item.method, item.path, item.target, item.body)
		if reply.status != item.status || !(strings.Contains(reply.body, item.code) || reply.errorType == item.code) {
			t.Errorf("%s = %d %q %s; want %d %s", item.name, reply.status, reply.errorType, reply.body, item.status, item.code)
		}
	}
	// The function-URL denial names neither the action nor the caller.
	reply := estateDo(t, server, EstateWorkloads, EstateHomeRegion, "lambda", http.MethodGet, "/2021-10-31/functions/orders-export/urls", "", "")
	if reply.status != http.StatusForbidden || !strings.Contains(reply.body, "Unable to determine service/operation name to be authorized") {
		t.Fatalf("function URL list denial = %d %s", reply.status, reply.body)
	}
	if count := EstateDeniedFunctionURLs(EstateWorkloads); count != 3 {
		t.Fatalf("denied function URLs in workloads = %d", count)
	}
	// No detector in us-east-1: an empty list, not an error.
	if reply := estateDo(t, server, EstateShared, EstateGlobalRegion, "guardduty", http.MethodGet, "/detector", "", ""); reply.body != `{"detectorIds":[]}` {
		t.Fatalf("us-east-1 detectors = %s", reply.body)
	}
	if reply := estateDo(t, server, EstateData, EstateHomeRegion, "access-analyzer", http.MethodGet, "/analyzer", "", ""); reply.body != `{"analyzers":[]}` {
		t.Fatalf("analyzers = %s", reply.body)
	}
}

func TestEstateGuardDutyServesAnAttackSequence(t *testing.T) {
	server := newEstate(t)
	call := estateCall{account: EstateShared, region: EstateHomeRegion}
	detector := estateDetector(call)
	reply := estateDo(t, server, EstateShared, EstateHomeRegion, "guardduty", http.MethodPost, "/detector/"+detector+"/findings/get", "",
		`{"findingIds":["`+EstateAttackSequenceFinding+`"]}`)
	var page struct {
		Findings []struct {
			Arn, Type                   string
			AssociatedAttackSequenceArn string
			Resource                    struct{ ResourceType string }
			Service                     struct {
				Detection struct {
					Sequence struct {
						Uid       string
						Signals   []struct{ Name string }
						Resources []struct{ ResourceType, Uid string }
					}
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(reply.body), &page); err != nil || len(page.Findings) != 1 {
		t.Fatalf("findings: %v %s", err, reply.body)
	}
	finding := page.Findings[0]
	if finding.Type != "AttackSequence:EKS/CompromisedCluster" || len(finding.Service.Detection.Sequence.Signals) != 2 {
		t.Fatalf("attack sequence = %+v", finding)
	}
	// The sequence is its own resource and points at itself; its ID is not
	// an ARN; the resources it names carry upper-case type names.
	sequence := finding.Service.Detection.Sequence
	if finding.Resource.ResourceType != "AttackSequence" || finding.AssociatedAttackSequenceArn != finding.Arn || strings.HasPrefix(sequence.Uid, "arn:") {
		t.Fatalf("attack sequence identity = %+v", finding)
	}
	types := []string{}
	for _, resource := range sequence.Resources {
		types = append(types, resource.ResourceType)
	}
	if strings.Join(types, ",") != "EKS_CLUSTER,EC2_INSTANCE,CONTAINER" {
		t.Fatalf("sequence resource types = %v", types)
	}
	// maxResults pages the finding IDs.
	reply = estateDo(t, server, EstateShared, EstateHomeRegion, "guardduty", http.MethodPost, "/detector/"+detector+"/findings", "", `{"maxResults":1}`)
	var ids struct {
		FindingIds []string
		NextToken  string
	}
	if err := json.Unmarshal([]byte(reply.body), &ids); err != nil || len(ids.FindingIds) != 1 || ids.NextToken == "" {
		t.Fatalf("first ID page = %v %s", err, reply.body)
	}
	reply = estateDo(t, server, EstateShared, EstateHomeRegion, "guardduty", http.MethodPost, "/detector/"+detector+"/findings", "",
		`{"maxResults":1,"nextToken":"`+ids.NextToken+`"}`)
	if err := json.Unmarshal([]byte(reply.body), &ids); err != nil || len(ids.FindingIds) != 1 || ids.NextToken != "" {
		t.Fatalf("last ID page = %v %s", err, reply.body)
	}
	// Inspector filters: a network finding has no exploitAvailable field,
	// so an exploit filter drops it.
	for _, item := range []struct{ criteria, want, unwanted string }{
		{`{"exploitAvailable":[{"comparison":"EQUALS","value":"YES"}],"findingStatus":[{"comparison":"EQUALS","value":"ACTIVE"}]}`, "PACKAGE_VULNERABILITY", "NETWORK_REACHABILITY"},
		{`{"findingType":[{"comparison":"EQUALS","value":"NETWORK_REACHABILITY"}]}`, "NETWORK_REACHABILITY", "PACKAGE_VULNERABILITY"},
	} {
		reply = estateDo(t, server, EstateShared, EstateHomeRegion, "inspector2", http.MethodPost, "/findings/list", "", `{"filterCriteria":`+item.criteria+`}`)
		if !strings.Contains(reply.body, `"type":"`+item.want+`"`) || strings.Contains(reply.body, `"type":"`+item.unwanted+`"`) {
			t.Fatalf("filter %s = %s", item.criteria, reply.body)
		}
	}
}

func TestEstateLeavesOtherScenariosAlone(t *testing.T) {
	server := New(t, "")
	request, err := http.NewRequest(http.MethodGet, "https://route53.amazonaws.com/2013-04-01/hostedzone/"+EstatePublicZoneID+"/rrset", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=ASIAKEY/20261122/us-east-1/route53/aws4_request, SignedHeaders=host, Signature=x")
	request.Header.Set(AccountHeader, EstateShared)
	response, err := server.HTTPClient().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if strings.Contains(string(data), "dualstack.") {
		t.Fatal("the default scenario served an estate record")
	}
	// In the estate scenario, a caller outside the four accounts gets the
	// ordinary fixture.
	reply := estateDo(t, server, "111122223333", EstateGlobalRegion, "iam", http.MethodPost, "/", "", url.Values{"Action": {"GetCredentialReport"}}.Encode())
	if strings.Contains(reply.body, "GetCredentialReportResult") {
		t.Fatalf("a non-estate caller got the estate report: %s", reply.body)
	}
	if len(EstateOperations()) < 40 {
		t.Fatalf("estate answers %d operations", len(EstateOperations()))
	}
}

func TestEstateAnswersDiscoveryCalls(t *testing.T) {
	server := newEstate(t)
	// The one cluster lives in the home Region; elsewhere the list is empty.
	if reply := estateDo(t, server, EstateWorkloads, EstateHomeRegion, "eks", http.MethodGet, "/clusters", "", ""); reply.body != `{"clusters":["`+estateClusterName(EstateWorkloads)+`"]}` {
		t.Fatalf("home clusters = %s", reply.body)
	}
	if reply := estateDo(t, server, EstateWorkloads, EstateGlobalRegion, "eks", http.MethodGet, "/clusters", "", ""); reply.body != `{"clusters":[]}` {
		t.Fatalf("us-east-1 clusters = %s", reply.body)
	}
	// DescribeRegions lists enabled Regions only, unless AllRegions asks
	// for the opt-in Regions that are off.
	regions := func(values url.Values) []struct{ Name, Status string } {
		reply := estateQuery(t, server, EstateData, EstateGlobalRegion, "ec2", values)
		var page struct {
			Items []struct {
				Name   string `xml:"regionName"`
				Status string `xml:"optInStatus"`
			} `xml:"regionInfo>item"`
		}
		if err := xml.Unmarshal([]byte(reply.body), &page); err != nil {
			t.Fatalf("%v: %s", err, reply.body)
		}
		out := []struct{ Name, Status string }{}
		for _, item := range page.Items {
			out = append(out, struct{ Name, Status string }{item.Name, item.Status})
		}
		return out
	}
	enabled := regions(url.Values{"Action": {"DescribeRegions"}})
	all := regions(url.Values{"Action": {"DescribeRegions"}, "AllRegions": {"true"}})
	home := false
	for _, region := range enabled {
		if region.Status == "not-opted-in" {
			t.Fatalf("enabled list holds %s", region.Name)
		}
		home = home || (region.Name == EstateHomeRegion && region.Status == "opted-in")
	}
	if !home || len(all) <= len(enabled) {
		t.Fatalf("regions enabled %v, all %v", enabled, all)
	}
}
