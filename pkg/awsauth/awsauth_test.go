package awsauth

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/aws/smithy-go"
)

const (
	testAccount  = "123456789012"
	testRoleARN  = "arn:aws:iam::123456789012:role/versus-reader"
	testRootARN  = "arn:aws:iam::123456789012:root"
	testExternal = "versus-external-id"
)

func TestValidateRoleARNAndRegions(t *testing.T) {
	for _, arm := range []string{
		"arn:aws:iam::123456789012:role/versus-reader",
		"arn:aws:iam::123456789012:role/team/versus-reader",
		"arn:aws-cn:iam::123456789012:role/versus-reader",
		"arn:aws-us-gov:iam::123456789012:role/team/nested/versus-reader",
		"arn:aws:iam::123456789012:role/reader+=,.@_-",
	} {
		if err := ValidateRoleARN(arm); err != nil {
			t.Errorf("valid ARN rejected: %q", arm)
		}
	}
	for _, arm := range []string{
		"",
		testRootARN,
		"arn:aws:iam::12345678901:role/reader",
		"arn:aws:iam::123456789012:role/",
		"arn:aws:iam::123456789012:role//reader",
		"arn:aws-iso:iam::123456789012:role/reader",
		"arn:aws:iam::123456789012:user/reader",
		"arn:aws:iam::123456789012:role/reader/",
		"arn:aws:iam::123456789012:role/reader\n",
		"arn:aws:iam::123456789012:role/" + strings.Repeat("a", 2048),
	} {
		if err := ValidateRoleARN(arm); err == nil {
			t.Errorf("invalid ARN accepted: %q", arm)
		}
	}

	for _, region := range []string{"us-east-1", "us-gov-west-1", "cn-north-1", "ap-southeast-3", "eu-central-1"} {
		if err := ValidateRegion(region); err != nil {
			t.Errorf("valid region rejected: %q", region)
		}
	}
	for _, region := range []string{"", "US-EAST-1", "us-east", "us-iso-east-1", "us-east-1\n", strings.Repeat("a", 40)} {
		if err := ValidateRegion(region); err == nil {
			t.Errorf("invalid region accepted: %q", region)
		}
	}
}

type fakeSTS struct {
	assumeInput      *sts.AssumeRoleInput
	webIdentityInput *sts.AssumeRoleWithWebIdentityInput
	callerInput      *sts.GetCallerIdentityInput
	calls            int
	err              error
	arn              string
	account          string
}

func (client *fakeSTS) credentials() *types.Credentials {
	return &types.Credentials{
		AccessKeyId:     aws.String("ASIAASSUMED"),
		SecretAccessKey: aws.String("assumed-secret"),
		SessionToken:    aws.String("assumed-session"),
		Expiration:      aws.Time(time.Now().Add(time.Hour)),
	}
}

func (client *fakeSTS) AssumeRole(_ context.Context, input *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	client.calls++
	client.assumeInput = input
	if client.err != nil {
		return nil, client.err
	}
	return &sts.AssumeRoleOutput{Credentials: client.credentials()}, nil
}

func (client *fakeSTS) AssumeRoleWithWebIdentity(_ context.Context, input *sts.AssumeRoleWithWebIdentityInput, _ ...func(*sts.Options)) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	client.calls++
	client.webIdentityInput = input
	if client.err != nil {
		return nil, client.err
	}
	return &sts.AssumeRoleWithWebIdentityOutput{Credentials: client.credentials()}, nil
}

func (client *fakeSTS) GetCallerIdentity(_ context.Context, input *sts.GetCallerIdentityInput, _ ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	client.calls++
	client.callerInput = input
	if client.err != nil {
		return nil, client.err
	}
	return &sts.GetCallerIdentityOutput{Arn: aws.String(client.arn), Account: aws.String(client.account)}, nil
}

func TestAssumeRolePassesExternalIDAndDurationToSTS(t *testing.T) {
	for _, test := range []struct {
		name       string
		spec       RoleSpec
		wantExtID  string
		wantDur    time.Duration
		wantPrefix string
	}{
		{"explicit", RoleSpec{RoleARN: testRoleARN, ExternalID: testExternal, SessionName: "versus-session", Duration: 30 * time.Minute}, testExternal, 30 * time.Minute, "versus-session"},
		{"defaults", RoleSpec{RoleARN: testRoleARN}, "", 15 * time.Minute, "versus"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := new(fakeSTS)
			provider := assumeRoleFrom(service, test.spec)
			credential, err := provider.Retrieve(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if service.calls != 1 || credential.AccessKeyID != "ASIAASSUMED" || aws.ToString(service.assumeInput.RoleArn) != testRoleARN {
				t.Fatalf("STS request = %+v, credential = %+v", service.assumeInput, credential)
			}
			gotExternal := ""
			if service.assumeInput.ExternalId != nil {
				gotExternal = *service.assumeInput.ExternalId
			}
			if gotExternal != test.wantExtID {
				t.Errorf("external id = %q, want %q", gotExternal, test.wantExtID)
			}
			if got := time.Duration(aws.ToInt32(service.assumeInput.DurationSeconds)) * time.Second; got != test.wantDur {
				t.Errorf("duration = %v, want %v", got, test.wantDur)
			}
			if !strings.HasPrefix(aws.ToString(service.assumeInput.RoleSessionName), test.wantPrefix) {
				t.Errorf("session name = %q, want prefix %q", aws.ToString(service.assumeInput.RoleSessionName), test.wantPrefix)
			}
		})
	}
}

func TestAssumeRoleRejectsUnsafeSpecs(t *testing.T) {
	for _, test := range []struct {
		name   string
		base   aws.CredentialsProvider
		spec   RoleSpec
		region string
		code   string
	}{
		{"nil base", nil, RoleSpec{RoleARN: testRoleARN}, "us-east-1", "no_server_identity"},
		{"partition mismatch", aws.AnonymousCredentials{}, RoleSpec{RoleARN: testRoleARN}, "cn-north-1", ""},
		{"gov region with commercial role", aws.AnonymousCredentials{}, RoleSpec{RoleARN: testRoleARN}, "us-gov-west-1", ""},
		{"invalid region", aws.AnonymousCredentials{}, RoleSpec{RoleARN: testRoleARN}, "us-east", ""},
		{"short session", aws.AnonymousCredentials{}, RoleSpec{RoleARN: testRoleARN, Duration: 14 * time.Minute}, "us-east-1", "session_duration_invalid"},
		{"long session", aws.AnonymousCredentials{}, RoleSpec{RoleARN: testRoleARN, Duration: 13 * time.Hour}, "us-east-1", "session_duration_invalid"},
		{"fractional session", aws.AnonymousCredentials{}, RoleSpec{RoleARN: testRoleARN, Duration: 90*time.Second + 500*time.Millisecond}, "us-east-1", "session_duration_invalid"},
		{"control characters in external id", aws.AnonymousCredentials{}, RoleSpec{RoleARN: testRoleARN, ExternalID: "bad\nvalue"}, "us-east-1", "external_id_mismatch"},
		{"invalid session name", aws.AnonymousCredentials{}, RoleSpec{RoleARN: testRoleARN, SessionName: "bad name"}, "us-east-1", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := assumeRole(context.Background(), test.base, test.spec, test.region, NewHTTPClient())
			if err == nil {
				t.Fatal("unsafe role spec accepted")
			}
			if test.code != "" && ErrorCode(err) != test.code {
				t.Fatalf("error code = %q, want %q", ErrorCode(err), test.code)
			}
		})
	}
}

func TestAssumeRoleRejectsAmbientEndpointOverrides(t *testing.T) {
	t.Setenv("AWS_ENDPOINT_URL_STS", "https://attacker.example")
	_, err := assumeRole(context.Background(), aws.AnonymousCredentials{}, RoleSpec{RoleARN: testRoleARN}, "us-east-1", NewHTTPClient())
	if ErrorCode(err) != "no_server_identity" {
		t.Fatalf("error = %v", err)
	}
}

func TestAssumeRoleMapsProviderFailuresWithoutEcho(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code string
	}{
		{"denied", &smithy.GenericAPIError{Code: "AccessDenied", Message: "sensitive-provider-detail"}, "trust_denied"},
		{"missing role", &smithy.GenericAPIError{Code: "AccessDenied", Message: "sensitive-provider-detail cannot be found"}, "role_not_found"},
		{"throttled", &smithy.GenericAPIError{Code: "Throttling", Message: "sensitive-provider-detail"}, "throttled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := assumeRoleFrom(&fakeSTS{err: test.err}, RoleSpec{RoleARN: testRoleARN})
			_, err := provider.Retrieve(context.Background())
			if ErrorCode(err) != test.code || strings.Contains(err.Error(), "sensitive-provider-detail") {
				t.Fatalf("error = %v, code = %q", err, ErrorCode(err))
			}
		})
	}
}

func TestErrorVocabularyMapping(t *testing.T) {
	timeoutError := &net.DNSError{Err: "sensitive-provider-detail", IsTimeout: true}
	networkError := &net.DNSError{Err: "sensitive-provider-detail"}
	for _, test := range []struct {
		name string
		err  error
		code string
	}{
		{"region disabled", &smithy.GenericAPIError{Code: "RegionDisabledException", Message: "sensitive-provider-detail"}, "region_disabled"},
		{"throttled", &smithy.GenericAPIError{Code: "ThrottlingException", Message: "sensitive-provider-detail"}, "throttled"},
		{"request limit", &smithy.GenericAPIError{Code: "RequestLimitExceeded", Message: "sensitive-provider-detail"}, "throttled"},
		{"request timeout", &smithy.GenericAPIError{Code: "RequestTimeout", Message: "sensitive-provider-detail"}, "timeout"},
		{"external id", &smithy.GenericAPIError{Code: "AccessDenied", Message: "sensitive-provider-detail ExternalId"}, "external_id_mismatch"},
		{"role not found", &smithy.GenericAPIError{Code: "AccessDenied", Message: "sensitive-provider-detail no such role"}, "role_not_found"},
		{"trust denied", &smithy.GenericAPIError{Code: "AccessDenied", Message: "sensitive-provider-detail"}, "trust_denied"},
		{"expired token", &smithy.GenericAPIError{Code: "ExpiredToken", Message: "sensitive-provider-detail"}, "no_server_identity"},
		{"bad key", &smithy.GenericAPIError{Code: "InvalidClientTokenId", Message: "sensitive-provider-detail"}, "no_server_identity"},
		{"bad duration", &smithy.GenericAPIError{Code: "InvalidParameterValue", Message: "sensitive-provider-detail duration"}, "session_duration_invalid"},
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"net timeout", timeoutError, "timeout"},
		{"net failure", networkError, "network"},
		{"unknown provider failure", errors.New("sensitive-provider-detail"), "network"},
		{"no error", nil, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			mapped := classifyError(test.err)
			if ErrorCode(mapped) != test.code {
				t.Fatalf("code = %q, want %q", ErrorCode(mapped), test.code)
			}
			if mapped != nil && strings.Contains(mapped.Error(), "sensitive-provider-detail") {
				t.Fatalf("error leaked provider detail: %v", mapped)
			}
		})
	}
}

func TestCallerIdentityShapeAndMapping(t *testing.T) {
	service := &fakeSTS{arn: "arn:aws:iam::" + testAccount + ":user/versus", account: testAccount}
	arn, account, err := callerIdentity(context.Background(), service)
	if err != nil || arn != service.arn || account != testAccount || service.callerInput == nil {
		t.Fatalf("identity = %q %q %v", arn, account, err)
	}
	if _, _, err := callerIdentity(context.Background(), &fakeSTS{err: &smithy.GenericAPIError{Code: "AccessDenied", Message: "sensitive-provider-detail"}}); ErrorCode(err) != "trust_denied" {
		t.Fatalf("mapping error = %v", err)
	}
	if _, _, err := callerIdentity(context.Background(), &fakeSTS{arn: "", account: ""}); ErrorCode(err) != "no_server_identity" {
		t.Fatalf("empty identity error = %v", err)
	}
}

func TestCallerIdentityRejectsBadConfig(t *testing.T) {
	if _, _, err := CallerIdentity(context.Background(), aws.Config{Region: "us-east"}); err == nil {
		t.Fatal("invalid region accepted")
	}
	if _, _, err := CallerIdentity(context.Background(), aws.Config{Region: "us-east-1"}); ErrorCode(err) != "no_server_identity" {
		t.Fatal("missing credentials accepted")
	}
}

func clearCredentialEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_ROLE_SESSION_NAME",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_CONTAINER_AUTHORIZATION_TOKEN", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE",
		"AWS_PROFILE", "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_STS", "AWS_EC2_METADATA_SERVICE_ENDPOINT",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "missing-credentials"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "missing-config"))
}

func unusedSTSFactory(t *testing.T) stsFactory {
	return func(string, aws.CredentialsProvider, aws.HTTPClient) stsAPI {
		t.Fatal("credential mode unexpectedly requested STS")
		return nil
	}
}

func TestBaseProviderStaticEnvironment(t *testing.T) {
	clearCredentialEnvironment(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-value")
	t.Setenv("AWS_SESSION_TOKEN", "session-value")
	provider, err := baseProvider("us-east-1", NewHTTPClient(), unusedSTSFactory(t))
	if err != nil {
		t.Fatal(err)
	}
	credential, err := provider.Retrieve(context.Background())
	if err != nil || credential.AccessKeyID != "AKIDEXAMPLE" || credential.SessionToken != "session-value" {
		t.Fatalf("credential = %+v, err = %v", credential, err)
	}
}

func TestBaseProviderRejectsIncompleteEnvironment(t *testing.T) {
	clearCredentialEnvironment(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	if _, err := baseProvider("us-east-1", NewHTTPClient(), unusedSTSFactory(t)); ErrorCode(err) != "no_server_identity" {
		t.Fatalf("error = %v", err)
	}
}

func TestBaseProviderWebIdentity(t *testing.T) {
	clearCredentialEnvironment(t)
	tokenFile := filepath.Join(t.TempDir(), "web-identity-token")
	if err := os.WriteFile(tokenFile, []byte("signed-web-identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", tokenFile)
	t.Setenv("AWS_ROLE_ARN", testRoleARN)
	t.Setenv("AWS_ROLE_SESSION_NAME", "irsa-session")
	service := new(fakeSTS)
	provider, err := baseProvider("us-east-1", NewHTTPClient(), func(string, aws.CredentialsProvider, aws.HTTPClient) stsAPI { return service })
	if err != nil {
		t.Fatal(err)
	}
	credential, err := provider.Retrieve(context.Background())
	if err != nil || credential.AccessKeyID != "ASIAASSUMED" {
		t.Fatalf("credential = %+v, err = %v", credential, err)
	}
	if service.webIdentityInput == nil || aws.ToString(service.webIdentityInput.WebIdentityToken) != "signed-web-identity" || aws.ToString(service.webIdentityInput.RoleSessionName) != "irsa-session" {
		t.Fatalf("web identity request = %+v", service.webIdentityInput)
	}
}

func TestBaseProviderSharedProfile(t *testing.T) {
	clearCredentialEnvironment(t)
	credentials := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(credentials, []byte("[default]\naws_access_key_id = AKIDPROFILE\naws_secret_access_key = profile-secret\nregion = us-east-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentials)
	provider, err := baseProvider("us-east-1", NewHTTPClient(), unusedSTSFactory(t))
	if err != nil {
		t.Fatal(err)
	}
	credential, err := provider.Retrieve(context.Background())
	if err != nil || credential.AccessKeyID != "AKIDPROFILE" {
		t.Fatalf("credential = %+v, err = %v", credential, err)
	}
}

func TestBaseProviderExplicitProfileMustExist(t *testing.T) {
	clearCredentialEnvironment(t)
	t.Setenv("AWS_PROFILE", "production")
	if _, err := baseProvider("us-east-1", NewHTTPClient(), unusedSTSFactory(t)); ErrorCode(err) != "no_server_identity" {
		t.Fatalf("error = %v", err)
	}
}

func TestSharedProfileRejectsActiveDirectives(t *testing.T) {
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

func TestSharedProfileIgnoresBenignSettings(t *testing.T) {
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

func TestContainerCredentialEndpointPolicy(t *testing.T) {
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func metadataResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"X-Aws-Ec2-Metadata-Token-Ttl-Seconds": []string{"21600"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func TestBaseProviderContainerCredentials(t *testing.T) {
	clearCredentialEnvironment(t)
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", "http://169.254.170.23/credentials")
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "169.254.170.23" {
			t.Errorf("container request host = %q", request.URL.Host)
		}
		return metadataResponse(request, http.StatusOK, `{"AccessKeyId":"ASIACONTAINER","SecretAccessKey":"container-secret","Token":"container-session","Expiration":"2030-01-01T00:00:00Z"}`), nil
	})
	provider, err := baseProvider("us-east-1", &http.Client{Transport: transport}, unusedSTSFactory(t))
	if err != nil {
		t.Fatal(err)
	}
	credential, err := provider.Retrieve(context.Background())
	if err != nil || credential.AccessKeyID != "ASIACONTAINER" {
		t.Fatalf("credential = %+v, err = %v", credential, err)
	}
}

func TestBaseProviderIMDS(t *testing.T) {
	clearCredentialEnvironment(t)
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/latest/api/token":
			return metadataResponse(request, http.StatusOK, "imds-token"), nil
		case "/latest/meta-data/iam/security-credentials/", "/latest/meta-data/iam/security-credentials":
			return metadataResponse(request, http.StatusOK, "versus-role"), nil
		case "/latest/meta-data/iam/security-credentials/versus-role":
			return metadataResponse(request, http.StatusOK, `{"Code":"Success","AccessKeyId":"ASIAIMDS","SecretAccessKey":"imds-secret","Token":"imds-session","Expiration":"2030-01-01T00:00:00Z"}`), nil
		default:
			return metadataResponse(request, http.StatusNotFound, ""), nil
		}
	})
	provider, err := baseProvider("us-east-1", &http.Client{Transport: transport}, unusedSTSFactory(t))
	if err != nil {
		t.Fatal(err)
	}
	credential, err := provider.Retrieve(context.Background())
	if err != nil || credential.AccessKeyID != "ASIAIMDS" {
		t.Fatalf("credential = %+v, err = %v", credential, err)
	}
}

func TestBaseProviderRejectsAmbientEndpointsAndRegions(t *testing.T) {
	clearCredentialEnvironment(t)
	t.Setenv("AWS_ENDPOINT_URL", "https://attacker.example")
	if _, err := baseProvider("us-east-1", NewHTTPClient(), unusedSTSFactory(t)); ErrorCode(err) != "no_server_identity" {
		t.Fatalf("endpoint override error = %v", err)
	}
	t.Setenv("AWS_ENDPOINT_URL", "")
	if _, err := baseProvider("us-east", NewHTTPClient(), unusedSTSFactory(t)); ErrorCode(err) != "no_server_identity" {
		t.Fatalf("region error = %v", err)
	}
}

func TestSTSEndpointsArePartitionPinned(t *testing.T) {
	for region, want := range map[string]string{
		"us-east-1":     "https://sts.us-east-1.amazonaws.com",
		"us-gov-west-1": "https://sts.us-gov-west-1.amazonaws.com",
		"cn-north-1":    "https://sts.cn-north-1.amazonaws.com.cn",
	} {
		if got := stsEndpoint(region); got != want {
			t.Errorf("stsEndpoint(%q) = %q, want %q", region, got, want)
		}
	}
}

func TestRoleSessionNameIsBounded(t *testing.T) {
	for _, name := range []string{"", "bad name", strings.Repeat("a", 65)} {
		if got := roleSessionName(RoleSpec{SessionName: name}); got == name || !strings.HasPrefix(got, "versus") || len(got) > sessionNameMax {
			t.Errorf("fallback session name = %q", got)
		}
	}
	if got := roleSessionName(RoleSpec{SessionName: "reader@example"}); got != "reader@example" {
		t.Errorf("valid session name rewritten to %q", got)
	}
}

func TestCredentialResolutionDoesNotUseProcessCredentials(t *testing.T) {
	for _, path := range []string{"awsauth.go", "credentials.go"} {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{
			`"os/exec"`,
			`github.com/aws/aws-sdk-go-v2/config`,
			`credentials/processcreds`,
			`credentials/ssocreds`,
		} {
			if strings.Contains(string(source), forbidden) {
				t.Errorf("%s contains forbidden credential dependency %q", path, forbidden)
			}
		}
	}
}
