package kubernetes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/aws-sdk-go-v2/service/sts/types"
)

func TestEKSTokenShapeAndDeterministicSignature(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	provider := credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret-value", "session-value")
	source := newEKSSource("production", "us-east-1", provider, func() time.Time { return now })
	header, err := source.Authorization(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	encoded := strings.TrimPrefix(header, "Bearer k8s-aws-v1.")
	if encoded == header {
		t.Fatalf("token shape = %q", header)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := url.Parse(string(decoded))
	if err != nil {
		t.Fatal(err)
	}
	query := signed.Query()
	for _, key := range []string{"X-Amz-Algorithm", "X-Amz-Credential", "X-Amz-Date", "X-Amz-Expires", "X-Amz-SignedHeaders", "X-Amz-Signature", "X-Amz-Security-Token"} {
		if query.Get(key) == "" {
			t.Errorf("missing %s in signed token", key)
		}
	}
	if query.Get("X-Amz-SignedHeaders") != "host;x-k8s-aws-id" || query.Get("Action") != "GetCallerIdentity" || query.Get("Version") != "2011-06-15" {
		t.Fatalf("signed query = %v", query)
	}
	if strings.Contains(header, "secret-value") || strings.Contains(header, "production") {
		t.Fatal("token exposed signing input")
	}
}

type failingCredentials struct{ secret string }

func (provider failingCredentials) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{}, &credentialTestError{value: provider.secret}
}

type credentialTestError struct{ value string }

func (err *credentialTestError) Error() string { return err.value }

func TestEKSCredentialErrorDoesNotLeakProviderDetails(t *testing.T) {
	source := newEKSSource("production", "us-east-1", failingCredentials{secret: "provider-secret"}, time.Now)
	_, err := source.Authorization(context.Background())
	if err == nil || strings.Contains(err.Error(), "provider-secret") {
		t.Fatalf("error = %v", err)
	}
}

type assumeRoleClient struct {
	roleARN string
	calls   int
}

func (client *assumeRoleClient) AssumeRole(_ context.Context, input *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	client.calls++
	client.roleARN = aws.ToString(input.RoleArn)
	expires := time.Now().Add(time.Hour)
	return &sts.AssumeRoleOutput{Credentials: &types.Credentials{
		AccessKeyId:     aws.String("ASIAASSUMED"),
		SecretAccessKey: aws.String("assumed-secret"),
		SessionToken:    aws.String("assumed-session"),
		Expiration:      &expires,
	}}, nil
}

func TestEKSAssumeRoleUsesConfiguredRoleWithoutNetwork(t *testing.T) {
	client := new(assumeRoleClient)
	provider := assumeRoleCredentials(client, "arn:aws:iam::123456789012:role/reader")
	credential, err := provider.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if client.calls != 1 || client.roleARN != "arn:aws:iam::123456789012:role/reader" || credential.AccessKeyID != "ASIAASSUMED" {
		t.Fatalf("calls = %d, role = %q, credential = %q", client.calls, client.roleARN, credential.AccessKeyID)
	}
}

func TestEKSStaticEnvironmentCredentialChain(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-value")
	t.Setenv("AWS_SESSION_TOKEN", "session-value")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "missing-credentials"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "missing-config"))
	source, err := newEKSCredentialSource(AuthOptions{EKSClusterName: "production", EKSRegion: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	header, err := source.Authorization(context.Background())
	if err != nil || !strings.HasPrefix(header, "Bearer k8s-aws-v1.") {
		t.Fatalf("header = %q, err = %v", header, err)
	}
}

func TestEKSStaticEnvironmentDoesNotParseUnusedProfile(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(profile, []byte("[default]\ncredential_process = helper\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-value")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", profile)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "missing-config"))
	if _, err := newEKSCredentialSource(AuthOptions{EKSClusterName: "production", EKSRegion: "us-east-1"}); err != nil {
		t.Fatalf("decisive environment credentials consulted unused profile: %v", err)
	}
}

func TestEKSIRSAUsesInjectedSTSOnlyForCredentialExchange(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "web-identity-token")
	if err := os.WriteFile(tokenFile, []byte("signed-web-identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", tokenFile)
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::123456789012:role/irsa-reader")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "missing-credentials"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "missing-config"))
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls++
		if err := request.ParseForm(); err != nil {
			t.Error(err)
		}
		if request.Form.Get("Action") != "AssumeRoleWithWebIdentity" || request.Form.Get("WebIdentityToken") != "signed-web-identity" {
			t.Errorf("STS form = %v", request.Form)
		}
		response.Header().Set("Content-Type", "text/xml")
		_, _ = response.Write([]byte(`<AssumeRoleWithWebIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleWithWebIdentityResult><Credentials><AccessKeyId>ASIAIRSA</AccessKeyId><SecretAccessKey>irsa-secret</SecretAccessKey><SessionToken>irsa-session</SessionToken><Expiration>2030-01-01T00:00:00Z</Expiration></Credentials><AssumedRoleUser><Arn>arn:aws:sts::123456789012:assumed-role/irsa-reader/versus</Arn><AssumedRoleId>AROA:versus</AssumedRoleId></AssumedRoleUser></AssumeRoleWithWebIdentityResult><ResponseMetadata><RequestId>request-id</RequestId></ResponseMetadata></AssumeRoleWithWebIdentityResponse>`))
	}))
	defer server.Close()
	factory := func(region string, provider aws.CredentialsProvider) eksSTSAPI {
		endpoint := server.URL
		return sts.New(sts.Options{Region: region, Credentials: provider, HTTPClient: server.Client(), BaseEndpoint: &endpoint})
	}
	source, err := newEKSCredentialSourceWithSTSFactory(AuthOptions{EKSClusterName: "production", EKSRegion: "us-east-1"}, factory)
	if err != nil {
		t.Fatal(err)
	}
	header, err := source.Authorization(context.Background())
	if err != nil || calls != 1 {
		t.Fatalf("header=%q calls=%d err=%v", header, calls, err)
	}
	encoded := strings.TrimPrefix(header, "Bearer k8s-aws-v1.")
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := url.Parse(string(decoded))
	if err != nil || signed.Host != "sts.us-east-1.amazonaws.com" {
		t.Fatalf("signed token URL = %q, err=%v", decoded, err)
	}
}

func TestEKSRejectsAmbientEndpointOverrides(t *testing.T) {
	t.Setenv("AWS_ENDPOINT_URL_STS", "https://attacker.example")
	if _, err := newEKSCredentialSource(AuthOptions{EKSClusterName: "production", EKSRegion: "us-east-1"}); err == nil {
		t.Fatal("ambient STS endpoint override accepted")
	}
}

func TestEKSSharedProfileRejectsActiveProviders(t *testing.T) {
	for _, directive := range []string{
		"credential_process = helper",
		"sso_session = production",
		"sso_start_url = https://example.awsapps.com/start",
		"source_profile = base",
		"role_arn = arn:aws:iam::123456789012:role/reader",
		"web_identity_token_file = /var/run/token",
		"credential_source = Ec2InstanceMetadata",
		"endpoint_url = https://attacker.example",
		"services = local-services",
		"custom_plugin = helper",
	} {
		t.Run(strings.Fields(directive)[0], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials")
			if err := os.WriteFile(path, []byte("[production]\n"+directive+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := parseAWSProfileFile(path, "production"); err == nil {
				t.Fatalf("directive accepted: %s", directive)
			}
		})
	}
}

func TestEKSSharedProfileIgnoresBenignAndPresentationSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials")
	body := "[production]\nregion = us-east-1\naws_access_key_id = AKID\naws_secret_access_key = secret\noutput = json\ncli_pager = less\nretry_mode = standard\nmax_attempts = 3\ncolor = on\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	values, found, err := parseAWSProfileFile(path, "production")
	if err != nil || !found || len(values) != 3 || values["aws_access_key_id"] != "AKID" {
		t.Fatalf("values=%v found=%v error=%v", values, found, err)
	}
}

func TestEKSBenignDefaultProfileDoesNotBlockAmbientProviders(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(configPath, []byte("[default]\noutput = json\ncolor = on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", configPath)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "missing-credentials"))
	t.Setenv("AWS_REGION", "us-east-1")
	if _, err := newEKSCredentialSource(AuthOptions{EKSClusterName: "production"}); err != nil {
		t.Fatalf("benign profile blocked IMDS: %v", err)
	}
	tokenFile := filepath.Join(t.TempDir(), "web-identity-token")
	if err := os.WriteFile(tokenFile, []byte("signed-web-identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", tokenFile)
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::123456789012:role/irsa-reader")
	if _, err := newEKSCredentialSource(AuthOptions{EKSClusterName: "production"}); err != nil {
		t.Fatalf("benign profile blocked IRSA: %v", err)
	}
}

func TestEKSContainerCredentialEndpointPolicy(t *testing.T) {
	for _, test := range []struct {
		name, endpoint string
		valid          bool
	}{
		{"pod identity", "http://169.254.170.23/v1/credentials", true},
		{"ECS", "http://169.254.170.2/credentials", true},
		{"loopback", "http://127.0.0.1:9911/credentials", true},
		{"localhost name", "http://localhost:9911/credentials", false},
		{"remote HTTP", "http://attacker.example/credentials", false},
		{"remote HTTPS", "https://attacker.example/credentials", false},
		{"link local port", "http://169.254.170.23:9911/credentials", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "")
			t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", test.endpoint)
			_, configured, err := containerCredentialsEndpoint()
			if (err == nil && configured) != test.valid {
				t.Fatalf("configured = %v, err = %v", configured, err)
			}
		})
	}
}

func TestEKSSTSEndpointsArePartitionPinned(t *testing.T) {
	for region, want := range map[string]string{
		"us-east-1":     "https://sts.us-east-1.amazonaws.com",
		"us-gov-west-1": "https://sts.us-gov-west-1.amazonaws.com",
		"cn-north-1":    "https://sts.cn-north-1.amazonaws.com.cn",
	} {
		if got := awsSTSEndpoint(region); got != want {
			t.Errorf("awsSTSEndpoint(%q) = %q, want %q", region, got, want)
		}
	}
}

func TestEKSBearerCannotOutliveSigningSession(t *testing.T) {
	origin := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	for _, sessionLifetime := range []time.Duration{5 * time.Minute, 30 * time.Minute} {
		t.Run(sessionLifetime.String(), func(t *testing.T) {
			now := origin
			calls := 0
			provider := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				calls++
				return aws.Credentials{AccessKeyID: "synthetic-key", SecretAccessKey: "synthetic-secret", SessionToken: "synthetic-session", CanExpire: true, Expires: origin.Add(sessionLifetime)}, nil
			})
			source := newEKSSource("synthetic-cluster", "us-east-1", provider, func() time.Time { return now }).(*expiringBearerSource)
			if _, err := source.Authorization(t.Context()); err != nil {
				t.Fatal(err)
			}
			expected := origin.Add(min(sessionLifetime, 14*time.Minute))
			if !source.expires.Equal(expected) {
				t.Fatalf("effective expiry=%v want=%v", source.expires, expected)
			}
			now = expected.Add(-time.Minute - time.Second)
			if _, err := source.Authorization(t.Context()); err != nil || calls != 1 {
				t.Fatal("bearer refreshed before effective boundary")
			}
			now = expected.Add(-time.Minute)
			if _, err := source.Authorization(t.Context()); err != nil || calls != 2 {
				t.Fatal("effective boundary did not retrieve signing credentials")
			}
			now = origin.Add(sessionLifetime + time.Second)
			if _, err := source.Authorization(t.Context()); err == nil {
				t.Fatal("expired signing session minted a bearer")
			}
		})
	}
}

type renewableSTSFixture struct {
	now        *time.Time
	lifetime   time.Duration
	calls      int
	assertions []string
	fail       bool
}

func (fixture *renewableSTSFixture) credentials() *types.Credentials {
	fixture.calls++
	return &types.Credentials{AccessKeyId: aws.String(fmt.Sprintf("synthetic-key-%d", fixture.calls)), SecretAccessKey: aws.String("synthetic-secret"), SessionToken: aws.String("synthetic-session"), Expiration: aws.Time(fixture.now.Add(fixture.lifetime))}
}
func (fixture *renewableSTSFixture) AssumeRole(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	if fixture.fail {
		return nil, errors.New("synthetic STS outage")
	}
	return &sts.AssumeRoleOutput{Credentials: fixture.credentials()}, nil
}
func (fixture *renewableSTSFixture) AssumeRoleWithWebIdentity(_ context.Context, input *sts.AssumeRoleWithWebIdentityInput, _ ...func(*sts.Options)) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	if fixture.fail {
		return nil, errors.New("synthetic STS outage")
	}
	fixture.assertions = append(fixture.assertions, aws.ToString(input.WebIdentityToken))
	return &sts.AssumeRoleWithWebIdentityOutput{Credentials: fixture.credentials()}, nil
}

func TestEKSRenewableRoleSessionBeforeAndAfterBearerBoundary(t *testing.T) {
	for _, lifetime := range []time.Duration{3 * time.Minute, 30 * time.Minute} {
		t.Run(lifetime.String(), func(t *testing.T) {
			now := time.Now().UTC()
			fixture := &renewableSTSFixture{now: &now, lifetime: lifetime}
			source := newEKSSource("synthetic-cluster", "us-east-1", assumeRoleCredentials(fixture, "synthetic-role"), func() time.Time { return now })
			first, err := source.Authorization(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if lifetime > 14*time.Minute {
				now = now.Add(13 * time.Minute)
				if _, err = source.Authorization(t.Context()); err != nil || fixture.calls != 1 {
					t.Fatal("bearer renewal unnecessarily exchanged role session")
				}
				now = now.Add(16 * time.Minute)
			} else {
				now = now.Add(2 * time.Minute)
			}
			second, err := source.Authorization(t.Context())
			if err != nil || fixture.calls != 2 || second == first {
				t.Fatalf("role/session renewal failed: calls=%d err=%v", fixture.calls, err)
			}
		})
	}
}

func TestEKSIRSAContainerAndIMDSRenewSessionsAndRecoverProviderOutage(t *testing.T) {
	for _, mode := range []string{"irsa", "container", "imds"} {
		t.Run(mode, func(t *testing.T) {
			for _, key := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_AUTHORIZATION_TOKEN", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"} {
				t.Setenv(key, "")
			}
			now := time.Now().UTC()
			fixture := &renewableSTSFixture{now: &now, lifetime: 3 * time.Minute}
			calls := 0
			outage := false
			assertionPath := filepath.Join(t.TempDir(), "assertion")
			if err := os.WriteFile(assertionPath, []byte("synthetic-assertion-one"), 0600); err != nil {
				t.Fatal(err)
			}
			if mode == "irsa" {
				t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", assertionPath)
				t.Setenv("AWS_ROLE_ARN", "synthetic-role")
			}
			if mode == "container" {
				t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", "http://169.254.170.23/credentials")
				t.Setenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", assertionPath)
			}
			transport := logStreamTestTransport(func(request *http.Request) (*http.Response, error) {
				if outage {
					return nil, errors.New("synthetic metadata outage")
				}
				body := ""
				status := http.StatusOK
				switch request.URL.Path {
				case "/latest/api/token":
					body = "synthetic-imds-token"
				case "/latest/meta-data/iam/security-credentials/", "/latest/meta-data/iam/security-credentials":
					body = "synthetic-role"
				case "/latest/meta-data/iam/security-credentials/synthetic-role", "/credentials":
					calls++
					if mode == "container" && calls >= 2 && request.Header.Get("Authorization") != "synthetic-assertion-two" {
						t.Error("container provider retained old authorization file")
					}
					raw, _ := json.Marshal(map[string]any{"Code": "Success", "AccessKeyId": fmt.Sprintf("synthetic-key-%d", calls), "SecretAccessKey": "synthetic-secret", "Token": "synthetic-session", "Expiration": now.Add(3 * time.Minute).Format(time.RFC3339Nano)})
					body = string(raw)
				default:
					status = http.StatusNotFound
				}
				return &http.Response{StatusCode: status, Header: http.Header{"X-Aws-Ec2-Metadata-Token-Ttl-Seconds": []string{"21600"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
			})
			provider, err := nativeAWSCredentialProviderWithHTTP("us-east-1", nil, false, func(string, aws.CredentialsProvider) eksSTSAPI { return fixture }, &http.Client{Transport: transport})
			if err != nil {
				t.Fatal(err)
			}
			source := newEKSSource("synthetic-cluster", "us-east-1", provider, func() time.Time { return now })
			first, err := source.Authorization(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			now = now.Add(2 * time.Minute)
			if err = os.WriteFile(assertionPath, []byte("synthetic-assertion-two"), 0600); err != nil {
				t.Fatal(err)
			}
			second, err := source.Authorization(t.Context())
			if err != nil || second == first {
				t.Fatalf("session rotation failed: %v", err)
			}
			if mode == "irsa" {
				if len(fixture.assertions) != 2 || fixture.assertions[1] != "synthetic-assertion-two" {
					t.Fatal("IRSA retained old assertion")
				}
			} else if calls != 2 {
				t.Fatalf("metadata sessions=%d", calls)
			}
			now = now.Add(2 * time.Minute)
			outage, fixture.fail = true, true
			if _, err = source.Authorization(t.Context()); err == nil || strings.Contains(err.Error(), "synthetic metadata") || strings.Contains(err.Error(), "synthetic STS") {
				t.Fatal("provider outage was hidden or leaked details")
			}
			outage, fixture.fail = false, false
			now = now.Add(2 * time.Second)
			if _, err = source.Authorization(t.Context()); err != nil {
				t.Fatalf("renewable source did not recover: %v", err)
			}
		})
	}
}

func TestEKSStaticSessionDoesNotInventRefreshAuthorityOrReloadEnvironment(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "synthetic-original")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "synthetic-secret")
	t.Setenv("AWS_SESSION_TOKEN", "synthetic-pasted-session")
	provider, err := nativeAWSCredentialProvider("us-east-1", nil, false, func(string, aws.CredentialsProvider) eksSTSAPI {
		t.Fatal("static session unexpectedly requested STS refresh authority")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := provider.Retrieve(t.Context())
	if err != nil || first.CanExpire {
		t.Fatal("static session invented expiry metadata")
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "synthetic-replacement")
	cached, err := provider.Retrieve(t.Context())
	if err != nil || cached.AccessKeyID != first.AccessKeyID {
		t.Fatal("static source silently claimed environment hot reload")
	}
}
