package kubernetes

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

const (
	googleOAuthTokenURL  = "https://oauth2.googleapis.com/token"
	googleSTSTokenURL    = "https://sts.googleapis.com/v1/token"
	googleMetadataURL    = "http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token"
	googleResponseLimit  = 64 << 10
	googleAssertionTTL   = time.Hour
	googleImpersonateTTL = 3600
)

type googleCredentialEndpoints struct {
	oauth, sts string
}

var productionGoogleCredentialEndpoints = googleCredentialEndpoints{oauth: googleOAuthTokenURL, sts: googleSTSTokenURL}

type gkeFileTokenSource struct {
	path      string
	client    *http.Client
	endpoints googleCredentialEndpoints
}

type googleServiceAccount struct {
	Type                 string `json:"type"`
	ProjectID            string `json:"project_id"`
	PrivateKeyID         string `json:"private_key_id"`
	PrivateKey           string `json:"private_key"`
	ClientEmail          string `json:"client_email"`
	ClientID             string `json:"client_id"`
	AuthURI              string `json:"auth_uri"`
	TokenURI             string `json:"token_uri"`
	AuthProviderCertURL  string `json:"auth_provider_x509_cert_url"`
	ClientCertificateURL string `json:"client_x509_cert_url"`
	UniverseDomain       string `json:"universe_domain"`
}

type googleExternalAccount struct {
	Type                           string `json:"type"`
	Audience                       string `json:"audience"`
	SubjectTokenType               string `json:"subject_token_type"`
	TokenURL                       string `json:"token_url"`
	ServiceAccountImpersonationURL string `json:"service_account_impersonation_url"`
	WorkforcePoolUserProject       string `json:"workforce_pool_user_project"`
	QuotaProjectID                 string `json:"quota_project_id"`
	UniverseDomain                 string `json:"universe_domain"`
	CredentialSource               struct {
		File   string `json:"file"`
		Format *struct {
			Type                  string `json:"type"`
			SubjectTokenFieldName string `json:"subject_token_field_name"`
		} `json:"format"`
	} `json:"credential_source"`
}

func (source *gkeFileTokenSource) Token(ctx context.Context) (*oauth2.Token, error) {
	data, err := readSecureBoundedFile(source.path, maxCredentialFileBytes)
	if err != nil {
		return nil, errors.New("GKE credentials file unavailable")
	}
	var header struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &header) != nil {
		return nil, errors.New("invalid GKE credentials file")
	}
	switch header.Type {
	case "service_account":
		var account googleServiceAccount
		if !decodeStrictJSON(data, &account) || account.TokenURI != googleOAuthTokenURL || account.ClientEmail == "" || account.PrivateKey == "" {
			return nil, errors.New("invalid GKE service account")
		}
		return serviceAccountToken(ctx, source.client, source.endpoints.oauth, account)
	case "external_account":
		var account googleExternalAccount
		if !decodeStrictJSON(data, &account) || account.TokenURL != googleSTSTokenURL || account.Audience == "" || account.SubjectTokenType == "" || !validGoogleImpersonationURL(account.ServiceAccountImpersonationURL) {
			return nil, errors.New("invalid GKE external account")
		}
		return externalAccountToken(ctx, source.client, source.endpoints, account)
	default:
		return nil, errors.New("unsupported GKE credential type")
	}
}

func serviceAccountToken(ctx context.Context, client *http.Client, endpoint string, account googleServiceAccount) (*oauth2.Token, error) {
	key, err := parseGooglePrivateKey(account.PrivateKey)
	if err != nil {
		return nil, errors.New("invalid GKE service account key")
	}
	now := time.Now()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": account.PrivateKeyID})
	claims, _ := json.Marshal(map[string]any{"iss": account.ClientEmail, "scope": googleCloudPlatformScope, "aud": googleOAuthTokenURL, "iat": now.Unix(), "exp": now.Add(googleAssertionTTL).Unix()})
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return nil, errors.New("GKE service account signing failed")
	}
	values := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)}}
	return requestGoogleToken(ctx, client, endpoint, values, "")
}

func parseGooglePrivateKey(value string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(value))
	if block == nil {
		return nil, errors.New("invalid PEM")
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		key, ok := parsed.(*rsa.PrivateKey)
		if ok {
			return key, key.Validate()
		}
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	return key, key.Validate()
}

func externalAccountToken(ctx context.Context, client *http.Client, endpoints googleCredentialEndpoints, account googleExternalAccount) (*oauth2.Token, error) {
	subject, err := googleSubjectToken(account)
	if err != nil {
		return nil, errors.New("GKE subject token unavailable")
	}
	values := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"audience":             {account.Audience},
		"requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"subject_token_type":   {account.SubjectTokenType},
		"subject_token":        {subject},
		"scope":                {googleCloudPlatformScope},
	}
	token, err := requestGoogleToken(ctx, client, endpoints.sts, values, "")
	if err != nil || account.ServiceAccountImpersonationURL == "" {
		return token, err
	}
	return impersonatedGoogleToken(ctx, client, account.ServiceAccountImpersonationURL, token.AccessToken)
}

func googleSubjectToken(account googleExternalAccount) (string, error) {
	if !validGoogleSubjectTokenFile(account.CredentialSource.File) {
		return "", errors.New("invalid subject token file")
	}
	data, err := readSecureBoundedFile(account.CredentialSource.File, maxTokenBytes)
	if err != nil {
		return "", err
	}
	if account.CredentialSource.Format == nil || account.CredentialSource.Format.Type == "text" {
		token := strings.TrimSpace(string(data))
		if token == "" || strings.ContainsAny(token, "\r\n") {
			return "", errors.New("invalid subject token")
		}
		return token, nil
	}
	if account.CredentialSource.Format.Type != "json" || account.CredentialSource.Format.SubjectTokenFieldName == "" {
		return "", errors.New("unsupported subject token format")
	}
	var document map[string]json.RawMessage
	if json.Unmarshal(data, &document) != nil || len(document) != 1 {
		return "", errors.New("invalid subject token JSON")
	}
	raw, ok := document[account.CredentialSource.Format.SubjectTokenFieldName]
	var token string
	if !ok || json.Unmarshal(raw, &token) != nil || strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return "", errors.New("invalid subject token JSON")
	}
	return strings.TrimSpace(token), nil
}

func requestGoogleToken(ctx context.Context, client *http.Client, endpoint string, values url.Values, bearer string) (*oauth2.Token, error) {
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("GKE credential endpoint unavailable")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, googleResponseLimit+1))
	if err != nil || len(body) > googleResponseLimit || response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, errors.New("GKE credential endpoint rejected the request")
	}
	var document struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
		IssuedType  string `json:"issued_token_type"`
		Scope       string `json:"scope"`
	}
	if !decodeStrictJSON(body, &document) || document.AccessToken == "" || document.ExpiresIn <= 0 || document.ExpiresIn > 86400 {
		return nil, errors.New("invalid GKE credential response")
	}
	return &oauth2.Token{AccessToken: document.AccessToken, TokenType: document.TokenType, Expiry: time.Now().Add(time.Duration(document.ExpiresIn) * time.Second)}, nil
}

func impersonatedGoogleToken(ctx context.Context, client *http.Client, endpoint, bearer string) (*oauth2.Token, error) {
	body, _ := json.Marshal(struct {
		Scope    []string `json:"scope"`
		Lifetime string   `json:"lifetime"`
	}{Scope: []string{googleCloudPlatformScope}, Lifetime: strconv.Itoa(googleImpersonateTTL) + "s"})
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("GKE impersonation endpoint unavailable")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, googleResponseLimit+1))
	if err != nil || len(data) > googleResponseLimit || response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, errors.New("GKE impersonation endpoint rejected the request")
	}
	var document struct {
		AccessToken string `json:"accessToken"`
		ExpireTime  string `json:"expireTime"`
	}
	if !decodeStrictJSON(data, &document) || document.AccessToken == "" {
		return nil, errors.New("invalid GKE impersonation response")
	}
	expiry, err := time.Parse(time.RFC3339, document.ExpireTime)
	if err != nil || !expiry.After(time.Now()) {
		return nil, errors.New("invalid GKE impersonation response")
	}
	return &oauth2.Token{AccessToken: document.AccessToken, TokenType: "Bearer", Expiry: expiry}, nil
}

type gkeMetadataTokenSource struct {
	client   *http.Client
	endpoint string
}

func (source *gkeMetadataTokenSource) Token(ctx context.Context) (*oauth2.Token, error) {
	endpoint := source.endpoint
	if endpoint == "" {
		endpoint = googleMetadataURL
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?scopes="+url.QueryEscape(googleCloudPlatformScope), nil)
	request.Header.Set("Metadata-Flavor", "Google")
	response, err := source.client.Do(request)
	if err != nil {
		return nil, errors.New("GKE metadata credentials unavailable")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, googleResponseLimit+1))
	if err != nil || len(body) > googleResponseLimit || response.StatusCode < 200 || response.StatusCode >= 300 || response.Header.Get("Metadata-Flavor") != "Google" {
		return nil, errors.New("invalid GKE metadata credential response")
	}
	var document struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if !decodeStrictJSON(body, &document) || document.AccessToken == "" || document.ExpiresIn <= 0 || document.ExpiresIn > 86400 {
		return nil, errors.New("invalid GKE metadata credential response")
	}
	return &oauth2.Token{AccessToken: document.AccessToken, TokenType: document.TokenType, Expiry: time.Now().Add(time.Duration(document.ExpiresIn) * time.Second)}, nil
}
