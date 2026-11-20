package kubernetes

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// The identities below are the placeholders of the recorded claim shapes.
const (
	entraTenant      = "aaaaaaaa-0000-4000-8000-000000000001"
	entraOtherTenant = "aaaaaaaa-0000-4000-8000-000000000009"
	entraObject      = "cccccccc-0000-4000-8000-000000000003"
	entraOtherObject = "cccccccc-0000-4000-8000-000000000007"
	entraServerID    = "6dae42f8-4368-4678-94ff-3960e28e3630"
)

// entraCloudShape is one cloud's documented issuers, as the recorded
// tenant metadata reports them. {tid} is the tenant.
type entraCloudShape struct {
	v1Issuer, v2Issuer string
	// v2KeysOnPartner serves the v2 keys from the second key host, as the
	// China cloud's v2 issuer host differs from its authority.
	v2KeysOnPartner bool
}

var entraCloudShapes = map[string]entraCloudShape{
	"public":     {v1Issuer: "https://sts.windows.net/{tid}/", v2Issuer: "https://login.microsoftonline.com/{tid}/v2.0"},
	"government": {v1Issuer: "https://sts.windows.net/{tid}/", v2Issuer: "https://login.microsoftonline.us/{tid}/v2.0"},
	"china":      {v1Issuer: "https://sts.chinacloudapi.cn/{tid}/", v2Issuer: "https://login.partner.microsoftonline.cn/{tid}/v2.0", v2KeysOnPartner: true},
}

// entraFake serves tenant metadata and signing keys the way the Entra ID
// endpoints do, on an authority host, a partner key host and an off-list host.
type entraFake struct {
	t         *testing.T
	shape     entraCloudShape
	authority *httptest.Server
	partner   *httptest.Server
	offList   *httptest.Server
	client    *http.Client
	clock     *fakeClock

	mu sync.Mutex
	// keys are the published signing keys by key ID; small keys are
	// published with a 1024-bit modulus.
	keys map[string]*rsa.PrivateKey
	// keyIssuer is the issuer member of every published v2 key.
	keyIssuer string
	// jwksOverride replaces the jwks_uri in metadata when set.
	jwksOverride string
	// keysAnswer replaces the key-set answer: a status, a redirect, an
	// oversized body or a hang.
	keysAnswer string
	paths      []string
	offListHit atomic.Int32
}

func newEntraFake(t *testing.T, cloud string) *entraFake {
	t.Helper()
	fake := &entraFake{t: t, shape: entraCloudShapes[cloud], keys: map[string]*rsa.PrivateKey{}, clock: newFakeClock()}
	fake.keys["PLACEHOLDER-KID-1"] = entraKeyPair(t, 2048)
	fake.authority = httptest.NewTLSServer(http.HandlerFunc(fake.serve))
	fake.partner = httptest.NewTLSServer(http.HandlerFunc(fake.serve))
	fake.offList = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.offListHit.Add(1)
		fake.serve(w, r)
	}))
	t.Cleanup(fake.authority.Close)
	t.Cleanup(fake.partner.Close)
	t.Cleanup(fake.offList.Close)
	pool := x509.NewCertPool()
	for _, server := range []*httptest.Server{fake.authority, fake.partner, fake.offList} {
		pool.AddCert(server.Certificate())
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	fake.client = &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	fake.keyIssuer = strings.ReplaceAll(fake.shape.v2Issuer, "{tid}", "{tenantid}")
	return fake
}

var entraKeyCache sync.Map

// entraKeyPair returns an RSA key of the given size, generated once per size
// and key slot for speed.
func entraKeyPair(t *testing.T, bits int, slot ...int) *rsa.PrivateKey {
	t.Helper()
	cacheKey := fmt.Sprint(bits, slot)
	if cached, ok := entraKeyCache.Load(cacheKey); ok {
		return cached.(*rsa.PrivateKey)
	}
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	entraKeyCache.Store(cacheKey, key)
	return key
}

func (fake *entraFake) host(server *httptest.Server) string {
	parsed, _ := url.Parse(server.URL)
	return parsed.Host
}

func (fake *entraFake) serve(w http.ResponseWriter, r *http.Request) {
	fake.mu.Lock()
	fake.paths = append(fake.paths, r.URL.Path)
	fake.mu.Unlock()
	path := strings.Trim(r.URL.Path, "/")
	segments := strings.Split(path, "/")
	switch {
	case strings.HasSuffix(path, "/v2.0/.well-known/openid-configuration"):
		keys := fake.authority.URL + "/" + segments[0] + "/discovery/v2.0/keys"
		if fake.shape.v2KeysOnPartner {
			keys = fake.partner.URL + "/" + segments[0] + "/discovery/v2.0/keys"
		}
		fake.document(w, segments[0], fake.shape.v2Issuer, keys)
	case strings.HasSuffix(path, "/.well-known/openid-configuration"):
		fake.document(w, segments[0], fake.shape.v1Issuer, fake.authority.URL+"/common/discovery/keys")
	case strings.HasSuffix(path, "discovery/keys") || strings.HasSuffix(path, "discovery/v2.0/keys"):
		fake.keySet(w, r, strings.Contains(path, "v2.0"))
	default:
		http.NotFound(w, r)
	}
}

func (fake *entraFake) document(w http.ResponseWriter, tenant, issuer, keys string) {
	switch tenant {
	case "contoso.onmicrosoft.com":
		tenant = entraTenant
	case "common", "organizations", "consumers":
		tenant = "{tenantid}"
	}
	fake.mu.Lock()
	if fake.jwksOverride != "" {
		keys = fake.jwksOverride
	}
	fake.mu.Unlock()
	writeJSON(w, map[string]any{"issuer": strings.ReplaceAll(issuer, "{tid}", tenant), "jwks_uri": keys, "id_token_signing_alg_values_supported": []string{"RS256"}})
}

func (fake *entraFake) keySet(w http.ResponseWriter, r *http.Request, v2 bool) {
	fake.mu.Lock()
	answer := fake.keysAnswer
	published := map[string]*rsa.PrivateKey{}
	for kid, key := range fake.keys {
		published[kid] = key
	}
	keyIssuer := fake.keyIssuer
	fake.mu.Unlock()
	switch answer {
	case "500":
		w.WriteHeader(http.StatusInternalServerError)
		return
	case "redirect":
		http.Redirect(w, r, fake.offList.URL+"/keys", http.StatusFound)
		return
	case "oversized":
		_, _ = w.Write([]byte(`{"keys":[],"padding":"` + strings.Repeat("x", entraDocumentLimit) + `"}`))
		return
	case "hang":
		<-r.Context().Done()
		return
	}
	keys := []map[string]any{}
	for kid, key := range published {
		// The recorded key shape, without the certificate members.
		member := map[string]any{"kty": "RSA", "use": "sig", "kid": kid,
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()), "cloud_instance_name": "microsoftonline.com"}
		if v2 {
			member["issuer"] = keyIssuer
		}
		keys = append(keys, member)
	}
	writeJSON(w, map[string]any{"keys": keys})
}

// entraToken is one access token's header and claims, from the recorded
// claim shapes.
type entraToken struct {
	version  string
	kid      string
	key      *rsa.PrivateKey
	tenant   string
	object   string
	audience string
	issuer   string
	issuedAt time.Time
	lifetime time.Duration
}

func (fake *entraFake) token(version string) entraToken {
	issuer := fake.shape.v1Issuer
	if version == "2.0" {
		issuer = fake.shape.v2Issuer
	}
	return entraToken{version: version, kid: "PLACEHOLDER-KID-1", key: fake.keys["PLACEHOLDER-KID-1"], tenant: entraTenant, object: entraObject, audience: entraServerID,
		issuer: strings.ReplaceAll(issuer, "{tid}", entraTenant), issuedAt: fake.clock.Now(), lifetime: 65 * time.Minute}
}

func (fake *entraFake) sign(token entraToken) string {
	fake.t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: token.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", token.kid))
	if err != nil {
		fake.t.Fatal(err)
	}
	claims := map[string]any{"aud": token.audience, "iss": token.issuer, "iat": token.issuedAt.Unix(), "nbf": token.issuedAt.Unix(), "exp": token.issuedAt.Add(token.lifetime).Unix(),
		"aio": "opaque-aio", "appid": "bbbbbbbb-0000-4000-8000-000000000002", "appidacr": "1", "idtyp": "app", "oid": token.object, "sub": token.object, "tid": token.tenant, "uti": "opaque-uti", "ver": token.version}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		fake.t.Fatal(err)
	}
	return "Bearer " + raw
}

func (fake *entraFake) verifier(tenant string) *entraTokenVerifier {
	return newEntraTokenVerifier(entraCloud{authority: fake.authority.URL, keyHosts: []string{fake.host(fake.authority), fake.host(fake.partner)}}, tenant, entraServerID, fake.client, fake.clock.Now, 2*time.Minute, nil)
}

func entraPrincipal(tenant, object string) string {
	encoded, _ := json.Marshal([]string{"verified-entra-id", tenant, object})
	return string(encoded)
}

// Each cloud and token version proves its token with the cloud's own
// documented issuers; a key URL off the cloud's list is never fetched.
func TestEntraProofPerCloudAndTokenVersion(t *testing.T) {
	for _, cloud := range []string{"public", "government", "china"} {
		for _, version := range []string{"1.0", "2.0"} {
			t.Run(cloud+"/v"+version, func(t *testing.T) {
				fake := newEntraFake(t, cloud)
				principal, outcome, err := fake.verifier(entraTenant).principal(t.Context(), fake.sign(fake.token(version)))
				if err != nil || outcome != proofProven || principal != entraPrincipal(entraTenant, entraObject) {
					t.Fatalf("principal=%s outcome=%d err=%v", principal, outcome, err)
				}
				// Versus builds the metadata URL itself.
				want := "/" + entraTenant + "/.well-known/openid-configuration"
				if version == "2.0" {
					want = "/" + entraTenant + "/v2.0/.well-known/openid-configuration"
				}
				fake.mu.Lock()
				first := fake.paths[0]
				fake.mu.Unlock()
				if first != want {
					t.Fatalf("metadata read from %s, want %s", first, want)
				}
			})
		}
	}
	for name, override := range map[string]func(*entraFake) string{
		"off-list host": func(fake *entraFake) string { return fake.offList.URL + "/keys" },
		"plain http":    func(fake *entraFake) string { return strings.Replace(fake.authority.URL, "https", "http", 1) + "/keys" },
		"user info": func(fake *entraFake) string {
			return strings.Replace(fake.authority.URL, "https://", "https://user@", 1) + "/keys"
		},
		"other port": func(fake *entraFake) string { return fake.authority.URL + "0/keys" },
	} {
		t.Run(name, func(t *testing.T) {
			fake := newEntraFake(t, "public")
			fake.jwksOverride = override(fake)
			_, outcome, err := fake.verifier(entraTenant).principal(t.Context(), fake.sign(fake.token("1.0")))
			var failure *proofError
			if outcome != proofUnavailable || !errors.As(err, &failure) || failure.class != "jwks_host_not_allowed" || failure.Code() != "proof_host_not_allowed" {
				t.Fatalf("an off-list key URL = %d %v", outcome, err)
			}
			if fake.offListHit.Load() != 0 {
				t.Fatal("an off-list key URL was fetched")
			}
		})
	}
}

// The tenant pin is a GUID: a configured domain pins the GUID in its own
// metadata's issuer; common, organizations and consumers have no pin; a
// managed identity pins the first fully verified token's tid and refuses
// another tenant after that, and a non-GUID tid never reaches a URL.
func TestEntraTenantPin(t *testing.T) {
	t.Run("domain", func(t *testing.T) {
		fake := newEntraFake(t, "public")
		verifier := fake.verifier("contoso.onmicrosoft.com")
		if principal, outcome, err := verifier.principal(t.Context(), fake.sign(fake.token("1.0"))); outcome != proofProven || principal != entraPrincipal(entraTenant, entraObject) {
			t.Fatalf("a domain tenant was not pinned to its GUID: %d %v", outcome, err)
		}
	})
	for _, tenant := range []string{"common", "organizations", "consumers"} {
		t.Run(tenant, func(t *testing.T) {
			fake := newEntraFake(t, "public")
			if _, outcome, _ := fake.verifier(tenant).principal(t.Context(), fake.sign(fake.token("1.0"))); outcome != proofNotApplicable {
				t.Fatalf("tenant %s proved a token: %d", tenant, outcome)
			}
		})
	}
	t.Run("managed identity", func(t *testing.T) {
		fake := newEntraFake(t, "public")
		verifier := fake.verifier("")
		other := fake.token("1.0")
		other.tenant, other.issuer = entraOtherTenant, strings.ReplaceAll(fake.shape.v1Issuer, "{tid}", entraOtherTenant)
		// An unverified token's tid does not pin anything.
		forged := other
		forged.key = entraKeyPair(t, 2048, 9)
		if _, outcome, _ := verifier.principal(t.Context(), fake.sign(forged)); outcome == proofProven || verifier.pin != "" {
			t.Fatal("an unverified token pinned the tenant")
		}
		if _, outcome, _ := verifier.principal(t.Context(), fake.sign(fake.token("1.0"))); outcome != proofProven || verifier.pin != entraTenant {
			t.Fatalf("the first verified token did not pin its tenant: %d", outcome)
		}
		if _, outcome, _ := verifier.principal(t.Context(), fake.sign(other)); outcome != proofNotApplicable {
			t.Fatalf("a token from another tenant was proven after the pin: %d", outcome)
		}
		fresh := fake.verifier("")
		badTenant := fake.token("1.0")
		badTenant.tenant = "../../evil"
		fake.mu.Lock()
		before := len(fake.paths)
		fake.mu.Unlock()
		if _, outcome, _ := fresh.principal(t.Context(), fake.sign(badTenant)); outcome != proofNotApplicable {
			t.Fatal("a non-GUID tid was used")
		}
		fake.mu.Lock()
		after := len(fake.paths)
		fake.mu.Unlock()
		if after != before {
			t.Fatal("a non-GUID tid reached a URL")
		}
	})
}

// Every check refuses a token it does not match, as a definite answer: not
// proven, and never a cluster 401.
func TestEntraProofRefusesTokensThatFailACheck(t *testing.T) {
	fake := newEntraFake(t, "public")
	weak := entraKeyPair(t, 1024)
	fake.keys["WEAK-KID"] = weak
	cases := map[string]func() string{
		"wrong signature": func() string {
			token := fake.token("1.0")
			token.key = entraKeyPair(t, 2048, 2)
			return fake.sign(token)
		},
		"wrong audience": func() string {
			token := fake.token("1.0")
			token.audience = "api://other-server"
			return fake.sign(token)
		},
		"wrong issuer": func() string {
			token := fake.token("1.0")
			token.issuer = "https://sts.windows.net/" + entraOtherTenant + "/"
			return fake.sign(token)
		},
		"v2 issuer on v1": func() string {
			token := fake.token("1.0")
			token.issuer = "https://login.microsoftonline.com/" + entraTenant + "/v2.0"
			return fake.sign(token)
		},
		"wrong tenant": func() string { token := fake.token("1.0"); token.tenant = entraOtherTenant; return fake.sign(token) },
		"non-GUID oid": func() string { token := fake.token("1.0"); token.object = "not-a-guid"; return fake.sign(token) },
		"expired": func() string {
			token := fake.token("1.0")
			token.issuedAt = fake.clock.Now().Add(-2 * time.Hour)
			return fake.sign(token)
		},
		"weak key": func() string {
			token := fake.token("1.0")
			token.kid, token.key = "WEAK-KID", weak
			return fake.sign(token)
		},
		"alg none": func() string {
			header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"PLACEHOLDER-KID-1"}`))
			claims := base64.RawURLEncoding.EncodeToString([]byte(`{"ver":"1.0","tid":"` + entraTenant + `","oid":"` + entraObject + `"}`))
			return "Bearer " + header + "." + claims + "."
		},
		"HS256": func() string {
			signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte("a-shared-secret-of-thirty-two-bytes")}, (&jose.SignerOptions{}).WithHeader("kid", "PLACEHOLDER-KID-1"))
			raw, _ := jwt.Signed(signer).Claims(map[string]any{"ver": "1.0", "tid": entraTenant, "oid": entraObject, "aud": entraServerID}).Serialize()
			return "Bearer " + raw
		},
		"unknown version": func() string { token := fake.token("3.0"); return fake.sign(token) },
		// A token that fails a definite check is refused even when its
		// lifetime claims would read as clock skew.
		"wrong audience, not valid yet": func() string {
			token := fake.token("1.0")
			token.audience, token.issuedAt = "api://other-server", fake.clock.Now().Add(30*time.Minute)
			return fake.sign(token)
		},
		"wrong tenant, not valid yet": func() string {
			token := fake.token("1.0")
			token.tenant, token.issuedAt = entraOtherTenant, fake.clock.Now().Add(30*time.Minute)
			return fake.sign(token)
		},
		"non-GUID oid, not valid yet": func() string {
			token := fake.token("1.0")
			token.object, token.issuedAt = "not-a-guid", fake.clock.Now().Add(30*time.Minute)
			return fake.sign(token)
		},
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			principal, outcome, err := fake.verifier(entraTenant).principal(t.Context(), header())
			if outcome != proofNotApplicable || principal != "" || errors.Is(err, ErrUnauthorized) {
				t.Fatalf("outcome=%d principal=%q err=%v", outcome, principal, err)
			}
		})
	}
	t.Run("key issuer member", func(t *testing.T) {
		fake := newEntraFake(t, "public")
		fake.keyIssuer = "https://login.microsoftonline.com/" + entraOtherTenant + "/v2.0"
		if _, outcome, _ := fake.verifier(entraTenant).principal(t.Context(), fake.sign(fake.token("2.0"))); outcome != proofNotApplicable {
			t.Fatalf("a key whose issuer member differs proved a token: %d", outcome)
		}
	})
	t.Run("unknown kid", func(t *testing.T) {
		token := fake.token("1.0")
		token.kid = "NOT-PUBLISHED"
		if _, outcome, _ := fake.verifier(entraTenant).principal(t.Context(), fake.sign(token)); outcome == proofProven {
			t.Fatal("a token with an unpublished key ID was proven")
		}
	})
}

// A new key ID served a minute late is proven within the refetch backoff;
// a removed key stops proving after the cache TTL.
func TestEntraKeyRotation(t *testing.T) {
	fake := newEntraFake(t, "public")
	verifier := fake.verifier(entraTenant)
	if _, outcome, _ := verifier.principal(t.Context(), fake.sign(fake.token("1.0"))); outcome != proofProven {
		t.Fatal("the first key did not prove")
	}
	rotated := fake.token("1.0")
	rotated.kid, rotated.key = "PLACEHOLDER-KID-2", entraKeyPair(t, 2048, 3)
	header := fake.sign(rotated)
	fetches := func() int {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		n := 0
		for _, path := range fake.paths {
			if strings.HasSuffix(path, "/keys") {
				n++
			}
		}
		return n
	}
	start := fake.clock.Now()
	proven := false
	for fake.clock.Now().Sub(start) < 3*time.Minute {
		if fake.clock.Now().Sub(start) >= time.Minute {
			fake.mu.Lock()
			fake.keys["PLACEHOLDER-KID-2"] = rotated.key
			fake.mu.Unlock()
		}
		if _, outcome, _ := verifier.principal(t.Context(), header); outcome == proofProven {
			proven = true
			break
		}
		fake.clock.advance(time.Second)
	}
	if !proven || fake.clock.Now().Sub(start) > time.Minute+entraKeyRefetch[len(entraKeyRefetch)-1] {
		t.Fatalf("a key published a minute late was not proven within the backoff (after %s)", fake.clock.Now().Sub(start))
	}
	if n := fetches(); n > 6 {
		t.Fatalf("an unknown key ID refetched %d times in a minute", n)
	}
	// The first key is removed: cached keys still prove until the TTL.
	old := fake.token("1.0")
	fake.mu.Lock()
	delete(fake.keys, "PLACEHOLDER-KID-1")
	fake.mu.Unlock()
	if _, outcome, _ := verifier.principal(t.Context(), fake.sign(old)); outcome != proofProven {
		t.Fatal("a cached key stopped proving before its TTL")
	}
	fake.clock.advance(entraCacheTTL)
	old.issuedAt = fake.clock.Now()
	if _, outcome, _ := verifier.principal(t.Context(), fake.sign(old)); outcome == proofProven {
		t.Fatal("a removed key still proved after the TTL")
	}
}

// A clock three minutes off proves; ten minutes off is unavailable, not a
// refusal, so nothing retires.
func TestEntraProofClockSkew(t *testing.T) {
	for _, test := range []struct {
		name    string
		skew    time.Duration
		outcome proofOutcome
	}{
		{"three minutes behind", -3 * time.Minute, proofProven},
		{"three minutes ahead", 3 * time.Minute, proofProven},
		{"ten minutes behind", -10 * time.Minute, proofUnavailable},
		{"ten minutes ahead", 10 * time.Minute, proofUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newEntraFake(t, "public")
			token := fake.token("1.0")
			// The token was issued a minute before it is checked and lives
			// five minutes. The token endpoint reported the same lifetime,
			// recorded on this process's skewed clock.
			token.issuedAt = fake.clock.Now().Add(-time.Minute)
			token.lifetime = 5 * time.Minute
			header := fake.sign(token)
			verifier := fake.verifier(entraTenant)
			recorded := token.issuedAt.Add(token.lifetime).Add(test.skew)
			verifier.expiry = func(string) time.Time { return recorded }
			fake.clock.advance(test.skew)
			_, outcome, err := verifier.principal(t.Context(), header)
			if outcome != test.outcome || errors.Is(err, ErrUnauthorized) {
				t.Fatalf("outcome=%d err=%v", outcome, err)
			}
		})
	}
}

// A key endpoint that fails, redirects, answers too much or hangs is
// unavailable.
func TestEntraKeyEndpointFailuresAreUnavailable(t *testing.T) {
	for _, answer := range []string{"500", "redirect", "oversized", "hang"} {
		t.Run(answer, func(t *testing.T) {
			fake := newEntraFake(t, "public")
			fake.keysAnswer = answer
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			_, outcome, err := fake.verifier(entraTenant).principal(ctx, fake.sign(fake.token("1.0")))
			if outcome != proofUnavailable || err == nil || errors.Is(err, ErrUnauthorized) {
				t.Fatalf("outcome=%d err=%v", outcome, err)
			}
			if fake.offListHit.Load() != 0 {
				t.Fatal("a redirect was followed")
			}
		})
	}
}

// With identity review denied, 100 refreshes of one Entra identity keep one
// epoch; a different object or tenant retires it.
func TestAKSProofKeepsOneEpochAcrossRefreshes(t *testing.T) {
	fake := newEntraFake(t, "public")
	cluster := newProofCluster(t)
	cluster.setReview(reviewDown(http.StatusForbidden))
	service := newTestService(t, cluster.server.URL, Scope{OrgID: "default", ClusterID: "aks", CredentialID: "configured"})
	var next atomic.Value
	mint := func(object, tenant string) string {
		token := entraToken{version: "1.0", kid: "PLACEHOLDER-KID-1", key: fake.keys["PLACEHOLDER-KID-1"], tenant: tenant, object: object, audience: entraServerID,
			issuer: strings.ReplaceAll(fake.shape.v1Issuer, "{tid}", tenant), issuedAt: time.Now(), lifetime: time.Hour}
		return strings.TrimPrefix(fake.sign(token), "Bearer ")
	}
	next.Store(mint(entraObject, entraTenant))
	base := &expiringBearerSource{now: time.Now, skew: 2 * time.Minute, refresh: func(context.Context) (string, time.Time, error) {
		return next.Load().(string), time.Now().Add(time.Hour), nil
	}}
	verifier := newEntraTokenVerifier(entraCloud{authority: fake.authority.URL, keyHosts: []string{fake.host(fake.authority)}}, entraTenant, entraServerID, fake.client, time.Now, 2*time.Minute, base.credentialExpiry)
	service.client.credentials = &aksCredentialSource{expiringBearerSource: base, proof: verifier.principal, host: verifier.host()}
	rotate := func(token string) {
		current, _ := base.Authorization(t.Context())
		next.Store(token)
		base.rejectBearer(current)
	}
	if err := service.validateCredential(t.Context()); err != nil || service.identity.source != principalProvider {
		t.Fatalf("the AKS proof did not start the epoch: %v", err)
	}
	key := service.captureCredentialID()
	for range 100 {
		rotate(mint(entraObject, entraTenant))
		if err := validateRenewal(t, service); err != nil {
			t.Fatalf("a refresh of the same identity retired the epoch: %v", err)
		}
	}
	if service.identity.ctx.Err() != nil || service.captureCredentialID() != key {
		t.Fatal("refreshes changed the epoch")
	}
	for name, token := range map[string]string{"object": mint(entraOtherObject, entraTenant), "tenant": mint(entraObject, entraOtherTenant)} {
		t.Run(name, func(t *testing.T) {
			rotate(mint(entraObject, entraTenant))
			changed := newService(t.Context(), service.client, service.Scope(), time.Minute)
			if !changed.identity.known {
				t.Fatal("the successor did not prove")
			}
			rotate(token)
			if err := validateRenewal(t, changed); err == nil || changed.identity.ctx.Err() == nil {
				t.Fatalf("a different %s kept the epoch: %v", name, err)
			}
		})
	}
}

// A token long expired by its claims is not read as clock skew just because
// a positive expires_in was recorded for it: a replayed token is not proven.
func TestEntraProofRefusesAReplayedExpiredToken(t *testing.T) {
	fake := newEntraFake(t, "public")
	token := fake.token("1.0")
	token.issuedAt = fake.clock.Now().Add(-2 * time.Hour)
	verifier := fake.verifier(entraTenant)
	verifier.expiry = func(string) time.Time { return fake.clock.Now().Add(time.Hour) }
	if _, outcome, err := verifier.principal(t.Context(), fake.sign(token)); outcome != proofNotApplicable || err != nil {
		t.Fatalf("a replayed expired token = %d %v, want not proven", outcome, err)
	}
}
