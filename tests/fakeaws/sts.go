package fakeaws

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// defaultRoleARN is used when a request asks for identity only.
const defaultRoleARN = "arn:aws:iam::111122223333:role/versus-reader"

// assumeDeniedReply is the real STS error that stands in for an internal
// scenario selector.
type assumeDeniedReply struct {
	status  int
	code    string
	message string
}

func (s *Server) serveAssumeDenied(writer http.ResponseWriter, request awsRequest) bool {
	if request.service != "sts" {
		return false
	}
	selector := assumeErrorForRequest(request)
	if selector == "" {
		return false
	}
	s.writeSTSError(writer, request.protocol, assumeDeniedWire(selector, request))
	return true
}

// assumeDeniedWire translates a scenario selector into the error AWS STS
// actually sends for that condition. The selector never reaches the wire.
func assumeDeniedWire(selector string, request awsRequest) assumeDeniedReply {
	roleARN := request.params.Get("RoleArn")
	if roleARN == "" {
		roleARN = defaultRoleARN
	}
	switch selector {
	case AssumeTrustDenied:
		return assumeDeniedReply{
			status:  http.StatusForbidden,
			code:    "AccessDenied",
			message: "User: arn:aws:iam::111122223333:user/versus is not authorized to perform: sts:AssumeRole on resource: " + roleARN,
		}
	case AssumeExternalIDMismatch:
		return assumeDeniedReply{
			status:  http.StatusForbidden,
			code:    "AccessDenied",
			message: "An error occurred (AccessDenied) when calling the AssumeRole operation: the external id is not authorized",
		}
	case AssumeRoleNotFound:
		return assumeDeniedReply{
			status:  http.StatusForbidden,
			code:    "AccessDenied",
			message: "The role with name " + roleNameForARN(roleARN) + " cannot be found",
		}
	case AssumeSessionDurationInvalid:
		return assumeDeniedReply{
			status:  http.StatusBadRequest,
			code:    "ValidationError",
			message: "The requested DurationSeconds exceeds the MaxSessionDuration of 3600 seconds set for this role",
		}
	}
	return assumeDeniedReply{
		status:  http.StatusForbidden,
		code:    "AccessDenied",
		message: "An error occurred (AccessDenied) when calling the AssumeRole operation",
	}
}

// writeSTSError emits the STS wire error shape for the request's protocol:
// query XML and JSON both carry the error type, the message, and a request id.
func (s *Server) writeSTSError(writer http.ResponseWriter, protocol string, reply assumeDeniedReply) {
	if protocol == protocolJSON {
		writer.Header().Set("x-amzn-RequestId", requestID)
		writeBody(writer, "application/x-amz-json-1.1", reply.status,
			`{"__type":`+strconv.Quote(reply.code)+
				`,"message":`+strconv.Quote(reply.message)+
				`,"RequestId":`+strconv.Quote(requestID)+`}`)
		return
	}
	writeBody(writer, "text/xml", reply.status, xmlErrorBody(reply.code, reply.message))
}

// roleNameForARN returns the final path segment of a role ARN.
func roleNameForARN(roleARN string) string {
	if index := strings.LastIndex(roleARN, "/"); index >= 0 && index+1 < len(roleARN) {
		return roleARN[index+1:]
	}
	return roleARN
}

func (s *Server) writeSTS(writer http.ResponseWriter, request awsRequest) {
	switch request.action {
	case "AssumeRole":
		s.writeAssumeRole(writer, request)
	case "GetCallerIdentity":
		s.writeCallerIdentity(writer, request)
	default:
		s.writeError(writer, request.protocol, http.StatusBadRequest, "InvalidAction", "Unsupported STS action")
	}
}

func (s *Server) writeAssumeRole(writer http.ResponseWriter, request awsRequest) {
	roleARN := request.params.Get("RoleArn")
	if roleARN == "" {
		roleARN = defaultRoleARN
	}
	account := accountForRoleARN(roleARN)
	if reason, ok := s.externalIDProblem(request.params); ok {
		s.writeError(writer, request.protocol, http.StatusForbidden, "AccessDenied", reason)
		return
	}
	duration := 3600
	if value, err := strconv.Atoi(request.params.Get("DurationSeconds")); err == nil && value > 0 {
		duration = value
	}
	expiration := time.Now().UTC().Add(time.Duration(duration) * time.Second).Format(time.RFC3339)
	expirationRFC1123 := time.Now().UTC().Add(time.Duration(duration) * time.Second).Format(time.RFC1123)

	if request.protocol == protocolQuery {
		inner := "<Credentials>" +
			xmlText("AccessKeyId", accessKeyID(account.ID)) +
			xmlText("SecretAccessKey", secretAccessKey(account.ID)) +
			xmlText("SessionToken", sessionToken(account.ID)) +
			xmlText("Expiration", expiration) +
			"</Credentials>" +
			"<AssumedRoleUser>" +
			xmlText("AssumedRoleId", "AROAVERSUSEXAMPLE:"+request.params.Get("RoleSessionName")) +
			xmlText("Arn", roleARN) +
			"</AssumedRoleUser>" +
			xmlText("PackedPolicySize", "0")
		writeXMLBody(writer, queryResult("AssumeRole", inner))
		return
	}
	body := `{"Credentials":{"AccessKeyId":` + strconv.Quote(accessKeyID(account.ID)) +
		`,"SecretAccessKey":` + strconv.Quote(secretAccessKey(account.ID)) +
		`,"SessionToken":` + strconv.Quote(sessionToken(account.ID)) +
		`,"Expiration":` + strconv.Quote(expirationRFC1123) + `},` +
		`"AssumedRoleUser":{"AssumedRoleId":` +
		strconv.Quote("AROAVERSUSEXAMPLE:"+request.params.Get("RoleSessionName")) +
		`,"Arn":` + strconv.Quote(roleARN) + `},` +
		`"PackedPolicySize":0}`
	writeJSONBody(writer, body)
}

// externalIDProblem returns the reason an AssumeRole call must be refused.
func (s *Server) externalIDProblem(params url.Values) (string, bool) {
	provided := params.Get("ExternalId")
	if provided == "" {
		return "An error occurred (AccessDenied) when calling the AssumeRole operation: an external id is required", true
	}
	s.mu.Lock()
	pinned := s.externalID
	s.mu.Unlock()
	if pinned != "" && provided != pinned {
		return "An error occurred (AccessDenied) when calling the AssumeRole operation: the external id is not authorized", true
	}
	if pinned == "" && !looksLikeExternalID(provided) {
		return "An error occurred (AccessDenied) when calling the AssumeRole operation: the external id is not authorized", true
	}
	return "", false
}

func looksLikeExternalID(value string) bool {
	trimmed := strings.TrimPrefix(value, "vrs-ext-")
	if trimmed == value || len(trimmed) < 8 {
		return false
	}
	for _, character := range trimmed {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func (s *Server) writeCallerIdentity(writer http.ResponseWriter, request awsRequest) {
	account := request.account
	if account.ID == "" {
		account = accountByID("111122223333")
	}
	principal := "arn:aws:iam::" + account.ID + ":role/versus-reader"
	if request.protocol == protocolQuery {
		inner := xmlText("UserId", "AROAVERSUSEXAMPLE:versus") +
			xmlText("Account", account.ID) +
			xmlText("Arn", principal)
		writeXMLBody(writer, queryResult("GetCallerIdentity", inner))
		return
	}
	body := `{"UserId":"AROAVERSUSEXAMPLE:versus","Account":` + strconv.Quote(account.ID) +
		`,"Arn":` + strconv.Quote(principal) + `}`
	writeJSONBody(writer, body)
}

func accountForRoleARN(roleARN string) Account {
	parts := strings.Split(roleARN, ":")
	if len(parts) >= 5 && digitsOnly(parts[4]) {
		return accountByID(parts[4])
	}
	return accountByID("111122223333")
}

// Credential values are deterministic per account so a test can tell which
// account a probe was made for from the token alone.
func accessKeyID(accountID string) string { return "ASIAVERSUS" + accountID }

func secretAccessKey(accountID string) string {
	return "versus-secret-" + accountID
}

func sessionToken(accountID string) string {
	return "FwoGZXIvYXdzE" + accountID + "session"
}
