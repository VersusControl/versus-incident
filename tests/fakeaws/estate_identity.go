package fakeaws

import (
	"encoding/base64"
	"net/http"
	"strings"
	"time"
)

// Identity and resource policies of the estate: role trust policies in
// every form the landing zone uses, the credential report, the
// organization, and the S3, KMS and ECR policies that name other accounts.

func init() {
	registerEstate(map[string]estateHandler{
		"iam:GetAccountAuthorizationDetails":        (*Server).estateAuthorizationDetails,
		"iam:GetRole":                               (*Server).estateGetRole,
		"iam:GetCredentialReport":                   (*Server).estateCredentialReport,
		"organizations:DescribeOrganization":        (*Server).estateDescribeOrganization,
		"organizations:ListAccounts":                (*Server).estateOrganizationsDenied,
		"organizations:ListRoots":                   (*Server).estateOrganizationsDenied,
		"organizations:ListPolicies":                (*Server).estateOrganizationsDenied,
		"organizations:ListDelegatedAdministrators": (*Server).estateOrganizationsDenied,
		"s3:ListBuckets":                            (*Server).estateListBuckets,
		"s3:GetBucketPolicy":                        (*Server).estateBucketPolicy,
		"kms:ListKeys":                              (*Server).estateListKeys,
		"kms:DescribeKey":                           (*Server).estateDescribeKey,
		"kms:GetKeyPolicy":                          (*Server).estateKeyPolicy,
		"ecr:DescribeRepositories":                  (*Server).estateRepositories,
		"ecr:GetRepositoryPolicy":                   (*Server).estateRepositoryPolicy,
	})
}

// Trust policy documents, written as IAM stores them: an account named in
// a policy is saved as its root ARN, a deleted role as its unique ID.
// Statement, Action and principal lists appear both as a single value and
// as a list, because IAM keeps the form the author used.
func trustAccountRoot(account string) string {
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::` + account +
		`:root"},"Action":"sts:AssumeRole"}]}`
}

func trustExternalID(account, externalID string) string {
	return `{"Version":"2012-10-17","Statement":[{"Sid":"VendorAccess","Effect":"Allow","Principal":{"AWS":"arn:aws:iam::` + account +
		`:root"},"Action":"sts:AssumeRole","Condition":{"StringEquals":{"sts:ExternalId":"` + externalID + `"}}}]}`
}

func trustOrganization() string {
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["arn:aws:iam::` + EstateWorkloads +
		`:root","arn:aws:iam::` + EstateShared + `:root"]},"Action":["sts:AssumeRole","sts:TagSession"],"Condition":{` +
		`"StringEquals":{"aws:PrincipalOrgID":"` + EstateOrgID + `"},` +
		`"ForAnyValue:StringLike":{"aws:PrincipalOrgPaths":["` + EstateOrgID + `/r-ab12/ou-ab12-workloads/*"]}}}]}`
}

func trustGitHubPinned(account string) string {
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Federated":"arn:aws:iam::` + account +
		`:oidc-provider/token.actions.githubusercontent.com"},"Action":"sts:AssumeRoleWithWebIdentity","Condition":{` +
		`"StringEquals":{"token.actions.githubusercontent.com:aud":"sts.amazonaws.com"},` +
		`"StringLike":{"token.actions.githubusercontent.com:sub":["repo:example-org/payments-api:ref:refs/heads/main",` +
		`"repo:example-org/payments-api:environment:production"]}}}]}`
}

func trustGitHubUnpinned(account string) string {
	return `{"Version":"2012-10-17","Statement":{"Effect":"Allow","Principal":{"Federated":"arn:aws:iam::` + account +
		`:oidc-provider/token.actions.githubusercontent.com"},"Action":"sts:AssumeRoleWithWebIdentity","Condition":{` +
		`"StringLike":{"token.actions.githubusercontent.com:sub":"repo:example-org/*"},` +
		`"ForAllValues:StringEquals":{"token.actions.githubusercontent.com:aud":"sts.amazonaws.com"}}}}`
}

func trustServiceAccount(account, issuer, namespace, serviceAccount string) string {
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Federated":"arn:aws:iam::` + account +
		`:oidc-provider/` + issuer + `"},"Action":"sts:AssumeRoleWithWebIdentity","Condition":{"StringEquals":{"` +
		issuer + `:sub":"system:serviceaccount:` + namespace + `:` + serviceAccount + `","` + issuer + `:aud":"sts.amazonaws.com"}}}]}`
}

// trustDeletedPrincipal: a deleted role inside a principal list, and a
// statement whose only principal is a deleted user, repeated with an empty
// Sid the way the console leaves it.
func trustDeletedPrincipal() string {
	deletedUser := `{"Sid":"","Effect":"Allow","Principal":{"AWS":"` + EstateDeletedUserID + `"},"Action":"sts:AssumeRole"}`
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["` + EstateDeletedRoleID +
		`","arn:aws:iam::` + EstateShared + `:role/ci-runner"]},"Action":"sts:AssumeRole"},` + deletedUser + `,` + deletedUser + `]}`
}

// trustServiceAccounts binds one role to several service accounts: the sub
// condition is a list and no aud condition is set.
func trustServiceAccounts(account, issuer, namespace string, serviceAccounts ...string) string {
	subjects := make([]string, 0, len(serviceAccounts))
	for _, name := range serviceAccounts {
		subjects = append(subjects, `"system:serviceaccount:`+namespace+`:`+name+`"`)
	}
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Federated":"arn:aws:iam::` + account +
		`:oidc-provider/` + issuer + `"},"Action":"sts:AssumeRoleWithWebIdentity","Condition":{"StringEquals":{"` +
		issuer + `:sub":[` + strings.Join(subjects, ",") + `]}}}]}`
}

// trustServiceConfused is a service trust guarded against the confused
// deputy: the source account and the source ARN, under two operators.
func trustServiceConfused(account, service, sourceARN string) string {
	return `{"Version":"2012-10-17","Statement":[{"Sid":"TrustScheduler","Effect":"Allow","Principal":{"Service":"` + service +
		`"},"Action":"sts:AssumeRole","Condition":{"StringEquals":{"aws:SourceAccount":"` + account +
		`"},"ArnEquals":{"aws:SourceArn":"` + sourceARN + `"}}}]}`
}

// trustGitHubWithCaller pins GitHub to one branch with a single sub string
// and adds a second statement for CI roles in two other accounts.
func trustGitHubWithCaller(account string) string {
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Federated":"arn:aws:iam::` + account +
		`:oidc-provider/token.actions.githubusercontent.com"},"Action":"sts:AssumeRoleWithWebIdentity","Condition":{` +
		`"StringEquals":{"token.actions.githubusercontent.com:aud":"sts.amazonaws.com"},` +
		`"StringLike":{"token.actions.githubusercontent.com:sub":"repo:example-org/etl-jobs:ref:refs/heads/main"}}},` +
		`{"Effect":"Allow","Principal":{"AWS":["arn:aws:iam::` + EstateShared + `:role/ci-runner","arn:aws:iam::` + EstateOutside +
		`:role/github-actions-runner-role"]},"Action":"sts:AssumeRole"}]}`
}

func trustSAML(account string) string {
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Federated":"arn:aws:iam::` + account +
		`:saml-provider/AWSSSO_0a1b2c3d4e5f6a7b_DO_NOT_DELETE"},"Action":["sts:AssumeRoleWithSAML","sts:TagSession"],` +
		`"Condition":{"StringEquals":{"SAML:aud":"https://signin.aws.amazon.com/saml"}}}]}`
}

func trustService(services ...string) string {
	if len(services) == 1 {
		return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"` + services[0] + `"},"Action":"sts:AssumeRole"}]}`
	}
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":["` + strings.Join(services, `","`) + `"]},"Action":"sts:AssumeRole"}]}`
}

// estateIssuer is the OIDC issuer host path of an account's EKS cluster.
func estateIssuer(account string) string {
	return "oidc.eks." + EstateHomeRegion + ".amazonaws.com/id/" + strings.Repeat(account[len(account)-1:], 8) + "0A1B2C3D4E5F6A7B8C9D0E1F"
}

type estateRole struct {
	path, name, id string
	created        time.Duration // before now
	trust          string
	lastUsed       time.Duration // before now; zero means never used
	attached       []string
	tags           [][2]string
}

func estateRoles(account string) []estateRole {
	roles := []estateRole{
		{path: "/", name: "lz-admin", created: days(900), trust: trustAccountRoot(EstateManagement), attached: []string{"AdministratorAccess"}},
		{path: "/", name: "lz-security-audit", created: days(900), trust: trustAccountRoot(EstateSecurityTooling),
			lastUsed: days(130), attached: []string{"SecurityAudit", "ReadOnlyAccess"}},
		{path: "/aws-reserved/sso.amazonaws.com/" + EstateAggregationRegion + "/", name: "AWSReservedSSO_ReadOnly_0a1b2c3d4e5f6a7b",
			created: days(700), trust: trustSAML(account), lastUsed: days(1), attached: []string{"ReadOnlyAccess"}},
		{path: "/", name: "eks-node", created: days(400), trust: trustService("ec2.amazonaws.com"), lastUsed: time.Hour,
			attached: []string{"AmazonEKSWorkerNodePolicy", "AmazonEC2ContainerRegistryReadOnly"},
			tags:     [][2]string{{"karpenter.sh/discovery", "estate-" + estateShort(account)}, {"cost-centre", ""}}},
	}
	switch account {
	case EstateHub:
		roles = append(roles, estateRole{path: "/", name: "network-deployer", created: days(500),
			trust:    `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::` + EstateShared + `:role/ci-runner"},"Action":"sts:AssumeRole"}]}`,
			lastUsed: days(3), attached: []string{"NetworkAdministrator"}},
			estateRole{path: "/service-role/", name: "scheduler-tag-audit", created: days(120),
				trust: trustServiceConfused(account, "scheduler.amazonaws.com", "arn:aws:scheduler:"+EstateHomeRegion+":"+account+":schedule/default/tag-audit")})
	case EstateWorkloads:
		roles = append(roles,
			estateRole{path: "/", name: "payments-api-irsa", created: days(200), trust: trustServiceAccount(account, estateIssuer(account), "payments", "payments-api"),
				lastUsed: time.Hour, tags: [][2]string{{"app", "payments"}}},
			estateRole{path: "/", name: "orders-worker-irsa", created: days(200), trust: trustServiceAccounts(account, estateIssuer(account), "orders", "orders-worker", "orders-migrate")},
			estateRole{path: "/", name: "gha-payments-deploy", created: days(300), trust: trustGitHubPinned(account), lastUsed: days(2)},
			estateRole{path: "/", name: "legacy-cross-deploy", created: days(800), trust: trustDeletedPrincipal(), lastUsed: days(240)},
			estateRole{path: "/", name: "lambda-exec", created: days(600), trust: trustService("lambda.amazonaws.com", "edgelambda.amazonaws.com"), lastUsed: days(1)})
	case EstateShared:
		roles = append(roles,
			estateRole{path: "/", name: "ci-runner", created: days(500), trust: trustGitHubUnpinned(account), lastUsed: time.Hour,
				attached: []string{"AdministratorAccess"}},
			estateRole{path: "/", name: "image-promoter", created: days(300), trust: trustAccountRoot(EstateWorkloads), lastUsed: days(6)})
	case EstateData:
		roles = append(roles,
			estateRole{path: "/", name: "vendor-monitoring", created: days(365), trust: trustExternalID(EstateVendor, EstateExternalIDValue), lastUsed: time.Hour,
				attached: []string{"ReadOnlyAccess"}},
			estateRole{path: "/", name: "etl-reader", created: days(250), trust: trustOrganization(), lastUsed: days(10)},
			estateRole{path: "/", name: "gha-etl-deploy", created: days(140), trust: trustGitHubWithCaller(account), lastUsed: days(4)},
			estateRole{path: "/service-role/", name: "batch-export", created: days(90), trust: trustAccountRoot(EstateOutside)})
	}
	for index := range roles {
		roles[index].id = "AROA" + estateShort(account) + strings.Repeat("X", 14-len(itoa(index))) + itoa(index)
	}
	return roles
}

func estateShort(account string) string {
	switch account {
	case EstateHub:
		return "HUB"
	case EstateWorkloads:
		return "WKL"
	case EstateShared:
		return "SHR"
	case EstateData:
		return "DAT"
	}
	return "OUT"
}

func (c estateCall) roleXML(role estateRole) string {
	arn := "arn:aws:iam::" + c.account + ":role" + role.path + role.name
	out := xmlText("Path", role.path) + xmlText("RoleName", role.name) + xmlText("RoleId", role.id) + xmlText("Arn", arn) +
		xmlText("CreateDate", iso(c.now.Add(-role.created))) + xmlText("AssumeRolePolicyDocument", policyEncode(role.trust))
	out += "<InstanceProfileList/><RolePolicyList/><AttachedManagedPolicies>"
	for _, policy := range role.attached {
		out += "<member>" + xmlText("PolicyName", policy) + xmlText("PolicyArn", "arn:aws:iam::aws:policy/"+policy) + "</member>"
	}
	out += "</AttachedManagedPolicies>"
	if len(role.tags) > 0 {
		out += "<Tags>"
		for _, tag := range role.tags {
			out += "<member>" + xmlText("Key", tag[0]) + xmlText("Value", tag[1]) + "</member>"
		}
		out += "</Tags>"
	} else {
		out += "<Tags/>"
	}
	if role.lastUsed == 0 {
		// IAM answers a role never used with an empty element, not a
		// missing one.
		out += "<RoleLastUsed/>"
	} else {
		out += "<RoleLastUsed>" + xmlText("LastUsedDate", iso(c.now.Add(-role.lastUsed))) + xmlText("Region", EstateHomeRegion) + "</RoleLastUsed>"
	}
	return out
}

func (s *Server) estateAuthorizationDetails(writer http.ResponseWriter, call estateCall) {
	inner := "<RoleDetailList>"
	for _, role := range estateRoles(call.account) {
		inner += "<member>" + call.roleXML(role) + "</member>"
	}
	inner += "</RoleDetailList><UserDetailList/><GroupDetailList/><Policies/><IsTruncated>false</IsTruncated>"
	writeXMLBody(writer, queryResult("GetAccountAuthorizationDetails", inner))
}

func (s *Server) estateGetRole(writer http.ResponseWriter, call estateCall) {
	name := call.request.params.Get("RoleName")
	for _, role := range estateRoles(call.account) {
		if role.name == name {
			writeXMLBody(writer, queryResult("GetRole", "<Role>"+call.roleXML(role)+xmlText("MaxSessionDuration", "3600")+"</Role>"))
			return
		}
	}
	estateError(writer, call, http.StatusNotFound, "NoSuchEntity", "The role with name "+name+" cannot be found.")
}

// credentialReportColumns is the header row of the IAM credential report.
var credentialReportColumns = []string{
	"user", "arn", "user_creation_time", "password_enabled", "password_last_used", "password_last_changed",
	"password_next_rotation", "mfa_active", "access_key_1_active", "access_key_1_last_rotated",
	"access_key_1_last_used_date", "access_key_1_last_used_region", "access_key_1_last_used_service",
	"access_key_2_active", "access_key_2_last_rotated", "access_key_2_last_used_date",
	"access_key_2_last_used_region", "access_key_2_last_used_service", "cert_1_active", "cert_1_last_rotated",
	"cert_2_active", "cert_2_last_rotated",
}

// CredentialReportColumns returns the report's header columns in order.
func CredentialReportColumns() []string { return append([]string(nil), credentialReportColumns...) }

// EstateCredentialReport is the CSV the workloads account's report decodes
// to, for the given clock.
func EstateCredentialReport(account string, now time.Time) string {
	now = now.UTC().Truncate(time.Hour)
	// Cells are RFC 3339 with a Z; only GeneratedTime uses +00:00.
	ts := func(ago time.Duration) string { return now.Add(-ago).Format("2006-01-02T15:04:05Z") }
	arn := func(user string) string { return "arn:aws:iam::" + account + ":user/" + user }
	noKey := []string{"false", "N/A", "N/A", "N/A", "N/A"}
	noCerts := []string{"false", "N/A", "false", "N/A"}
	row := func(parts ...[]string) string {
		var cells []string
		for _, part := range parts {
			cells = append(cells, part...)
		}
		return strings.Join(cells, ",")
	}
	lines := []string{strings.Join(credentialReportColumns, ",")}
	// Root: a password that is enabled, used and changed, and a rotation
	// column that does not apply.
	lines = append(lines, row([]string{"<root_account>", "arn:aws:iam::" + account + ":root", ts(days(1500)), "true", ts(days(518)),
		ts(days(1499)), "not_supported", "true"}, noKey, noKey, noCerts))
	if account == EstateWorkloads {
		// A machine user with no password and a key two years old that was
		// used yesterday.
		lines = append(lines, row([]string{"svc-legacy-sync", arn("svc-legacy-sync"), ts(days(800)), "false", "N/A", "N/A", "N/A", "false"},
			[]string{"true", ts(days(760)), ts(days(1)), EstateHomeRegion, "s3"}, noKey, noCerts))
		// Two keys: the old one used today, the new one never used.
		lines = append(lines, row([]string{"svc-reporting", arn("svc-reporting"), ts(days(500)), "false", "N/A", "N/A", "N/A", "false"},
			[]string{"true", ts(days(430)), ts(3 * time.Hour), EstateHomeRegion, "secretsmanager"},
			[]string{"true", ts(days(30)), "N/A", "N/A", "N/A"}, noCerts))
		// Both keys deactivated, yet still carrying their last use.
		lines = append(lines, row([]string{"svc-s3-export", arn("svc-s3-export"), ts(days(380)), "false", "N/A", "N/A", "N/A", "false"},
			[]string{"false", ts(days(345)), ts(days(110)), "ap-southeast-1", "s3"},
			[]string{"false", ts(days(360)), ts(days(110)), "ap-southeast-1", "sts"}, noCerts))
		// A console password never used since IAM started tracking.
		lines = append(lines, row([]string{"break-glass", arn("break-glass"), ts(days(1200)), "true", "no_information", ts(days(1100)), "N/A", "false"},
			noKey, noKey, noCerts))
	}
	// A user with no credential at all.
	lines = append(lines, row([]string{"deploy-bot", arn("deploy-bot"), ts(days(300)), "false", "N/A", "N/A", "N/A", "false"}, noKey, noKey, noCerts))
	return strings.Join(lines, "\n")
}

func (s *Server) estateCredentialReport(writer http.ResponseWriter, call estateCall) {
	if call.account == EstateHub {
		// No report has ever been generated in the hub, and the reader
		// never generates one.
		// IAM answers with the code alone: no Message element.
		writeBody(writer, "text/xml", http.StatusGone, `<ErrorResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/">`+
			`<Error><Type>Sender</Type><Code>ReportNotPresent</Code></Error>`+xmlText("RequestId", requestID)+`</ErrorResponse>`)
		return
	}
	content := base64.StdEncoding.EncodeToString([]byte(EstateCredentialReport(call.account, call.now)))
	writeXMLBody(writer, queryResult("GetCredentialReport", xmlText("Content", content)+xmlText("ReportFormat", "text/csv")+
		xmlText("GeneratedTime", iso(call.now.Add(-2*time.Hour)))))
}

func (s *Server) estateDescribeOrganization(writer http.ResponseWriter, call estateCall) {
	writeJSONBody(writer, mustJSON(map[string]any{"Organization": map[string]any{
		"Id":                   EstateOrgID,
		"Arn":                  "arn:aws:organizations::" + EstateManagement + ":organization/" + EstateOrgID,
		"FeatureSet":           "ALL",
		"MasterAccountArn":     "arn:aws:organizations::" + EstateManagement + ":account/" + EstateOrgID + "/" + EstateManagement,
		"MasterAccountId":      EstateManagement,
		"MasterAccountEmail":   "aws-management@example.com",
		"AvailablePolicyTypes": []any{map[string]any{"Type": "SERVICE_CONTROL_POLICY", "Status": "ENABLED"}},
	}}))
}

// estateOrganizationsDenied: a member account can describe its
// organization but never list it.
func (s *Server) estateOrganizationsDenied(writer http.ResponseWriter, call estateCall) {
	estateError(writer, call, http.StatusBadRequest, "AccessDeniedException", "You don't have permissions to access this resource.")
}

// estateBuckets are the buckets of each account; a bucket's policy is set
// only where it names another account.
func estateBuckets(account string) []string {
	switch account {
	case EstateWorkloads:
		return []string{"wkl-exports-0a1b2c", "wkl-assets-3d4e5f", "wkl-logs-6a7b8c"}
	case EstateShared:
		return []string{"shr-ci-cache-9d0e1f"}
	case EstateData:
		return []string{"dat-landing-2a3b4c", "dat-archive-5d6e7f", "dat-tmp-8a9b0c"}
	}
	return nil
}

func (s *Server) estateListBuckets(writer http.ResponseWriter, call estateCall) {
	body := `<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Owner><ID>` +
		strings.Repeat(estateShort(call.account), 16)[:64] + `</ID></Owner><Buckets>`
	for _, bucket := range estateBuckets(call.account) {
		body += `<Bucket>` + xmlText("Name", bucket) + xmlText("CreationDate", isoMillis(call.now.Add(-days(400)))) +
			xmlText("BucketRegion", EstateHomeRegion) + `</Bucket>`
	}
	writeRESTXMLBody(writer, body+`</Buckets></ListAllMyBucketsResult>`)
}

// estateBucketPolicies are the bucket policies that exist. S3 returns the
// document as the raw response body, not wrapped in XML. The assets bucket
// names the hub only through a CloudFront source ARN, under a condition
// key written in upper case.
func estateBucketPolicies() map[string]string {
	return map[string]string{
		"wkl-exports-0a1b2c": `{"Version":"2012-10-17","Statement":[{"Sid":"DataAccountRead","Effect":"Allow","Principal":{"AWS":["arn:aws:iam::` +
			EstateData + `:role/etl-reader","arn:aws:iam::` + EstateData + `:root"]},"Action":["s3:GetObject","s3:ListBucket"],` +
			`"Resource":["arn:aws:s3:::wkl-exports-0a1b2c","arn:aws:s3:::wkl-exports-0a1b2c/*"]},` +
			`{"Sid":"DenyInsecureTransport","Effect":"Deny","Principal":"*","Action":"s3:*","Resource":["arn:aws:s3:::wkl-exports-0a1b2c",` +
			`"arn:aws:s3:::wkl-exports-0a1b2c/*"],"Condition":{"Bool":{"aws:SecureTransport":"false"}}}]}`,
		"wkl-assets-3d4e5f": `{"Version":"2012-10-17","Statement":[{"Sid":"AllowCloudFrontServicePrincipal","Effect":"Allow",` +
			`"Principal":{"Service":"cloudfront.amazonaws.com"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::wkl-assets-3d4e5f/*",` +
			`"Condition":{"StringEquals":{"AWS:SourceArn":"arn:aws:cloudfront::` + EstateHub + `:distribution/E1ESTATEEXAMPLE"}}}]}`,
		"dat-landing-2a3b4c": `{"Version":"2012-10-17","Id":"landing","Statement":[{"Sid":"WorkloadsWrite","Effect":"Allow","Principal":{"AWS":"arn:aws:iam::` +
			EstateWorkloads + `:root"},"Action":["s3:PutObject","s3:PutObjectAcl"],"Resource":"arn:aws:s3:::dat-landing-2a3b4c/*",` +
			`"Condition":{"StringEquals":{"s3:x-amz-acl":"bucket-owner-full-control"}}}]}`,
	}
}

func (s *Server) estateBucketPolicy(writer http.ResponseWriter, call estateCall) {
	bucket := bucketOf(call.request)
	if policy, ok := estateBucketPolicies()[bucket]; ok {
		writeBody(writer, "application/json", http.StatusOK, policy)
		return
	}
	writeBody(writer, "application/xml", http.StatusNotFound, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+`<Error><Code>NoSuchBucketPolicy</Code>`+
		`<Message>The bucket policy does not exist</Message>`+xmlText("BucketName", bucket)+xmlText("RequestId", "0A1B2C3D4E5F6A7B")+
		xmlText("HostId", "estate-host-id")+`</Error>`)
}

const (
	estateDataKeyID    = "0d0d0d0d-1111-4222-8333-444444444444"
	estateManagedKeyID = "0e0e0e0e-5555-4666-8777-888888888888"
)

func (s *Server) estateListKeys(writer http.ResponseWriter, call estateCall) {
	keys := []any{}
	if call.account == EstateData && call.home() {
		for _, id := range []string{estateDataKeyID, estateManagedKeyID} {
			keys = append(keys, map[string]any{"KeyId": id, "KeyArn": call.arn("kms", "key/"+id)})
		}
	}
	writeJSONBody(writer, mustJSON(map[string]any{"Keys": keys, "Truncated": false}))
}

func (s *Server) estateDescribeKey(writer http.ResponseWriter, call estateCall) {
	id := jsonString(jsonBody(call.request), "KeyId")
	if call.account != EstateData || (id != estateDataKeyID && id != estateManagedKeyID) {
		estateError(writer, call, http.StatusBadRequest, "NotFoundException", "Key '"+call.arn("kms", "key/"+id)+"' does not exist")
		return
	}
	manager, description := "CUSTOMER", "landing bucket encryption"
	if id == estateManagedKeyID {
		manager, description = "AWS", "Default key that protects my S3 objects when no other key is defined"
	}
	writeJSONBody(writer, mustJSON(map[string]any{"KeyMetadata": map[string]any{
		"AWSAccountId": call.account, "KeyId": id, "Arn": call.arn("kms", "key/"+id), "CreationDate": epochSeconds(call.now.Add(-days(600))),
		"Enabled": true, "Description": description, "KeyUsage": "ENCRYPT_DECRYPT", "KeyState": "Enabled", "Origin": "AWS_KMS",
		"KeyManager": manager, "CustomerMasterKeySpec": "SYMMETRIC_DEFAULT", "KeySpec": "SYMMETRIC_DEFAULT",
		"EncryptionAlgorithms": []string{"SYMMETRIC_DEFAULT"}, "MultiRegion": false,
		"CurrentKeyMaterialId": strings.Repeat(id[:8], 8),
	}}))
}

// estateKeyPolicy is the data account's key policy: the account root, then
// a grant to the workloads account limited to calls through S3.
func (s *Server) estateKeyPolicy(writer http.ResponseWriter, call estateCall) {
	id := jsonString(jsonBody(call.request), "KeyId")
	if call.account != EstateData || id != estateDataKeyID {
		estateError(writer, call, http.StatusBadRequest, "NotFoundException", "Key '"+call.arn("kms", "key/"+id)+"' does not exist")
		return
	}
	policy := `{
  "Version" : "2012-10-17",
  "Id" : "key-consolepolicy-3",
  "Statement" : [ {
    "Sid" : "Enable IAM User Permissions",
    "Effect" : "Allow",
    "Principal" : {
      "AWS" : "arn:aws:iam::` + EstateData + `:root"
    },
    "Action" : "kms:*",
    "Resource" : "*"
  }, {
    "Sid" : "Allow use of the key by the workloads account",
    "Effect" : "Allow",
    "Principal" : {
      "AWS" : "arn:aws:iam::` + EstateWorkloads + `:root"
    },
    "Action" : [ "kms:Encrypt", "kms:Decrypt", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:DescribeKey" ],
    "Resource" : "*",
    "Condition" : {
      "StringEquals" : {
        "kms:ViaService" : "s3.` + EstateHomeRegion + `.amazonaws.com",
        "kms:CallerAccount" : "` + EstateWorkloads + `"
      }
    }
  }, {
    "Sid" : "Allow access through EBS for all principals in the account that are authorized to use EBS",
    "Effect" : "Allow",
    "Principal" : {
      "AWS" : "*"
    },
    "Action" : [ "kms:Encrypt", "kms:Decrypt", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:CreateGrant", "kms:DescribeKey" ],
    "Resource" : "*",
    "Condition" : {
      "StringEquals" : {
        "kms:ViaService" : "ec2.` + EstateHomeRegion + `.amazonaws.com",
        "kms:CallerAccount" : "` + EstateData + `"
      }
    }
  } ]
}`
	writeJSONBody(writer, mustJSON(map[string]any{"Policy": policy, "PolicyName": "default"}))
}

func estateRepositories(account string) []string {
	if account == EstateShared {
		return []string{"platform/payments-api", "platform/base-images", "tools/scratch"}
	}
	return nil
}

func (s *Server) estateRepositories(writer http.ResponseWriter, call estateCall) {
	repositories := []any{}
	if call.home() {
		for index, name := range estateRepositories(call.account) {
			mutability := "IMMUTABLE"
			if index == 2 {
				mutability = "MUTABLE"
			}
			repositories = append(repositories, map[string]any{
				"repositoryArn": call.arn("ecr", "repository/"+name), "registryId": call.account, "repositoryName": name,
				"repositoryUri": call.account + ".dkr.ecr." + call.region + ".amazonaws.com/" + name,
				"createdAt":     epochSeconds(call.now.Add(-days(500 - 100*index))), "imageTagMutability": mutability,
				"imageScanningConfiguration": map[string]any{"scanOnPush": index != 2},
				"encryptionConfiguration":    map[string]any{"encryptionType": "AES256"},
			})
		}
	}
	writeJSONBody(writer, mustJSON(map[string]any{"repositories": repositories}))
}

// estateRepositoryPolicy keeps ECR's policyText exactly as submitted:
// indented, with newlines.
func (s *Server) estateRepositoryPolicy(writer http.ResponseWriter, call estateCall) {
	name := jsonString(jsonBody(call.request), "repositoryName")
	var policy string
	switch name {
	case "platform/payments-api":
		policy = "{\n  \"Version\" : \"2012-10-17\",\n  \"Statement\" : [ {\n    \"Sid\" : \"OrgPull\",\n    \"Effect\" : \"Allow\",\n" +
			"    \"Principal\" : \"*\",\n    \"Action\" : [ \"ecr:BatchCheckLayerAvailability\", \"ecr:BatchGetImage\", \"ecr:GetDownloadUrlForLayer\" ],\n" +
			"    \"Condition\" : {\n      \"StringEquals\" : {\n        \"aws:PrincipalOrgID\" : \"" + EstateOrgID + "\"\n      }\n    }\n  } ]\n}"
	case "platform/base-images":
		policy = "{\n  \"Version\" : \"2012-10-17\",\n  \"Statement\" : [ {\n    \"Sid\" : \"SpokePull\",\n    \"Effect\" : \"Allow\",\n" +
			"    \"Principal\" : {\n      \"AWS\" : [ \"arn:aws:iam::" + EstateWorkloads + ":root\", \"arn:aws:iam::" + EstateData + ":root\" ]\n    },\n" +
			"    \"Action\" : [ \"ecr:BatchGetImage\", \"ecr:GetDownloadUrlForLayer\" ]\n  }, {\n    \"Sid\" : \"LambdaECRImageCrossAccountRetrievalPolicy\",\n" +
			"    \"Effect\" : \"Allow\",\n    \"Principal\" : {\n      \"Service\" : \"lambda.amazonaws.com\"\n    },\n" +
			"    \"Action\" : [ \"ecr:GetDownloadUrlForLayer\", \"ecr:BatchGetImage\" ],\n    \"Condition\" : {\n      \"StringLike\" : {\n" +
			"        \"aws:sourceArn\" : [ \"arn:aws:lambda:" + EstateHomeRegion + ":" + EstateWorkloads + ":function:*\", \"arn:aws:lambda:" +
			EstateHomeRegion + ":" + EstateShared + ":function:*\" ]\n      }\n    }\n  } ]\n}"
	default:
		estateError(writer, call, http.StatusBadRequest, "RepositoryPolicyNotFoundException",
			"Repository policy does not exist for the repository with name '"+name+"' in the registry with id '"+call.account+"'")
		return
	}
	writeJSONBody(writer, mustJSON(map[string]any{"registryId": call.account, "repositoryName": name, "policyText": policy}))
}
