package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// eksTokenPrefix marks a presigned STS GetCallerIdentity request used as a
// Kubernetes bearer token.
const eksTokenPrefix = "k8s-aws-v1."

var awsAccountPattern = regexp.MustCompile(`^\d{12}$`)

// eksCredentialSource issues EKS bearer tokens and proves which AWS principal
// each token carries. A renewed token for the same IAM principal keeps the
// same credential epoch, so a 14-minute token refresh is not a new identity.
type eksCredentialSource struct {
	*expiringBearerSource
	identity *eksIdentityVerifier
}

func (source *eksCredentialSource) verifiedCredentialPrincipal(ctx context.Context, header string) (string, proofOutcome, error) {
	return source.identity.principal(ctx, strings.TrimPrefix(header, "Bearer "))
}

// proofHost is the STS host the proof calls.
func (source *eksCredentialSource) proofHost() string {
	if source.identity == nil || source.identity.endpoint == nil {
		return ""
	}
	return source.identity.endpoint.Host
}

func (source *eksCredentialSource) provesRenewals() {}

func (source *eksCredentialSource) normalizeReviewedUser(user *reviewedUserInfo) {
	normalizeEKSReviewedUser(user)
}

// eksIdentityVerifier sends the token's own presigned request to STS. STS
// answers with the identity that signed it, which is exactly what EKS
// authenticates, so the proof is bound to these token bytes.
type eksIdentityVerifier struct {
	endpoint    *url.URL
	clusterName string
	client      *http.Client

	mu          sync.Mutex
	cachedToken [32]byte
	cached      string
}

func newEKSIdentityVerifier(endpoint, clusterName string, client *http.Client) *eksIdentityVerifier {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil
	}
	return &eksIdentityVerifier{endpoint: parsed, clusterName: clusterName, client: client}
}

// principal proves the token's identity. A token that is not a presigned
// request to the configured STS endpoint, or an identity STS reports in a
// form with no stable principal, is not applicable: it keeps the byte-bound
// epoch. A failed STS call is unavailable; STS refusing the signature is a
// definite rejection.
func (verifier *eksIdentityVerifier) principal(ctx context.Context, token string) (string, proofOutcome, error) {
	if verifier == nil || verifier.client == nil {
		return "", proofNotApplicable, nil
	}
	digest := sha256.Sum256([]byte(token))
	verifier.mu.Lock()
	if verifier.cached != "" && digest == verifier.cachedToken {
		cached := verifier.cached
		verifier.mu.Unlock()
		return cached, proofProven, nil
	}
	verifier.mu.Unlock()

	target, ok := verifier.presignedRequest(token)
	if !ok {
		return "", proofNotApplicable, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return "", proofNotApplicable, nil
	}
	request.Header.Set("x-k8s-aws-id", verifier.clusterName)
	request.Header.Set("Accept", "application/json")
	response, err := verifier.client.Do(request)
	if err != nil {
		return "", proofUnavailable, proofTransportError(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", proofUnavailable, stsAnswerError(response)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 64<<10+1))
	if err != nil || len(raw) > 64<<10 {
		return "", proofUnavailable, &proofError{class: "invalid_answer"}
	}
	var result struct {
		GetCallerIdentityResponse struct {
			GetCallerIdentityResult struct {
				Account string `json:"Account"`
				Arn     string `json:"Arn"`
				UserID  string `json:"UserId"`
			} `json:"GetCallerIdentityResult"`
		} `json:"GetCallerIdentityResponse"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return "", proofUnavailable, &proofError{class: "invalid_answer"}
	}
	caller := result.GetCallerIdentityResponse.GetCallerIdentityResult
	principal, ok := canonicalAWSPrincipal(caller.Account, caller.Arn, caller.UserID)
	if !ok {
		return "", proofNotApplicable, nil
	}
	verifier.mu.Lock()
	verifier.cachedToken, verifier.cached = digest, principal
	verifier.mu.Unlock()
	return principal, proofProven, nil
}

// presignedRequest accepts only a GetCallerIdentity request to the configured
// STS endpoint, so the token can never direct this process elsewhere.
func (verifier *eksIdentityVerifier) presignedRequest(token string) (*url.URL, bool) {
	encoded, found := strings.CutPrefix(token, eksTokenPrefix)
	if !found || encoded == "" {
		return nil, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, false
	}
	target, err := url.Parse(string(decoded))
	if err != nil || target.Scheme != verifier.endpoint.Scheme || target.Host != verifier.endpoint.Host || (target.Path != "/" && target.Path != "") || target.User != nil || target.Fragment != "" {
		return nil, false
	}
	query := target.Query()
	if query.Get("Action") != "GetCallerIdentity" || query.Get("X-Amz-Signature") == "" {
		return nil, false
	}
	return target, true
}

// canonicalAWSPrincipal removes the per-session parts of an AWS identity.
// Every session of one IAM role maps to that role; a different role, user or
// account is a different principal.
func canonicalAWSPrincipal(account, arn, userID string) (string, bool) {
	if !awsAccountPattern.MatchString(account) || userID == "" || len(arn) > 2048 || len(userID) > 512 {
		return "", false
	}
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] == "" || parts[4] != account {
		return "", false
	}
	partition, service, resource := parts[1], parts[2], parts[5]
	switch {
	case service == "sts" && strings.HasPrefix(resource, "assumed-role/"):
		segments := strings.Split(strings.TrimPrefix(resource, "assumed-role/"), "/")
		if len(segments) != 2 || segments[0] == "" || segments[1] == "" {
			return "", false
		}
		arn = "arn:" + partition + ":iam::" + account + ":role/" + segments[0]
	case service == "iam" || service == "sts":
	default:
		return "", false
	}
	// A role's unique ID carries the session after a colon.
	unique, _, _ := strings.Cut(userID, ":")
	encoded, err := json.Marshal([]string{account, arn, unique})
	if err != nil {
		return "", false
	}
	return "aws-sts:" + string(encoded), true
}

// eksSessionExtras are the AWS IAM authenticator fields that change with each
// STS session rather than with the authenticated principal.
var eksSessionExtras = []string{"accessKeyId", "sessionName", "principalId", "arn", "sigs.k8s.io/aws-iam-authenticator/principalId"}

// normalizeEKSReviewedUser removes per-session fields from an EKS identity.
// Only identities issued by the AWS IAM authenticator, which always carry
// canonicalArn, are changed. Username, UID, groups and the canonical role
// remain part of the principal.
func normalizeEKSReviewedUser(user *reviewedUserInfo) {
	if user == nil || len(user.Extra["canonicalArn"]) == 0 {
		return
	}
	session := ""
	if values := user.Extra["sessionName"]; len(values) == 1 {
		session = values[0]
	}
	for _, key := range eksSessionExtras {
		delete(user.Extra, key)
	}
	if session == "" {
		return
	}
	// Default EKS role mappings put the session name at the end of the username
	// and, for some mappings, the UID.
	if trimmed, found := strings.CutSuffix(user.Username, "/"+session); found {
		user.Username = trimmed + "/{{SessionName}}"
	}
	if trimmed, found := strings.CutSuffix(user.UID, ":"+session); found {
		user.UID = trimmed
	}
}

// stsThrottleCodes are the STS error codes that mean "slow down".
var stsThrottleCodes = map[string]bool{"Throttling": true, "ThrottlingException": true, "RequestLimitExceeded": true, "TooManyRequestsException": true}

// stsAnswerError classifies an STS answer other than 200. Only a 401 or 403
// is a definite rejection of the signed request (InvalidClientTokenId,
// SignatureDoesNotMatch, ExpiredToken and the like). A throttling code, a
// 429 or a 5xx is unavailable and keeps its Retry-After; any other answer is
// unavailable too, since nothing in it says the token is bad.
func stsAnswerError(response *http.Response) error {
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return fmt.Errorf("kubernetes: identity provider refused the credential: %w", ErrUnauthorized)
	case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500:
		return proofStatusError(response, time.Now())
	}
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 16<<10))
	if stsThrottleCodes[stsErrorCode(raw)] {
		failure := proofStatusError(response, time.Now())
		failure.class = "429"
		return failure
	}
	return &proofError{class: "4xx"}
}

// stsErrorCode reads the error code from a JSON or XML STS error body. It
// returns "" when there is none.
func stsErrorCode(raw []byte) string {
	var document struct {
		Error struct {
			Code string `json:"Code"`
		} `json:"Error"`
	}
	if json.Unmarshal(raw, &document) == nil && document.Error.Code != "" {
		return document.Error.Code
	}
	var envelope struct {
		Error struct {
			Code string `xml:"Code"`
		} `xml:"Error"`
	}
	if xml.Unmarshal(raw, &envelope) == nil {
		return envelope.Error.Code
	}
	return ""
}
