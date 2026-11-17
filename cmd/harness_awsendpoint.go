//go:build harness_awsendpoint || harness_fakeaws

package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// This file is compiled only into test builds (the harness_awsendpoint tag,
// or its older alias harness_fakeaws). Release builds carry neither the
// variables below nor the redirect, so a release binary always talks to AWS.
const (
	// harnessAWSEndpointEnv is one base URL every AWS service is sent to: the
	// in-repo fake AWS server or an AWS API emulator.
	harnessAWSEndpointEnv = "VERSUS_HARNESS_AWS_ENDPOINT_URL"
	// harnessAWSCABundleEnv optionally names a PEM file the redirected
	// transport trusts, for an emulator that serves its own self-signed CA.
	harnessAWSCABundleEnv = "VERSUS_HARNESS_AWS_CA_BUNDLE"
	// harnessFakeAWSEnv is the older name of harnessAWSEndpointEnv, still read
	// so existing harness scripts keep working.
	harnessFakeAWSEnv = "VERSUS_HARNESS_FAKEAWS_URL"
)

const (
	harnessAWSRequestTimeout = 60 * time.Second
	harnessAWSMaxCABundle    = 1 << 20
)

// awsHarnessHTTPClient returns the transport every AWS client shares in a
// test build. It is nil when no endpoint is configured, so AWS calls go to
// AWS. A configured but unusable endpoint (bad URL, public host, unreadable
// CA bundle) yields a client that refuses every request: a misconfigured
// test build fails closed instead of falling back to real AWS.
func awsHarnessHTTPClient() aws.HTTPClient {
	raw, name := os.Getenv(harnessAWSEndpointEnv), harnessAWSEndpointEnv
	if raw == "" {
		raw, name = os.Getenv(harnessFakeAWSEnv), harnessFakeAWSEnv
	}
	if raw == "" {
		return nil
	}
	client, err := newHarnessAWSClient(raw, os.Getenv(harnessAWSCABundleEnv))
	if err != nil {
		log.Printf("aws harness endpoint: %s rejected, every AWS call will fail: %v", name, err)
		return harnessRefusingClient{err: err}
	}
	return client
}

// newHarnessAWSClient builds the redirecting client for one base URL and an
// optional CA bundle path.
func newHarnessAWSClient(rawURL, caBundlePath string) (aws.HTTPClient, error) {
	base, err := parseHarnessAWSEndpoint(rawURL)
	if err != nil {
		return nil, err
	}
	var roots *x509.CertPool
	if caBundlePath != "" {
		if base.Scheme != "https" {
			return nil, errors.New("a CA bundle needs an https endpoint")
		}
		if roots, err = loadHarnessCABundle(caBundlePath); err != nil {
			return nil, err
		}
	}
	// A private transport: the CA bundle lives only here, never in
	// http.DefaultTransport or the hardened client real AWS calls use.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: harnessDialControl}
	transport.DialContext = dialer.DialContext
	return &http.Client{
		Transport: harnessRedirect{base: base, next: transport},
		Timeout:   harnessAWSRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

// parseHarnessAWSEndpoint accepts a bare http(s) origin whose host is local:
// loopback, a private address, or a name that cannot be a public host.
func parseHarnessAWSEndpoint(rawURL string) (*url.URL, error) {
	base, err := url.Parse(rawURL)
	if err != nil {
		return nil, errors.New("not a URL")
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, errors.New("scheme must be http or https")
	}
	if base.Host == "" || base.User != nil || (base.Path != "" && base.Path != "/") || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("must be a bare scheme://host[:port] origin")
	}
	if !harnessHostAllowed(base.Hostname()) {
		return nil, errors.New("host must be loopback, a private address or a local name")
	}
	return &url.URL{Scheme: base.Scheme, Host: base.Host}, nil
}

// harnessHostAllowed accepts loopback and private IP literals, localhost,
// the machine's own hostname, and names that only resolve inside a local or
// container network (single-label service names, *.localhost, *.internal).
// The dial check still rejects any of them that resolves to a public address.
func harnessHostAllowed(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return harnessAddrAllowed(ip)
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return false
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") {
		return true
	}
	if own, err := os.Hostname(); err == nil && strings.EqualFold(strings.TrimSuffix(own, "."), host) {
		return true
	}
	return !strings.Contains(host, ".")
}

// harnessAddrAllowed: only loopback and private (RFC 1918, IPv6 ULA)
// addresses. Link-local is excluded, which keeps the instance metadata
// service out of reach.
func harnessAddrAllowed(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate()
}

// harnessDialControl runs on the resolved address of every connection the
// redirected transport opens, so a local-looking name that resolves to a
// public address is still refused.
func harnessDialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !harnessAddrAllowed(ip) {
		return fmt.Errorf("aws harness endpoint: refusing to dial %s", host)
	}
	return nil
}

func loadHarnessCABundle(path string) (*x509.CertPool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("CA bundle unreadable")
	}
	defer file.Close()
	pem, err := io.ReadAll(io.LimitReader(file, harnessAWSMaxCABundle+1))
	if err != nil || len(pem) > harnessAWSMaxCABundle {
		return nil, errors.New("CA bundle unreadable or too large")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("CA bundle holds no PEM certificate")
	}
	return roots, nil
}

// harnessRedirect sends a request, already signed for the real AWS host, to
// the harness endpoint. Path, query, body and headers are untouched, and the
// Host header stays the signed AWS host, so an emulator that verifies SigV4
// sees the service, region and host the SDK signed.
type harnessRedirect struct {
	base *url.URL
	next http.RoundTripper
}

func (redirect harnessRedirect) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	target := *request.URL
	target.Scheme = redirect.base.Scheme
	target.Host = redirect.base.Host
	clone.URL = &target
	if clone.Host == "" {
		clone.Host = request.URL.Host
	}
	return redirect.next.RoundTrip(clone)
}

// harnessRefusingClient fails every AWS call of a misconfigured test build.
type harnessRefusingClient struct{ err error }

func (refuse harnessRefusingClient) Do(*http.Request) (*http.Response, error) {
	return nil, refuse.err
}
