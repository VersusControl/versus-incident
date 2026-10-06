package k8s

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	kubernetes "github.com/VersusControl/versus-incident/pkg/kubernetes"
)

const maxResponseBytes = 4 << 20
const maxRequestBytes = 64 << 10

var ErrRequestFailed = errors.New("kubernetes action request failed")

type API interface {
	Do(context.Context, string, string, url.Values, []byte) ([]byte, error)
}

type ClientConfig struct {
	Endpoint             string
	CAFile               string
	CAData               []byte
	ServerName           string
	Credentials          kubernetes.CredentialSource
	Timeout              time.Duration
	AllowLoopback        bool
	AllowPrivateNetworks bool
	EndpointCIDRs        []string
}

type endpointPolicy struct {
	allowLoopback bool
	allowPrivate  bool
	allowedCIDRs  []*net.IPNet
}

type client struct {
	base        *url.URL
	credentials kubernetes.CredentialSource
	timeout     time.Duration
	http        *http.Client
}

func NewClient(config ClientConfig) (API, error) {
	policy := endpointPolicy{allowLoopback: config.AllowLoopback, allowPrivate: config.AllowPrivateNetworks}
	for _, raw := range config.EndpointCIDRs {
		_, network, err := net.ParseCIDR(strings.TrimSpace(raw))
		if err != nil {
			return nil, errors.New("kubernetes action endpoint policy is invalid")
		}
		policy.allowedCIDRs = append(policy.allowedCIDRs, network)
	}
	endpoint, err := url.Parse(strings.TrimSpace(config.Endpoint))
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "" && endpoint.Path != "/" {
		return nil, errors.New("kubernetes action endpoint is invalid")
	}
	if endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && policy.allowLoopback && isLoopbackHost(endpoint.Hostname())) {
		return nil, errors.New("kubernetes action endpoint must use HTTPS")
	}
	if config.Credentials == nil {
		return nil, errors.New("kubernetes actor credential is unavailable")
	}
	if config.CAFile != "" && len(config.CAData) != 0 {
		return nil, errors.New("kubernetes action CA configuration conflicts")
	}
	if len(config.CAData) > 4<<20 {
		return nil, errors.New("kubernetes action CA is too large")
	}
	caData := config.CAData
	if config.CAFile != "" {
		file, openErr := os.Open(config.CAFile)
		if openErr != nil {			return nil, errors.New("kubernetes action CA is unavailable")
		}
		caData, err = io.ReadAll(io.LimitReader(file, (4<<20)+1))
		closeErr := file.Close()
		if err != nil || closeErr != nil || len(caData) == 0 || len(caData) > 4<<20 {
			return nil, errors.New("kubernetes action CA is unavailable")
		}
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: strings.TrimSpace(config.ServerName)}
	if len(caData) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caData) {
			return nil, errors.New("kubernetes action CA is invalid")
		}
		tlsConfig.RootCAs = pool
	}
	tlsConfig.GetClientCertificate = config.Credentials.ClientCertificate
	timeout := config.Timeout
	if timeout <= 0 || timeout > time.Minute {
		timeout = 10 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = tlsConfig
	transport.DialContext = guardedDial(&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}, policy)
	return &client{
		base: endpoint, credentials: config.Credentials, timeout: timeout,
		http: &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func (client *client) Do(ctx context.Context, method, apiPath string, query url.Values, body []byte) ([]byte, error) {
	if client == nil || client.http == nil || ctx == nil || !allowedMethod(method) || len(body) > maxRequestBytes || !strings.HasPrefix(apiPath, "/") || strings.Contains(apiPath, "..") || strings.ContainsAny(apiPath, "\r\n?#%") {
		return nil, ErrRequestFailed
	}
	endpoint := *client.base
	endpoint.Path = apiPath
	endpoint.RawQuery = query.Encode()
	requestCtx, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, method, endpoint.String(), strings.NewReader(string(body)))
	if err != nil {
		return nil, ErrRequestFailed
	}
	request.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		contentType := "application/json"
		if method == http.MethodPatch {
			contentType = "application/strategic-merge-patch+json"
		}
		request.Header.Set("Content-Type", contentType)
	}
	authorization, err := client.credentials.Authorization(requestCtx)
	if err != nil || strings.ContainsAny(authorization, "\r\n") {
		return nil, errors.New("kubernetes actor credential is unavailable")
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := client.http.Do(request)
	if err != nil {
		return nil, ErrRequestFailed
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: HTTP %d", ErrRequestFailed, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return nil, ErrRequestFailed
	}
	return data, nil
}

func allowedMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodPatch || method == http.MethodPost
}

func guardedDial(dialer *net.Dialer, policy endpointPolicy) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("kubernetes action endpoint is invalid")
		}
		ips := []net.IP{net.ParseIP(host)}
		if ips[0] == nil {
			resolved, lookupErr := net.DefaultResolver.LookupIPAddr(ctx, host)
			if lookupErr != nil || len(resolved) == 0 {
				return nil, errors.New("kubernetes action endpoint is unavailable")
			}
			ips = ips[:0]
			for _, item := range resolved {
				ips = append(ips, item.IP)
			}
		}
		for _, ip := range ips {
			if !policy.allows(ip) {
				return nil, errors.New("kubernetes action endpoint is outside the egress policy")
			}
		}
		var lastErr error
		for _, ip := range ips {
			connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}
}

func (policy endpointPolicy) allows(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	if ip.IsLoopback() {
		return policy.allowLoopback
	}
	if !ip.IsPrivate() {
		return true
	}
	if policy.allowPrivate {
		return true
	}
	for _, network := range policy.allowedCIDRs {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}