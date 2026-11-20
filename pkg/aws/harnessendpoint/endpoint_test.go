//go:build harness_awsendpoint || harness_fakeaws

package harnessendpoint

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/budgets"
	"github.com/aws/aws-sdk-go-v2/service/computeoptimizer"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/aws/aws-sdk-go-v2/service/costoptimizationhub"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/VersusControl/versus-incident/pkg/awsauth"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// TestHarnessBuildRedirectsAWSToTheFixture: in a test build, a real SDK call
// to the real STS host lands on the fake endpoint, under the new variable and
// under its older name.
func TestHarnessBuildRedirectsAWSToTheFixture(t *testing.T) {
	for _, env := range []string{harnessAWSEndpointEnv, harnessFakeAWSEnv} {
		t.Run(env, func(t *testing.T) {
			fake := fakeaws.New(t, "new-account-connect")
			t.Setenv(harnessAWSEndpointEnv, "")
			t.Setenv(harnessFakeAWSEnv, "")
			t.Setenv(env, fake.URL())
			client := HTTPClient()
			if client == nil {
				t.Fatal("test build did not build a redirect")
			}
			api := sts.NewFromConfig(aws.Config{Region: "us-east-1", HTTPClient: client, RetryMaxAttempts: 1,
				Credentials: credentials.NewStaticCredentialsProvider("AKIDHARNESS", "secret", "session-111122223333")})
			if _, err := api.GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{}); err != nil {
				t.Fatalf("redirected call = %v", err)
			}
			if fake.OperationCount("sts:GetCallerIdentity") != 1 {
				t.Fatal("call did not reach the fixture")
			}
		})
	}
}

// TestHarnessEndpointSendsSignedCallsToATLSEmulator: with one HTTPS base URL
// and its CA bundle, STS, EC2 and the cost services all reach an emulator
// serving a private CA. Each request keeps the real AWS Host header and a
// SigV4 signature for the real service and region that verifies as received.
func TestHarnessEndpointSendsSignedCallsToATLSEmulator(t *testing.T) {
	emulator, bundle := newCustomCAServer(t)
	t.Setenv(harnessFakeAWSEnv, "")
	t.Setenv(harnessAWSEndpointEnv, emulator.server.URL)
	t.Setenv(harnessAWSCABundleEnv, bundle)
	client := HTTPClient()
	if _, refused := client.(harnessRefusingClient); client == nil || refused {
		t.Fatalf("test build did not accept the emulator: %#v", client)
	}
	const accessKey, secret, token = "AKIDEMULATOR", "emulator-secret", "emulator-session"
	cfg := func(region string) aws.Config {
		return aws.Config{Region: region, HTTPClient: client, RetryMaxAttempts: 1,
			Credentials: credentials.NewStaticCredentialsProvider(accessKey, secret, token)}
	}
	ctx := context.Background()
	if _, err := sts.NewFromConfig(cfg("eu-west-1")).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}); err != nil {
		t.Fatalf("sts through the emulator = %v", err)
	}
	_, _ = ec2.NewFromConfig(cfg("ap-southeast-2")).DescribeRegions(ctx, &ec2.DescribeRegionsInput{})
	_, _ = costexplorer.NewFromConfig(cfg("us-east-1")).GetCostAndUsage(ctx, &costexplorer.GetCostAndUsageInput{
		Granularity: cetypes.GranularityDaily, Metrics: []string{"UnblendedCost"},
		TimePeriod: &cetypes.DateInterval{Start: aws.String("2026-11-01"), End: aws.String("2026-11-02")}})
	_, _ = budgets.NewFromConfig(cfg("us-east-1")).DescribeBudgets(ctx, &budgets.DescribeBudgetsInput{AccountId: aws.String("111122223333")})
	_, _ = computeoptimizer.NewFromConfig(cfg("eu-central-1")).GetEC2InstanceRecommendations(ctx, &computeoptimizer.GetEC2InstanceRecommendationsInput{})
	_, _ = costoptimizationhub.NewFromConfig(cfg("us-east-1")).ListRecommendations(ctx, &costoptimizationhub.ListRecommendationsInput{})

	want := []struct{ host, region, service string }{
		{"sts.eu-west-1.amazonaws.com", "eu-west-1", "sts"},
		{"ec2.ap-southeast-2.amazonaws.com", "ap-southeast-2", "ec2"},
		{"ce.us-east-1.amazonaws.com", "us-east-1", "ce"},
		{"budgets.amazonaws.com", "us-east-1", "budgets"},
		{"compute-optimizer.eu-central-1.amazonaws.com", "eu-central-1", "compute-optimizer"},
		{"cost-optimization-hub.us-east-1.amazonaws.com", "us-east-1", "cost-optimization-hub"},
	}
	got := emulator.received()
	if len(got) != len(want) {
		t.Fatalf("emulator saw %d requests, want %d: %+v", len(got), len(want), got)
	}
	for i, request := range got {
		if request.Host != want[i].host {
			t.Errorf("request %d Host = %q, want the signed AWS host %q", i, request.Host, want[i].host)
		}
		scope := "/" + want[i].region + "/" + want[i].service + "/aws4_request"
		if !strings.Contains(request.Header.Get("Authorization"), "Credential="+accessKey+"/") ||
			!strings.Contains(request.Header.Get("Authorization"), scope) {
			t.Errorf("request %d Authorization = %q, want scope %q", i, request.Header.Get("Authorization"), scope)
		}
		if err := verifySigV4(request, accessKey, secret, want[i].service, want[i].region); err != nil {
			t.Errorf("request %d to %s: %v", i, want[i].host, err)
		}
	}
}

// TestHarnessCABundleStaysOffTheRealTransport: after a test build trusts the
// emulator's CA, the hardened client real AWS calls use and the process
// default transport still reject that CA.
func TestHarnessCABundleStaysOffTheRealTransport(t *testing.T) {
	emulator, bundle := newCustomCAServer(t)
	harness, err := newHarnessAWSClient(emulator.server.URL, bundle)
	if err != nil {
		t.Fatal(err)
	}
	request := func() *http.Request {
		r, _ := http.NewRequest(http.MethodPost, emulator.server.URL+"/", strings.NewReader("Action=GetCallerIdentity&Version=2011-06-15"))
		return r
	}
	response, err := harness.Do(request())
	if err != nil {
		t.Fatalf("redirected transport does not trust the bundle: %v", err)
	}
	response.Body.Close()
	var unknown *tls.CertificateVerificationError
	if _, err := awsauth.NewHTTPClient().Do(request()); !errors.As(err, &unknown) {
		t.Fatalf("hardened AWS client trusted the harness CA: err=%v", err)
	}
	if _, err := (&http.Client{Transport: http.DefaultTransport}).Do(request()); !errors.As(err, &unknown) {
		t.Fatalf("default transport trusted the harness CA: err=%v", err)
	}
	if config := http.DefaultTransport.(*http.Transport).TLSClientConfig; config != nil && config.RootCAs != nil {
		t.Fatal("default transport root pool was replaced")
	}
}

// TestHarnessEndpointRefusesPublicHosts: the redirect only ever targets
// loopback, private addresses and local names. A public host, the metadata
// address or a malformed origin yields a client that refuses every call.
func TestHarnessEndpointRefusesPublicHosts(t *testing.T) {
	for _, raw := range []string{
		"https://8.8.8.8", "https://example.com", "https://sts.us-east-1.amazonaws.com",
		"http://169.254.169.254", "http://0.0.0.0:9297", "https://[2001:4860:4860::8888]",
		"ftp://127.0.0.1", "https://127.0.0.1/prefix", "https://user:pw@127.0.0.1", "not a url", "https://127.0.0.1?x=1",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := newHarnessAWSClient(raw, ""); err == nil {
				t.Fatal("public or malformed endpoint accepted")
			}
			t.Setenv(harnessFakeAWSEnv, "")
			t.Setenv(harnessAWSEndpointEnv, raw)
			client := HTTPClient()
			if client == nil {
				t.Fatal("misconfigured endpoint fell back to real AWS")
			}
			request, _ := http.NewRequest(http.MethodPost, "https://sts.us-east-1.amazonaws.com/", nil)
			if _, err := client.Do(request); err == nil {
				t.Fatal("misconfigured endpoint sent a request")
			}
		})
	}
	for _, raw := range []string{
		"https://127.0.0.1:9297", "http://[::1]:4566", "https://10.0.0.5", "https://192.168.1.20",
		"https://172.20.0.3", "https://[fd00::1]", "https://localhost:9297", "https://aws-emulator:9297",
		"https://host.docker.internal:9297", "https://emulator.localhost",
	} {
		if _, err := newHarnessAWSClient(raw, ""); err != nil {
			t.Errorf("%s refused: %v", raw, err)
		}
	}
	for _, address := range []string{"8.8.8.8:443", "169.254.169.254:80", "[2001:4860:4860::8888]:443"} {
		if harnessDialControl("tcp", address, nil) == nil {
			t.Errorf("dial to %s allowed", address)
		}
	}
	if err := harnessDialControl("tcp", "127.0.0.1:9297", nil); err != nil {
		t.Errorf("loopback dial refused: %v", err)
	}
}

// TestHarnessCABundleRejectsBadInput: a CA bundle must be a readable PEM file
// and only applies to an https endpoint.
func TestHarnessCABundleRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, bundle := newCustomCAServer(t)
	cases := map[string][2]string{
		"missing file": {"https://127.0.0.1:9297", filepath.Join(dir, "absent.pem")},
		"not PEM":      {"https://127.0.0.1:9297", garbage},
		"http origin":  {"http://127.0.0.1:9297", bundle},
	}
	for name, in := range cases {
		if _, err := newHarnessAWSClient(in[0], in[1]); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// customCAServer is an HTTPS server whose certificate chains to a CA made for
// the test, recording every request it receives.
type customCAServer struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []*http.Request
}

func (s *customCAServer) received() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.requests...)
}

func newCustomCAServer(t *testing.T) (*customCAServer, string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "harness test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "emulator"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	emulator := &customCAServer{}
	emulator.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		recorded := r.Clone(context.Background())
		recorded.Body = io.NopCloser(bytes.NewReader(body))
		emulator.mu.Lock()
		emulator.requests = append(emulator.requests, recorded)
		emulator.mu.Unlock()
		if strings.HasPrefix(r.Host, "sts.") {
			w.Header().Set("Content-Type", "text/xml")
			_, _ = io.WriteString(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult>`+
				`<Arn>arn:aws:iam::000000000000:root</Arn><UserId>000000000000</UserId><Account>000000000000</Account>`+
				`</GetCallerIdentityResult><ResponseMetadata><RequestId>harness</RequestId></ResponseMetadata></GetCallerIdentityResponse>`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	emulator.server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}
	emulator.server.StartTLS()
	t.Cleanup(emulator.server.Close)
	bundle := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return emulator, bundle
}

// verifySigV4 recomputes the signature of a received request over exactly
// the headers it says it signed, the way a SigV4-checking emulator does.
func verifySigV4(received *http.Request, accessKey, secret, service, region string) error {
	authorization := received.Header.Get("Authorization")
	signed := ""
	for _, part := range strings.Split(strings.TrimPrefix(authorization, "AWS4-HMAC-SHA256 "), ", ") {
		if value, ok := strings.CutPrefix(part, "SignedHeaders="); ok {
			signed = value
		}
	}
	if signed == "" {
		return errors.New("no SignedHeaders in Authorization")
	}
	signingTime, err := time.Parse("20060102T150405Z", received.Header.Get("X-Amz-Date"))
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(received.Body)
	replay, err := http.NewRequest(received.Method, "https://"+received.Host+received.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	for _, name := range strings.Split(signed, ";") {
		switch name {
		case "host", "content-length":
		default:
			replay.Header[http.CanonicalHeaderKey(name)] = received.Header.Values(name)
		}
	}
	if strings.Contains(";"+signed+";", ";content-length;") {
		replay.ContentLength = int64(len(body))
	} else {
		replay.ContentLength = 0
	}
	digest := sha256.Sum256(body)
	creds := aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secret, SessionToken: received.Header.Get("X-Amz-Security-Token")}
	if err := v4.NewSigner().SignHTTP(context.Background(), creds, replay, hex.EncodeToString(digest[:]), service, region, signingTime); err != nil {
		return err
	}
	if replay.Header.Get("Authorization") != authorization {
		return errors.New("signature does not verify against the received request:\n got  " + authorization + "\n want " + replay.Header.Get("Authorization"))
	}
	return nil
}
