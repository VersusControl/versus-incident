package kubernetes

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
)

type resolverSequence struct {
	answers [][]net.IPAddr
	next    int
	err     error
}

func (resolver *resolverSequence) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	if resolver.err != nil {
		return nil, resolver.err
	}
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

func TestPodLogStreamGuardedDNSFailurePreservesSafeDiagnosis(t *testing.T) {
	cause := &net.DNSError{Err: "private-provider-detail", Name: "private-endpoint.invalid"}
	dialer := &recordingDialer{}
	dial := guardedDialContext(dialer, &resolverSequence{err: cause}, endpointPolicy{})
	client, err := NewClient(Config{Endpoint: "https://cluster.example"})
	if err != nil {
		t.Fatal("client construction failed")
	}
	client.streamHTTP.Transport = logStreamTestTransport(func(request *http.Request) (*http.Response, error) {
		_, dialErr := dial(request.Context(), "tcp", request.URL.Hostname()+":443")
		return nil, dialErr
	})
	_, err = client.openPodLogStream(context.Background(), "/api/v1/namespaces/default/pods/pod/log", false)
	detail := DiagnoseError(err)
	if !errors.Is(err, cause) || detail.Code != "dns_resolution_failed" || !detail.Retryable || len(dialer.addresses) != 0 {
		t.Fatalf("DNS diagnosis=%+v cause preserved=%t dial attempts=%d", detail, errors.Is(err, cause), len(dialer.addresses))
	}
	payload, _ := json.Marshal(detail)
	if strings.Contains(err.Error()+string(payload), "private-") {
		t.Fatal("DNS failure exposed private data")
	}
}

func TestPodLogStreamTransportGuardsAndRotatingCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirect" {
			http.Redirect(writer, request, "https://169.254.169.254/secret", 302)
			return
		}
		seen = append(seen, request.Header.Get("Authorization"))
		writer.Write([]byte("2026-10-01T00:00:00Z safe\n"))
	}))
	defer server.Close()
	client, err := NewClient(Config{Endpoint: server.URL, AllowLoopbackHTTP: true, TokenFile: path, ServerName: "cluster.example"})
	if err != nil {
		t.Fatal(err)
	}
	transport := client.streamHTTP.Transport.(*http.Transport)
	if client.streamHTTP.Timeout != 0 || transport.Proxy != nil || transport.ResponseHeaderTimeout != 10*time.Second || transport.TLSClientConfig.ServerName != "cluster.example" || transport.DialContext == nil || transport.MaxResponseHeaderBytes != 64<<10 {
		t.Fatal("stream transport lost its guards or bounds")
	}
	for _, token := range []string{"first", "rotated"} {
		if err := os.WriteFile(path, []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
		body, err := client.openPodLogStream(context.Background(), "/logs", false)
		if err != nil {
			t.Fatal(err)
		}
		body.Close()
	}
	if len(seen) != 2 || seen[0] != "Bearer first" || seen[1] != "Bearer rotated" {
		t.Fatal("stream credential rotation failed")
	}
	if _, err := client.openPodLogStream(context.Background(), "/redirect", false); !errors.Is(err, ErrRedirect) {
		t.Fatal("stream followed an unsafe redirect")
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

func TestPodLogStreamClonedTransportPreservesMTLS(t *testing.T) {
	var requests int
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.TLS == nil || len(request.TLS.PeerCertificates) != 1 {
			t.Error("client certificate missing")
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		requests++
		writer.Write([]byte(`{}`))
	}))
	server.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	server.StartTLS()
	defer server.Close()
	certificate := server.TLS.Certificates[0]
	credentials := &credentialSource{certificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &certificate, nil }}
	client, err := NewClient(Config{Endpoint: server.URL, AllowLoopback: true, Credentials: credentials, CAData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})})
	if err != nil {
		t.Fatal("mTLS client construction failed")
	}
	var output map[string]any
	if err := client.GetJSON(context.Background(), "/api", &output); err != nil {
		t.Fatalf("inventory diagnosis=%+v", DiagnoseError(err))
	}
	body, err := client.openPodLogStream(context.Background(), "/api/v1/namespaces/default/pods/pod/log?previous=true&follow=false&timestamps=true", true)
	if err != nil {
		t.Fatalf("stream diagnosis=%+v", DiagnoseError(err))
	}
	body.Close()
	if requests != 2 {
		t.Fatalf("authenticated requests=%d", requests)
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
	slowCanceled := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/slow":
			<-request.Context().Done()
			close(slowCanceled)
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
	client, err := NewClient(Config{Endpoint: server.URL, CAFile: caFile, Timeout: 5 * time.Second, MaxBodyBytes: 64, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err := client.GetJSON(context.Background(), "/ok", &output); err != nil {
		t.Fatalf("verified TLS request: %v", err)
	}
	wrongKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongCA := &x509.Certificate{SerialNumber: big.NewInt(42), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	wrongDER, err := x509.CreateCertificate(rand.Reader, wrongCA, wrongCA, &wrongKey.PublicKey, wrongKey)
	if err != nil {
		t.Fatal(err)
	}
	wrongCAFile := filepath.Join(t.TempDir(), "wrong-ca.pem")
	if err := os.WriteFile(wrongCAFile, []byte("-----BEGIN CERTIFICATE-----\n"+encodeBase64Lines(wrongDER)+"\n-----END CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	untrusted, err := NewClient(Config{Endpoint: server.URL, CAFile: wrongCAFile, Timeout: 5 * time.Second, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	var unknownCA x509.UnknownAuthorityError
	if err := untrusted.GetJSON(t.Context(), "/ok", &output); !errors.As(err, &unknownCA) {
		t.Fatalf("wrong CA must reject server certificate: %v", err)
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
	slowContext, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	started := time.Now()
	if err := client.GetJSON(slowContext, "/slow", &output); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 4*time.Second {
		t.Fatalf("caller deadline took %s; must finish before the five-second client timeout", elapsed)
	}
	select {
	case <-slowCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("deadline did not cancel the upstream request")
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

// A real cluster 403 names what it denied, and nothing else. The bounded Status
// body is read for the denied verb, resource and namespace only: the identity is
// classified rather than quoted, because a cloud ARN carries the account id, and
// no token, URL, address, or payload text can reach the message.
func TestClusterForbiddenNamesTheDeniedRequestWithoutLeakingTheBody(t *testing.T) {
	statusBody := `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"pods \"api-7d9f\" is forbidden: User \"arn:aws:sts::111122223333:assumed-role/versus-prod/versus\" cannot get resource \"pods/log\" in API group \"\" in the namespace \"vikki-mobile-dev\"","reason":"Forbidden","details":{"name":"api-7d9f","kind":"pods"},"code":403}`
	hostileBody := `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"pods \"x\" is forbidden: User \"Bearer eyJhbGciOiJIUzI1NiJ9.T0K3N\" cannot get resource \"https://169.254.169.254/latest/meta-data?token=abc%\" in API group \"\" in the namespace \"\u001b[31mPwned_Bad\"","details":{"kind":"http://evil.example/pods?token=abc","namespace":"..%s"}}`
	oversized := `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"` + strings.Repeat("x", forbiddenStatusLimit) + `"}`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/denied":
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte(statusBody))
		case "/hostile":
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte(hostileBody))
		case "/denied-huge":
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte(oversized))
		default:
			http.Error(writer, `{"error":"no body, no secret-token"}`, http.StatusForbidden)
		}
	}))
	defer server.Close()
	client, err := NewClient(Config{Endpoint: server.URL, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	err = client.GetJSON(t.Context(), "/denied", &output)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("a real cluster denial must stay a permission answer: %v", err)
	}
	var denied *permissionDeniedError
	if !errors.As(err, &denied) || denied.Verb != "get" || denied.Resource != "pods/log" || denied.Namespace != "vikki-mobile-dev" || denied.Principal != "an assumed role" {
		t.Fatalf("denied request lost: %#v", denied)
	}
	detail := DiagnoseError(err)
	if detail.Code != "cluster_permission_denied" || detail.Retryable || !strings.Contains(detail.Message, "get on pods/log in namespace vikki-mobile-dev") || !strings.Contains(detail.Message, "an assumed role") {
		t.Fatalf("diagnosis = %+v", detail)
	}
	// The identity is an input to a classification, never an output: an ARN names
	// the cloud account, so none of it may appear.
	for _, leaked := range []string{"111122223333", "arn:aws", "assumed-role", "versus-prod", "api-7d9f", `"details"`, "secret-token"} {
		if strings.Contains(detail.Message, leaked) || strings.Contains(detail.Action, leaked) || strings.Contains(err.Error(), leaked) {
			t.Fatalf("denial detail leaked %q: %+v", leaked, detail)
		}
	}
	// A hostile Status body cannot smuggle a token, a URL, an address, a query
	// string, a terminal escape, or a format verb into the answer.
	hostile := client.GetJSON(t.Context(), "/hostile", &output)
	hostileDetail := DiagnoseError(hostile)
	for _, canary := range []string{"eyJhbGciOiJIUzI1NiJ9", "T0K3N", "169.254.169.254", "http://", "https://", "token=abc", "%s", "%25", "\u001b", "Pwned", "evil.example", ".."} {
		for _, surface := range []string{hostileDetail.Message, hostileDetail.Action, hostile.Error()} {
			if strings.Contains(surface, canary) {
				t.Fatalf("hostile denial body leaked %q: %+v", canary, hostileDetail)
			}
		}
	}
	if !errors.Is(hostile, ErrForbidden) {
		t.Fatalf("hostile denial stopped being a permission answer: %v", hostile)
	}
	if err := client.GetJSON(t.Context(), "/denied-huge", &output); !errors.Is(err, ErrForbidden) || errors.As(err, &denied) {
		t.Fatalf("an oversized forbidden body produced detail: %v", err)
	}
	if err := client.GetJSON(t.Context(), "/denied-plain", &output); !errors.Is(err, ErrForbidden) || errors.As(err, &denied) || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("a forbidden body without a Status kept detail: %v", err)
	}
}

// Every field the denial parser keeps is allowlisted, so a text value that only
// happens to look printable is still refused rather than echoed.
func TestDeniedFieldsAreAllowlisted(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
		kind  string
		want  string
	}{
		{name: "resource", value: "pods/log", kind: "resource", want: "pods/log"},
		{name: "resource with dot", value: "controllerrevisions", kind: "resource", want: "controllerrevisions"},
		{name: "resource url", value: "https://evil.example/pods", kind: "resource", want: ""},
		{name: "resource query", value: "pods?token=abc", kind: "resource", want: ""},
		{name: "resource two segments", value: "pods/log/exec", kind: "resource", want: ""},
		{name: "resource upper case", value: "Pods", kind: "resource", want: ""},
		{name: "resource ip", value: "169.254.169.254", kind: "resource", want: ""},
		{name: "namespace", value: "vikki-mobile-dev", kind: "namespace", want: "vikki-mobile-dev"},
		{name: "namespace underscore", value: "Bad_NS", kind: "namespace", want: ""},
		{name: "namespace escape", value: "\u001b[31m", kind: "namespace", want: ""},
		{name: "namespace format verb", value: "%s", kind: "namespace", want: ""},
		{name: "namespace dots", value: "..", kind: "namespace", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := test.want
			switch test.kind {
			case "resource":
				got = deniedResource(test.value)
			case "namespace":
				got = deniedNamespace(test.value)
			}
			if got != test.want {
				t.Fatalf("kind=%s value=%q got %q want %q", test.kind, test.value, got, test.want)
			}
		})
	}
	for _, verb := range []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"} {
		if deniedVerb(verb) != verb {
			t.Fatalf("known verb %q refused", verb)
		}
	}
	for _, verb := range []string{"", "list pods", "GET", "exec", "%s", "get;rm"} {
		if deniedVerb(verb) != "" {
			t.Fatalf("verb %q accepted", verb)
		}
	}
}

// safeDeniedField drops anything that could steer a terminal or a format string.
func TestSafeDeniedFieldRejectsControlAndFormatText(t *testing.T) {
	for _, value := range []string{"\u001b[31mPwned", "bell\u0007x", "del\u007fx", "c1\u0085x", "%s", "%x%n", "quote\"d", "back\\slash", "tab\there", "line\nbreak"} {
		if got := safeDeniedField(value, 253); got != "" {
			t.Fatalf("value %q accepted as %q", value, got)
		}
	}
	// A control rune that is also Unicode whitespace is trimmed, never echoed.
	for _, value := range []string{"c1\u0085", "\u000bvt\u000b"} {
		if got := safeDeniedField(value, 253); strings.ContainsFunc(got, unicode.IsControl) {
			t.Fatalf("value %q kept a control rune as %q", value, got)
		}
	}
	if got := safeDeniedField("  arn:aws:sts::1:assumed-role/x  ", 253); got != "arn:aws:sts::1:assumed-role/x" {
		t.Fatalf("a plain value was altered: %q", got)
	}
	if got := safeDeniedField(strings.Repeat("a", 254), 253); got != "" {
		t.Fatal("an oversized value was kept")
	}
}
