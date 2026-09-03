package kubernetes

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type resolverSequence struct {
	answers [][]net.IPAddr
	next    int
}

func (resolver *resolverSequence) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	answer := resolver.answers[resolver.next]
	if resolver.next < len(resolver.answers)-1 {
		resolver.next++
	}
	return answer, nil
}

type recordingDialer struct {
	addresses []string
}

func (dialer *recordingDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	dialer.addresses = append(dialer.addresses, "attempted")
	return nil, errors.New("dial attempted")
}

func TestNewClientRejectsUnsafeEndpoints(t *testing.T) {
	for _, endpoint := range []string{"", "ftp://cluster.example", "https://user:secret@cluster.example", "https://cluster.example?next=x", "http://cluster.example", "https://cluster.example/base", "https://169.254.169.254", "https://[fe80::1]"} {
		if _, err := NewClient(Config{Endpoint: endpoint}); !errors.Is(err, ErrInvalidEndpoint) {
			t.Errorf("NewClient(%q) error = %v", endpoint, err)
		}
	}
}

func TestEndpointPolicyDistinguishesPublicPrivateAndUnsafeAddresses(t *testing.T) {
	production := endpointPolicy{}
	private := endpointPolicy{allowPrivate: true}
	loopbackTest := endpointPolicy{allowLoopback: true}
	_, cidr, _ := net.ParseCIDR("10.20.0.0/16")
	allowlisted := endpointPolicy{allowedCIDRs: []*net.IPNet{cidr}}
	for _, test := range []struct {
		name    string
		policy  endpointPolicy
		address string
		want    bool
	}{
		{"public", production, "203.0.113.10", true},
		{"private denied", production, "10.20.1.4", false},
		{"private enabled", private, "10.20.1.4", true},
		{"private CIDR", allowlisted, "10.20.1.4", true},
		{"private outside CIDR", allowlisted, "10.21.1.4", false},
		{"loopback production", production, "127.0.0.1", false},
		{"loopback test", loopbackTest, "127.0.0.1", true},
		{"metadata despite private", private, "169.254.169.254", false},
		{"IPv4 link local", production, "169.254.2.3", false},
		{"IPv6 link local", production, "fe80::1", false},
		{"multicast", production, "224.0.0.1", false},
		{"unspecified", production, "0.0.0.0", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.policy.allows(net.ParseIP(test.address)); got != test.want {
				t.Fatalf("allows(%s) = %v, want %v", test.address, got, test.want)
			}
		})
	}
}

func TestGuardedDialRejectsMixedAndReboundDNSBeforeDial(t *testing.T) {
	dialer := &recordingDialer{}
	resolver := &resolverSequence{answers: [][]net.IPAddr{
		{{IP: net.ParseIP("203.0.113.10")}, {IP: net.ParseIP("169.254.169.254")}},
	}}
	dial := guardedDialContext(dialer, resolver, endpointPolicy{})
	if _, err := dial(context.Background(), "tcp", "cluster.example:443"); !errors.Is(err, ErrInvalidEndpoint) {
		t.Fatalf("mixed public/metadata resolution error = %v", err)
	}
	if len(dialer.addresses) != 0 {
		t.Fatalf("unsafe mixed resolution dialed %v", dialer.addresses)
	}

	resolver = &resolverSequence{answers: [][]net.IPAddr{
		{{IP: net.ParseIP("203.0.113.10")}},
		{{IP: net.ParseIP("fe80::1")}},
	}}
	dial = guardedDialContext(dialer, resolver, endpointPolicy{})
	if _, err := dial(context.Background(), "tcp", "cluster.example:443"); err == nil || errors.Is(err, ErrInvalidEndpoint) {
		t.Fatalf("initial public resolution error = %v", err)
	}
	if _, err := dial(context.Background(), "tcp", "cluster.example:443"); !errors.Is(err, ErrInvalidEndpoint) {
		t.Fatalf("rebound link-local resolution error = %v", err)
	}
	if len(dialer.addresses) != 1 {
		t.Fatalf("dial attempts = %d, want only initial public target", len(dialer.addresses))
	}
}

func TestClientPreservesConfiguredTLSServerName(t *testing.T) {
	client, err := NewClient(Config{Endpoint: "https://203.0.113.10", ServerName: "cluster.example"})
	if err != nil {
		t.Fatal(err)
	}
	transport := client.http.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.TLSClientConfig.ServerName != "cluster.example" {
		t.Fatalf("proxy configured=%t serverName=%q", transport.Proxy != nil, transport.TLSClientConfig.ServerName)
	}
}

func TestClientRejectsOversizedTokenFile(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(strings.Repeat("x", int(maxTokenBytes)+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(Config{Endpoint: "http://127.0.0.1", TokenFile: tokenFile, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err := client.GetJSON(context.Background(), "/api", &output); err == nil || err.Error() != "kubernetes: credential unavailable" {
		t.Fatalf("oversized token error = %v", err)
	}
}

func TestClientReadsRotatingTokenAndRefusesRedirects(t *testing.T) {
	directory := t.TempDir()
	tokenFile := filepath.Join(directory, "token")
	if err := os.WriteFile(tokenFile, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	seen := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirect" {
			http.Redirect(writer, request, "/api", http.StatusFound)
			return
		}
		seen = append(seen, request.Header.Get("Authorization"))
		_ = json.NewEncoder(writer).Encode(map[string]bool{"ok": true})
	}))
	defer server.Close()
	client, err := NewClient(Config{Endpoint: server.URL, TokenFile: tokenFile, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	var output map[string]bool
	if err := client.GetJSON(context.Background(), "/api", &output); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.GetJSON(context.Background(), "/api", &output); err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, ",") != "Bearer first,Bearer second" {
		t.Fatalf("authorization headers = %v", seen)
	}
	if err := client.GetJSON(context.Background(), "/redirect", &output); !errors.Is(err, ErrRedirect) {
		t.Fatalf("redirect error = %v", err)
	}
}

func TestClientCertificateCredentialReloadsRotatedFiles(t *testing.T) {
	directory := t.TempDir()
	certificateFile := filepath.Join(directory, "client.crt")
	keyFile := filepath.Join(directory, "client.key")
	writeCertificate := func(server *httptest.Server, modified time.Time) []byte {
		t.Helper()
		certificate := server.TLS.Certificates[0]
		certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})
		keyDER, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
		if err := os.WriteFile(certificateFile, certificatePEM, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(certificateFile, modified, modified); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(keyFile, modified, modified); err != nil {
			t.Fatal(err)
		}
		return certificate.Certificate[0]
	}
	firstServer := httptest.NewTLSServer(http.NotFoundHandler())
	defer firstServer.Close()
	firstRaw := writeCertificate(firstServer, time.Now().Add(-time.Minute))
	source, err := clientCertificateCredential(certificateFile, keyFile, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := source.ClientCertificate(nil)
	if err != nil || !bytes.Equal(first.Certificate[0], firstRaw) {
		t.Fatalf("first certificate = %v, %v", first, err)
	}
	secondServer := httptest.NewTLSServer(http.NotFoundHandler())
	defer secondServer.Close()
	secondRaw := writeCertificate(secondServer, time.Now())
	second, err := source.ClientCertificate(nil)
	if err != nil || !bytes.Equal(second.Certificate[0], secondRaw) {
		t.Fatalf("rotated certificate = %v, %v", second, err)
	}
}

func TestClientTLSDeadlineBodyCapAndBoundedForbidden(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/slow":
			<-request.Context().Done()
		case "/large":
			_, _ = writer.Write([]byte(`{"value":"` + strings.Repeat("x", 256) + `"}`))
		case "/forbidden":
			http.Error(writer, `raw Kubernetes Status with secret-token`, http.StatusForbidden)
		case "/invalid":
			http.Error(writer, `raw Kubernetes Status with secret-token`, http.StatusBadRequest)
		default:
			_ = json.NewEncoder(writer).Encode(map[string]bool{"ok": true})
		}
	}))
	defer server.Close()
	certificate := server.Certificate()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	pem := "-----BEGIN CERTIFICATE-----\n" + encodeBase64Lines(certificate.Raw) + "\n-----END CERTIFICATE-----\n"
	if err := os.WriteFile(caFile, []byte(pem), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(Config{Endpoint: server.URL, CAFile: caFile, Timeout: 25 * time.Millisecond, MaxBodyBytes: 64, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err := client.GetJSON(context.Background(), "/ok", &output); err != nil {
		t.Fatalf("verified TLS request: %v", err)
	}
	if err := client.GetJSON(context.Background(), "/large", &output); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("large response error = %v", err)
	}
	if err := client.GetJSON(context.Background(), "/forbidden", &output); !errors.Is(err, ErrForbidden) || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("forbidden error = %v", err)
	}
	if err := client.GetJSON(context.Background(), "/invalid", &output); !errors.Is(err, ErrInvalidArguments) || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("invalid arguments error = %v", err)
	}
	if err := client.GetJSON(context.Background(), "/slow", &output); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("deadline error = %v", err)
	}
}

func encodeBase64Lines(data []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var builder strings.Builder
	for offset := 0; offset < len(data); offset += 3 {
		end := offset + 3
		if end > len(data) {
			end = len(data)
		}
		chunk := data[offset:end]
		value := uint32(chunk[0]) << 16
		if len(chunk) > 1 {
			value |= uint32(chunk[1]) << 8
		}
		if len(chunk) > 2 {
			value |= uint32(chunk[2])
		}
		builder.WriteByte(alphabet[(value>>18)&63])
		builder.WriteByte(alphabet[(value>>12)&63])
		if len(chunk) > 1 {
			builder.WriteByte(alphabet[(value>>6)&63])
		} else {
			builder.WriteByte('=')
		}
		if len(chunk) > 2 {
			builder.WriteByte(alphabet[value&63])
		} else {
			builder.WriteByte('=')
		}
		if builder.Len()%65 == 64 {
			builder.WriteByte('\n')
		}
	}
	return strings.TrimSuffix(builder.String(), "\n")
}
