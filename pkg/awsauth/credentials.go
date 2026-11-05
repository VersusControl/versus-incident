package awsauth

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go-v2/credentials/endpointcreds"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
)

const (
	awsIMDSEndpoint       = "http://169.254.169.254"
	awsECSCredentialsHost = "169.254.170.2"
	awsEKSContainerHost   = "169.254.170.23"
	maxTokenBytes         = int64(64 << 10)
	maxCredentialBytes    = int64(1 << 20)
)

type boundedIdentityTokenFile string

func (path boundedIdentityTokenFile) GetIdentityToken() ([]byte, error) {
	data, err := readSecureBoundedFile(string(path), maxTokenBytes)
	token := strings.TrimSpace(string(data))
	if err != nil || token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("web identity token unavailable")
	}
	return []byte(token), nil
}

type stsFactory func(region string, provider aws.CredentialsProvider, client aws.HTTPClient) stsAPI

// BaseProvider resolves the Versus principal from the host environment. The
// chain is static environment, web identity, shared profile (static keys
// only), container credentials, then IMDS. Any ambiguous or unsupported
// provider fails closed as no_server_identity.
func BaseProvider(region string) (aws.CredentialsProvider, error) {
	return baseProvider(region, NewHTTPClient(), newSTSClient)
}

func baseProvider(region string, client aws.HTTPClient, newSTS stsFactory) (aws.CredentialsProvider, error) {
	if rejectAWSEndpointEnvironment() != nil || ValidateRegion(region) != nil {
		return nil, fmt.Errorf("%w", ErrNoServerIdentity)
	}
	profile, profileExplicit := os.Getenv("AWS_PROFILE"), os.Getenv("AWS_PROFILE") != ""
	if profile == "" {
		profile = "default"
	}
	if !validProfileName(profile) {
		return nil, fmt.Errorf("%w", ErrNoServerIdentity)
	}
	profileValues := make(map[string]string)
	profileFound := false
	if profileExplicit || !hasExplicitAWSProviderEnvironment() {
		var err error
		profileValues, profileFound, err = loadAWSStaticProfile(profile)
		if err != nil || (profileExplicit && (!profileFound || profileValues["aws_access_key_id"] == "" || profileValues["aws_secret_access_key"] == "")) {
			return nil, fmt.Errorf("%w", ErrNoServerIdentity)
		}
	}
	return nativeProvider(region, profileValues, profileFound, newSTS, client)
}

func nativeProvider(region string, profile map[string]string, profileFound bool, newSTS stsFactory, client aws.HTTPClient) (aws.CredentialsProvider, error) {
	accessKey, secretKey := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY")
	if accessKey != "" || secretKey != "" || os.Getenv("AWS_SESSION_TOKEN") != "" {
		if accessKey == "" || secretKey == "" {
			return nil, fmt.Errorf("%w", ErrNoServerIdentity)
		}
		return aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(accessKey, secretKey, os.Getenv("AWS_SESSION_TOKEN"))), nil
	}

	webIdentityFile, webIdentityRole := os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE"), os.Getenv("AWS_ROLE_ARN")
	if webIdentityFile != "" || webIdentityRole != "" {
		if !validFederatedTokenPath(webIdentityFile) || ValidateRoleARN(webIdentityRole) != nil {
			return nil, fmt.Errorf("%w", ErrNoServerIdentity)
		}
		provider := stscreds.NewWebIdentityRoleProvider(newSTS(region, aws.AnonymousCredentials{}, client), webIdentityRole, boundedIdentityTokenFile(webIdentityFile), func(options *stscreds.WebIdentityRoleOptions) {
			options.RoleSessionName = roleSessionName(RoleSpec{SessionName: os.Getenv("AWS_ROLE_SESSION_NAME")})
		})
		return mappedProvider{aws.NewCredentialsCache(provider)}, nil
	}

	if profileFound && (profile["aws_access_key_id"] != "" || profile["aws_secret_access_key"] != "" || profile["aws_session_token"] != "") {
		accessKey, secretKey = profile["aws_access_key_id"], profile["aws_secret_access_key"]
		if accessKey == "" || secretKey == "" {
			return nil, fmt.Errorf("%w", ErrNoServerIdentity)
		}
		return aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(accessKey, secretKey, profile["aws_session_token"])), nil
	}

	if endpoint, configured, err := containerCredentialsEndpoint(); err != nil {
		return nil, fmt.Errorf("%w", ErrNoServerIdentity)
	} else if configured {
		token, tokenFile := os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN"), os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE")
		if token != "" && tokenFile != "" {
			return nil, fmt.Errorf("%w", ErrNoServerIdentity)
		}
		if token != "" && (len(token) > int(maxTokenBytes) || strings.ContainsAny(token, "\x00\r\n")) {
			return nil, fmt.Errorf("%w", ErrNoServerIdentity)
		}
		if tokenFile != "" && !validFederatedTokenPath(tokenFile) {
			return nil, fmt.Errorf("%w", ErrNoServerIdentity)
		}
		provider := endpointcreds.New(endpoint, func(options *endpointcreds.Options) {
			options.HTTPClient = client
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
		return mappedProvider{aws.NewCredentialsCache(provider)}, nil
	}

	imdsClient := imds.New(imds.Options{
		Endpoint:          awsIMDSEndpoint,
		EndpointMode:      imds.EndpointModeStateIPv4,
		HTTPClient:        client,
		ClientEnableState: imds.ClientEnabled,
		EnableFallback:    aws.FalseTernary,
	})
	provider := ec2rolecreds.New(func(options *ec2rolecreds.Options) { options.Client = imdsClient })
	return mappedProvider{aws.NewCredentialsCache(provider)}, nil
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
	data, err := readSecureBoundedFile(path, maxCredentialBytes)
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
		if !ok || key == "" || strings.ContainsAny(value, "\x00\r\n") || dangerousProfileDirective(key) {
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

func dangerousProfileDirective(key string) bool {
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

func validProfileName(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\x00\r\n")
}

func validFederatedTokenPath(value string) bool {
	return value != "" && len(value) <= 4096 && filepath.IsAbs(value) && filepath.Clean(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func readSecureBoundedFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("credential file is not private")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		return nil, errors.New("bounded credential file read failed")
	}
	return data, nil
}
