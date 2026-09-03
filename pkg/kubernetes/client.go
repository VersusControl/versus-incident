// Package kubernetes provides bounded, read-only access to Kubernetes APIs.
package kubernetes

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const (
	defaultTimeout      = 10 * time.Second
	defaultMaxBodyBytes = int64(4 << 20)
	maxTokenBytes       = int64(64 << 10)
	maxCABytes          = int64(4 << 20)
)

var (
	ErrInvalidEndpoint  = errors.New("kubernetes: invalid endpoint")
	ErrUnauthorized     = errors.New("kubernetes: unauthorized")
	ErrForbidden        = errors.New("kubernetes: forbidden")
	ErrNotFound         = errors.New("kubernetes: not found")
	ErrResponseTooLarge = errors.New("kubernetes: response too large")
	ErrRedirect         = errors.New("kubernetes: redirect refused")
	ErrInvalidArguments = errors.New("kubernetes: invalid arguments")
	ErrOperationBudget  = errors.New("kubernetes: operation budget exhausted")
)

// Config defines one Kubernetes API connection. Credentials are read from
// TokenFile for every request so projected service-account rotation is honored.
type Config struct {
	Endpoint             string
	TokenFile            string
	CAFile               string
	CAData               []byte
	ServerName           string
	Credentials          CredentialSource
	Timeout              time.Duration
	MaxBodyBytes         int64
	AllowLoopbackHTTP    bool
	AllowLoopback        bool
	AllowPrivateNetworks bool
	EndpointCIDRs        []string
}

type endpointPolicy struct {
	allowLoopback bool
	allowPrivate  bool
	allowedCIDRs  []*net.IPNet
}

type ipResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type contextDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

// Client is a read-only, bounded Kubernetes API transport.
type Client struct {
	base         *url.URL
	credentials  CredentialSource
	timeout      time.Duration
	maxBodyBytes int64
	http         *http.Client
}

// NewClient validates transport policy and constructs a read-only API client.
func NewClient(config Config) (*Client, error) {
	policy, err := newEndpointPolicy(config)
	if err != nil {
		return nil, err
	}
	base, err := validateEndpoint(config.Endpoint, policy)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: strings.TrimSpace(config.ServerName)}
	if config.CAFile != "" && len(config.CAData) > 0 {
		return nil, errors.New("kubernetes: conflicting CA configuration")
	}
	caPEM := config.CAData
	if config.CAFile != "" {
		caPEM, err = readBoundedFile(config.CAFile, maxCABytes)
		if err != nil {
			return nil, errors.New("kubernetes: CA unavailable")
		}
	}
	if len(caPEM) > int(maxCABytes) {
		return nil, errors.New("kubernetes: CA unavailable")
	}
	if len(caPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("kubernetes: CA file contains no certificates")
		}
		tlsConfig.RootCAs = pool
	}
	credentials := config.Credentials
	if credentials != nil && config.TokenFile != "" {
		return nil, errors.New("kubernetes: conflicting credential configuration")
	}
	if credentials == nil && config.TokenFile != "" {
		credentials = tokenFileCredential(config.TokenFile)
	}
	if credentials != nil {
		tlsConfig.GetClientCertificate = credentials.ClientCertificate
	}
	timeout := config.Timeout
	if timeout <= 0 || timeout > time.Minute {
		timeout = defaultTimeout
	}
	maxBodyBytes := config.MaxBodyBytes
	if maxBodyBytes <= 0 || maxBodyBytes > 32<<20 {
		maxBodyBytes = defaultMaxBodyBytes
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	transport.Proxy = nil
	transport.DialContext = guardedDialContext(&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}, net.DefaultResolver, policy)
	return &Client{
		base: base, credentials: credentials, timeout: timeout, maxBodyBytes: maxBodyBytes,
		http: &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrRedirect }},
	}, nil
}

// GetJSON performs one authenticated, bounded GET and decodes its JSON body.
func (client *Client) GetJSON(ctx context.Context, apiPath string, output any) error {
	data, err := client.get(ctx, apiPath, "application/json")
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, output); err != nil {
		return errors.New("kubernetes: invalid API response")
	}
	return nil
}

func (client *Client) get(ctx context.Context, apiPath, accept string) ([]byte, error) {
	data, truncated, err := client.getBounded(ctx, apiPath, accept, client.maxBodyBytes)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, ErrResponseTooLarge
	}
	return data, nil
}

func (client *Client) getBounded(ctx context.Context, apiPath, accept string, maxBytes int64) ([]byte, bool, error) {
	if client == nil || client.http == nil {
		return nil, false, errors.New("kubernetes: client unavailable")
	}
	ctx, cancelOperation := ensureOperationBudget(ctx)
	defer cancelOperation()
	budget := operationBudgetFrom(ctx)
	if !budget.takeRequest() {
		return nil, false, ErrOperationBudget
	}
	requestURL, err := client.requestURL(apiPath)
	if err != nil {
		return nil, false, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, false, fmt.Errorf("kubernetes: create request: %w", err)
	}
	request.Header.Set("Accept", accept)
	if client.credentials != nil {
		authorization, credentialErr := client.credentials.Authorization(requestCtx)
		if credentialErr != nil {
			return nil, false, credentialErr
		}
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
	}
	response, err := client.http.Do(request)
	if err != nil {
		if errors.Is(err, ErrRedirect) {
			return nil, false, ErrRedirect
		}
		if budget.expired() {
			return nil, false, ErrOperationBudget
		}
		return nil, false, fmt.Errorf("kubernetes: request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, false, classifyStatus(response.StatusCode)
	}
	if maxBytes <= 0 || maxBytes > client.maxBodyBytes {
		maxBytes = client.maxBodyBytes
	}
	limited := io.LimitReader(response.Body, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, false, fmt.Errorf("kubernetes: read response: %w", err)
	}
	if !budget.takeBytes(int64(len(data))) {
		return nil, false, ErrOperationBudget
	}
	truncated := int64(len(data)) > maxBytes
	if truncated {
		data = data[:maxBytes]
	}
	return data, truncated, nil
}

func (client *Client) requestURL(apiPath string) (string, error) {
	if !strings.HasPrefix(apiPath, "/") || strings.ContainsAny(apiPath, "\r\n") {
		return "", ErrInvalidEndpoint
	}
	reference, err := url.Parse(apiPath)
	if err != nil || reference.IsAbs() || reference.Host != "" {
		return "", ErrInvalidEndpoint
	}
	result := *client.base
	result.Path = path.Clean("/" + strings.TrimPrefix(reference.Path, "/"))
	result.RawQuery = reference.RawQuery
	return result.String(), nil
}

func newEndpointPolicy(config Config) (endpointPolicy, error) {
	policy := endpointPolicy{allowLoopback: config.AllowLoopbackHTTP || config.AllowLoopback, allowPrivate: config.AllowPrivateNetworks}
	for _, value := range config.EndpointCIDRs {
		_, network, err := net.ParseCIDR(strings.TrimSpace(value))
		if err != nil {
			return endpointPolicy{}, ErrInvalidEndpoint
		}
		policy.allowedCIDRs = append(policy.allowedCIDRs, network)
	}
	return policy, nil
}

func validateEndpoint(value string, policy endpointPolicy) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, ErrInvalidEndpoint
	}
	if parsed.Scheme != "https" {
		if parsed.Scheme != "http" || !policy.allowLoopback || !isLoopbackHost(parsed.Hostname()) {
			return nil, ErrInvalidEndpoint
		}
	}
	if address := net.ParseIP(parsed.Hostname()); address != nil && !policy.allows(address) {
		return nil, ErrInvalidEndpoint
	}
	parsed.Path = ""
	return parsed, nil
}

func guardedDialContext(dialer contextDialer, resolver ipResolver, policy endpointPolicy) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, ErrInvalidEndpoint
		}
		addresses := []net.IPAddr{{IP: net.ParseIP(host)}}
		if addresses[0].IP == nil {
			addresses, err = resolver.LookupIPAddr(ctx, host)
			if err != nil || len(addresses) == 0 {
				return nil, ErrInvalidEndpoint
			}
		}
		for _, candidate := range addresses {
			if !policy.allows(candidate.IP) {
				return nil, ErrInvalidEndpoint
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

func (policy endpointPolicy) allows(address net.IP) bool {
	if address == nil || address.IsUnspecified() || address.IsMulticast() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() {
		return false
	}
	if address.IsLoopback() {
		return policy.allowLoopback
	}
	if !address.IsPrivate() {
		return true
	}
	if policy.allowPrivate {
		return true
	}
	for _, network := range policy.allowedCIDRs {
		if network.Contains(address) {
			return true
		}
	}
	return false
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func classifyStatus(status int) error {
	switch status {
	case http.StatusBadRequest:
		return ErrInvalidArguments
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		return ErrForbidden
	case http.StatusNotFound:
		return ErrNotFound
	default:
		return fmt.Errorf("kubernetes: API status %d", status)
	}
}
