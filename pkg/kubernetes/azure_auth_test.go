package kubernetes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAKSNativeCredentialRequests(t *testing.T) {
	requests := make(chan *http.Request, 3)
	forms := make(chan url.Values, 3)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests <- request.Clone(request.Context())
		_ = request.ParseForm()
		forms <- request.Form
		_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": "native-token", "expires_in": 3600})
	}))
	defer server.Close()

	tokenFile := filepath.Join(t.TempDir(), "federated-token")
	if err := os.WriteFile(tokenFile, []byte("assertion-value"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := AuthOptions{AKSServerID: "api://aks-server", AKSTenantID: "tenant", AKSClientID: "client"}
	tests := []struct {
		name    string
		refresh func(context.Context) (string, time.Time, error)
		method  string
		field   string
		value   string
	}{
		{"workload identity", azureWorkloadIdentityRefresh(server.Client(), server.URL, func() AuthOptions { value := base; value.AKSFederatedTokenFile = tokenFile; return value }()), http.MethodPost, "client_assertion", "assertion-value"},
		{"client secret", azureClientSecretRefresh(server.Client(), server.URL, func() AuthOptions { value := base; value.AKSClientSecret = "client-secret"; return value }()), http.MethodPost, "client_secret", "client-secret"},
		{"managed identity", azureManagedIdentityRefresh(server.Client(), server.URL, base), http.MethodGet, "resource", "api://aks-server"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			token, expires, err := test.refresh(context.Background())
			if err != nil || token != "native-token" || !expires.After(time.Now()) {
				t.Fatalf("token = %q, expires = %v, err = %v", token, expires, err)
			}
			request := <-requests
			form := <-forms
			if request.Method != test.method || form.Get(test.field) != test.value {
				t.Fatalf("request = %s, form = %v", request.Method, form)
			}
			if test.method == http.MethodPost && form.Get("scope") != "api://aks-server/.default" {
				t.Fatalf("scope = %q", form.Get("scope"))
			}
			if test.method == http.MethodGet && request.Header.Get("Metadata") != "true" {
				t.Fatal("managed identity metadata header missing")
			}
		})
	}
}

func TestAKSErrorsDoNotExposeSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, "client-secret assertion-value", http.StatusUnauthorized)
	}))
	defer server.Close()
	_, _, err := azureClientSecretRefresh(server.Client(), server.URL, AuthOptions{AKSServerID: "server", AKSTenantID: "tenant", AKSClientID: "client", AKSClientSecret: "client-secret"})(context.Background())
	if err == nil || strings.Contains(err.Error(), "client-secret") {
		t.Fatalf("error = %v", err)
	}
}

func TestAKSWorkloadIdentityReopensTokenAndChecksPermissions(t *testing.T) {
	directory := t.TempDir()
	firstTarget := filepath.Join(directory, "federated-token-v1")
	if err := os.WriteFile(firstTarget, []byte("first-assertion"), 0o644); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(directory, "federated-token")
	if err := os.Symlink(firstTarget, tokenPath); err != nil {
		t.Fatal(err)
	}
	seen := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			t.Error(err)
		}
		seen = append(seen, request.Form.Get("client_assertion"))
		_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": "native-token", "expires_in": 3600})
	}))
	defer server.Close()
	refresh := azureWorkloadIdentityRefresh(server.Client(), server.URL, AuthOptions{
		AKSServerID: "server", AKSTenantID: "tenant", AKSClientID: "client", AKSFederatedTokenFile: tokenPath,
	})
	if _, _, err := refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	secondTarget := filepath.Join(directory, "federated-token-v2")
	if err := os.WriteFile(secondTarget, []byte("rotated-assertion"), 0o644); err != nil {
		t.Fatal(err)
	}
	nextLink := filepath.Join(directory, "federated-token-next")
	if err := os.Symlink(secondTarget, nextLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(nextLink, tokenPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "first-assertion" || seen[1] != "rotated-assertion" {
		t.Fatalf("assertions = %v", seen)
	}

	secret := "federated-secret-value"
	if err := os.WriteFile(secondTarget, []byte(secret+strings.Repeat("x", int(maxTokenBytes))), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := refresh(context.Background()); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("oversized token error = %v", err)
	}
	if err := os.WriteFile(secondTarget, []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secondTarget, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, _, err := refresh(context.Background()); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("writable token error = %v", err)
	}
}

func TestAKSRejectsOversizedResponseAndUnknownEnvironment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{"access_token":"token","expires_in":3600,"padding":"` + strings.Repeat("x", azureTokenResponseLimit) + `"}`))
	}))
	defer server.Close()
	_, _, err := azureClientSecretRefresh(server.Client(), server.URL, AuthOptions{AKSServerID: "server", AKSTenantID: "tenant", AKSClientID: "client", AKSClientSecret: "secret"})(context.Background())
	if err == nil {
		t.Fatal("oversized AKS response accepted")
	}
	_, err = newAKSCredentialSource(AuthOptions{AKSCredentialMode: "managed_identity", AKSServerID: "server", AKSClientID: "client", AKSEnvironment: "custom"})
	if err == nil {
		t.Fatal("unknown AKS cloud environment accepted")
	}
	if _, err := newAKSCredentialSource(AuthOptions{AKSCredentialMode: "managed_identity", AKSServerID: "server"}); err != nil {
		t.Fatalf("managed identity without optional client ID: %v", err)
	}
}

func TestAzureExpirationBoundsTokenLifetime(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		response azureTokenResponse
		want     time.Time
		valid    bool
	}{
		{name: "valid numeric expires in", response: azureTokenResponse{ExpiresIn: float64(3600)}, want: now.Add(time.Hour), valid: true},
		{name: "valid string expires in maximum", response: azureTokenResponse{ExpiresIn: "86400"}, want: now.Add(24 * time.Hour), valid: true},
		{name: "valid numeric expires on", response: azureTokenResponse{ExpiresOn: float64(now.Add(time.Hour).Unix())}, want: now.Add(time.Hour), valid: true},
		{name: "valid string expires on maximum", response: azureTokenResponse{ExpiresOn: strconv.FormatInt(now.Add(24*time.Hour).Unix(), 10)}, want: now.Add(24 * time.Hour), valid: true},
		{name: "huge numeric expires in", response: azureTokenResponse{ExpiresIn: 1e100}},
		{name: "huge string expires in", response: azureTokenResponse{ExpiresIn: "9223372036854775808"}},
		{name: "far future numeric expires on", response: azureTokenResponse{ExpiresOn: float64(now.Add(24*time.Hour + time.Second).Unix())}},
		{name: "far future string expires on", response: azureTokenResponse{ExpiresOn: "9223372036854775807"}},
		{name: "zero expires in", response: azureTokenResponse{ExpiresIn: float64(0)}},
		{name: "negative expires in", response: azureTokenResponse{ExpiresIn: "-1"}},
		{name: "zero expires on", response: azureTokenResponse{ExpiresOn: float64(0)}},
		{name: "negative expires on", response: azureTokenResponse{ExpiresOn: "-1"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, valid := azureExpiration(test.response, now)
			if valid != test.valid || (valid && !got.Equal(test.want)) {
				t.Fatalf("expiration = %v, valid = %v; want %v, %v", got, valid, test.want, test.valid)
			}
		})
	}
}
