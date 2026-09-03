package kubernetes

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	azureTokenResponseLimit = 64 << 10
	azureRequestTimeout     = 10 * time.Second
	azureMaxTokenLifetime   = 24 * time.Hour
	managedIdentityURL      = "http://169.254.169.254/metadata/identity/oauth2/token"
)

var azureAuthorityHosts = map[string]string{
	"public":     "https://login.microsoftonline.com",
	"government": "https://login.microsoftonline.us",
	"china":      "https://login.chinacloudapi.cn",
}

type azureTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   any    `json:"expires_in"`
	ExpiresOn   any    `json:"expires_on"`
}

func newAKSCredentialSource(options AuthOptions) (CredentialSource, error) {
	environment := options.AKSEnvironment
	if environment == "" {
		environment = "public"
	}
	authority, ok := azureAuthorityHosts[environment]
	if !ok || !validCloudIdentifier(options.AKSServerID, 2048) || (options.AKSClientID != "" && !validCloudIdentifier(options.AKSClientID, 256)) {
		return nil, errors.New("kubernetes: invalid AKS configuration")
	}
	client := fixedCredentialHTTPClient()
	var refresh func(context.Context) (string, time.Time, error)
	switch options.AKSCredentialMode {
	case "workload_identity":
		if !validCloudIdentifier(options.AKSTenantID, 256) || !validCloudIdentifier(options.AKSClientID, 256) || !validFederatedTokenPath(options.AKSFederatedTokenFile) || options.AKSClientSecret != "" {
			return nil, errors.New("kubernetes: invalid AKS workload identity configuration")
		}
		refresh = azureWorkloadIdentityRefresh(client, authority, options)
	case "client_secret":
		if !validCloudIdentifier(options.AKSTenantID, 256) || !validCloudIdentifier(options.AKSClientID, 256) || options.AKSClientSecret == "" || len(options.AKSClientSecret) > int(maxTokenBytes) || options.AKSFederatedTokenFile != "" {
			return nil, errors.New("kubernetes: invalid AKS client secret configuration")
		}
		refresh = azureClientSecretRefresh(client, authority, options)
	case "managed_identity":
		if options.AKSTenantID != "" || options.AKSClientSecret != "" || options.AKSFederatedTokenFile != "" {
			return nil, errors.New("kubernetes: invalid AKS managed identity configuration")
		}
		refresh = azureManagedIdentityRefresh(client, managedIdentityURL, options)
	default:
		return nil, errors.New("kubernetes: unsupported AKS credential mode")
	}
	return &expiringBearerSource{refresh: refresh, now: time.Now, skew: 2 * time.Minute}, nil
}

func fixedCredentialHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &http.Client{
		Transport: transport,
		Timeout:   azureRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func azureWorkloadIdentityRefresh(client *http.Client, authority string, options AuthOptions) func(context.Context) (string, time.Time, error) {
	return func(ctx context.Context) (string, time.Time, error) {
		assertion, err := readSecureBoundedFile(options.AKSFederatedTokenFile, maxTokenBytes)
		assertionValue := strings.TrimSpace(string(assertion))
		if err != nil || assertionValue == "" || strings.ContainsAny(assertionValue, "\r\n") {
			return "", time.Time{}, errors.New("kubernetes: AKS federated credential unavailable")
		}
		values := url.Values{
			"client_id":             {options.AKSClientID},
			"client_assertion":      {assertionValue},
			"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
			"grant_type":            {"client_credentials"},
			"scope":                 {azureScope(options.AKSServerID)},
		}
		return requestAzureToken(ctx, client, authority+"/"+url.PathEscape(options.AKSTenantID)+"/oauth2/v2.0/token", values)
	}
}

func azureClientSecretRefresh(client *http.Client, authority string, options AuthOptions) func(context.Context) (string, time.Time, error) {
	return func(ctx context.Context) (string, time.Time, error) {
		values := url.Values{
			"client_id":     {options.AKSClientID},
			"client_secret": {options.AKSClientSecret},
			"grant_type":    {"client_credentials"},
			"scope":         {azureScope(options.AKSServerID)},
		}
		return requestAzureToken(ctx, client, authority+"/"+url.PathEscape(options.AKSTenantID)+"/oauth2/v2.0/token", values)
	}
}

func azureManagedIdentityRefresh(client *http.Client, endpoint string, options AuthOptions) func(context.Context) (string, time.Time, error) {
	return func(ctx context.Context) (string, time.Time, error) {
		values := url.Values{"api-version": {"2018-02-01"}, "resource": {options.AKSServerID}}
		if options.AKSClientID != "" {
			values.Set("client_id", options.AKSClientID)
		}
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+values.Encode(), nil)
		request.Header.Set("Metadata", "true")
		return executeAzureTokenRequest(client, request)
	}
}

func requestAzureToken(ctx context.Context, client *http.Client, endpoint string, values url.Values) (string, time.Time, error) {
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return executeAzureTokenRequest(client, request)
}

func executeAzureTokenRequest(client *http.Client, request *http.Request) (string, time.Time, error) {
	response, err := client.Do(request)
	if err != nil {
		return "", time.Time{}, errors.New("kubernetes: AKS credential endpoint unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, azureTokenResponseLimit))
		return "", time.Time{}, errors.New("kubernetes: AKS credential endpoint rejected the request")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, azureTokenResponseLimit+1))
	if err != nil || len(body) > azureTokenResponseLimit {
		return "", time.Time{}, errors.New("kubernetes: invalid AKS credential response")
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	var document azureTokenResponse
	if decoder.Decode(&document) != nil || decoder.Decode(new(any)) != io.EOF {
		return "", time.Time{}, errors.New("kubernetes: invalid AKS credential response")
	}
	expires, ok := azureExpiration(document, time.Now())
	if !ok || document.AccessToken == "" {
		return "", time.Time{}, errors.New("kubernetes: invalid AKS credential response")
	}
	return document.AccessToken, expires, nil
}

func azureExpiration(document azureTokenResponse, now time.Time) (time.Time, bool) {
	if seconds, ok := numericString(document.ExpiresIn); ok {
		if seconds <= 0 || seconds > int64(azureMaxTokenLifetime/time.Second) {
			return time.Time{}, false
		}
		return now.Add(time.Duration(seconds) * time.Second), true
	}
	if epoch, ok := numericString(document.ExpiresOn); ok {
		expires := time.Unix(epoch, 0)
		return expires, expires.After(now) && !expires.After(now.Add(azureMaxTokenLifetime))
	}
	return time.Time{}, false
}

func numericString(value any) (int64, bool) {
	switch typed := value.(type) {
	case string:
		result, err := strconv.ParseInt(typed, 10, 64)
		return result, err == nil
	case float64:
		if typed < -9223372036854775808 || typed >= 9223372036854775808 || typed != float64(int64(typed)) {
			return 0, false
		}
		return int64(typed), true
	default:
		return 0, false
	}
}

func azureScope(serverID string) string {
	return strings.TrimSuffix(serverID, "/") + "/.default"
}
