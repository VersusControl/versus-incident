package kubernetes

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

type countingTokenSource struct {
	mu    sync.Mutex
	calls int
	token *oauth2.Token
	err   error
}

func (source *countingTokenSource) Token(context.Context) (*oauth2.Token, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.calls++
	return source.token, source.err
}

func TestGKECredentialCacheAndRefresh(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	provider := &countingTokenSource{token: &oauth2.Token{AccessToken: "adc-token", Expiry: now.Add(time.Hour)}}
	source := newGKESource(provider, func() time.Time { return now })
	for index := 0; index < 8; index++ {
		header, err := source.Authorization(context.Background())
		if err != nil || header != "Bearer adc-token" {
			t.Fatalf("header = %q, err = %v", header, err)
		}
	}
	if provider.calls != 1 {
		t.Fatalf("token source calls = %d", provider.calls)
	}
}

func TestGKECredentialErrorIsRedacted(t *testing.T) {
	provider := &countingTokenSource{err: errors.New("service-account-secret")}
	source := newGKESource(provider, time.Now)
	_, err := source.Authorization(context.Background())
	if err == nil || strings.Contains(err.Error(), "service-account-secret") {
		t.Fatalf("error = %v", err)
	}
}

type contextCheckingTokenSource struct {
	want context.Context
}

func (source contextCheckingTokenSource) Token(ctx context.Context) (*oauth2.Token, error) {
	if ctx != source.want {
		return nil, errors.New("caller context was not forwarded")
	}
	return &oauth2.Token{AccessToken: "context-token", Expiry: time.Now().Add(time.Hour)}, nil
}

func TestGKESourceForwardsCallerContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), struct{}{}, "caller")
	source := newGKESource(contextCheckingTokenSource{want: ctx}, time.Now)
	if _, err := source.Authorization(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestGKECredentialEndpointsAreClosed(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want bool
	}{
		{"service account", `{"type":"service_account","token_uri":"https://oauth2.googleapis.com/token"}`, true},
		{"external account", `{"type":"external_account","token_url":"https://sts.googleapis.com/v1/token","credential_source":{"file":"/var/run/secrets/google/token"}}`, true},
		{"external account impersonation", `{"type":"external_account","token_url":"https://sts.googleapis.com/v1/token","service_account_impersonation_url":"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/reader%40example.iam.gserviceaccount.com:generateAccessToken","credential_source":{"file":"/var/run/secrets/google/token","format":{"type":"text"}}}`, true},
		{"arbitrary service account", `{"type":"service_account","token_uri":"https://attacker.example/token"}`, false},
		{"arbitrary external account", `{"type":"external_account","token_url":"https://attacker.example/token","credential_source":{"file":"/var/run/secrets/google/token"}}`, false},
		{"authorized user", `{"type":"authorized_user","token_uri":"https://oauth2.googleapis.com/token"}`, false},
		{"external authorized user", `{"type":"external_account_authorized_user","token_url":"https://sts.googleapis.com/v1/token"}`, false},
		{"impersonated service account", `{"type":"impersonated_service_account","source_credentials":{"type":"service_account"}}`, false},
		{"unknown type", `{"type":"future_account"}`, false},
		{"external URL source", `{"type":"external_account","token_url":"https://sts.googleapis.com/v1/token","credential_source":{"url":"https://attacker.example/token"}}`, false},
		{"external executable source", `{"type":"external_account","token_url":"https://sts.googleapis.com/v1/token","credential_source":{"executable":{"command":"helper"}}}`, false},
		{"external AWS source", `{"type":"external_account","token_url":"https://sts.googleapis.com/v1/token","credential_source":{"environment_id":"aws1","url":"http://169.254.169.254"}}`, false},
		{"external nested source credentials", `{"type":"external_account","token_url":"https://sts.googleapis.com/v1/token","credential_source":{"file":"/var/run/secrets/google/token","source_credentials":{"type":"authorized_user"}}}`, false},
		{"arbitrary impersonation", `{"type":"external_account","token_url":"https://sts.googleapis.com/v1/token","service_account_impersonation_url":"https://attacker.example/v1/projects/-/serviceAccounts/a:generateAccessToken","credential_source":{"file":"/var/run/secrets/google/token"}}`, false},
		{"impersonation query", `{"type":"external_account","token_url":"https://sts.googleapis.com/v1/token","service_account_impersonation_url":"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/a:generateAccessToken?target=other","credential_source":{"file":"/var/run/secrets/google/token"}}`, false},
		{"impersonation nested path", `{"type":"external_account","token_url":"https://sts.googleapis.com/v1/token","service_account_impersonation_url":"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/a/b:generateAccessToken","credential_source":{"file":"/var/run/secrets/google/token"}}`, false},
		{"invalid JSON", `{`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, got := validGoogleCredentialEndpoints([]byte(test.body))
			if got != test.want {
				t.Fatalf("valid = %v, want %v", got, test.want)
			}
		})
	}
}

func TestGKEExecutableEnvironmentCannotEnableExecutableCredentials(t *testing.T) {
	t.Setenv("GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES", "1")
	_, valid := validGoogleCredentialEndpoints([]byte(`{"type":"external_account","token_url":"https://sts.googleapis.com/v1/token","credential_source":{"executable":{"command":"helper"}}}`))
	if valid {
		t.Fatal("executable credential source accepted")
	}
}

func TestGKERejectsAmbientApplicationCredentials(t *testing.T) {
	credentialsPath := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(credentialsPath, []byte(`{"type":"service_account","token_uri":"https://oauth2.googleapis.com/token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credentialsPath)
	for _, configured := range []string{"", credentialsPath} {
		if _, err := newGKECredentialSource(configured); err == nil || !strings.Contains(err.Error(), "ambient GKE credentials are unsupported") || strings.Contains(err.Error(), credentialsPath) {
			t.Fatalf("credentials_file=%q error=%v", configured, err)
		}
	}
}

func TestGKERejectsAmbientMetadataHostInEveryCredentialMode(t *testing.T) {
	credentialsPath := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(credentialsPath, []byte(`{"type":"service_account","token_uri":"https://oauth2.googleapis.com/token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	override := "metadata.attacker.example:8080"
	t.Setenv("GCE_METADATA_HOST", override)
	for _, configured := range []string{"", credentialsPath} {
		if _, err := newGKECredentialSource(configured); err == nil || !strings.Contains(err.Error(), "application default credentials unavailable") || strings.Contains(err.Error(), override) {
			t.Fatalf("credentials_file=%q error=%v", configured, err)
		}
	}
}

func TestGKEExternalAccountReopensSubjectTokenAndChecksPermissions(t *testing.T) {
	directory := t.TempDir()
	firstTarget := filepath.Join(directory, "subject-token-v1")
	if err := os.WriteFile(firstTarget, []byte("first-subject"), 0o600); err != nil {
		t.Fatal(err)
	}
	subjectPath := filepath.Join(directory, "subject-token")
	if err := os.Symlink(firstTarget, subjectPath); err != nil {
		t.Fatal(err)
	}
	credentialsPath := filepath.Join(directory, "credentials.json")
	body := fmt.Sprintf(`{"type":"external_account","audience":"//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/pool/providers/provider","subject_token_type":"urn:ietf:params:oauth:token-type:jwt","token_url":"https://sts.googleapis.com/v1/token","credential_source":{"file":%q,"format":{"type":"text"}}}`, subjectPath)
	if err := os.WriteFile(credentialsPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	seen := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/sts" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if err := request.ParseForm(); err != nil {
			t.Error(err)
		}
		seen = append(seen, request.Form.Get("subject_token"))
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"access_token":"access-token","token_type":"Bearer","expires_in":3600,"issued_token_type":"urn:ietf:params:oauth:token-type:access_token"}`))
	}))
	defer server.Close()
	source := &gkeFileTokenSource{path: credentialsPath, client: server.Client(), endpoints: googleCredentialEndpoints{sts: server.URL + "/sts"}}
	if _, err := source.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	secondTarget := filepath.Join(directory, "subject-token-v2")
	if err := os.WriteFile(secondTarget, []byte("rotated-subject"), 0o600); err != nil {
		t.Fatal(err)
	}
	nextLink := filepath.Join(directory, "subject-token-next")
	if err := os.Symlink(secondTarget, nextLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(nextLink, subjectPath); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "first-subject" || seen[1] != "rotated-subject" {
		t.Fatalf("subject tokens = %v", seen)
	}
	if err := os.WriteFile(secondTarget, []byte(strings.Repeat("x", int(maxTokenBytes)+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Token(context.Background()); err == nil {
		t.Fatal("oversized subject token accepted on refresh")
	}
	if err := os.WriteFile(secondTarget, []byte("rotated-subject"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secondTarget, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Token(context.Background()); err == nil {
		t.Fatal("group/world-writable subject token accepted on refresh")
	}
}

func TestGKEServiceAccountSignsFixedAudienceAndRechecksFile(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	credentialsPath := filepath.Join(t.TempDir(), "service-account.json")
	body, err := json.Marshal(googleServiceAccount{Type: "service_account", PrivateKeyID: "key-id", PrivateKey: string(privateKey), ClientEmail: "reader@example.iam.gserviceaccount.com", TokenURI: googleOAuthTokenURL})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialsPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			t.Error(err)
		}
		parts := strings.Split(request.Form.Get("assertion"), ".")
		if len(parts) != 3 {
			t.Errorf("JWT parts = %d", len(parts))
		} else {
			claimsJSON, decodeErr := base64.RawURLEncoding.DecodeString(parts[1])
			var claims struct {
				Audience string `json:"aud"`
			}
			if decodeErr != nil || json.Unmarshal(claimsJSON, &claims) != nil || claims.Audience != googleOAuthTokenURL {
				t.Errorf("JWT audience = %q, decode error = %v", claims.Audience, decodeErr)
			}
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"access_token":"service-token","token_type":"Bearer","expires_in":3600}`))
	}))
	defer server.Close()
	source := &gkeFileTokenSource{path: credentialsPath, client: server.Client(), endpoints: googleCredentialEndpoints{oauth: server.URL}}
	token, err := source.Token(context.Background())
	if err != nil || token.AccessToken != "service-token" {
		t.Fatalf("token=%+v err=%v", token, err)
	}
	if err := os.Chmod(credentialsPath, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Token(context.Background()); err == nil {
		t.Fatal("group/world-writable service-account file accepted on refresh")
	}
}

func TestGKEMetadataUsesRequiredHeaderAndBoundedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Metadata-Flavor") != "Google" || request.URL.Query().Get("scopes") != googleCloudPlatformScope {
			t.Errorf("metadata request headers=%v query=%v", request.Header, request.URL.Query())
		}
		response.Header().Set("Metadata-Flavor", "Google")
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"access_token":"metadata-token","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer server.Close()
	token, err := (&gkeMetadataTokenSource{client: server.Client(), endpoint: server.URL}).Token(context.Background())
	if err != nil || token.AccessToken != "metadata-token" {
		t.Fatalf("token=%+v err=%v", token, err)
	}
}

func TestGKENativeRequestsRespectCancellation(t *testing.T) {
	serviceAccount := testGoogleServiceAccount(t)
	subjectPath := filepath.Join(t.TempDir(), "subject-token")
	if err := os.WriteFile(subjectPath, []byte("subject-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	externalAccount := googleExternalAccount{
		Audience:         "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/pool/providers/provider",
		SubjectTokenType: "urn:ietf:params:oauth:token-type:jwt",
	}
	externalAccount.CredentialSource.File = subjectPath

	tests := []struct {
		name   string
		invoke func(context.Context, *http.Client, string) (*oauth2.Token, error)
	}{
		{name: "service account OAuth", invoke: func(ctx context.Context, client *http.Client, endpoint string) (*oauth2.Token, error) {
			return serviceAccountToken(ctx, client, endpoint, serviceAccount)
		}},
		{name: "external account STS", invoke: func(ctx context.Context, client *http.Client, endpoint string) (*oauth2.Token, error) {
			return externalAccountToken(ctx, client, googleCredentialEndpoints{sts: endpoint}, externalAccount)
		}},
		{name: "service account impersonation", invoke: func(ctx context.Context, client *http.Client, endpoint string) (*oauth2.Token, error) {
			return impersonatedGoogleToken(ctx, client, endpoint, "sts-token")
		}},
		{name: "metadata", invoke: func(ctx context.Context, client *http.Client, endpoint string) (*oauth2.Token, error) {
			return (&gkeMetadataTokenSource{client: client, endpoint: endpoint}).Token(ctx)
		}},
	}
	for _, test := range tests {
		t.Run(test.name+" canceled before request", func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				requests.Add(1)
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := test.invoke(ctx, server.Client(), server.URL); err == nil {
				t.Fatal("canceled request succeeded")
			}
			if requests.Load() != 0 {
				t.Fatalf("requests = %d", requests.Load())
			}
		})
		t.Run(test.name+" canceled during request", func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				close(started)
				<-release
			}))
			defer server.Close()
			defer close(release)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				_, err := test.invoke(ctx, server.Client(), server.URL)
				done <- err
			}()
			<-started
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("canceled request succeeded")
				}
			case <-time.After(250 * time.Millisecond):
				t.Fatal("canceled request did not return promptly")
			}
		})
	}
}

func TestGKEExternalAccountUsesOneDeadlineAcrossBothHops(t *testing.T) {
	subjectPath := filepath.Join(t.TempDir(), "subject-token")
	if err := os.WriteFile(subjectPath, []byte("subject-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	account := googleExternalAccount{
		Audience:         "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/pool/providers/provider",
		SubjectTokenType: "urn:ietf:params:oauth:token-type:jwt",
	}
	account.CredentialSource.File = subjectPath

	for _, test := range []struct {
		name          string
		callerTimeout time.Duration
		clientTimeout time.Duration
		firstHopDelay time.Duration
		maxElapsed    time.Duration
	}{
		{name: "caller deadline", callerTimeout: 120 * time.Millisecond, clientTimeout: time.Second, firstHopDelay: 60 * time.Millisecond, maxElapsed: 250 * time.Millisecond},
		{name: "client deadline", callerTimeout: time.Second, clientTimeout: 80 * time.Millisecond, firstHopDelay: 20 * time.Millisecond, maxElapsed: 200 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			secondHopStarted := make(chan struct{})
			releaseSecondHop := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/sts":
					select {
					case <-time.After(test.firstHopDelay):
						_, _ = writer.Write([]byte(`{"access_token":"sts-token","token_type":"Bearer","expires_in":3600}`))
					case <-request.Context().Done():
					}
				case "/impersonate":
					close(secondHopStarted)
					<-releaseSecondHop
				default:
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()
			defer close(releaseSecondHop)
			client := server.Client()
			client.Timeout = test.clientTimeout
			account.ServiceAccountImpersonationURL = server.URL + "/impersonate"
			ctx, cancel := context.WithTimeout(context.Background(), test.callerTimeout)
			defer cancel()
			started := time.Now()
			_, err := externalAccountToken(ctx, client, googleCredentialEndpoints{sts: server.URL + "/sts"}, account)
			elapsed := time.Since(started)
			if err == nil {
				t.Fatal("two-hop request unexpectedly succeeded")
			}
			select {
			case <-secondHopStarted:
			default:
				t.Fatal("impersonation hop was not reached")
			}
			if elapsed > test.maxElapsed {
				t.Fatalf("two-hop request took %v, want at most %v", elapsed, test.maxElapsed)
			}
		})
	}
}

func testGoogleServiceAccount(t *testing.T) googleServiceAccount {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return googleServiceAccount{
		PrivateKeyID: "key-id",
		PrivateKey:   string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
		ClientEmail:  "reader@example.iam.gserviceaccount.com",
		TokenURI:     googleOAuthTokenURL,
	}
}

func TestNativeKubernetesAuthenticationDoesNotImportOSExec(t *testing.T) {
	for _, path := range []string{"config_auth.go", "eks_auth.go", "azure_auth.go", "gke_auth.go", "gke_native.go"} {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{
			`"os/exec"`,
			`github.com/aws/aws-sdk-go-v2/config`,
			`credentials/processcreds`,
			`credentials/ssocreds`,
			`golang.org/x/oauth2/google`,
			`golang.org/x/oauth2/google/externalaccount`,
			`google.DefaultTokenSource`,
			`google.CredentialsFromJSON`,
		} {
			if strings.Contains(string(source), forbidden) {
				t.Fatalf("%s contains forbidden credential dependency %q", path, forbidden)
			}
		}
	}
}
