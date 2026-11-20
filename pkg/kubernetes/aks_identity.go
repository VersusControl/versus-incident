package kubernetes

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/logging"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// aksReviewedExtraAllowlist is every identity review extra key an AKS
// principal keeps, matched exactly. The Entra object ID names the identity;
// every other key describes one token or where it was issued, and is
// dropped. It is a constant of the code, not a setting.
var aksReviewedExtraAllowlist = []string{"oid"}

// aksCredentialSource issues Entra ID access tokens for an AKS cluster. Its
// renewals are proved before use, so an hourly refresh of the same identity
// keeps the credential epoch.
type aksCredentialSource struct {
	*expiringBearerSource
	// proof is the provider proof of a token's identity; nil means the
	// token cannot be proved by the provider, and identity review decides.
	proof func(context.Context, string) (string, proofOutcome, error)
	// host is the provider proof host named when it cannot be reached.
	host string
}

func (source *aksCredentialSource) verifiedCredentialPrincipal(ctx context.Context, header string) (string, proofOutcome, error) {
	if source.proof == nil {
		return "", proofNotApplicable, nil
	}
	return source.proof(ctx, header)
}

func (source *aksCredentialSource) proofHost() string { return source.host }

func (source *aksCredentialSource) provesRenewals() {}

func (source *aksCredentialSource) normalizeReviewedUser(user *reviewedUserInfo) {
	keepReviewedExtras(user, aksReviewedExtraAllowlist)
}

// keepReviewedExtras deletes every extra key not on allowlist. It only
// deletes keys: username, UID, groups and the kept keys' values are never
// changed. An answer with no extra left and one with no extra at all give
// the same principal.
func keepReviewedExtras(user *reviewedUserInfo, allowlist []string) {
	if user == nil {
		return
	}
	for key := range user.Extra {
		kept := false
		for _, allowed := range allowlist {
			if key == allowed {
				kept = true
				break
			}
		}
		if !kept {
			delete(user.Extra, key)
		}
	}
	if len(user.Extra) == 0 {
		user.Extra = nil
	}
}

// entraCloud is one Azure cloud's identity hosts. Versus builds metadata URLs
// from authority only; keyHosts are the only hosts a document's jwks_uri may
// name. The China partner host serves China keys only and is never asked for
// metadata.
type entraCloud struct {
	authority string
	keyHosts  []string
}

var entraClouds = map[string]entraCloud{
	"public":     {authority: "https://login.microsoftonline.com", keyHosts: []string{"login.microsoftonline.com"}},
	"government": {authority: "https://login.microsoftonline.us", keyHosts: []string{"login.microsoftonline.us"}},
	"china":      {authority: "https://login.chinacloudapi.cn", keyHosts: []string{"login.chinacloudapi.cn", "login.partner.microsoftonline.cn"}},
}

const (
	// entraCacheTTL is how long metadata and signing keys are reused.
	entraCacheTTL = 24 * time.Hour
	// entraDocumentLimit caps a metadata or key-set body.
	entraDocumentLimit = 64 << 10
	// entraMinimumKeyBits is the smallest RSA modulus accepted.
	entraMinimumKeyBits = 2048
	// entraMaxClockSkew is the largest gap between a token's claimed expiry
	// and the expiry the token endpoint reported that is read as clock skew.
	entraMaxClockSkew = 15 * time.Minute
)

// entraKeyRefetch is the wait between refetches of a key set that lacks a
// token's key ID; after the last step the proof's own failure backoff
// applies.
var entraKeyRefetch = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}

var canonicalGUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// entraTokenVerifier proves which Entra ID identity an AKS access token
// carries: the signature with the tenant's published keys, then audience,
// tenant, object ID and issuer, then lifetime. The principal is the tenant and
// object ID.
//
// The tenant is pinned as a GUID: the configured GUID, the GUID in the
// issuer of a configured domain's own metadata, or, for a managed identity,
// the tid of the first token that passes every check. A tenant that cannot
// be pinned (common, organizations, consumers) makes the proof not
// applicable.
//
// A token the checks refuse is not proven: a definite answer with no grace,
// never a cluster 401. A failure to read metadata or keys is unavailable.
type entraTokenVerifier struct {
	authority string
	keyHosts  []string
	tenant    string
	audience  string
	client    *http.Client
	now       func() time.Time
	leeway    time.Duration
	// expiry is the source's recorded expiry of a token, used to tell a
	// clock that disagrees with the token from a token that expired.
	expiry func(header string) time.Time

	mu        sync.Mutex
	pin       string
	noPin     bool
	documents map[string]entraDocument
	keySets   map[string]*entraKeySet
}

type entraDocument struct {
	issuer  string
	jwksURI string
	fetched time.Time
}

type entraKeySet struct {
	keys      map[string]entraKey
	fetched   time.Time
	misses    int
	nextFetch time.Time
}

type entraKey struct {
	key    *rsa.PublicKey
	issuer string
}

type entraClaims struct {
	jwt.Claims
	TenantID string `json:"tid"`
	ObjectID string `json:"oid"`
	Version  string `json:"ver"`
}

func newEntraTokenVerifier(cloud entraCloud, tenant, audience string, client *http.Client, now func() time.Time, leeway time.Duration, expiry func(string) time.Time) *entraTokenVerifier {
	verifier := &entraTokenVerifier{authority: cloud.authority, keyHosts: cloud.keyHosts, audience: audience, client: client, now: now, leeway: leeway, expiry: expiry,
		documents: map[string]entraDocument{}, keySets: map[string]*entraKeySet{}}
	switch lowered := strings.ToLower(strings.TrimSpace(tenant)); {
	case lowered == "":
	case canonicalGUID.MatchString(lowered):
		verifier.pin = lowered
	case lowered == "common" || lowered == "organizations" || lowered == "consumers":
		verifier.noPin = true
	default:
		verifier.tenant = tenant
	}
	return verifier
}

// host is the authority host named when it cannot be reached.
func (verifier *entraTokenVerifier) host() string {
	parsed, err := url.Parse(verifier.authority)
	if err != nil {
		return ""
	}
	return parsed.Host
}

func (verifier *entraTokenVerifier) principal(ctx context.Context, header string) (string, proofOutcome, error) {
	notProven := func() (string, proofOutcome, error) { return "", proofNotApplicable, nil }
	token, err := jwt.ParseSigned(strings.TrimPrefix(header, "Bearer "), []jose.SignatureAlgorithm{jose.RS256})
	if err != nil || len(token.Headers) != 1 || token.Headers[0].KeyID == "" || token.Headers[0].Algorithm != string(jose.RS256) {
		return notProven()
	}
	var unverified entraClaims
	if token.UnsafeClaimsWithoutVerification(&unverified) != nil {
		return notProven()
	}
	version := ""
	switch unverified.Version {
	case "1.0":
		version = "v1"
	case "2.0":
		version = "v2"
	default:
		return notProven()
	}
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	tenant, outcome, err := verifier.tenantLocked(ctx, unverified.TenantID)
	if outcome != proofProven {
		return "", outcome, err
	}
	document, err := verifier.documentLocked(ctx, version, tenant)
	if err != nil {
		return "", proofUnavailable, err
	}
	expectedIssuer := strings.ReplaceAll(document.issuer, "{tenantid}", tenant)
	if unverified.Issuer != expectedIssuer {
		return notProven()
	}
	key, err := verifier.keyLocked(ctx, document.jwksURI, token.Headers[0].KeyID)
	if err != nil {
		return "", proofUnavailable, err
	}
	if key.key.N.BitLen() < entraMinimumKeyBits || key.issuer != "" && strings.ReplaceAll(key.issuer, "{tenantid}", tenant) != expectedIssuer {
		return notProven()
	}
	var claims entraClaims
	if token.Claims(key.key, &claims) != nil {
		return notProven()
	}
	// The audience, tenant and object ID are checked before the lifetime: a
	// token that fails one of them is refused whatever the clock says, so it
	// is never reported as clock skew.
	audience := strings.TrimSuffix(verifier.audience, "/")
	if len(claims.Audience) != 1 || (strings.TrimSuffix(claims.Audience[0], "/") != audience && strings.TrimSuffix(claims.Audience[0], "/") != "api://"+strings.TrimPrefix(audience, "api://")) {
		return notProven()
	}
	if claims.TenantID != tenant || !canonicalGUID.MatchString(claims.ObjectID) || claims.Issuer != expectedIssuer {
		return notProven()
	}
	now := verifier.now()
	if claims.NotBefore != nil && claims.NotBefore.Time().After(now.Add(verifier.leeway)) || claims.IssuedAt != nil && claims.IssuedAt.Time().After(now.Add(verifier.leeway)) {
		// A token just issued that is not valid yet means the clocks
		// disagree: nothing is wrong with the token.
		return "", proofUnavailable, &proofError{class: "clock_skew"}
	}
	if claims.Expiry == nil {
		return notProven()
	}
	if now.After(claims.Expiry.Time().Add(verifier.leeway)) {
		// Expired by its claims while the token endpoint's lifetime says it
		// is valid: the clocks disagree, but only by a bounded amount. A
		// larger gap is a replayed or stale token, not skew.
		if verifier.expiry != nil {
			recorded := verifier.expiry(header)
			if recorded.After(now) && recorded.Sub(claims.Expiry.Time()) <= entraMaxClockSkew {
				return "", proofUnavailable, &proofError{class: "clock_skew"}
			}
		}
		return notProven()
	}
	if verifier.pin == "" {
		// A managed identity's tenant is pinned only now, after every check.
		verifier.pin = tenant
	}
	encoded, err := json.Marshal([]string{"verified-entra-id", tenant, claims.ObjectID})
	if err != nil {
		return notProven()
	}
	return string(encoded), proofProven, nil
}

// tenantLocked returns the tenant GUID a token is checked against. The token's
// own tid is read before verification only for a managed identity with no pin
// yet, to choose the metadata document, and only when it is a canonical GUID.
func (verifier *entraTokenVerifier) tenantLocked(ctx context.Context, tokenTenant string) (string, proofOutcome, error) {
	switch {
	case verifier.noPin:
		return "", proofNotApplicable, nil
	case verifier.pin != "":
		if tokenTenant != verifier.pin {
			return "", proofNotApplicable, nil
		}
		return verifier.pin, proofProven, nil
	case verifier.tenant != "":
		// A configured domain: the GUID comes from the issuer of the
		// domain's own metadata.
		document, err := verifier.fetchDocument(ctx, verifier.authority+"/"+url.PathEscape(verifier.tenant)+"/.well-known/openid-configuration")
		if err != nil {
			return "", proofUnavailable, err
		}
		if strings.Contains(document.issuer, "{tenantid}") {
			verifier.noPin = true
			return "", proofNotApplicable, nil
		}
		parsed, err := url.Parse(document.issuer)
		if err != nil {
			return "", proofUnavailable, &proofError{class: "invalid_answer"}
		}
		for _, segment := range strings.Split(parsed.Path, "/") {
			if canonicalGUID.MatchString(segment) {
				verifier.pin = segment
				return verifier.tenantLocked(ctx, tokenTenant)
			}
		}
		verifier.noPin = true
		return "", proofNotApplicable, nil
	}
	if !canonicalGUID.MatchString(tokenTenant) {
		return "", proofNotApplicable, nil
	}
	return tokenTenant, proofProven, nil
}

// documentLocked returns the cached metadata for a token version and tenant.
// Versus builds the URL from the cloud's authority and the tenant GUID.
func (verifier *entraTokenVerifier) documentLocked(ctx context.Context, version, tenant string) (entraDocument, error) {
	cacheKey := version + "|" + tenant
	if document, ok := verifier.documents[cacheKey]; ok && verifier.now().Sub(document.fetched) < entraCacheTTL {
		return document, nil
	}
	address := verifier.authority + "/" + tenant + "/.well-known/openid-configuration"
	if version == "v2" {
		address = verifier.authority + "/" + tenant + "/v2.0/.well-known/openid-configuration"
	}
	document, err := verifier.fetchDocument(ctx, address)
	if err != nil {
		return entraDocument{}, err
	}
	if !verifier.allowedKeyURI(document.jwksURI) {
		// Opening egress would not fix this, so it has its own cause.
		return entraDocument{}, &proofError{class: "jwks_host_not_allowed"}
	}
	verifier.documents[cacheKey] = document
	return document, nil
}

// allowedKeyURI accepts only an https URL on one of this cloud's key hosts,
// with no user info and no port.
func (verifier *entraTokenVerifier) allowedKeyURI(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	return slices.Contains(verifier.keyHosts, parsed.Host)
}

func (verifier *entraTokenVerifier) fetchDocument(ctx context.Context, address string) (entraDocument, error) {
	var document struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := verifier.getJSON(ctx, address, &document); err != nil {
		return entraDocument{}, err
	}
	if document.Issuer == "" || document.JWKSURI == "" {
		return entraDocument{}, &proofError{class: "invalid_answer"}
	}
	return entraDocument{issuer: document.Issuer, jwksURI: document.JWKSURI, fetched: verifier.now()}, nil
}

// keyLocked returns the signing key for a key ID. Keys are reused for
// entraCacheTTL; a key ID the set lacks refetches it on the entraKeyRefetch
// schedule, not once.
func (verifier *entraTokenVerifier) keyLocked(ctx context.Context, address, keyID string) (entraKey, error) {
	now := verifier.now()
	set := verifier.keySets[address]
	if set != nil && now.Sub(set.fetched) >= entraCacheTTL {
		set = nil
	}
	if set != nil {
		if key, ok := set.keys[keyID]; ok {
			return key, nil
		}
		if now.Before(set.nextFetch) {
			return entraKey{}, &proofError{class: "unknown_key"}
		}
	}
	keys, err := verifier.fetchKeys(ctx, address)
	if err != nil {
		return entraKey{}, err
	}
	misses := 0
	if set != nil {
		misses = set.misses
	}
	set = &entraKeySet{keys: keys, fetched: now}
	verifier.keySets[address] = set
	if key, ok := keys[keyID]; ok {
		return key, nil
	}
	set.misses = misses + 1
	set.nextFetch = now.Add(entraKeyRefetch[min(set.misses, len(entraKeyRefetch))-1])
	logging.Debugf("kubernetes: AKS identity proof key not published yet; refetch in %s", entraKeyRefetch[min(set.misses, len(entraKeyRefetch))-1])
	return entraKey{}, &proofError{class: "unknown_key"}
}

func (verifier *entraTokenVerifier) fetchKeys(ctx context.Context, address string) (map[string]entraKey, error) {
	var document struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := verifier.getJSON(ctx, address, &document); err != nil {
		return nil, err
	}
	keys := map[string]entraKey{}
	for _, raw := range document.Keys {
		var member struct {
			KeyType string `json:"kty"`
			KeyID   string `json:"kid"`
			Issuer  string `json:"issuer"`
		}
		var key jose.JSONWebKey
		if json.Unmarshal(raw, &member) != nil || member.KeyType != "RSA" || member.KeyID == "" || key.UnmarshalJSON(raw) != nil {
			continue
		}
		public, ok := key.Key.(*rsa.PublicKey)
		if !ok {
			continue
		}
		keys[member.KeyID] = entraKey{key: public, issuer: member.Issuer}
	}
	return keys, nil
}

// getJSON reads one bounded JSON document. Redirects are never followed: the
// client returns them, and they are unavailable.
func (verifier *entraTokenVerifier) getJSON(ctx context.Context, address string, output any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return &proofError{class: "invalid_answer"}
	}
	request.Header.Set("Accept", "application/json")
	response, err := verifier.client.Do(request)
	if err != nil {
		return proofTransportError(err)
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500:
		return proofStatusError(response, verifier.now())
	case response.StatusCode != http.StatusOK:
		return &proofError{class: unexpectedStatusClass(response.StatusCode)}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, entraDocumentLimit+1))
	if err != nil {
		return proofTransportError(err)
	}
	if len(raw) > entraDocumentLimit || json.Unmarshal(raw, output) != nil {
		return &proofError{class: "invalid_answer"}
	}
	return nil
}
