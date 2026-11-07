package kubernetes

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeSTS answers presigned GetCallerIdentity requests with the identity named
// in the synthetic token's query, the way STS answers for the signer.
type fakeSTS struct {
	server *httptest.Server
	calls  atomic.Int32
}

func newFakeSTS(t *testing.T, cluster string) *fakeSTS {
	t.Helper()
	fake := &fakeSTS{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.calls.Add(1)
		query := r.URL.Query()
		if r.Method != http.MethodGet || query.Get("Action") != "GetCallerIdentity" || r.Header.Get("x-k8s-aws-id") != cluster {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		role, session, account := query.Get("role"), query.Get("session"), query.Get("account")
		if account == "" {
			account = "123456789012"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"GetCallerIdentityResponse": map[string]any{"GetCallerIdentityResult": map[string]string{
			"Account": account,
			"Arn":     "arn:aws:sts::" + account + ":assumed-role/" + role + "/" + session,
			"UserId":  "AROA" + strings.ToUpper(role) + ":" + session,
		}}})
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *fakeSTS) token(role, session string, extra url.Values) string {
	query := url.Values{"Action": {"GetCallerIdentity"}, "Version": {"2011-06-15"}, "X-Amz-Signature": {"sig-" + role + "-" + session}, "role": {role}, "session": {session}}
	for key, values := range extra {
		query[key] = values
	}
	return eksTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(fake.server.URL+"/?"+query.Encode()))
}

func (fake *fakeSTS) verifier(cluster string) *eksIdentityVerifier {
	return newEKSIdentityVerifier(fake.server.URL, cluster, fake.server.Client())
}

func TestCanonicalAWSPrincipalRemovesOnlySessionParts(t *testing.T) {
	role, ok := canonicalAWSPrincipal("123456789012", "arn:aws:sts::123456789012:assumed-role/reader/session-1", "AROAREADER:session-1")
	if !ok {
		t.Fatal("assumed role rejected")
	}
	again, _ := canonicalAWSPrincipal("123456789012", "arn:aws:sts::123456789012:assumed-role/reader/session-2", "AROAREADER:session-2")
	if role != again || !strings.Contains(role, "arn:aws:iam::123456789012:role/reader") {
		t.Fatalf("sessions of one role differ: %q %q", role, again)
	}
	for name, input := range map[string][3]string{
		"other role":    {"123456789012", "arn:aws:sts::123456789012:assumed-role/writer/session-1", "AROAWRITER:session-1"},
		"other account": {"210987654321", "arn:aws:sts::210987654321:assumed-role/reader/session-1", "AROAREADER:session-1"},
		"iam user":      {"123456789012", "arn:aws:iam::123456789012:user/reader", "AIDAREADER"},
	} {
		other, ok := canonicalAWSPrincipal(input[0], input[1], input[2])
		if !ok || other == role {
			t.Fatalf("%s collapsed into the role principal: %q", name, other)
		}
	}
	for name, input := range map[string][3]string{
		"bad account":        {"12345", "arn:aws:sts::12345:assumed-role/reader/s", "AROA:s"},
		"account mismatch":   {"123456789012", "arn:aws:sts::210987654321:assumed-role/reader/s", "AROA:s"},
		"not an arn":         {"123456789012", "reader", "AROA:s"},
		"foreign service":    {"123456789012", "arn:aws:s3::123456789012:bucket", "AROA:s"},
		"missing session":    {"123456789012", "arn:aws:sts::123456789012:assumed-role/reader", "AROA"},
		"missing user id":    {"123456789012", "arn:aws:sts::123456789012:assumed-role/reader/s", ""},
		"nested role suffix": {"123456789012", "arn:aws:sts::123456789012:assumed-role/reader/a/b", "AROA:a"},
	} {
		if principal, ok := canonicalAWSPrincipal(input[0], input[1], input[2]); ok {
			t.Fatalf("%s accepted as %q", name, principal)
		}
	}
}

func TestEKSIdentityVerifierProvesTokenAndCachesPerToken(t *testing.T) {
	fake := newFakeSTS(t, "production")
	verifier := fake.verifier("production")
	first, err := verifier.principal(t.Context(), fake.token("reader", "s1", nil))
	if err != nil || first == "" {
		t.Fatalf("principal=%q err=%v", first, err)
	}
	for i := 0; i < 3; i++ {
		if again, _ := verifier.principal(t.Context(), fake.token("reader", "s1", nil)); again != first {
			t.Fatalf("cached principal changed: %q", again)
		}
	}
	if fake.calls.Load() != 1 {
		t.Fatalf("unchanged token re-verified: %d calls", fake.calls.Load())
	}
	renewed, _ := verifier.principal(t.Context(), fake.token("reader", "s2", nil))
	if renewed != first || fake.calls.Load() != 2 {
		t.Fatalf("renewed session principal=%q calls=%d", renewed, fake.calls.Load())
	}
	if other, _ := verifier.principal(t.Context(), fake.token("writer", "s1", nil)); other == "" || other == first {
		t.Fatalf("different role reused principal: %q", other)
	}
}

func TestEKSIdentityVerifierRefusesTokensItCannotProve(t *testing.T) {
	fake := newFakeSTS(t, "production")
	verifier := fake.verifier("production")
	elsewhere := eksTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte("https://attacker.invalid/?Action=GetCallerIdentity&X-Amz-Signature=x"))
	otherAction := eksTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(fake.server.URL+"/?Action=AssumeRole&X-Amz-Signature=x"))
	unsigned := eksTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(fake.server.URL+"/?Action=GetCallerIdentity"))
	for name, token := range map[string]string{
		"other host":    elsewhere,
		"other action":  otherAction,
		"unsigned":      unsigned,
		"not eks":       "opaque-token",
		"bad encoding":  eksTokenPrefix + "%%%",
		"sts rejects":   newFakeSTS(t, "another-cluster").token("reader", "s1", nil),
		"wrong account": fake.token("reader", "s1", url.Values{"account": {"12"}}),
	} {
		principal, err := verifier.principal(t.Context(), token)
		if err != nil || principal != "" {
			t.Fatalf("%s proved %q err=%v", name, principal, err)
		}
	}
	if calls := fake.calls.Load(); calls != 1 {
		t.Fatalf("only the wrong-account token may reach STS; calls=%d", calls)
	}
}

func TestEKSReviewedIdentityIgnoresSessionFields(t *testing.T) {
	user := func(session, key string) reviewedUserInfo {
		return reviewedUserInfo{
			Username: "arn:aws:sts::123456789012:assumed-role/reader/" + session,
			UID:      "aws-iam-authenticator:123456789012:AROAREADER",
			Groups:   []string{"versus-readers"},
			Extra: map[string][]string{
				"accessKeyId":  {key},
				"arn":          {"arn:aws:sts::123456789012:assumed-role/reader/" + session},
				"canonicalArn": {"arn:aws:iam::123456789012:role/reader"},
				"principalId":  {"AROAREADER:" + session},
				"sessionName":  {session},
				"sigs.k8s.io/aws-iam-authenticator/principalId": {"AROAREADER"},
			},
		}
	}
	first, second := user("aws-go-sdk-1", "ASIAONE"), user("aws-go-sdk-2", "ASIATWO")
	normalizeEKSReviewedUser(&first)
	normalizeEKSReviewedUser(&second)
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Fatalf("session refresh changed principal:\n%s\n%s", a, b)
	}
	if first.Extra["canonicalArn"] == nil || first.Groups[0] != "versus-readers" {
		t.Fatal("stable identity fields removed")
	}
	other := user("aws-go-sdk-1", "ASIAONE")
	other.Groups = []string{"versus-admins"}
	normalizeEKSReviewedUser(&other)
	c, _ := json.Marshal(other)
	if string(c) == string(a) {
		t.Fatal("group change hidden by normalization")
	}
	plain := reviewedUserInfo{Username: "alice", Extra: map[string][]string{"sessionName": {"x"}}}
	normalizeEKSReviewedUser(&plain)
	if plain.Extra["sessionName"] == nil {
		t.Fatal("non-EKS identity was normalized")
	}
}

func TestEKSTokenRenewalKeepsEpochWithoutIdentityReview(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Self-subject review is unavailable, as on clusters that deny it.
		w.WriteHeader(http.StatusForbidden)
	}))
	defer api.Close()
	fake := newFakeSTS(t, "production")
	service := newTestService(t, api.URL, Scope{OrgID: "default", ClusterID: "eks"})
	service.client.credentials = &eksCredentialSource{expiringBearerSource: &expiringBearerSource{}, identity: fake.verifier("production")}
	bearer := func(role, session string) string { return "Bearer " + fake.token(role, session, nil) }
	if err := service.acceptCredential(t.Context(), bearer("reader", "s1")); err != nil {
		t.Fatal(err)
	}
	principal := service.historyPrincipal()
	if principal == "" {
		t.Fatal("verified EKS identity did not bind history")
	}
	for i, session := range []string{"s1", "s2", "s3", "s4"} {
		if err := service.acceptCredential(t.Context(), bearer("reader", session)); err != nil {
			t.Fatalf("renewal %d retired the epoch: %v", i, err)
		}
	}
	if service.identity.ctx.Err() != nil || service.historyPrincipal() != principal {
		t.Fatal("same-role renewals replaced the credential epoch")
	}
	if err := service.acceptCredential(t.Context(), bearer("writer", "s1")); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("role change kept the epoch: %v", err)
	}
	if service.identity.ctx.Err() == nil {
		t.Fatal("role change left the old epoch active")
	}
}
