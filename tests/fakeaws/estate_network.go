package fakeaws

import (
	"net/http"
	"strings"
	"time"
)

// Network links of the estate: the hub's transit gateway and its RAM
// share, the shared-services DNS zones whose aliases point at load
// balancers and a distribution in other accounts, load balancers with pod
// IP targets, and one EKS cluster per account.

func init() {
	registerEstate(map[string]estateHandler{
		"ec2:DescribeTransitGateways":                (*Server).estateTransitGateways,
		"ec2:DescribeTransitGatewayAttachments":      (*Server).estateTransitGatewayAttachments,
		"ec2:DescribeTransitGatewayVpcAttachments":   (*Server).estateTransitGatewayVPCAttachments,
		"ram:GetResourceShares":                      (*Server).estateResourceShares,
		"ram:GetResourceShareAssociations":           (*Server).estateResourceShareAssociations,
		"ram:ListResources":                          (*Server).estateSharedResources,
		"route53:ListHostedZones":                    (*Server).estateHostedZones,
		"route53:GetHostedZone":                      (*Server).estateHostedZone,
		"route53:ListResourceRecordSets":             (*Server).estateRecordSets,
		"cloudfront:ListDistributions":               (*Server).estateDistributions,
		"elasticloadbalancing:DescribeLoadBalancers": (*Server).estateLoadBalancers,
		"elasticloadbalancing:DescribeTargetGroups":  (*Server).estateTargetGroups,
		"elasticloadbalancing:DescribeTargetHealth":  (*Server).estateTargetHealth,
		"eks:ListClusters":                           (*Server).estateClusters,
		"eks:DescribeCluster":                        (*Server).estateCluster,
		"ec2:DescribeRegions":                        (*Server).estateRegions,
		"eks:ListInsights":                           (*Server).estateInsights,
		"eks:DescribeInsight":                        (*Server).estateInsight,
	})
}

// Load balancer DNS names. An application load balancer's name is
// <name>-<id>.<region>.elb.amazonaws.com (internal ones start with
// "internal-"); a network load balancer's is
// <name>-<hex>.elb.<region>.amazonaws.com. Route 53 aliases add a
// "dualstack." prefix to the first form and end every name with a dot.
const (
	EstateHubALBDNS       = "edge-public-1234567890." + EstateHomeRegion + ".elb.amazonaws.com"
	EstateWorkloadsALBDNS = "internal-k8s-payments-paymentsa-0a1b2c3d4e-1234567890." + EstateHomeRegion + ".elb.amazonaws.com"
	EstateDataNLBDNS      = "k8s-ingest-ingestnl-5f6a7b8c9d-0123456789abcdef.elb." + EstateHomeRegion + ".amazonaws.com"
	EstateDistributionDNS = "d1a2b3c4d5e6f7.cloudfront.net"
	// EstateDanglingAliasDNS names a load balancer in no estate account.
	EstateDanglingAliasDNS = "dualstack.legacy-gone-9876543210." + EstateHomeRegion + ".elb.amazonaws.com."
	estateELBZone          = "Z0ELBEXAMPLEZONE0001"
	estateNLBZone          = "Z0NLBEXAMPLEZONE0002"
)

func estateAttachmentID(account string) string {
	return "tgw-attach-0" + strings.Repeat(account[len(account)-1:], 16)
}

func (s *Server) estateTransitGateways(writer http.ResponseWriter, call estateCall) {
	items := ""
	if call.home() {
		items = "<item>" + xmlText("transitGatewayId", EstateTransitGateway) +
			xmlText("transitGatewayArn", "arn:aws:ec2:"+call.region+":"+EstateHub+":transit-gateway/"+EstateTransitGateway) +
			xmlText("state", "available") + xmlText("ownerId", EstateHub) + xmlText("description", "landing zone transit") +
			xmlText("creationTime", isoMillis(call.now.Add(-days(1000)))) + "<options>" + xmlText("amazonSideAsn", "64512") +
			xmlText("autoAcceptSharedAttachments", "enable") + xmlText("defaultRouteTableAssociation", "disable") +
			xmlText("defaultRouteTablePropagation", "disable") + xmlText("vpnEcmpSupport", "enable") + xmlText("dnsSupport", "enable") +
			xmlText("securityGroupReferencingSupport", "enable") + xmlText("multicastSupport", "disable") + "</options>" +
			call.ownerTags(EstateHub, [][2]string{{"Name", "lz-transit"}, {"landing_zone", "true"}, {"cost-centre", ""}}) + "</item>"
	}
	writeEC2(writer, "DescribeTransitGateways", "<transitGatewaySet>"+items+"</transitGatewaySet>", "")
}

// ownerTags is the tag set a shared resource shows: tags belong to the
// account that set them, so a receiving account sees an empty set.
func (c estateCall) ownerTags(owner string, tags [][2]string) string {
	if c.account != owner {
		return "<tagSet/>"
	}
	return ec2TagSet(tags)
}

type estateAttachment struct {
	id, resourceType, resourceID, resourceOwner string
}

// estateAttachments are the attachments the caller can see: every one from
// the hub, only its own from a spoke.
func (c estateCall) estateAttachments() []estateAttachment {
	if !c.home() {
		return nil
	}
	all := []estateAttachment{}
	for _, account := range EstateAccounts() {
		all = append(all, estateAttachment{id: estateAttachmentID(account), resourceType: "vpc", resourceID: estateVPC(account), resourceOwner: account})
	}
	all = append(all,
		estateAttachment{id: "tgw-attach-0f0f0f0f0f0f0f0f1", resourceType: "vpn", resourceID: "vpn-0a0a0a0a0a0a0a0a1", resourceOwner: EstateHub},
		estateAttachment{id: "tgw-attach-0f0f0f0f0f0f0f0f2", resourceType: "peering", resourceID: "tgw-0b0b0b0b0b0b0b0b2", resourceOwner: EstateOutside})
	if c.account == EstateHub {
		return all
	}
	for _, attachment := range all {
		if attachment.resourceOwner == c.account {
			return []estateAttachment{attachment}
		}
	}
	return nil
}

func (s *Server) estateTransitGatewayAttachments(writer http.ResponseWriter, call estateCall) {
	items := ""
	for _, attachment := range call.estateAttachments() {
		items += "<item>" + xmlText("transitGatewayAttachmentId", attachment.id) + xmlText("transitGatewayId", EstateTransitGateway) +
			xmlText("transitGatewayOwnerId", EstateHub) + xmlText("resourceOwnerId", attachment.resourceOwner) +
			xmlText("resourceType", attachment.resourceType) + xmlText("resourceId", attachment.resourceID) + xmlText("state", "available") +
			"<association>" + xmlText("transitGatewayRouteTableId", "tgw-rtb-0c0c0c0c0c0c0c0c3") + xmlText("state", "associated") + "</association>" +
			xmlText("creationTime", isoMillis(call.now.Add(-days(900)))) + "<tagSet/></item>"
	}
	writeEC2(writer, "DescribeTransitGatewayAttachments", "<transitGatewayAttachments>"+items+"</transitGatewayAttachments>", "")
}

func (s *Server) estateTransitGatewayVPCAttachments(writer http.ResponseWriter, call estateCall) {
	items := ""
	for _, attachment := range call.estateAttachments() {
		if attachment.resourceType != "vpc" {
			continue
		}
		items += "<item>" + xmlText("transitGatewayAttachmentId", attachment.id) + xmlText("transitGatewayId", EstateTransitGateway) +
			xmlText("vpcId", attachment.resourceID) + xmlText("vpcOwnerId", attachment.resourceOwner) + xmlText("state", "available") +
			"<subnetIds><item>subnet-0" + strings.Repeat(attachment.resourceOwner[11:], 16) + "</item></subnetIds>" +
			xmlText("creationTime", isoMillis(call.now.Add(-days(900)))) + "<options>" + xmlText("dnsSupport", "enable") +
			xmlText("securityGroupReferencingSupport", "disable") + xmlText("ipv6Support", "disable") +
			xmlText("applianceModeSupport", "disable") + "</options><tagSet/></item>"
	}
	writeEC2(writer, "DescribeTransitGatewayVpcAttachments", "<transitGatewayVpcAttachments>"+items+"</transitGatewayVpcAttachments>", "")
}

type estateShare struct {
	arn, name, owner string
	// principals are the accounts or organizational units the share is
	// associated with.
	principals    []string
	resources     []string
	allowExternal bool
	tags          [][2]string
}

// EstateOrganizationalUnits are the units the hub's transit share is
// associated with. RAM names a unit, not the organization, when a landing
// zone shares by OU.
func EstateOrganizationalUnits() []string {
	prefix := "arn:aws:organizations::" + EstateManagement + ":ou/" + EstateOrgID + "/"
	return []string{prefix + "ou-ab12-workloads", prefix + "ou-ab12-shared", prefix + "ou-ab12-data"}
}

func estateShares(now time.Time) []estateShare {
	return []estateShare{
		{arn: "arn:aws:ram:" + EstateHomeRegion + ":" + EstateHub + ":resource-share/0a0a0a0a-1b1b-4c4c-8d8d-0e0e0e0e0e01", name: "TGW Share lz",
			owner: EstateHub, principals: EstateOrganizationalUnits(), allowExternal: true, tags: [][2]string{{"Name", "TGW Share lz"}, {"landing_zone", "true"}},
			resources: []string{"arn:aws:ec2:" + EstateHomeRegion + ":" + EstateHub + ":transit-gateway/" + EstateTransitGateway}},
		{arn: "arn:aws:ram:" + EstateHomeRegion + ":" + EstateWorkloads + ":resource-share/0a0a0a0a-1b1b-4c4c-8d8d-0e0e0e0e0e02", name: "resolver-rules",
			owner: EstateWorkloads, principals: []string{EstateShared},
			resources: []string{"arn:aws:route53resolver:" + EstateHomeRegion + ":" + EstateWorkloads + ":resolver-rule/rslvr-rr-0a1b2c3d4e5f6a7b8"}},
	}
}

// receives reports whether account is a principal of the share: named
// directly, or through an organizational unit (every estate account sits
// in one).
func (share estateShare) receives(account string) bool {
	if share.owner == account {
		return false
	}
	for _, principal := range share.principals {
		if principal == account || strings.HasPrefix(principal, "arn:aws:organizations") {
			return true
		}
	}
	return false
}

// visibleShares are the shares the caller owns (SELF) or receives
// (OTHER-ACCOUNTS). The transit share goes to the landing-zone units, so
// every spoke receives it.
func (c estateCall) visibleShares(owner string) []estateShare {
	var out []estateShare
	if !c.home() {
		return nil
	}
	for _, share := range estateShares(c.now) {
		if (owner == "SELF" && share.owner == c.account) || (owner == "OTHER-ACCOUNTS" && share.receives(c.account)) {
			out = append(out, share)
		}
	}
	return out
}

// estateResourceShares: the owner sees the share's tags (only when it has
// any) and its configuration; a receiver sees neither key.
func (s *Server) estateResourceShares(writer http.ResponseWriter, call estateCall) {
	shares := []any{}
	for _, share := range call.visibleShares(jsonString(jsonBody(call.request), "resourceOwner")) {
		entry := map[string]any{
			"resourceShareArn": share.arn, "name": share.name, "owningAccountId": share.owner, "allowExternalPrincipals": share.allowExternal,
			"status": "ACTIVE", "creationTime": epochSeconds(call.now.Add(-days(900) + 609*time.Millisecond)),
			"lastUpdatedTime": epochSeconds(call.now.Add(-days(300) + 144*time.Millisecond)), "featureSet": "STANDARD",
		}
		if share.owner == call.account {
			if len(share.tags) > 0 {
				tags := []any{}
				for _, tag := range share.tags {
					tags = append(tags, map[string]string{"key": tag[0], "value": tag[1]})
				}
				entry["tags"] = tags
			}
			entry["resourceShareConfiguration"] = map[string]any{"retainSharingOnAccountLeaveOrganization": false}
		}
		shares = append(shares, entry)
	}
	writeRESTJSONBody(writer, mustJSON(map[string]any{"resourceShares": shares}))
}

// estateResourceShareAssociations lists the principals of the shares the
// caller owns, one entry per principal; a receiver sees none.
func (s *Server) estateResourceShareAssociations(writer http.ResponseWriter, call estateCall) {
	associations := []any{}
	for _, share := range call.visibleShares("SELF") {
		for _, principal := range share.principals {
			associations = append(associations, map[string]any{
				"resourceShareArn": share.arn, "resourceShareName": share.name, "associatedEntity": principal,
				"associationType": "PRINCIPAL", "status": "ASSOCIATED", "creationTime": epochSeconds(call.now.Add(-days(900) + 237*time.Millisecond)),
				"lastUpdatedTime": epochSeconds(call.now.Add(-days(900) + 2077*time.Millisecond)), "external": false,
			})
		}
	}
	writeRESTJSONBody(writer, mustJSON(map[string]any{"resourceShareAssociations": associations}))
}

func (s *Server) estateSharedResources(writer http.ResponseWriter, call estateCall) {
	resources := []any{}
	for _, share := range call.visibleShares(jsonString(jsonBody(call.request), "resourceOwner")) {
		for _, arn := range share.resources {
			kind := "ec2:TransitGateway"
			if strings.Contains(arn, "route53resolver") {
				kind = "route53resolver:ResolverRule"
			}
			resources = append(resources, map[string]any{
				"arn": arn, "type": kind, "resourceShareArn": share.arn, "status": "AVAILABLE",
				"creationTime": epochSeconds(call.now.Add(-days(900))), "lastUpdatedTime": epochSeconds(call.now.Add(-days(300))),
				"resourceRegionScope": "REGIONAL",
			})
		}
	}
	writeRESTJSONBody(writer, mustJSON(map[string]any{"resources": resources}))
}

// ---- Route 53 (shared services owns every zone) ----

func estateZoneXML(id, name string, private bool, count int) string {
	// A public zone reports its accelerated-recovery state; a private zone
	// answers an empty Features element.
	privateZone, features := "false", "<Features><AcceleratedRecoveryStatus>DISABLED</AcceleratedRecoveryStatus></Features>"
	if private {
		privateZone, features = "true", "<Features/>"
	}
	return xmlText("Id", "/hostedzone/"+id) + xmlText("Name", name) + xmlText("CallerReference", "terraform-"+strings.ToLower(id[len(id)-6:])) +
		"<Config>" + xmlText("Comment", "Managed by Terraform") + xmlText("PrivateZone", privateZone) + "</Config>" +
		xmlText("ResourceRecordSetCount", itoa(count)) + features
}

func (s *Server) estateHostedZones(writer http.ResponseWriter, call estateCall) {
	zones := ""
	if call.account == EstateShared {
		zones = "<HostedZone>" + estateZoneXML(EstatePublicZoneID, EstatePublicZone, false, len(estatePublicRecords())) + "</HostedZone>" +
			"<HostedZone>" + estateZoneXML(EstatePrivateZoneID, EstatePrivateZone, true, len(estatePrivateRecords())) + "</HostedZone>"
	}
	writeRESTXMLBody(writer, `<ListHostedZonesResponse xmlns="https://route53.amazonaws.com/doc/2013-04-01/"><HostedZones>`+zones+
		`</HostedZones><IsTruncated>false</IsTruncated><MaxItems>100</MaxItems></ListHostedZonesResponse>`)
}

// estatePrivateZoneVPCs are the VPCs associated with the private zone. The
// zone owner sees each VPC ID and Region, never which account owns it.
func estatePrivateZoneVPCs() [][2]string {
	return [][2]string{
		{EstateHomeRegion, estateVPC(EstateShared)},
		{EstateHomeRegion, estateVPC(EstateHub)},
		{EstateHomeRegion, estateVPC(EstateWorkloads)},
		{EstateHomeRegion, estateVPC(EstateData)},
		{EstateHomeRegion, estateVPC(EstateOutside)},
		{EstateAggregationRegion, "vpc-0100000000000a0b2"},
	}
}

func (s *Server) estateHostedZone(writer http.ResponseWriter, call estateCall) {
	id := call.request.path[strings.LastIndex(call.request.path, "/")+1:]
	if call.account != EstateShared || (id != EstatePublicZoneID && id != EstatePrivateZoneID) {
		estateError(writer, call, http.StatusNotFound, "NoSuchHostedZone", "No hosted zone found with ID: "+id)
		return
	}
	body := `<GetHostedZoneResponse xmlns="https://route53.amazonaws.com/doc/2013-04-01/"><HostedZone>`
	if id == EstatePublicZoneID {
		body += estateZoneXML(id, EstatePublicZone, false, len(estatePublicRecords())) + `</HostedZone><DelegationSet><NameServers>` +
			`<NameServer>ns-101.awsdns-12.com</NameServer><NameServer>ns-1302.awsdns-34.org</NameServer></NameServers></DelegationSet>`
	} else {
		body += estateZoneXML(id, EstatePrivateZone, true, len(estatePrivateRecords())) + `</HostedZone><VPCs>`
		for _, vpc := range estatePrivateZoneVPCs() {
			body += `<VPC>` + xmlText("VPCRegion", vpc[0]) + xmlText("VPCId", vpc[1]) + `</VPC>`
		}
		body += `</VPCs>`
	}
	writeRESTXMLBody(writer, body+`</GetHostedZoneResponse>`)
}

// aliasRecord evaluates target health only for the hub's apex; most real
// aliases leave it off.
func aliasRecord(name, kind, zone, target string) string {
	evaluate := "false"
	if name == EstatePublicZone {
		evaluate = "true"
	}
	return `<ResourceRecordSet>` + xmlText("Name", name) + xmlText("Type", kind) + `<AliasTarget>` + xmlText("HostedZoneId", zone) +
		xmlText("DNSName", target) + xmlText("EvaluateTargetHealth", evaluate) + `</AliasTarget></ResourceRecordSet>`
}

func plainRecord(name, kind string, ttl int, values ...string) string {
	out := `<ResourceRecordSet>` + xmlText("Name", name) + xmlText("Type", kind) + xmlText("TTL", itoa(ttl)) + `<ResourceRecords>`
	for _, value := range values {
		out += `<ResourceRecord>` + xmlText("Value", value) + `</ResourceRecord>`
	}
	return out + `</ResourceRecords></ResourceRecordSet>`
}

// estatePublicRecords: names are lowercase with a trailing dot, a wildcard
// label comes back octal-escaped (\052), an alias to an application load
// balancer usually carries the dualstack prefix but not always, an alias
// to a network load balancer does not, the same alias repeats for A and
// AAAA, and a CNAME value has no trailing dot.
func estatePublicRecords() []string {
	zone := EstatePublicZone
	return []string{
		plainRecord(zone, "NS", 172800, "ns-101.awsdns-12.com.", "ns-1302.awsdns-34.org."),
		plainRecord(zone, "SOA", 900, "ns-101.awsdns-12.com. awsdns-hostmaster.amazon.com. 1 7200 900 1209600 86400"),
		aliasRecord(zone, "A", estateELBZone, "dualstack."+EstateHubALBDNS+"."),
		aliasRecord(zone, "AAAA", estateELBZone, "dualstack."+EstateHubALBDNS+"."),
		aliasRecord(`\052.`+zone, "A", estateELBZone, "dualstack."+EstateWorkloadsALBDNS+"."),
		aliasRecord("api."+zone, "A", estateELBZone, EstateHubALBDNS+"."),
		plainRecord("assets."+zone, "CNAME", 300, EstateDistributionDNS),
		aliasRecord("cdn."+zone, "A", EstateCloudFrontZone, EstateDistributionDNS+"."),
		aliasRecord("ingest."+zone, "A", estateNLBZone, EstateDataNLBDNS+"."),
		aliasRecord("legacy."+zone, "A", estateELBZone, EstateDanglingAliasDNS),
		aliasRecord("payments."+zone, "A", estateELBZone, "dualstack."+EstateWorkloadsALBDNS+"."),
		plainRecord("_0a1b2c3d4e5f."+zone, "CNAME", 300, "_6a7b8c9d0e1f.mhbtsbpdnt.acm-validations.aws."),
	}
}

func estatePrivateRecords() []string {
	zone := EstatePrivateZone
	return []string{
		plainRecord(zone, "NS", 172800, "ns-1536.awsdns-00.co.uk.", "ns-0.awsdns-00.com."),
		plainRecord(zone, "SOA", 900, "ns-1536.awsdns-00.co.uk. awsdns-hostmaster.amazon.com. 1 7200 900 1209600 86400"),
		plainRecord("db."+zone, "CNAME", 60, "payments-db.cluster-c0a1b2c3d4e5."+EstateHomeRegion+".rds.amazonaws.com"),
		aliasRecord("payments."+zone, "A", estateELBZone, "dualstack."+EstateWorkloadsALBDNS+"."),
	}
}

func (s *Server) estateRecordSets(writer http.ResponseWriter, call estateCall) {
	parts := strings.Split(strings.Trim(call.request.path, "/"), "/")
	var records []string
	switch {
	case call.account != EstateShared:
	case parts[2] == EstatePublicZoneID:
		records = estatePublicRecords()
	case parts[2] == EstatePrivateZoneID:
		records = estatePrivateRecords()
	}
	if records == nil {
		estateError(writer, call, http.StatusNotFound, "NoSuchHostedZone", "No hosted zone found with ID: "+parts[2])
		return
	}
	writeRESTXMLBody(writer, `<ListResourceRecordSetsResponse xmlns="https://route53.amazonaws.com/doc/2013-04-01/"><ResourceRecordSets>`+
		strings.Join(records, "")+`</ResourceRecordSets><IsTruncated>false</IsTruncated><MaxItems>300</MaxItems></ListResourceRecordSetsResponse>`)
}

// estateDistributions: the hub's distribution is reached through a name in
// the shared-services zone, and its origin is another name in that zone
// that leads to the workloads load balancer.
func (s *Server) estateDistributions(writer http.ResponseWriter, call estateCall) {
	items, count := "", "0"
	if call.account == EstateHub {
		count = "1"
		items = `<Items><DistributionSummary>` + xmlText("Id", "E1ESTATEEXAMPLE") +
			xmlText("ARN", "arn:aws:cloudfront::"+EstateHub+":distribution/E1ESTATEEXAMPLE") + xmlText("Status", "Deployed") +
			xmlText("LastModifiedTime", isoMillis(call.now.Add(-days(40)))) + xmlText("DomainName", EstateDistributionDNS) +
			`<Aliases><Quantity>1</Quantity><Items><CNAME>cdn.shop.example.com</CNAME></Items></Aliases>` +
			`<Origins><Quantity>1</Quantity><Items><Origin>` + xmlText("Id", "payments-origin") + xmlText("DomainName", "payments.shop.example.com") +
			xmlText("OriginPath", "") + `<CustomOriginConfig><HTTPPort>80</HTTPPort><HTTPSPort>443</HTTPSPort>` +
			`<OriginProtocolPolicy>https-only</OriginProtocolPolicy></CustomOriginConfig></Origin></Items></Origins>` +
			`<ViewerCertificate>` + xmlText("ACMCertificateArn", "arn:aws:acm:us-east-1:"+EstateHub+":certificate/"+estateCertificateID(EstateHub, 1)) +
			`<SSLSupportMethod>sni-only</SSLSupportMethod><MinimumProtocolVersion>TLSv1.2_2021</MinimumProtocolVersion></ViewerCertificate>` +
			xmlText("Enabled", "true") + `</DistributionSummary></Items>`
	}
	writeRESTXMLBody(writer, `<DistributionList xmlns="http://cloudfront.amazonaws.com/doc/2020-05-31/"><Marker></Marker><MaxItems>100</MaxItems>`+
		`<IsTruncated>false</IsTruncated><Quantity>`+count+`</Quantity>`+items+`</DistributionList>`)
}

// ---- ELBv2 ----

type estateLoadBalancer struct {
	name, id, dns, scheme, kind string
}

func estateLoadBalancers(account string) []estateLoadBalancer {
	switch account {
	case EstateHub:
		return []estateLoadBalancer{{name: "edge-public", id: "app/edge-public/0a1b2c3d4e5f6a7b", dns: EstateHubALBDNS, scheme: "internet-facing", kind: "application"}}
	case EstateWorkloads:
		return []estateLoadBalancer{{name: "k8s-payments-paymentsa-0a1b2c3d4e", id: "app/k8s-payments-paymentsa-0a1b2c3d4e/1b2c3d4e5f6a7b8c",
			dns: EstateWorkloadsALBDNS, scheme: "internal", kind: "application"}}
	case EstateData:
		return []estateLoadBalancer{{name: "k8s-ingest-ingestnl-5f6a7b8c9d", id: "net/k8s-ingest-ingestnl-5f6a7b8c9d/2c3d4e5f6a7b8c9d",
			dns: EstateDataNLBDNS, scheme: "internal", kind: "network"}}
	}
	return nil
}

func (s *Server) estateLoadBalancers(writer http.ResponseWriter, call estateCall) {
	members := ""
	if call.home() {
		for _, lb := range estateLoadBalancers(call.account) {
			members += "<member>" + xmlText("LoadBalancerArn", call.arn("elasticloadbalancing", "loadbalancer/"+lb.id)) +
				xmlText("DNSName", lb.dns) + xmlText("CanonicalHostedZoneId", estateELBZone) +
				xmlText("CreatedTime", isoMillis(call.now.Add(-days(200)))) + xmlText("LoadBalancerName", lb.name) +
				xmlText("Scheme", lb.scheme) + xmlText("VpcId", estateVPC(call.account)) + "<State><Code>active</Code></State>" +
				xmlText("Type", lb.kind) + xmlText("IpAddressType", "ipv4") + "</member>"
		}
	}
	writeQuery(writer, "DescribeLoadBalancers", "<LoadBalancers>"+members+"</LoadBalancers>")
}

type estateTargetGroup struct {
	name, id, kind, lb string
	port               int
	// health is one TargetHealth state per target: healthy, or an
	// unhealthy reason code.
	health []string
}

// estateTargetGroups: every IP target is a pod address on a node's ENI.
// Workloads has a group with one target failing on response code; data
// has a group with nothing healthy and a group no load balancer uses.
func estateTargetGroups(account string) []estateTargetGroup {
	switch account {
	case EstateHub:
		return []estateTargetGroup{{name: "edge-web", id: "targetgroup/edge-web/0a0a0a0a0a0a0a01", kind: "instance", lb: "app/edge-public/0a1b2c3d4e5f6a7b",
			port: 443, health: []string{"healthy", "healthy"}}}
	case EstateWorkloads:
		return []estateTargetGroup{
			{name: "k8s-payments-paymentsapi-0b0b0b0b0b", id: "targetgroup/k8s-payments-paymentsapi-0b0b0b0b0b/0b0b0b0b0b0b0b02", kind: "ip",
				lb: "app/k8s-payments-paymentsa-0a1b2c3d4e/1b2c3d4e5f6a7b8c", port: 8080,
				health: []string{"healthy", "healthy", "Target.ResponseCodeMismatch"}},
		}
	case EstateData:
		return []estateTargetGroup{
			{name: "k8s-ingest-ingestnl-0c0c0c0c0c", id: "targetgroup/k8s-ingest-ingestnl-0c0c0c0c0c/0c0c0c0c0c0c0c03", kind: "ip",
				lb: "net/k8s-ingest-ingestnl-5f6a7b8c9d/2c3d4e5f6a7b8c9d", port: 9000,
				health: []string{"Target.FailedHealthChecks", "Target.FailedHealthChecks"}},
			{name: "k8s-old-oldsvc-0d0d0d0d0d", id: "targetgroup/k8s-old-oldsvc-0d0d0d0d0d/0d0d0d0d0d0d0d04", kind: "ip", port: 80},
		}
	}
	return nil
}

func (s *Server) estateTargetGroups(writer http.ResponseWriter, call estateCall) {
	members := ""
	if call.home() {
		for _, group := range estateTargetGroups(call.account) {
			lbs := "<LoadBalancerArns/>"
			if group.lb != "" {
				lbs = "<LoadBalancerArns><member>" + xmlEscape(call.arn("elasticloadbalancing", "loadbalancer/"+group.lb)) + "</member></LoadBalancerArns>"
			}
			// A network load balancer's group speaks TCP and has no path,
			// matcher or protocol version.
			protocol, checks := "HTTP", ""
			if strings.HasPrefix(group.lb, "net/") {
				protocol = "TCP"
			} else {
				checks = xmlText("HealthCheckPath", "/healthz") + "<Matcher>" + xmlText("HttpCode", "200-399") + "</Matcher>" +
					xmlText("ProtocolVersion", "HTTP1")
			}
			members += "<member>" + xmlText("TargetGroupArn", call.arn("elasticloadbalancing", group.id)) + xmlText("TargetGroupName", group.name) +
				xmlText("Protocol", protocol) + xmlText("Port", itoa(group.port)) + xmlText("VpcId", estateVPC(call.account)) +
				xmlText("HealthCheckProtocol", protocol) + xmlText("HealthCheckPort", "traffic-port") + xmlText("HealthCheckEnabled", "true") +
				xmlText("HealthCheckIntervalSeconds", "15") + xmlText("HealthCheckTimeoutSeconds", "5") + xmlText("HealthyThresholdCount", "2") +
				xmlText("UnhealthyThresholdCount", "2") + checks + xmlText("TargetType", group.kind) + xmlText("IpAddressType", "ipv4") + lbs + "</member>"
		}
	}
	writeQuery(writer, "DescribeTargetGroups", "<TargetGroups>"+members+"</TargetGroups>")
}

var estateHealthDescriptions = map[string]string{
	"Target.ResponseCodeMismatch": "Health checks failed with these codes: [503]",
	"Target.FailedHealthChecks":   "Health checks failed",
}

// estatePodIP is the secondary address of a node ENI that a pod holds.
func estatePodIP(account string, index int) string {
	return "10." + account[11:] + ".42." + itoa(17+index*9)
}

func (s *Server) estateTargetHealth(writer http.ResponseWriter, call estateCall) {
	arn := call.request.params.Get("TargetGroupArn")
	for _, group := range estateTargetGroups(call.account) {
		if arn != call.arn("elasticloadbalancing", group.id) {
			continue
		}
		members := ""
		for index, state := range group.health {
			id, zone := estatePodIP(call.account, index), call.region+"a"
			if group.kind == "instance" {
				id = "i-0" + strings.Repeat(call.account[11:], 15) + itoa(index)
			}
			health := "<TargetHealth><State>healthy</State></TargetHealth>"
			if state != "healthy" {
				health = "<TargetHealth><State>unhealthy</State>" + xmlText("Reason", state) +
					xmlText("Description", estateHealthDescriptions[state]) + "</TargetHealth>"
			}
			// Every description carries the administrative override block,
			// even when no override is active.
			members += "<member><Target>" + xmlText("Id", id) + xmlText("Port", itoa(group.port)) + xmlText("AvailabilityZone", zone) +
				"</Target>" + xmlText("HealthCheckPort", itoa(group.port)) + health +
				"<AdministrativeOverride><State>no_override</State><Reason>AdministrativeOverride.NoOverride</Reason>" +
				"<Description>No override is currently active on target</Description></AdministrativeOverride></member>"
		}
		writeQuery(writer, "DescribeTargetHealth", "<TargetHealthDescriptions>"+members+"</TargetHealthDescriptions>")
		return
	}
	estateError(writer, call, http.StatusBadRequest, "TargetGroupNotFound", "Target groups '"+arn+"' not found")
}

// ---- EKS ----

func estateClusterName(account string) string {
	return "estate-" + strings.ToLower(estateShort(account))
}

// estateClusters lists the account's one cluster in the home Region; every
// other Region answers an empty list.
func (s *Server) estateClusters(writer http.ResponseWriter, call estateCall) {
	clusters := []string{}
	if call.home() {
		clusters = append(clusters, estateClusterName(call.account))
	}
	writeRESTJSONBody(writer, mustJSON(map[string]any{"clusters": clusters}))
}

func (s *Server) estateCluster(writer http.ResponseWriter, call estateCall) {
	name := strings.Trim(call.request.path, "/")[len("clusters/"):]
	if !call.home() || name != estateClusterName(call.account) {
		estateError(writer, call, http.StatusNotFound, "ResourceNotFoundException", "No cluster found for name: "+name+".")
		return
	}
	issuer := estateIssuer(call.account)
	writeRESTJSONBody(writer, mustJSON(map[string]any{"cluster": map[string]any{
		"name": name, "arn": call.arn("eks", "cluster/"+name), "createdAt": epochSeconds(call.now.Add(-days(400))),
		"version": "1.31", "platformVersion": "eks.19", "status": "ACTIVE",
		"endpoint": "https://" + issuer[strings.LastIndex(issuer, "/")+1:] + ".gr7." + call.region + ".eks.amazonaws.com",
		"roleArn":  "arn:aws:iam::" + call.account + ":role/eks-cluster",
		"resourcesVpcConfig": map[string]any{
			"subnetIds":              []string{"subnet-0" + strings.Repeat(call.account[11:], 16), "subnet-1" + strings.Repeat(call.account[11:], 16)},
			"securityGroupIds":       []string{"sg-1" + strings.Repeat(call.account[11:], 16)},
			"clusterSecurityGroupId": "sg-0" + strings.Repeat(call.account[11:], 16), "vpcId": estateVPC(call.account),
			"endpointPublicAccess": true, "endpointPrivateAccess": true, "publicAccessCidrs": []string{"0.0.0.0/0"},
		},
		"kubernetesNetworkConfig": map[string]any{"serviceIpv4Cidr": "172.20.0.0/16", "ipFamily": "ipv4", "elasticLoadBalancing": map[string]any{"enabled": false}},
		"logging": map[string]any{"clusterLogging": []any{map[string]any{
			"types": []string{"api", "audit", "authenticator", "controllerManager", "scheduler"}, "enabled": false}}},
		"identity": map[string]any{"oidc": map[string]any{"issuer": "https://" + issuer}},
		"certificateAuthority": map[string]any{"data": "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCg==",
			"active": map[string]any{"id": "0c0c0c0c-0000-4000-8000-0000000000ca", "activatedBy": "EKS"}},
		"accessConfig":              map[string]any{"authenticationMode": "API"},
		"upgradePolicy":             map[string]any{"supportType": "STANDARD"},
		"tags":                      map[string]string{"karpenter.sh/discovery": name, "terraform": "true", "cost-centre": ""},
		"deletionProtection":        false,
		"controlPlaneScalingConfig": map[string]any{"tier": "standard"},
		"encryptionConfig": []any{map[string]any{"resources": []string{"secrets"},
			"provider": map[string]any{"keyArn": call.arn("kms", "key/0f0f0f0f-"+call.account[8:12]+"-4000-8000-0000000000ee")}}},
		// A healthy cluster has no health element at all.
	}}))
}

type estateInsightRow struct {
	id, name, category, status, reason string
}

func estateInsightRows(account string) []estateInsightRow {
	rows := []estateInsightRow{
		{id: "0a0a0a0a-0000-4000-8000-0000000000a1", name: "Kubelet version skew", category: "UPGRADE_READINESS", status: "PASSING",
			reason: "Node kubelet versions match the cluster control plane version."},
		{id: "0a0a0a0a-0000-4000-8000-0000000000a2", name: "Cluster health issues", category: "MISCONFIGURATION", status: "PASSING",
			reason: "No cluster health issues detected."},
	}
	switch account {
	case EstateWorkloads:
		rows = append(rows,
			estateInsightRow{id: "0a0a0a0a-0000-4000-8000-0000000000b1", name: "Deprecated APIs removed in Kubernetes v1.32", category: "UPGRADE_READINESS",
				status: "ERROR", reason: "Deprecated API usage detected within last 30 days and your cluster is on Kubernetes v1.31."},
			estateInsightRow{id: "0a0a0a0a-0000-4000-8000-0000000000b2", name: "Amazon EKS add-on version compatibility", category: "UPGRADE_READINESS",
				status: "WARNING", reason: "One or more add-ons are not compatible with the next Kubernetes version."})
	case EstateShared:
		rows = append(rows, estateInsightRow{id: "0a0a0a0a-0000-4000-8000-0000000000c1", name: "EKS add-on version compatibility",
			category: "UPGRADE_READINESS", status: "WARNING", reason: "One or more add-ons are not compatible with the next Kubernetes version."})
	}
	return rows
}

func (c estateCall) insightJSON(row estateInsightRow) map[string]any {
	return map[string]any{
		"id": row.id, "name": row.name, "category": row.category, "kubernetesVersion": "1.32",
		"lastRefreshTime": epochSeconds(c.now.Add(-6 * time.Hour)), "lastTransitionTime": epochSeconds(c.now.Add(-days(9))),
		"description":   "Checks for usage of deprecated Kubernetes APIs that will be removed in the next minor version.",
		"insightStatus": map[string]any{"status": row.status, "reason": row.reason},
	}
}

func (s *Server) estateInsights(writer http.ResponseWriter, call estateCall) {
	insights := []any{}
	for _, row := range estateInsightRows(call.account) {
		insights = append(insights, call.insightJSON(row))
	}
	writeRESTJSONBody(writer, mustJSON(map[string]any{"insights": insights}))
}

func (s *Server) estateInsight(writer http.ResponseWriter, call estateCall) {
	id := call.request.path[strings.LastIndex(call.request.path, "/")+1:]
	for _, row := range estateInsightRows(call.account) {
		if row.id != id {
			continue
		}
		insight := call.insightJSON(row)
		insight["recommendation"] = "Update manifests and API clients to use newer Kubernetes APIs if applicable before upgrading to Kubernetes v1.32."
		insight["additionalInfo"] = map[string]string{"EKS update cluster documentation": "https://docs.aws.amazon.com/eks/latest/userguide/update-cluster.html"}
		// The summary always carries both detail lists; only the one that
		// matches the insight is filled. A passing insight names no
		// resource; a node-level insight names nodes, not API paths.
		resources, deprecations := []any{}, []any{}
		switch {
		case row.status == "PASSING":
		case strings.HasPrefix(row.name, "Deprecated APIs"):
			resources = append(resources, map[string]any{
				"insightStatus":         map[string]any{"status": row.status, "reason": row.reason},
				"kubernetesResourceUri": "/apis/flowcontrol.apiserver.k8s.io/v1beta3/flowschemas",
			})
			deprecations = append(deprecations, map[string]any{
				"usage": "/apis/flowcontrol.apiserver.k8s.io/v1beta3/flowschemas", "replacedWith": "/apis/flowcontrol.apiserver.k8s.io/v1/flowschemas",
				"stopServingVersion": "1.32", "startServingReplacementVersion": "1.29",
				"clientStats": []any{map[string]any{"userAgent": "kube-controller", "numberOfRequestsLast30Days": 1288,
					"lastRequestTime": epochSeconds(call.now.Add(-2 * time.Hour))}},
			})
		default:
			resources = append(resources, map[string]any{
				"insightStatus":         map[string]any{"status": row.status, "reason": row.reason},
				"kubernetesResourceUri": "ip-10-" + call.account[11:] + "-14-197." + call.region + ".compute.internal",
			})
		}
		insight["resources"] = resources
		insight["categorySpecificSummary"] = map[string]any{"deprecationDetails": deprecations, "addonCompatibilityDetails": []any{}}
		writeRESTJSONBody(writer, mustJSON(map[string]any{"insight": insight}))
		return
	}
	estateError(writer, call, http.StatusNotFound, "ResourceNotFoundException", "The specified insight could not be found.")
}

// ---- Regions ----

// estateRegions: the home Region is opt-in and enabled; most opt-in Regions
// are off. Without AllRegions, EC2 lists only the enabled Regions; with it,
// the disabled ones come back as not-opted-in.
func (s *Server) estateRegions(writer http.ResponseWriter, call estateCall) {
	type region struct{ name, status string }
	regions := []region{
		{"us-east-1", "opt-in-not-required"}, {"us-west-2", "opt-in-not-required"}, {EstateAggregationRegion, "opt-in-not-required"},
		{"ap-southeast-1", "opt-in-not-required"}, {EstateHomeRegion, "opted-in"},
		{"ap-east-1", "not-opted-in"}, {"me-central-1", "not-opted-in"}, {"af-south-1", "not-opted-in"},
	}
	all := strings.EqualFold(call.request.params.Get("AllRegions"), "true")
	items := ""
	for _, entry := range regions {
		if entry.status == "not-opted-in" && !all {
			continue
		}
		items += "<item>" + xmlText("regionName", entry.name) + xmlText("regionEndpoint", "ec2."+entry.name+".amazonaws.com") +
			xmlText("optInStatus", entry.status) + "</item>"
	}
	writeEC2(writer, "DescribeRegions", "<regionInfo>"+items+"</regionInfo>", "")
}
