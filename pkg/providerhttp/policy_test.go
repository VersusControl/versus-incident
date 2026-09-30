package providerhttp

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"testing"
)

func TestNormalizeOriginAndDestinationPolicy(t *testing.T) {
	for _, address := range []string{"", "ftp://example.com", "https://user@example.com", "https://example.com/path", "https://example.com?x=1", "https://example.com#x"} {
		if _, err := NormalizeOrigin(address); err == nil {
			t.Fatalf("NormalizeOrigin(%q) succeeded", address)
		}
	}
	if got, err := NormalizeOrigin(" HTTPS://Example.COM:8443/ "); err != nil || got != "https://example.com:8443" {
		t.Fatalf("normalized origin = %q, %v", got, err)
	}
	for _, address := range []string{"169.254.169.254", "100.100.100.200", "fd00:ec2::254", "0.0.0.0", "224.0.0.1"} {
		if AddressAllowed(net.ParseIP(address), Policy{AllowLoopback: true, AllowPrivate: true}) {
			t.Fatalf("address %s allowed", address)
		}
	}
	if HostAllowed("metadata.google.internal", Policy{AllowPrivate: true}) {
		t.Fatal("metadata hostname allowed")
	}
}

func TestGuardedDialValidatesEveryAnswerAndPinsDialedIP(t *testing.T) {
	dialed := ""
	dial := func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = address
		return nil, errors.New("dial stopped")
	}
	mixedLookup := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}, {IP: net.ParseIP("127.0.0.1")}}, nil
	}
	if _, err := guardedDialContext(mixedLookup, dial, Policy{})(context.Background(), "tcp", "metrics.example:443"); err == nil || dialed != "" {
		t.Fatalf("mixed DNS error=%v dialed=%q", err, dialed)
	}

	lookups := 0
	rebindingLookup := func(context.Context, string) ([]net.IPAddr, error) {
		lookups++
		if lookups == 1 {
			return []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	guarded := guardedDialContext(rebindingLookup, dial, Policy{})
	_, _ = guarded(context.Background(), "tcp", "metrics.example:443")
	if dialed != "203.0.113.10:443" {
		t.Fatalf("dialed = %q", dialed)
	}
	dialed = ""
	if _, err := guarded(context.Background(), "tcp", "metrics.example:443"); err == nil || dialed != "" {
		t.Fatalf("rebound DNS error=%v dialed=%q", err, dialed)
	}
}

func TestGuardedDialRequiresLoopbackForEveryAnswer(t *testing.T) {
	policy := Policy{AllowLoopback: true, AllowPrivate: true, RequireLoopback: true}
	for _, answers := range [][]string{
		{"203.0.113.10"}, {"10.0.0.2"},
		{"127.0.0.1", "203.0.113.10"}, {"10.0.0.2", "127.0.0.1"},
		{"127.0.0.1", "10.0.0.2"}, {"203.0.113.10", "127.0.0.1"},
	} {
		dialed := ""
		lookup := func(context.Context, string) ([]net.IPAddr, error) {
			resolved := make([]net.IPAddr, 0, len(answers))
			for _, address := range answers {
				resolved = append(resolved, net.IPAddr{IP: net.ParseIP(address)})
			}
			return resolved, nil
		}
		dial := func(_ context.Context, _, address string) (net.Conn, error) {
			dialed = address
			return nil, errors.New("dial stopped")
		}
		if _, err := guardedDialContext(lookup, dial, policy)(context.Background(), "tcp", "localhost:3200"); err == nil || dialed != "" {
			t.Fatalf("answers=%v error=%v dialed=%q", answers, err, dialed)
		}
	}

	for _, rebound := range []string{"203.0.113.10", "10.0.0.2"} {
		lookups := 0
		lookup := func(context.Context, string) ([]net.IPAddr, error) {
			lookups++
			if lookups == 1 {
				return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
			}
			return []net.IPAddr{{IP: net.ParseIP(rebound)}}, nil
		}
		dials := 0
		dial := func(_ context.Context, _, address string) (net.Conn, error) {
			if address != "127.0.0.1:3200" {
				t.Errorf("dialed %q", address)
			}
			dials++
			return nil, errors.New("dial stopped")
		}
		guarded := guardedDialContext(lookup, dial, policy)
		_, _ = guarded(context.Background(), "tcp", "api.localhost:3200")
		if _, err := guarded(context.Background(), "tcp", "api.localhost:3200"); err == nil || lookups != 2 || dials != 1 {
			t.Fatalf("rebound=%s error=%v lookups=%d dials=%d", rebound, err, lookups, dials)
		}
	}

	for _, address := range []string{"127.0.0.1", "::1"} {
		dialed := ""
		dial := func(_ context.Context, _, destination string) (net.Conn, error) {
			dialed = destination
			return nil, errors.New("dial stopped")
		}
		destination := net.JoinHostPort(address, "3200")
		_, _ = guardedDialContext(nil, dial, policy)(context.Background(), "tcp", destination)
		if dialed != destination {
			t.Fatalf("loopback destination=%q dialed=%q", destination, dialed)
		}
	}
}

func TestNewTransportDisablesProxyAndRequiresTLS12(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	transport := NewTransport(Policy{})
	if transport.Proxy != nil || transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("unsafe transport proxy_set=%t tls=%+v", transport.Proxy != nil, transport.TLSClientConfig)
	}
	client := &http.Client{CheckRedirect: RefuseRedirects}
	if err := client.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect error = %v", err)
	}
}
