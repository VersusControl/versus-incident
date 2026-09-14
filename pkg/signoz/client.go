package signoz

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

var allowedPaths = map[string]struct{}{QueryRangePath: {}, FieldKeysPath: {}, FieldValuesPath: {}, MetricsPath: {}}

const minimumAPIKeyLength = 8

// StatusError carries an upstream status without including its response body.
type StatusError struct {
	StatusCode int
}

func (err *StatusError) Error() string {
	switch err.StatusCode {
	case http.StatusMultipleChoices, http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return fmt.Sprintf("SigNoz endpoint redirected: status %d; configure the final address", err.StatusCode)
	case http.StatusUnauthorized:
		return "SigNoz authentication failed: status 401"
	case http.StatusForbidden:
		return "SigNoz access denied: status 403"
	case http.StatusTooManyRequests:
		return "SigNoz rate limit reached: status 429"
	default:
		return fmt.Sprintf("SigNoz read failed: status %d", err.StatusCode)
	}
}

// Retryable reports whether an ingest caller may safely retry a read.
func Retryable(err error) bool {
	var status *StatusError
	return errors.As(err, &status) && (status.StatusCode == http.StatusTooManyRequests || status.StatusCode >= 500)
}

// Client owns the shared read-only SigNoz HTTP transport.
type Client struct {
	baseURL, apiKey string
	maximumBytes    int64
	httpClient      *http.Client
}

// NewClient validates a source endpoint and constructs its read-only transport.
func NewClient(config Config, policy Policy) (*Client, error) {
	address := strings.TrimRight(strings.TrimSpace(config.Address), "/")
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("%w: address must be an http(s) origin", ErrInvalidConfig)
	}
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, fmt.Errorf("%w: api key is required", ErrInvalidConfig)
	}
	if len(config.APIKey) < minimumAPIKeyLength {
		return nil, fmt.Errorf("%w: api key must be at least %d bytes", ErrInvalidConfig, minimumAPIKeyLength)
	}
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("%w: api key requires verified HTTPS", ErrInvalidConfig)
	}
	if config.InsecureSkipVerify {
		return nil, fmt.Errorf("%w: api key requires verified HTTPS; insecure_skip_verify cannot be enabled", ErrInvalidConfig)
	}
	if !validEndpointHost(parsed.Hostname(), config.AllowLoopback, config.AllowPrivate) {
		return nil, fmt.Errorf("%w: address host is not permitted", ErrInvalidConfig)
	}
	policy = normalizePolicy(policy)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = guardedDialContext(&net.Dialer{}, config.AllowLoopback, config.AllowPrivate)
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: config.RootCAs}
	return &Client{baseURL: address, apiKey: config.APIKey, maximumBytes: policy.MaximumBytes, httpClient: &http.Client{Transport: transport, Timeout: policy.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func validEndpointHost(host string, allowLoopback, allowPrivate bool) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" || host == "metadata" || host == "metadata.google" || host == "metadata.google.internal" || host == "instance-data" || host == "instance-data.ec2.internal" {
		return false
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return allowLoopback
	}
	address := net.ParseIP(host)
	if address == nil {
		return true
	}
	return validResolvedAddress(address, allowLoopback, allowPrivate)
}

func guardedDialContext(dialer *net.Dialer, allowLoopback, allowPrivate bool) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("SigNoz read failed: invalid destination")
		}
		addresses := []net.IPAddr{{IP: net.ParseIP(host)}}
		if addresses[0].IP == nil {
			addresses, err = net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil || len(addresses) == 0 {
				return nil, fmt.Errorf("SigNoz read failed: destination resolution failed")
			}
		}
		for _, candidate := range addresses {
			if !validResolvedAddress(candidate.IP, allowLoopback, allowPrivate) {
				return nil, fmt.Errorf("SigNoz read failed: destination is not permitted")
			}
		}
		var lastErr error
		for _, candidate := range addresses {
			connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}
}

func validResolvedAddress(address net.IP, allowLoopback, allowPrivate bool) bool {
	if address == nil || address.IsUnspecified() || address.IsMulticast() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() {
		return false
	}
	if address.IsLoopback() {
		return allowLoopback
	}
	return allowPrivate || !address.IsPrivate()
}

// Endpoint returns one allowlisted endpoint URL.
func (client *Client) Endpoint(path string) (string, error) {
	if _, ok := allowedPaths[path]; !ok {
		return "", fmt.Errorf("%w: endpoint is not allowlisted", ErrInvalidArgument)
	}
	return client.baseURL + path, nil
}

// Do executes one bounded request against an allowlisted read endpoint.
func (client *Client) Do(ctx context.Context, method, path string, query url.Values, payload any) ([]byte, error) {
	endpoint, err := client.Endpoint(path)
	if err != nil {
		return nil, err
	}
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode SigNoz request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("create SigNoz request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("SIGNOZ-API-KEY", client.apiKey)
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("SigNoz read failed: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, client.maximumBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read SigNoz response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &StatusError{StatusCode: response.StatusCode}
	}
	if int64(len(data)) > client.maximumBytes {
		return nil, ErrResponseTooLarge
	}
	return data, nil
}
