package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// googleServiceAccountAnswer is the documented token-info answer for a
// service-account token that asks only for cloud-platform: no sub, no email.
func googleServiceAccountAnswer(azp string) map[string]any {
	return map[string]any{"azp": azp, "aud": azp, "scope": "https://www.googleapis.com/auth/cloud-platform", "exp": "1795003600", "expires_in": "3599", "access_type": "online"}
}

// googleInvalidToken is the recorded answer for a token token-info cannot
// read.
var googleInvalidToken = map[string]any{"error": "invalid_token", "error_description": "Invalid Value"}

// tokenInfoFake answers token-info per token and records each request.
type tokenInfoFake struct {
	server *httptest.Server
	mu     sync.Mutex
	answer func(token string) (int, string)
	calls  atomic.Int32
	// requests records each request's method, query and body token.
	requests []tokenInfoRequest
}

type tokenInfoRequest struct {
	method, query, contentType, token string
}

func newTokenInfoFake(t *testing.T) *tokenInfoFake {
	t.Helper()
	fake := &tokenInfoFake{}
	fake.answer = func(string) (int, string) {
		return http.StatusOK, mustJSON(googleServiceAccountAnswer("100000000000000000001"))
	}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		fake.mu.Lock()
		fake.requests = append(fake.requests, tokenInfoRequest{method: r.Method, query: r.URL.RawQuery, contentType: r.Header.Get("Content-Type"), token: form.Get("access_token")})
		answer := fake.answer
		fake.mu.Unlock()
		status, payload := answer(form.Get("access_token"))
		switch payload {
		case "redirect":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		case "hang":
			<-r.Context().Done()
			return
		}
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "120")
		}
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *tokenInfoFake) setAnswer(answer func(token string) (int, string)) {
	fake.mu.Lock()
	fake.answer = answer
	fake.mu.Unlock()
}

func (fake *tokenInfoFake) verifier(kind gkeTokenKind) *googleTokenInfoVerifier {
	client := fake.server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &googleTokenInfoVerifier{endpoint: fake.server.URL + "/tokeninfo", client: client, kind: kind}
}

func mustJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

const googleServiceAccountPrincipal = `["verified-google","100000000000000000001"]`

// Whether the proof applies is decided by the credential kind: a
// service-account key, an impersonating external account and the metadata
// server are proved; a federated token never reaches token-info.
func TestGoogleTokenInfoPerCredentialKind(t *testing.T) {
	kinds := map[string]gkeTokenKind{
		"service_account":              googleTokenKind([]byte(`{"type":"service_account"}`)),
		"external_account+impersonate": googleTokenKind([]byte(`{"type":"external_account","service_account_impersonation_url":"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/reader@example.iam.gserviceaccount.com:generateAccessToken"}`)),
		"metadata server":              gkeMetadataToken,
	}
	for name, kind := range kinds {
		t.Run(name, func(t *testing.T) {
			fake := newTokenInfoFake(t)
			principal, outcome, err := fake.verifier(kind).principal(t.Context(), "Bearer ya29.token-value")
			if err != nil || outcome != proofProven || principal != googleServiceAccountPrincipal {
				t.Fatalf("principal=%s outcome=%d err=%v", principal, outcome, err)
			}
		})
	}
	t.Run("external_account without impersonation", func(t *testing.T) {
		fake := newTokenInfoFake(t)
		kind := googleTokenKind([]byte(`{"type":"external_account"}`))
		if _, outcome, _ := fake.verifier(kind).principal(t.Context(), "Bearer federated-token"); outcome != proofNotApplicable || fake.calls.Load() != 0 {
			t.Fatalf("a federated token was sent to token-info: outcome=%d calls=%d", outcome, fake.calls.Load())
		}
	})
}

// Only a well-formed 200 whose azp is a service-account ID, with aud equal
// to azp, a positive expires_in and sub absent or equal to azp, proves a
// token. A 400 invalid_token is a definite answer; anything else is
// unavailable and decides nothing.
func TestGoogleTokenInfoAnswers(t *testing.T) {
	with := func(change func(map[string]any)) string {
		answer := googleServiceAccountAnswer("100000000000000000001")
		change(answer)
		return mustJSON(answer)
	}
	for _, test := range []struct {
		name    string
		status  int
		payload string
		outcome proofOutcome
		class   string
	}{
		{"documented answer without sub", http.StatusOK, mustJSON(googleServiceAccountAnswer("100000000000000000001")), proofProven, ""},
		{"sub equal to azp", http.StatusOK, with(func(a map[string]any) { a["sub"] = "100000000000000000001" }), proofProven, ""},
		{"email never used", http.StatusOK, with(func(a map[string]any) { a["email"] = "reader@example.iam.gserviceaccount.com" }), proofProven, ""},
		{"numeric expires_in", http.StatusOK, with(func(a map[string]any) { a["expires_in"] = 3599 }), proofProven, ""},
		{"sub differs from azp", http.StatusOK, with(func(a map[string]any) { a["sub"] = "100000000000000000002" }), proofNotApplicable, ""},
		{"aud differs from azp", http.StatusOK, with(func(a map[string]any) { a["aud"] = "100000000000000000002" }), proofNotApplicable, ""},
		{"non-numeric azp", http.StatusOK, mustJSON(googleServiceAccountAnswer("1234.apps.googleusercontent.com")), proofNotApplicable, ""},
		{"azp longer than 32 digits", http.StatusOK, mustJSON(googleServiceAccountAnswer(strings.Repeat("1", 33))), proofNotApplicable, ""},
		{"missing azp", http.StatusOK, with(func(a map[string]any) { delete(a, "azp") }), proofNotApplicable, ""},
		{"expired", http.StatusOK, with(func(a map[string]any) { a["expires_in"] = "0" }), proofNotApplicable, ""},
		{"invalid_token", http.StatusBadRequest, mustJSON(googleInvalidToken), proofNotApplicable, ""},
		{"other 400", http.StatusBadRequest, `{"error":"invalid_request","error_description":"Either access_token, id_token, or token_handle required"}`, proofUnavailable, "4xx"},
		{"401", http.StatusUnauthorized, mustJSON(googleInvalidToken), proofUnavailable, "4xx"},
		{"403", http.StatusForbidden, `{}`, proofUnavailable, "4xx"},
		{"404", http.StatusNotFound, `{}`, proofUnavailable, "4xx"},
		{"429", http.StatusTooManyRequests, `{}`, proofUnavailable, "429"},
		{"500", http.StatusInternalServerError, `{}`, proofUnavailable, "5xx"},
		{"redirect", http.StatusFound, "redirect", proofUnavailable, "3xx"},
		{"oversized", http.StatusOK, `{"azp":"1","padding":"` + strings.Repeat("x", googleTokenInfoLimit) + `"}`, proofUnavailable, "invalid_answer"},
		{"not JSON", http.StatusOK, `<html>ok</html>`, proofUnavailable, "invalid_answer"},
		{"JSON array", http.StatusOK, `[]`, proofUnavailable, "invalid_answer"},
		{"timeout", http.StatusOK, "hang", proofUnavailable, "timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newTokenInfoFake(t)
			fake.setAnswer(func(string) (int, string) { return test.status, test.payload })
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			principal, outcome, err := fake.verifier(gkeServiceAccountToken).principal(ctx, "Bearer ya29.token-value")
			if outcome != test.outcome || errors.Is(err, ErrUnauthorized) {
				t.Fatalf("outcome=%d err=%v, want %d", outcome, err, test.outcome)
			}
			if test.outcome == proofProven && principal != googleServiceAccountPrincipal {
				t.Fatalf("principal = %s", principal)
			}
			var failure *proofError
			if test.class != "" && (!errors.As(err, &failure) || failure.class != test.class) {
				t.Fatalf("class = %v, want %s", err, test.class)
			}
			if test.status == http.StatusTooManyRequests && failure.retryAfter != 120*time.Second {
				t.Fatalf("Retry-After lost: %s", failure.retryAfter)
			}
			if test.payload == "redirect" && fake.calls.Load() != 1 {
				t.Fatal("a redirect was followed")
			}
		})
	}
}

// The token travels in a POST body, never in the URL.
func TestGoogleTokenIsSentInTheBody(t *testing.T) {
	fake := newTokenInfoFake(t)
	if _, outcome, _ := fake.verifier(gkeServiceAccountToken).principal(t.Context(), "Bearer ya29.token-value"); outcome != proofProven {
		t.Fatal("the token was not proven")
	}
	fake.mu.Lock()
	request := fake.requests[0]
	fake.mu.Unlock()
	if request.method != http.MethodPost || request.query != "" || request.token != "ya29.token-value" || request.contentType != "application/x-www-form-urlencoded" {
		t.Fatalf("token-info request = %+v", request)
	}
}

// gkeProofService connects a GKE source with the token-info proof to a
// cluster that denies identity review.
func gkeProofService(t *testing.T, fake *tokenInfoFake, kind gkeTokenKind) (*Service, func(string)) {
	t.Helper()
	cluster := newProofCluster(t)
	cluster.setReview(reviewDown(http.StatusForbidden))
	service := newTestService(t, cluster.server.URL, Scope{OrgID: "default", ClusterID: "gke", CredentialID: "configured"})
	var next atomic.Value
	next.Store("token-0")
	base := &expiringBearerSource{now: time.Now, skew: 2 * time.Minute, refresh: func(context.Context) (string, time.Time, error) {
		return next.Load().(string), time.Now().Add(time.Hour), nil
	}}
	service.client.credentials = &gkeCredentialSource{expiringBearerSource: base, proof: fake.verifier(kind).principal, host: "oauth2.googleapis.com"}
	rotate := func(token string) {
		current, _ := base.Authorization(t.Context())
		next.Store(token)
		base.rejectBearer(current)
	}
	return service, rotate
}

// With review denied, 100 refreshes of one service account keep one epoch;
// a different azp retires it, and so does a 400 after the epoch was proven.
func TestGKEProofKeepsOneEpochAcrossRefreshes(t *testing.T) {
	fake := newTokenInfoFake(t)
	service, rotate := gkeProofService(t, fake, gkeServiceAccountToken)
	if err := service.validateCredential(t.Context()); err != nil || service.identity.source != principalProvider {
		t.Fatalf("the GKE proof did not start the epoch: %v", err)
	}
	key := service.captureCredentialID()
	for n := 1; n <= 100; n++ {
		rotate(fmt.Sprintf("token-%d", n))
		if err := validateRenewal(t, service); err != nil {
			t.Fatalf("refresh %d retired the epoch: %v", n, err)
		}
	}
	if service.identity.ctx.Err() != nil || service.captureCredentialID() != key {
		t.Fatal("refreshes changed the epoch")
	}
	for name, answer := range map[string]string{
		"different azp":       mustJSON(googleServiceAccountAnswer("100000000000000000002")),
		"400 after the proof": mustJSON(googleInvalidToken),
	} {
		t.Run(name, func(t *testing.T) {
			fake.setAnswer(func(string) (int, string) {
				return http.StatusOK, mustJSON(googleServiceAccountAnswer("100000000000000000001"))
			})
			// Each case starts its own epoch on a fresh token, whichever
			// case ran before it.
			rotate("start-" + strings.ReplaceAll(name, " ", "-"))
			current := newService(t.Context(), service.client, service.Scope(), time.Minute)
			if !current.identity.known {
				t.Fatal("the successor did not prove")
			}
			status := http.StatusOK
			if strings.Contains(answer, "invalid_token") {
				status = http.StatusBadRequest
			}
			fake.setAnswer(func(string) (int, string) { return status, answer })
			rotate("changed-" + strings.ReplaceAll(name, " ", "-"))
			if err := validateRenewal(t, current); err == nil || current.identity.ctx.Err() == nil {
				t.Fatalf("%s kept the epoch: %v", name, err)
			}
		})
	}
}

// Token-info throttled, failing or timing out on a renewal is unavailable:
// the previous token serves and nothing retires.
func TestGKEProofOutageAtARenewalNeverRetires(t *testing.T) {
	for name, answer := range map[string]func(string) (int, string){
		"429":     func(string) (int, string) { return http.StatusTooManyRequests, `{}` },
		"500":     func(string) (int, string) { return http.StatusInternalServerError, `{}` },
		"timeout": func(string) (int, string) { return http.StatusOK, "hang" },
	} {
		t.Run(name, func(t *testing.T) {
			fake := newTokenInfoFake(t)
			service, rotate := gkeProofService(t, fake, gkeServiceAccountToken)
			if err := service.validateCredential(t.Context()); err != nil {
				t.Fatal(err)
			}
			fake.setAnswer(answer)
			rotate("renewed")
			if err := validateRenewal(t, service); err != nil || service.identity.ctx.Err() != nil {
				t.Fatalf("a token-info outage retired the epoch: %v", err)
			}
		})
	}
}

// Metadata server: a 400 on a never-proven epoch makes that token not
// applicable; a later token answered 200 is proven, in its own epoch.
func TestGKEMetadataTokenIsAskedOncePerToken(t *testing.T) {
	fake := newTokenInfoFake(t)
	fake.setAnswer(func(token string) (int, string) {
		if token == "token-0" {
			return http.StatusBadRequest, mustJSON(googleInvalidToken)
		}
		return http.StatusOK, mustJSON(googleServiceAccountAnswer("100000000000000000001"))
	})
	service, rotate := gkeProofService(t, fake, gkeMetadataToken)
	for range 3 {
		if err := service.validateCredential(t.Context()); err != nil {
			t.Fatalf("a 400 on a never-proven epoch failed a request: %v", err)
		}
	}
	if service.identity.known || fake.calls.Load() != 1 {
		t.Fatalf("a 400 was not read as not applicable for that token, asked once: known=%t calls=%d", service.identity.known, fake.calls.Load())
	}
	rotate("token-1")
	if err := service.validateCredential(t.Context()); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("a new token kept the never-proven epoch: %v", err)
	}
	successor := newService(t.Context(), service.client, service.Scope(), time.Minute)
	if !successor.identity.known || successor.identity.source != principalProvider {
		t.Fatal("a later token answered 200 was not proven")
	}
}
