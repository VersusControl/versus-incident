// Package providerhttp provides hardened outbound HTTP policy for configured providers.
package providerhttp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Policy controls explicitly approved local-network destinations and TLS behavior.
type Policy struct {
	AllowLoopback      bool
	AllowPrivate       bool
	RequireLoopback    bool
	InsecureSkipVerify bool
	RootCAs            *x509.CertPool
}

// NormalizeOrigin validates and canonicalizes an exact HTTP(S) origin.
func NormalizeOrigin(address string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(address))
	if err != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") || parsed.RawPath != "" {
		return "", fmt.Errorf("provider address must be an http(s) origin")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("provider address must be an http(s) origin")
	}
	hostname := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if hostname == "" || strings.Contains(hostname, "%") {
		return "", fmt.Errorf("provider address must be an http(s) origin")
	}
	host := hostname
	if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	if port := parsed.Port(); port != "" {
		host = net.JoinHostPort(hostname, port)
	}
	return parsed.Scheme + "://" + host, nil
}

// HostAllowed rejects known metadata names and disallowed literal addresses.
func HostAllowed(host string, policy Policy) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" || host == "metadata" || host == "metadata.google" || host == "metadata.google.internal" || host == "instance-data" || host == "instance-data.ec2.internal" {
		return false
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return policy.AllowLoopback
	}
	address := net.ParseIP(host)
	return address == nil || AddressAllowed(address, policy)
}

// AddressAllowed applies the provider destination policy to one resolved address.
func AddressAllowed(address net.IP, policy Policy) bool {
	if address == nil || address.IsUnspecified() || address.IsMulticast() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() {
		return false
	}
	for _, metadataAddress := range []string{"100.100.100.200", "fd00:ec2::254"} {
		if address.Equal(net.ParseIP(metadataAddress)) {
			return false
		}
	}
	if address.IsLoopback() {
		return policy.AllowLoopback
	}
	return policy.AllowPrivate || !address.IsPrivate()
}

// NewTransport returns a transport that ignores ambient proxies and validates
// every DNS answer before dialing the validated IP directly.
func NewTransport(policy Policy) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	dialer := &net.Dialer{}
	transport.DialContext = guardedDialContext(net.DefaultResolver.LookupIPAddr, dialer.DialContext, policy)
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: policy.RootCAs, InsecureSkipVerify: policy.InsecureSkipVerify}
	return transport
}

func guardedDialContext(lookup func(context.Context, string) ([]net.IPAddr, error), dial func(context.Context, string, string) (net.Conn, error), policy Policy) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, destination string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(destination)
		if err != nil {
			return nil, fmt.Errorf("provider read failed: invalid destination")
		}
		addresses := []net.IPAddr{{IP: net.ParseIP(host)}}
		if addresses[0].IP == nil {
			addresses, err = lookup(ctx, host)
			if err != nil || len(addresses) == 0 {
				return nil, fmt.Errorf("provider read failed: destination resolution failed")
			}
		}
		for _, candidate := range addresses {
			if !AddressAllowed(candidate.IP, policy) || (policy.RequireLoopback && !candidate.IP.IsLoopback()) {
				return nil, fmt.Errorf("provider read failed: destination is not permitted")
			}
		}
		var lastErr error
		for _, candidate := range addresses {
			connection, dialErr := dial(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}
}

// RefuseRedirects prevents credential forwarding and cross-origin redirects.
func RefuseRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
