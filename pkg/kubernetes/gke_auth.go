package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

const googleCloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

func newGKECredentialSource(credentialsFile string) (CredentialSource, error) {
	var source gkeTokenSource
	if strings.TrimSpace(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")) != "" {
		return nil, errors.New("kubernetes: ambient GKE credentials are unsupported")
	}
	if os.Getenv("GCE_METADATA_HOST") != "" {
		return nil, errors.New("kubernetes: GKE application default credentials unavailable")
	}
	if credentialsFile == "" {
		source = &gkeMetadataTokenSource{client: fixedCredentialHTTPClient()}
	} else {
		if !filepath.IsAbs(credentialsFile) || filepath.Clean(credentialsFile) != credentialsFile {
			return nil, errors.New("kubernetes: GKE credentials file unavailable or writable by other users")
		}
		data, err := readSecureBoundedFile(credentialsFile, maxCredentialFileBytes)
		if err != nil {
			return nil, errors.New("kubernetes: GKE credentials file unavailable")
		}
		subjectTokenFile, valid := validGoogleCredentialEndpoints(data)
		if !valid || (subjectTokenFile != "" && !validGoogleSubjectTokenFile(subjectTokenFile)) {
			return nil, errors.New("kubernetes: invalid GKE credentials file")
		}
		source = &gkeFileTokenSource{path: credentialsFile, client: fixedCredentialHTTPClient(), endpoints: productionGoogleCredentialEndpoints}
	}
	return newGKESource(source, time.Now), nil
}

func validGoogleCredentialEndpoints(data []byte) (string, bool) {
	var document struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &document) != nil {
		return "", false
	}
	switch document.Type {
	case "service_account":
		var serviceAccount struct {
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
		return "", decodeStrictJSON(data, &serviceAccount) && serviceAccount.TokenURI == "https://oauth2.googleapis.com/token"
	case "external_account":
		var externalAccount struct {
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
		if !decodeStrictJSON(data, &externalAccount) || externalAccount.TokenURL != "https://sts.googleapis.com/v1/token" || externalAccount.CredentialSource.File == "" || !validGoogleImpersonationURL(externalAccount.ServiceAccountImpersonationURL) {
			return "", false
		}
		if format := externalAccount.CredentialSource.Format; format != nil && format.Type != "text" && (format.Type != "json" || strings.TrimSpace(format.SubjectTokenFieldName) == "") {
			return "", false
		}
		return externalAccount.CredentialSource.File, true
	default:
		return "", false
	}
}

func decodeStrictJSON(data []byte, destination any) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(destination) == nil && decoder.Decode(new(any)) == io.EOF
}

func validGoogleImpersonationURL(value string) bool {
	if value == "" {
		return true
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "iamcredentials.googleapis.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	account := strings.TrimSuffix(strings.TrimPrefix(parsed.EscapedPath(), "/v1/projects/-/serviceAccounts/"), ":generateAccessToken")
	return account != "" && len(account) <= 512 && !strings.Contains(account, "/") && parsed.EscapedPath() == "/v1/projects/-/serviceAccounts/"+account+":generateAccessToken"
}

func validGoogleSubjectTokenFile(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	data, err := readSecureBoundedFile(path, maxTokenBytes)
	return err == nil && len(data) > 0
}

type gkeTokenSource interface {
	Token(context.Context) (*oauth2.Token, error)
}

func newGKESource(source gkeTokenSource, now func() time.Time) CredentialSource {
	refresh := func(ctx context.Context) (string, time.Time, error) {
		token, err := source.Token(ctx)
		if err != nil {
			return "", time.Time{}, errors.New("kubernetes: GKE credentials unavailable")
		}
		if token == nil || token.AccessToken == "" || !token.Expiry.After(now()) {
			return "", time.Time{}, errors.New("kubernetes: invalid GKE credential response")
		}
		return token.AccessToken, token.Expiry, nil
	}
	return &expiringBearerSource{refresh: refresh, now: now, skew: 2 * time.Minute}
}
