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

// awsCaller is the identity a synthetic presigned token was signed for.
type awsCaller struct {
	account, role, user, session, group string
}

func (caller awsCaller) values() url.Values {
	return url.Values{"account": {caller.account}, "role": {caller.role}, "user": {caller.user}, "session": {caller.session}, "group": {caller.group}}
}

func callerFromValues(query url.Values) awsCaller {
	return awsCaller{account: query.Get("account"), role: query.Get("role"), user: query.Get("user"), session: query.Get("session"), group: query.Get("group")}
}

// arn and uniqueID are what STS reports for the signer: an assumed-role
// session, or an IAM user.
func (caller awsCaller) arn() string {
	if caller.user != "" {
		return "arn:aws:iam::" + caller.account + ":user/" + caller.user
	}
	return "arn:aws:sts::" + caller.account + ":assumed-role/" + caller.role + "/" + caller.session
}

func (caller awsCaller) uniqueID() string {
	if caller.user != "" {
		return "AIDA" + strings.ToUpper(caller.user)
	}
	return "AROA" + strings.ToUpper(caller.role) + ":" + caller.session
}

// callerSTS answers presigned GetCallerIdentity requests for roles and users.
type callerSTS struct {
	server *httptest.Server
	calls  atomic.Int32
}

func newCallerSTS(t *testing.T, cluster string) *callerSTS {
	t.Helper()
	fake := &callerSTS{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.calls.Add(1)
		query := r.URL.Query()
		if r.Method != http.MethodGet || query.Get("Action") != "GetCallerIdentity" || query.Get("X-Amz-Signature") == "" || r.Header.Get("x-k8s-aws-id") != cluster {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		caller := callerFromValues(query)
		_ = json.NewEncoder(w).Encode(map[string]any{"GetCallerIdentityResponse": map[string]any{"GetCallerIdentityResult": map[string]string{
			"Account": caller.account, "Arn": caller.arn(), "UserId": caller.uniqueID(),
		}}})
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func encodeEKSToken(target string) string {
	return eksTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(target))
}

// bearer is a signed token for the caller. Each STS session signs different
// bytes, as a real 14-minute renewal does.
func (fake *callerSTS) bearer(caller awsCaller) string {
	query := caller.values()
	query.Set("Action", "GetCallerIdentity")
	query.Set("Version", "2011-06-15")
	query.Set("X-Amz-Signature", "sig-"+caller.account+"-"+caller.role+caller.user+"-"+caller.session)
	return "Bearer " + encodeEKSToken(fake.server.URL+"/?"+query.Encode())
}

// eksReviewAPI answers SelfSubjectReview the way the AWS IAM authenticator
// reports an EKS identity, including the fields that change every session.
// With review disabled it denies the review, as clusters that forbid it do.
func eksReviewAPI(t *testing.T, review bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !review || r.URL.Path != "/apis/authentication.k8s.io/v1/selfsubjectreviews" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "+eksTokenPrefix))
		target, parseErr := url.Parse(string(decoded))
		if err != nil || parseErr != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		caller := callerFromValues(target.Query())
		canonical := "arn:aws:iam::" + caller.account + ":role/" + caller.role
		username := caller.arn()
		if caller.user != "" {
			canonical = username
		}
		writeJSON(w, map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview", "status": map[string]any{"userInfo": map[string]any{
			"username": username,
			"uid":      "aws-iam-authenticator:" + caller.account + ":" + strings.SplitN(caller.uniqueID(), ":", 2)[0],
			"groups":   []string{caller.group, "system:authenticated"},
			"extra": map[string][]string{
				"accessKeyId":  {"ASIA" + strings.ToUpper(caller.session)},
				"arn":          {caller.arn()},
				"canonicalArn": {canonical},
				"principalId":  {caller.uniqueID()},
				"sessionName":  {caller.session},
				"sigs.k8s.io/aws-iam-authenticator/principalId": {strings.SplitN(caller.uniqueID(), ":", 2)[0]},
			},
		}}})
	}))
	t.Cleanup(server.Close)
	return server
}

func eksEpochService(t *testing.T, review bool) (*Service, *callerSTS) {
	t.Helper()
	sts := newCallerSTS(t, "production")
	service := newTestService(t, eksReviewAPI(t, review).URL, Scope{OrgID: "default", ClusterID: "eks", CredentialID: "eks-configured"})
	service.client.credentials = &eksCredentialSource{expiringBearerSource: &expiringBearerSource{}, identity: newEKSIdentityVerifier(sts.server.URL, "production", sts.server.Client())}
	return service, sts
}

// TestEKSRenewalsKeepTheEpochOnlyForTheSamePrincipal renews an EKS token
// across four STS sessions of one role, with and without Kubernetes identity
// review, then presents one identity change. The sessions keep the credential
// epoch and history principal; every change of role, account, user or group
// retires the epoch, and it stays retired.
func TestEKSRenewalsKeepTheEpochOnlyForTheSamePrincipal(t *testing.T) {
	reader := awsCaller{account: "123456789012", role: "reader", group: "versus-readers"}
	changes := map[string]awsCaller{
		"other role":     {account: "123456789012", role: "writer", group: "versus-readers"},
		"other account":  {account: "210987654321", role: "reader", group: "versus-readers"},
		"iam user":       {account: "123456789012", user: "reader", group: "versus-readers"},
		"other iam user": {account: "123456789012", user: "operator", group: "versus-readers"},
		"renamed role":   {account: "123456789012", role: "reader-v2", group: "versus-readers"},
	}
	// Group membership is visible only through identity review; without it the
	// STS identity alone is the principal.
	reviewedChanges := map[string]awsCaller{"other group": {account: "123456789012", role: "reader", group: "versus-admins"}}
	for name, changed := range changes {
		reviewedChanges[name] = changed
	}
	for _, review := range []bool{false, true} {
		cases, mode := changes, "without review"
		if review {
			cases, mode = reviewedChanges, "with review"
		}
		for name, changed := range cases {
			t.Run(mode+"/"+name, func(t *testing.T) {
				service, sts := eksEpochService(t, review)
				start := reader
				start.session = "aws-go-sdk-1"
				if err := service.acceptCredential(t.Context(), sts.bearer(start)); err != nil {
					t.Fatal(err)
				}
				principal := service.historyPrincipal()
				if principal == "" {
					t.Fatal("verified EKS identity did not bind history")
				}
				if reviewed := service.identity.reviewPrincipal != ""; reviewed != review {
					t.Fatalf("identity review used=%t, want %t", reviewed, review)
				}
				for _, session := range []string{"aws-go-sdk-2", "aws-go-sdk-3", "aws-go-sdk-4"} {
					renewed := reader
					renewed.session = session
					if err := service.acceptCredential(t.Context(), sts.bearer(renewed)); err != nil {
						t.Fatalf("renewal to %s retired the epoch: %v", session, err)
					}
				}
				if service.identity.ctx.Err() != nil || service.historyPrincipal() != principal {
					t.Fatal("same-role renewals replaced the credential epoch or history principal")
				}
				changed.session = "aws-go-sdk-5"
				if err := service.acceptCredential(t.Context(), sts.bearer(changed)); !errors.Is(err, ErrCredentialUnavailable) {
					t.Fatalf("%s kept the epoch: %v", name, err)
				}
				if service.identity.ctx.Err() == nil {
					t.Fatalf("%s left the old epoch active", name)
				}
				// A retired epoch is not revived by the original principal.
				start.session = "aws-go-sdk-6"
				if err := service.acceptCredential(t.Context(), sts.bearer(start)); !errors.Is(err, ErrCredentialUnavailable) {
					t.Fatalf("retired epoch accepted the original principal again: %v", err)
				}
			})
		}
	}
}

// TestEKSUnprovedTokensDoNotRideAProvedEpoch presents renewals that cannot be
// proved: a token for another host, another STS action, or without a
// signature. None of them reaches STS, none is coalesced into the proved
// principal, and each retires the epoch rather than inherit it.
func TestEKSUnprovedTokensDoNotRideAProvedEpoch(t *testing.T) {
	reader := awsCaller{account: "123456789012", role: "reader", session: "aws-go-sdk-1"}
	for name, target := range map[string]func(sts *callerSTS) string{
		"other host": func(*callerSTS) string {
			return "https://sts.attacker.invalid/?" + reader.values().Encode() + "&Action=GetCallerIdentity&X-Amz-Signature=x"
		},
		"other action": func(sts *callerSTS) string {
			return sts.server.URL + "/?" + reader.values().Encode() + "&Action=AssumeRole&X-Amz-Signature=x"
		},
		"unsigned": func(sts *callerSTS) string {
			return sts.server.URL + "/?" + reader.values().Encode() + "&Action=GetCallerIdentity"
		},
		"other path": func(sts *callerSTS) string {
			return sts.server.URL + "/redirect?" + reader.values().Encode() + "&Action=GetCallerIdentity&X-Amz-Signature=x"
		},
		"embedded credentials": func(sts *callerSTS) string {
			return strings.Replace(sts.server.URL, "://", "://user:pass@", 1) + "/?" + reader.values().Encode() + "&Action=GetCallerIdentity&X-Amz-Signature=x"
		},
	} {
		t.Run(name, func(t *testing.T) {
			service, sts := eksEpochService(t, false)
			if err := service.acceptCredential(t.Context(), sts.bearer(reader)); err != nil {
				t.Fatal(err)
			}
			if service.historyPrincipal() == "" {
				t.Fatal("verified EKS identity did not bind history")
			}
			before := sts.calls.Load()
			unproved := "Bearer " + encodeEKSToken(target(sts))
			if principal, err := service.client.credentials.(*eksCredentialSource).verifiedCredentialPrincipal(t.Context(), unproved); err != nil || principal != "" {
				t.Fatalf("unproved token proved %q err=%v", principal, err)
			}
			if err := service.acceptCredential(t.Context(), unproved); !errors.Is(err, ErrCredentialUnavailable) {
				t.Fatalf("an unproved renewal kept the proved epoch: %v", err)
			}
			if service.identity.ctx.Err() == nil {
				t.Fatal("an unproved renewal left the proved epoch active")
			}
			if calls := sts.calls.Load(); calls != before {
				t.Fatalf("an unproved token reached STS: %d calls", calls-before)
			}
		})
	}
}

// TestEKSUnprovedTokensStayByteBound starts from a token that cannot be
// proved. The same bytes keep the epoch; any other bytes retire it, because
// nothing proves they belong to the same principal.
func TestEKSUnprovedTokensStayByteBound(t *testing.T) {
	service, sts := eksEpochService(t, false)
	elsewhere := func(session string) string {
		return "Bearer " + encodeEKSToken("https://sts.attacker.invalid/?Action=GetCallerIdentity&X-Amz-Signature="+session)
	}
	for i := 0; i < 3; i++ {
		if err := service.acceptCredential(t.Context(), elsewhere("s1")); err != nil {
			t.Fatalf("identical unproved token %d retired the epoch: %v", i, err)
		}
	}
	if err := service.acceptCredential(t.Context(), elsewhere("s2")); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("different unproved bytes kept the epoch: %v", err)
	}
	if sts.calls.Load() != 0 {
		t.Fatalf("unproved tokens reached STS: %d calls", sts.calls.Load())
	}
}
