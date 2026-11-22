package fakeaws

import "net/http"

// The world answers the link reads of a single standalone account: no
// transit gateway, no shares, no hosted zone, no key or repository, no
// bucket policy, and no organization. Each is the empty answer AWS gives,
// so a world sync stays clean while the link units run.

func init() {
	for operation, handler := range map[string]worldHandler{
		"ec2:DescribeTransitGateways":           (*Server).worldTransitGateways,
		"ec2:DescribeTransitGatewayAttachments": (*Server).worldTransitGatewayAttachments,
		"ram:ListResources":                     (*Server).worldSharedResources,
		"ram:GetResourceShareAssociations":      (*Server).worldShareAssociations,
		"route53:ListHostedZones":               (*Server).worldHostedZones,
		"organizations:DescribeOrganization":    (*Server).worldOrganization,
		"kms:ListKeys":                          (*Server).worldKeys,
		"ecr:DescribeRepositories":              (*Server).worldRepositories,
		"s3:GetBucketPolicy":                    (*Server).worldBucketPolicy,
	} {
		worldHandlers[operation] = handler
	}
}

func (s *Server) worldTransitGateways(writer http.ResponseWriter, _ awsRequest, _ world) {
	writeEC2(writer, "DescribeTransitGateways", "<transitGatewaySet/>", "")
}

func (s *Server) worldTransitGatewayAttachments(writer http.ResponseWriter, _ awsRequest, _ world) {
	writeEC2(writer, "DescribeTransitGatewayAttachments", "<transitGatewayAttachments/>", "")
}

func (s *Server) worldSharedResources(writer http.ResponseWriter, _ awsRequest, _ world) {
	writeRESTJSONBody(writer, `{"resources":[]}`)
}

func (s *Server) worldShareAssociations(writer http.ResponseWriter, _ awsRequest, _ world) {
	writeRESTJSONBody(writer, `{"resourceShareAssociations":[]}`)
}

func (s *Server) worldHostedZones(writer http.ResponseWriter, _ awsRequest, _ world) {
	writeRESTXMLBody(writer, `<ListHostedZonesResponse xmlns="https://route53.amazonaws.com/doc/2013-04-01/"><HostedZones/>`+
		`<IsTruncated>false</IsTruncated><MaxItems>100</MaxItems></ListHostedZonesResponse>`)
}

// worldOrganization: a standalone account belongs to no organization.
func (s *Server) worldOrganization(writer http.ResponseWriter, request awsRequest, _ world) {
	writeErrorFor(writer, request, http.StatusBadRequest, "AWSOrganizationsNotInUseException",
		"Your account is not a member of an organization.")
}

func (s *Server) worldKeys(writer http.ResponseWriter, _ awsRequest, _ world) {
	writeJSONBody(writer, `{"Keys":[],"Truncated":false}`)
}

func (s *Server) worldRepositories(writer http.ResponseWriter, _ awsRequest, _ world) {
	writeJSONBody(writer, `{"repositories":[]}`)
}

func (s *Server) worldBucketPolicy(writer http.ResponseWriter, request awsRequest, _ world) {
	writeWorldError(writer, request, http.StatusNotFound, "NoSuchBucketPolicy")
}
