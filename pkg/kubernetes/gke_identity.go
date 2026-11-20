package kubernetes

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// gkeReviewedExtraAllowlist is every identity review extra key a GKE
// principal keeps: none. The user assertions GKE adds change with every
// token; the username, UID and groups name the identity.
var gkeReviewedExtraAllowlist = []string{}

// gkeCredentialSource issues Google access tokens for a GKE cluster. Its
// renewals are proved before use, so an hourly refresh of the same identity
// keeps the credential epoch.
type gkeCredentialSource struct {
	*expiringBearerSource
	// proof is the provider proof of a token's identity; nil means the
	// token cannot be proved by the provider, and identity review decides.
	proof func(context.Context, string) (string, proofOutcome, error)
	// host is the provider proof host named when it cannot be reached.
	host string
}

func (source *gkeCredentialSource) verifiedCredentialPrincipal(ctx context.Context, header string) (string, proofOutcome, error) {
	if source.proof == nil {
		return "", proofNotApplicable, nil
	}
	return source.proof(ctx, header)
}

func (source *gkeCredentialSource) proofHost() string { return source.host }

func (source *gkeCredentialSource) provesRenewals() {}

func (source *gkeCredentialSource) normalizeReviewedUser(user *reviewedUserInfo) {
	keepReviewedExtras(user, gkeReviewedExtraAllowlist)
}

// googleTokenInfoURL is Google's token-info endpoint. The host is fixed.
const googleTokenInfoURL = "https://oauth2.googleapis.com/tokeninfo"

// googleTokenInfoLimit caps a token-info answer.
const googleTokenInfoLimit = 16 << 10

// gkeTokenKind is the kind of Google token a GKE credential issues. Whether
// the token-info proof applies is decided by it, never by an answer: the
// endpoint answers a federated token and a bad one the same way.
type gkeTokenKind uint8

const (
	// gkeServiceAccountToken comes from a service-account key or from
	// impersonating a service account.
	gkeServiceAccountToken gkeTokenKind = iota
	// gkeFederatedToken comes from workload identity federation without
	// impersonation; token-info cannot read it.
	gkeFederatedToken
	// gkeMetadataToken comes from the metadata server, which hands out
	// either kind; each new token is asked once.
	gkeMetadataToken
)

var googleServiceAccountID = regexp.MustCompile(`^[0-9]{1,32}$`)

// googleTokenInfoVerifier proves a Google access token's identity with
// token-info. The principal is the service account's unique ID (azp), only
// for an answer whose azp is digits, aud equals azp, expires_in is positive
// and sub is absent or equal to azp; email never enters it. Every token
// needs its own 200.
//
// Only a well-formed 200 and a 400 whose error is invalid_token are definite
// answers; a 200 that fails the checks or that 400 is not proven, with no
// grace. Anything else is unavailable and decides nothing.
type googleTokenInfoVerifier struct {
	endpoint string
	client   *http.Client
	kind     gkeTokenKind
}

func (verifier *googleTokenInfoVerifier) principal(ctx context.Context, header string) (string, proofOutcome, error) {
	if verifier.kind == gkeFederatedToken {
		return "", proofNotApplicable, nil
	}
	response, err := postGoogleTokenInfo(ctx, verifier.client, verifier.endpoint, strings.TrimPrefix(header, "Bearer "))
	if err != nil {
		return "", proofUnavailable, proofTransportError(err)
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500:
		return "", proofUnavailable, proofStatusError(response, time.Now())
	case response.StatusCode != http.StatusOK && response.StatusCode != http.StatusBadRequest:
		return "", proofUnavailable, &proofError{class: unexpectedStatusClass(response.StatusCode)}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, googleTokenInfoLimit+1))
	if err != nil {
		return "", proofUnavailable, proofTransportError(err)
	}
	var answer map[string]any
	if len(raw) > googleTokenInfoLimit || json.Unmarshal(raw, &answer) != nil || answer == nil {
		return "", proofUnavailable, &proofError{class: "invalid_answer"}
	}
	if response.StatusCode == http.StatusBadRequest {
		if answer["error"] == "invalid_token" {
			return "", proofNotApplicable, nil
		}
		return "", proofUnavailable, &proofError{class: "4xx"}
	}
	azp, _ := answer["azp"].(string)
	audience, _ := answer["aud"].(string)
	expires, ok := numericString(answer["expires_in"])
	subject, hasSubject := answer["sub"]
	if !googleServiceAccountID.MatchString(azp) || audience != azp || !ok || expires <= 0 || hasSubject && subject != azp {
		return "", proofNotApplicable, nil
	}
	encoded, err := json.Marshal([]string{"verified-google", azp})
	if err != nil {
		return "", proofNotApplicable, nil
	}
	return string(encoded), proofProven, nil
}

// googleTokenKind reads the token kind from a credentials file: an external
// account without impersonation issues federated tokens.
func googleTokenKind(data []byte) gkeTokenKind {
	var document struct {
		Type                           string `json:"type"`
		ServiceAccountImpersonationURL string `json:"service_account_impersonation_url"`
	}
	if json.Unmarshal(data, &document) == nil && document.Type == "external_account" && document.ServiceAccountImpersonationURL == "" {
		return gkeFederatedToken
	}
	return gkeServiceAccountToken
}
