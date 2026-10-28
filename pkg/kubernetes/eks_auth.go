package kubernetes

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go-v2/credentials/endpointcreds"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const (
	emptySHA256           = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	awsIMDSEndpoint       = "http://169.254.169.254"
	awsECSCredentialsHost = "169.254.170.2"
	awsEKSContainerHost   = "169.254.170.23"
)

var awsRegionPattern = regexp.MustCompile(`^[a-z]{2}(?:-gov)?-[a-z]+-\d+$`)

type boundedIdentityTokenFile string

type eksSTSAPI interface {
	stscreds.AssumeRoleAPIClient
	stscreds.AssumeRoleWithWebIdentityAPIClient
}

type eksSTSFactory func(string, aws.CredentialsProvider) eksSTSAPI

func (path boundedIdentityTokenFile) GetIdentityToken() ([]byte, error) {
	data, err := readSecureBoundedFile(string(path), maxTokenBytes)
	token := strings.TrimSpace(string(data))
	if err != nil || token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("web identity token unavailable")
	}
	return []byte(token), nil
}

func newEKSCredentialSource(options AuthOptions) (CredentialSource, error) {
	return newEKSCredentialSourceWithSTSFactory(options, func(region string, provider aws.CredentialsProvider) eksSTSAPI {
		return newFixedSTSClient(region, provider)
	})
}

func newEKSCredentialSourceWithSTSFactory(options AuthOptions, newSTS eksSTSFactory) (CredentialSource, error) {
	if !validCloudIdentifier(options.EKSClusterName, 256) || rejectAWSEndpointEnvironment() != nil {
		return nil, errors.New("kubernetes: invalid EKS configuration")
	}
	profile, profileExplicit := options.EKSProfile, options.EKSProfile != ""
	if profile == "" {
		profile = strings.TrimSpace(os.Getenv("AWS_PROFILE"))
		profileExplicit = profile != ""
	}
	if profile == "" {
		profile = "default"
	}
	if !validCloudIdentifier(profile, 256) {
		return nil, errors.New("kubernetes: invalid EKS configuration")
	}
	profileValues := make(map[string]string)
	profileFound := false
	regionConfigured := firstNonEmpty(options.EKSRegion, os.Getenv("AWS_REGION"), os.Getenv("AWS_DEFAULT_REGION")) != ""
	if profileExplicit || !regionConfigured || !hasExplicitAWSProviderEnvironment() {
		var err error
		profileValues, profileFound, err = loadAWSStaticProfile(profile)
		if err != nil || (profileExplicit && (!profileFound || profileValues["aws_access_key_id"] == "" || profileValues["aws_secret_access_key"] == "")) {
			return nil, errors.New("kubernetes: EKS credential configuration unavailable")
		}
	}
	region := firstNonEmpty(options.EKSRegion, os.Getenv("AWS_REGION"), os.Getenv("AWS_DEFAULT_REGION"), profileValues["region"])
	if !awsRegionPattern.MatchString(region) {
		return nil, errors.New("kubernetes: EKS region unavailable")
	}

	provider, err := nativeAWSCredentialProvider(region, profileValues, profileFound, newSTS)
	if err != nil {
		return nil, errors.New("kubernetes: EKS credential configuration unavailable")
	}
	if options.EKSRoleARN != "" {
		if !validCloudIdentifier(options.EKSRoleARN, 2048) {
			return nil, errors.New("kubernetes: invalid EKS configuration")
		}
		provider = assumeRoleCredentials(newSTS(region, provider), options.EKSRoleARN)
	}
	return newEKSSource(options.EKSClusterName, region, provider, time.Now), nil
}

func nativeAWSCredentialProvider(region string, profile map[string]string, profileFound bool, newSTS eksSTSFactory) (aws.CredentialsProvider, error) {
	accessKey, secretKey := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY")
	if accessKey != "" || secretKey != "" || os.Getenv("AWS_SESSION_TOKEN") != "" {
		if accessKey == "" || secretKey == "" {
			return nil, errors.New("incomplete environment credentials")
		}
		return aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(accessKey, secretKey, os.Getenv("AWS_SESSION_TOKEN"))), nil
	}

	webIdentityFile, webIdentityRole := os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE"), os.Getenv("AWS_ROLE_ARN")
	if webIdentityFile != "" || webIdentityRole != "" {
		if !validFederatedTokenPath(webIdentityFile) || !validCloudIdentifier(webIdentityRole, 2048) {
			return nil, errors.New("invalid web identity configuration")
		}
		provider := stscreds.NewWebIdentityRoleProvider(newSTS(region, aws.AnonymousCredentials{}), webIdentityRole, boundedIdentityTokenFile(webIdentityFile), func(options *stscreds.WebIdentityRoleOptions) {
			options.RoleSessionName = validSessionName(os.Getenv("AWS_ROLE_SESSION_NAME"))
		})
		return aws.NewCredentialsCache(provider), nil
	}

	if profileFound && (profile["aws_access_key_id"] != "" || profile["aws_secret_access_key"] != "" || profile["aws_session_token"] != "") {
		accessKey, secretKey = profile["aws_access_key_id"], profile["aws_secret_access_key"]
		if accessKey == "" || secretKey == "" {
			return nil, errors.New("incomplete profile credentials")
		}
		return aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(accessKey, secretKey, profile["aws_session_token"])), nil
	}

	if endpoint, configured, err := containerCredentialsEndpoint(); err != nil {
		return nil, err
	} else if configured {
		token, tokenFile := os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN"), os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE")
		if token != "" && tokenFile != "" {
			return nil, errors.New("conflicting container authorization tokens")
		}
		if token != "" && (len(token) > int(maxTokenBytes) || strings.ContainsAny(token, "\x00\r\n")) {
			return nil, errors.New("invalid container authorization token")
		}
		if tokenFile != "" && !validFederatedTokenPath(tokenFile) {
			return nil, errors.New("invalid container authorization token file")
		}
		provider := endpointcreds.New(endpoint, func(options *endpointcreds.Options) {
			options.HTTPClient = fixedCredentialHTTPClient()
			if tokenFile != "" {
				options.AuthorizationTokenProvider = endpointcreds.TokenProviderFunc(func() (string, error) {
					data, err := readSecureBoundedFile(tokenFile, maxTokenBytes)
					token := strings.TrimSpace(string(data))
					if err != nil || token == "" || strings.ContainsAny(token, "\r\n") {
						return "", errors.New("container authorization token unavailable")
					}
					return token, nil
				})
			} else {
				options.AuthorizationToken = token
			}
		})
		return aws.NewCredentialsCache(provider), nil
	}

	imdsClient := imds.New(imds.Options{
		Endpoint:          awsIMDSEndpoint,
		EndpointMode:      imds.EndpointModeStateIPv4,
		HTTPClient:        fixedCredentialHTTPClient(),
		ClientEnableState: imds.ClientEnabled,
		EnableFallback:    aws.FalseTernary,
	})
	provider := ec2rolecreds.New(func(options *ec2rolecreds.Options) { options.Client = imdsClient })
	return aws.NewCredentialsCache(provider), nil
}

func hasExplicitAWSProviderEnvironment() bool {
	for _, name := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
	} {
		if os.Getenv(name) != "" {
			return true
		}
	}
	return false
}

func newFixedSTSClient(region string, provider aws.CredentialsProvider) *sts.Client {
	endpoint := awsSTSEndpoint(region)
	return sts.New(sts.Options{Region: region, Credentials: provider, HTTPClient: fixedCredentialHTTPClient(), BaseEndpoint: &endpoint, RetryMaxAttempts: 3})
}

func awsSTSEndpoint(region string) string {
	if strings.HasPrefix(region, "cn-") {
		return "https://sts." + region + ".amazonaws.com.cn"
	}
	return "https://sts." + region + ".amazonaws.com"
}

func containerCredentialsEndpoint() (string, bool, error) {
	relative, full := os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"), os.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI")
	if relative != "" && full != "" {
		return "", false, errors.New("conflicting container credential endpoints")
	}
	if relative != "" {
		if !strings.HasPrefix(relative, "/") || strings.ContainsAny(relative, "\x00\r\n?#%") || filepath.Clean(relative) != relative {
			return "", false, errors.New("invalid relative container credential endpoint")
		}
		return "http://" + awsECSCredentialsHost + relative, true, nil
	}
	if full == "" {
		return "", false, nil
	}
	parsed, err := url.Parse(full)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path == "" || filepath.Clean(parsed.Path) != parsed.Path {
		return "", false, errors.New("invalid full container credential endpoint")
	}
	host := parsed.Hostname()
	allowedLinkLocal := parsed.Scheme == "http" && parsed.Port() == "" && (host == awsECSCredentialsHost || host == awsEKSContainerHost)
	allowedLoopback := parsed.Scheme == "http" && (host == "127.0.0.1" || host == "::1")
	if !allowedLinkLocal && !allowedLoopback {
		return "", false, errors.New("invalid full container credential endpoint")
	}
	return parsed.String(), true, nil
}

func rejectAWSEndpointEnvironment() error {
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if value != "" && (name == "AWS_ENDPOINT_URL" || strings.HasPrefix(name, "AWS_ENDPOINT_URL_") || name == "AWS_EC2_METADATA_SERVICE_ENDPOINT") {
			return errors.New("ambient AWS endpoint override rejected")
		}
	}
	return nil
}

func loadAWSStaticProfile(profile string) (map[string]string, bool, error) {
	values := make(map[string]string)
	found := false
	configSection := "profile " + profile
	if profile == "default" {
		configSection = "default"
	}
	paths := []struct{ path, section string }{
		{awsSharedPath("AWS_SHARED_CREDENTIALS_FILE", "credentials"), profile},
		{awsSharedPath("AWS_CONFIG_FILE", "config"), configSection},
	}
	for _, candidate := range paths {
		parsed, present, err := parseAWSProfileFile(candidate.path, candidate.section)
		if err != nil {
			return nil, false, err
		}
		if !present {
			continue
		}
		found = true
		for key, value := range parsed {
			if existing := values[key]; existing != "" && existing != value {
				return nil, false, errors.New("conflicting profile value")
			}
			values[key] = value
		}
	}
	return values, found, nil
}

func parseAWSProfileFile(path, selected string) (map[string]string, bool, error) {
	data, err := readSecureBoundedFile(path, maxCredentialFileBytes)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	allowed := map[string]bool{"aws_access_key_id": true, "aws_secret_access_key": true, "aws_session_token": true, "region": true}
	benign := map[string]bool{"output": true, "cli_pager": true, "retry_mode": true, "max_attempts": true}
	values := make(map[string]string)
	active, found := false, false
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			active = strings.TrimSpace(line[1:len(line)-1]) == selected
			found = found || active
			continue
		}
		if !active {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(value)
		if !ok || key == "" || strings.ContainsAny(value, "\x00\r\n") || dangerousAWSProfileDirective(key) {
			return nil, false, errors.New("unsupported shared profile")
		}
		if benign[key] || !allowed[key] {
			continue
		}
		if value == "" || values[key] != "" {
			return nil, false, errors.New("unsupported shared profile")
		}
		values[key] = value
	}
	if scanner.Err() != nil {
		return nil, false, errors.New("invalid shared profile")
	}
	return values, found, nil
}

func dangerousAWSProfileDirective(key string) bool {
	if key == "aws_access_key_id" || key == "aws_secret_access_key" || key == "aws_session_token" {
		return false
	}
	for _, fragment := range []string{"credential", "sso", "source_profile", "role_arn", "web_identity", "endpoint", "services", "plugin", "process", "command", "exec", "token", "auth"} {
		if strings.Contains(key, fragment) {
			return true
		}
	}
	return false
}

func awsSharedPath(environment, name string) string {
	if path := strings.TrimSpace(os.Getenv(environment)); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".aws", name)
	}
	return filepath.Join(home, ".aws", name)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func validSessionName(value string) string {
	if value != "" && validCloudIdentifier(value, 64) {
		return value
	}
	return "versus-kubernetes"
}

func assumeRoleCredentials(client stscreds.AssumeRoleAPIClient, roleARN string) aws.CredentialsProvider {
	return aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(client, roleARN))
}

func newEKSSource(clusterName, region string, provider aws.CredentialsProvider, now func() time.Time) CredentialSource {
	source := &expiringBearerSource{now: now, skew: time.Minute}
	source.refresh = func(ctx context.Context) (string, time.Time, error) {
		credential, err := provider.Retrieve(ctx)
		if err != nil {
			return "", time.Time{}, errors.New("kubernetes: EKS credentials unavailable")
		}
		requestURL, _ := url.Parse(awsSTSEndpoint(region) + "/")
		query := requestURL.Query()
		query.Set("Action", "GetCallerIdentity")
		query.Set("Version", "2011-06-15")
		query.Set("X-Amz-Expires", "60")
		requestURL.RawQuery = query.Encode()
		request := &http.Request{Method: http.MethodGet, URL: requestURL, Header: http.Header{"x-k8s-aws-id": []string{clusterName}}}
		signedURL, signedHeaders, err := v4.NewSigner().PresignHTTP(ctx, credential, request, emptySHA256, "sts", region, now())
		if err != nil || signedHeaders.Get("x-k8s-aws-id") == "" {
			return "", time.Time{}, errors.New("kubernetes: EKS token signing failed")
		}
		token := "k8s-aws-v1." + base64.RawURLEncoding.EncodeToString([]byte(signedURL))
		return token, now().Add(14 * time.Minute), nil
	}
	return source
}

func validCloudIdentifier(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && !strings.ContainsAny(value, "\x00\r\n")
}
