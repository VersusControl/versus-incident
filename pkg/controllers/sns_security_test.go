package controllers

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// countingTransport stands in for the network so a test can prove a request was
// never issued. Every attempt is recorded before any canned response is
// returned, so "refused with no outbound request" is an assertion on zero, not
// on an absent error.
type countingTransport struct {
	calls    []string
	respond  func(*http.Request) (*http.Response, error)
	failLoud bool
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.calls = append(t.calls, req.URL.String())
	if t.failLoud {
		return nil, errors.New("transport should not have been reached")
	}
	if t.respond != nil {
		return t.respond(req)
	}
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader("ok")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// withTransport swaps the package client for one that cannot leave the test
// process and restores the original afterwards.
func withTransport(t *testing.T, tr *countingTransport) {
	t.Helper()
	prev := snsHTTPClient
	client := newSNSHTTPClient()
	client.Transport = tr
	snsHTTPClient = client
	t.Cleanup(func() { snsHTTPClient = prev })
}

func resetCertCache(t *testing.T) {
	t.Helper()
	snsCertMu.Lock()
	snsCertCache = map[string]*rsa.PublicKey{}
	snsCertMu.Unlock()
	t.Cleanup(func() {
		snsCertMu.Lock()
		snsCertCache = map[string]*rsa.PublicKey{}
		snsCertMu.Unlock()
	})
}

// TestConfirmSNSSubscription_RefusesHostileURLs is the SSRF guarantee: every
// destination an attacker would pick when posting an unauthenticated
// SubscriptionConfirmation is refused, and the process makes no request at all
// while refusing it.
func TestConfirmSNSSubscription_RefusesHostileURLs(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"cloud instance metadata", "https://169.254.169.254/latest/meta-data/iam/security-credentials/"},
		{"metadata via decimal ip literal", "https://2852039166/latest/meta-data/"},
		{"loopback", "https://127.0.0.1/confirm"},
		{"loopback by name", "https://localhost/confirm"},
		{"rfc1918 private", "https://10.0.0.5/confirm"},
		{"link local ipv6", "https://[fe80::1]/confirm"},
		{"plain http aws host", "http://sns.us-east-1.amazonaws.com/?Action=ConfirmSubscription"},
		{"non aws host", "https://evil.example.com/?Action=ConfirmSubscription"},
		{"aws host as a subdomain of an attacker domain", "https://sns.us-east-1.amazonaws.com.evil.example/"},
		{"attacker subdomain under amazonaws", "https://sns.us-east-1.amazonaws.com.cn.evil.test/"},
		{"userinfo smuggling the real host", "https://sns.us-east-1.amazonaws.com@evil.example/"},
		{"userinfo with credentials", "https://user:pass@sns.us-east-1.amazonaws.com/"},
		{"explicit port", "https://sns.us-east-1.amazonaws.com:8080/"},
		{"file scheme", "file:///etc/passwd"},
		{"gopher scheme", "gopher://127.0.0.1:6379/_INFO"},
		{"empty", ""},
		{"relative", "/confirm"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &countingTransport{failLoud: true}
			withTransport(t, tr)

			err := confirmSNSSubscription(context.Background(), tc.url)
			if err == nil {
				t.Fatalf("confirmSNSSubscription(%q) succeeded, want refusal", tc.url)
			}
			if len(tr.calls) != 0 {
				t.Fatalf("confirmSNSSubscription(%q) issued %d request(s) %v, want none", tc.url, len(tr.calls), tr.calls)
			}
		})
	}
}

// TestConfirmSNSSubscription_LegitimateURL proves the hardening did not close
// the door on the real flow: a genuine AWS SNS confirmation URL is fetched.
func TestConfirmSNSSubscription_LegitimateURL(t *testing.T) {
	for _, raw := range []string{
		"https://sns.us-east-1.amazonaws.com/?Action=ConfirmSubscription&TopicArn=arn:aws:sns:us-east-1:1:t&Token=abc",
		"https://sns.eu-west-3.amazonaws.com/?Action=ConfirmSubscription&Token=abc",
		"https://sns.cn-north-1.amazonaws.com.cn/?Action=ConfirmSubscription&Token=abc",
	} {
		t.Run(raw, func(t *testing.T) {
			tr := &countingTransport{}
			withTransport(t, tr)

			if err := confirmSNSSubscription(context.Background(), raw); err != nil {
				t.Fatalf("confirmSNSSubscription(%q) = %v, want success", raw, err)
			}
			if len(tr.calls) != 1 {
				t.Fatalf("issued %d request(s), want exactly 1", len(tr.calls))
			}
		})
	}
}

// TestConfirmSNSSubscription_RefusesRedirect covers the bypass a host allowlist
// alone leaves open: an allowed host answering with a redirect into a private
// address. The redirect is never followed.
func TestConfirmSNSSubscription_RefusesRedirect(t *testing.T) {
	for _, location := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"https://127.0.0.1/admin",
		"https://evil.example.com/",
	} {
		t.Run(location, func(t *testing.T) {
			tr := &countingTransport{respond: func(req *http.Request) (*http.Response, error) {
				h := make(http.Header)
				h.Set("Location", location)
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     h,
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}, nil
			}}
			withTransport(t, tr)

			err := confirmSNSSubscription(context.Background(), "https://sns.us-east-1.amazonaws.com/?Action=ConfirmSubscription")
			if err == nil {
				t.Fatal("redirect was followed, want refusal")
			}
			if !strings.Contains(err.Error(), "redirect refused") {
				t.Fatalf("error = %v, want a redirect refusal", err)
			}
			if len(tr.calls) != 1 {
				t.Fatalf("issued %d request(s), want exactly 1 (the redirect must not be followed)", len(tr.calls))
			}
		})
	}
}

// TestSNSAddrAllowed covers the connect-time guard, which is what stops a host
// that passes the name allowlist from resolving to a non-public address.
func TestSNSAddrAllowed(t *testing.T) {
	blocked := []string{
		"169.254.169.254:443", // instance metadata
		"127.0.0.1:443",
		"10.1.2.3:443",
		"172.16.0.1:443",
		"192.168.1.1:443",
		"100.64.0.1:443", // carrier NAT
		"0.0.0.0:443",
		"[::1]:443",
		"[fe80::1]:443",
		"[fd00::1]:443",                // IPv6 unique local
		"[::ffff:169.254.169.254]:443", // v4 metadata tunnelled in v6
		"[::ffff:127.0.0.1]:443",       // v4 loopback tunnelled in v6
		"[64:ff9b::a9fe:a9fe]:443",     // metadata behind NAT64
		"224.0.0.1:443",                // multicast
		"255.255.255.255:443",          // broadcast
		"198.18.0.1:443",               // benchmarking
		"192.88.99.1:443",              // 6to4 relay anycast
		"[2002:a9fe:a9fe::]:443",       // metadata addressed through 6to4
		"[2001:0:a9fe:a9fe::]:443",     // metadata addressed through Teredo
		"[::7f00:1]:443",               // IPv4-compatible loopback
		"[64:ff9b:1::a9fe:a9fe]:443",   // metadata behind local-use NAT64
		"[100::1]:443",                 // discard-only
		"not-an-address:443",
	}
	for _, addr := range blocked {
		if err := snsAddrAllowed(addr); err == nil {
			t.Errorf("snsAddrAllowed(%q) allowed, want refusal", addr)
		}
	}

	allowed := []string{"52.94.236.248:443", "[2600:1f18::1]:443"}
	for _, addr := range allowed {
		if err := snsAddrAllowed(addr); err != nil {
			t.Errorf("snsAddrAllowed(%q) = %v, want allowed", addr, err)
		}
	}
}

func TestSNSBlockedAddr_InvalidIsBlocked(t *testing.T) {
	var zero netip.Addr
	if !snsBlockedAddr(zero) {
		t.Fatal("the zero address was allowed, want blocked")
	}
}

// TestValidateSNSURL_SigningCertURLSharesTheAllowlist proves the certificate
// URL is held to the same rules as SubscribeURL — an unchecked cert URL is the
// same SSRF one level down.
func TestSNSSigningKey_RefusesHostileCertURLs(t *testing.T) {
	resetCertCache(t)
	for _, raw := range []string{
		"http://sns.us-east-1.amazonaws.com/SimpleNotificationService-9c6465fa7f48f806.pem",
		"https://169.254.169.254/SimpleNotificationService-9c6465fa7f48f806.pem",
		"https://evil.example.com/SimpleNotificationService-9c6465fa7f48f806.pem",
		"https://sns.us-east-1.amazonaws.com.evil.example/SimpleNotificationService-9c6465fa7f48f806.pem",
		"https://sns.us-east-1.amazonaws.com:8443/SimpleNotificationService-9c6465fa7f48f806.pem",
		"https://user@sns.us-east-1.amazonaws.com/SimpleNotificationService-9c6465fa7f48f806.pem",
		// An allowed host, but a path SNS does not serve a certificate from.
		"https://sns.us-east-1.amazonaws.com/?Action=Publish",
		"https://sns.us-east-1.amazonaws.com/anything.pem",
		"https://sns.us-east-1.amazonaws.com/../../SimpleNotificationService-9c6465fa7f48f806.pem",
		// The suffix check must run on the escaped path, or a percent-encoded
		// spelling passes validation and a different path is fetched.
		"https://sns.us-east-1.amazonaws.com/SimpleNotificationService-9c6465fa7f48f806%2epem",
	} {
		tr := &countingTransport{failLoud: true}
		withTransport(t, tr)
		if _, err := snsSigningKey(raw); err == nil {
			t.Errorf("snsSigningKey(%q) succeeded, want refusal", raw)
		}
		if len(tr.calls) != 0 {
			t.Errorf("snsSigningKey(%q) issued %d request(s), want none", raw, len(tr.calls))
		}
	}
}

// TestSNSSigningKey_CachesOneFetchPerCertificate covers the amplification
// vector: /sns is unauthenticated and the key must be fetched before a
// signature can be judged, so spellings that name the same certificate must
// not each cost an outbound request.
func TestSNSSigningKey_CachesOneFetchPerCertificate(t *testing.T) {
	resetCertCache(t)
	f := newSigningFixture(t)
	tr := f.serve(t)

	for _, spelling := range []string{
		f.certURL,
		"https://SNS.US-EAST-1.AMAZONAWS.COM/SimpleNotificationService-9c6465fa7f48f806f0ad0a4b25a1ec6a.pem",
		"https://sns.us-east-1.amazonaws.com./SimpleNotificationService-9c6465fa7f48f806f0ad0a4b25a1ec6a.pem",
		"https://sns.us-east-1.amazonaws.com:/SimpleNotificationService-9c6465fa7f48f806f0ad0a4b25a1ec6a.pem",
		f.certURL + "?cachebust=1",
		f.certURL + "#cachebust",
	} {
		if _, err := snsSigningKey(spelling); err != nil {
			t.Fatalf("snsSigningKey(%q) = %v, want success", spelling, err)
		}
	}
	if len(tr.calls) != 1 {
		t.Fatalf("issued %d request(s) %v, want exactly 1", len(tr.calls), tr.calls)
	}
}

// signingFixture is a throwaway RSA key plus a self-signed certificate served
// at a legitimate SNS cert URL, so signature verification can be exercised end
// to end without a network.
type signingFixture struct {
	key     *rsa.PrivateKey
	certPEM []byte
	certURL string
}

func newSigningFixture(t *testing.T) *signingFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "sns.us-east-1.amazonaws.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return &signingFixture{
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		certURL: "https://sns.us-east-1.amazonaws.com/SimpleNotificationService-9c6465fa7f48f806f0ad0a4b25a1ec6a.pem",
	}
}

// serve installs a transport that answers the certificate URL and nothing else.
func (f *signingFixture) serve(t *testing.T) *countingTransport {
	t.Helper()
	tr := &countingTransport{respond: func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != f.certURL {
			return nil, errors.New("unexpected fetch: " + req.URL.String())
		}
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(bytes.NewReader(f.certPEM)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}}
	withTransport(t, tr)
	return tr
}

// sign produces the body AWS would send: the message fields plus a real RSA
// signature over the canonical string for its type.
func (f *signingFixture) sign(t *testing.T, fields map[string]string, version string) []byte {
	t.Helper()
	keys, ok := snsSignableKeys[fields["Type"]]
	if !ok {
		t.Fatalf("no signable key set for type %q", fields["Type"])
	}
	var canonical strings.Builder
	for _, k := range keys {
		v, present := fields[k]
		if !present {
			continue
		}
		canonical.WriteString(k)
		canonical.WriteByte('\n')
		canonical.WriteString(v)
		canonical.WriteByte('\n')
	}

	var hash crypto.Hash
	var digest []byte
	if version == "1" {
		hash = crypto.SHA1
		sum := sha1.Sum([]byte(canonical.String()))
		digest = sum[:]
	} else {
		hash = crypto.SHA256
		sum := sha256.Sum256([]byte(canonical.String()))
		digest = sum[:]
	}
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, hash, digest)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	body := map[string]string{}
	for k, v := range fields {
		body[k] = v
	}
	body["SignatureVersion"] = version
	body["Signature"] = base64.StdEncoding.EncodeToString(sig)
	body["SigningCertURL"] = f.certURL

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func confirmationFields() map[string]string {
	return map[string]string{
		"Type":         "SubscriptionConfirmation",
		"MessageId":    "165545c9-2a5c-472c-8df2-7ff2be2b3b1b",
		"Token":        "2336412f37",
		"TopicArn":     "arn:aws:sns:us-east-1:123456789012:MyTopic",
		"Message":      "You have chosen to subscribe to the topic.",
		"SubscribeURL": "https://sns.us-east-1.amazonaws.com/?Action=ConfirmSubscription&Token=2336412f37",
		"Timestamp":    time.Now().UTC().Format(time.RFC3339),
	}
}

// TestVerifySNSMessage_ValidSignature proves a genuinely signed message passes,
// for both signature versions AWS emits.
func TestVerifySNSMessage_ValidSignature(t *testing.T) {
	for _, version := range []string{"1", "2"} {
		t.Run("SignatureVersion "+version, func(t *testing.T) {
			resetCertCache(t)
			f := newSigningFixture(t)
			f.serve(t)

			raw := f.sign(t, confirmationFields(), version)
			var msg SNSMessage
			if err := json.Unmarshal(raw, &msg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if err := verifySNSMessage(raw, &msg); err != nil {
				t.Fatalf("verifySNSMessage = %v, want success", err)
			}
		})
	}
}

// TestVerifySNSMessage_Rejects covers every way an attacker-supplied body fails
// the gate. Each case starts from a body that verifies and breaks exactly one
// thing, so a pass here cannot be an accident of an already-invalid fixture.
func TestVerifySNSMessage_Rejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(fields map[string]string, msg *SNSMessage, raw []byte) []byte
	}{
		{
			name: "tampered subscribe url",
			mutate: func(_ map[string]string, msg *SNSMessage, raw []byte) []byte {
				// The exact attack: keep AWS's signature, swap the URL the
				// server would fetch.
				msg.SubscribeURL = "https://169.254.169.254/latest/meta-data/"
				return mustRepack(raw, "SubscribeURL", msg.SubscribeURL)
			},
		},
		{
			name: "tampered message body",
			mutate: func(_ map[string]string, msg *SNSMessage, raw []byte) []byte {
				msg.Message = "injected"
				return mustRepack(raw, "Message", msg.Message)
			},
		},
		{
			name: "corrupted signature",
			mutate: func(_ map[string]string, msg *SNSMessage, raw []byte) []byte {
				msg.Signature = base64.StdEncoding.EncodeToString([]byte("not a signature"))
				return mustRepack(raw, "Signature", msg.Signature)
			},
		},
		{
			name: "signature is not base64",
			mutate: func(_ map[string]string, msg *SNSMessage, raw []byte) []byte {
				msg.Signature = "%%%%"
				return mustRepack(raw, "Signature", msg.Signature)
			},
		},
		{
			name: "no signature at all",
			mutate: func(_ map[string]string, msg *SNSMessage, raw []byte) []byte {
				msg.Signature = ""
				return mustRepack(raw, "Signature", "")
			},
		},
		{
			name: "unknown signature version",
			mutate: func(_ map[string]string, msg *SNSMessage, raw []byte) []byte {
				msg.SignatureVersion = "99"
				return mustRepack(raw, "SignatureVersion", "99")
			},
		},
		{
			name: "signing cert url off the allowlist",
			mutate: func(_ map[string]string, msg *SNSMessage, raw []byte) []byte {
				msg.SigningCertURL = "https://evil.example.com/cert.pem"
				return mustRepack(raw, "SigningCertURL", msg.SigningCertURL)
			},
		},
		{
			name: "replayed message",
			mutate: func(_ map[string]string, msg *SNSMessage, raw []byte) []byte {
				old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
				msg.Timestamp = old
				return mustRepack(raw, "Timestamp", old)
			},
		},
		{
			name: "unknown message type",
			mutate: func(_ map[string]string, msg *SNSMessage, raw []byte) []byte {
				msg.Type = "Whatever"
				return mustRepack(raw, "Type", "Whatever")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetCertCache(t)
			f := newSigningFixture(t)
			f.serve(t)

			fields := confirmationFields()
			raw := f.sign(t, fields, "1")
			var msg SNSMessage
			if err := json.Unmarshal(raw, &msg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			raw = tc.mutate(fields, &msg, raw)

			if err := verifySNSMessage(raw, &msg); err == nil {
				t.Fatal("verifySNSMessage accepted the message, want rejection")
			}
		})
	}
}

// TestVerifySNSMessage_CanonicalStringComesFromTheRawBody proves the signed
// form is rebuilt from the wire bytes, not from the decoded struct: a field the
// struct does not model cannot be dropped out of the canonical string to forge
// a match.
func TestVerifySNSMessage_CanonicalStringComesFromTheRawBody(t *testing.T) {
	resetCertCache(t)
	f := newSigningFixture(t)
	f.serve(t)

	fields := map[string]string{
		"Type":      "Notification",
		"MessageId": "22b80b92-fdea-4c2c-8f9d-bdfb0c7bf324",
		"TopicArn":  "arn:aws:sns:us-east-1:123456789012:MyTopic",
		"Subject":   "an optional subject",
		"Message":   "hello",
		"Timestamp": time.Now().UTC().Format(time.RFC3339),
	}
	raw := f.sign(t, fields, "2")
	var msg SNSMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := verifySNSMessage(raw, &msg); err != nil {
		t.Fatalf("signed notification with a Subject rejected: %v", err)
	}

	// Removing the signed Subject from the wire body must break the signature.
	stripped := mustDelete(raw, "Subject")
	if err := verifySNSMessage(stripped, &msg); err == nil {
		t.Fatal("dropping a signed field still verified, want rejection")
	}
}

func TestSNSCanonicalString_RejectsNonStringSignedField(t *testing.T) {
	_, err := snsCanonicalString([]byte(`{"Type":"Notification","Message":{"nested":true}}`), snsSignableKeys["Notification"])
	if err == nil {
		t.Fatal("a non-string signed field was accepted, want rejection")
	}
}

// TestDecodeSNSMessage_RefusesCaseVariantFields is the parser-differential
// guarantee. encoding/json matches struct fields case-insensitively and lets
// the last matching key win, while the canonical string is rebuilt by
// exact-key lookup — so a body carrying both "Message" and "message" would be
// signed over one value and acted on with the other. The decode is what stops
// the two from ever reading different bytes.
func TestDecodeSNSMessage_RefusesCaseVariantFields(t *testing.T) {
	for name, body := range map[string]string{
		"message payload swapped":  `{"Type":"Notification","MessageId":"m","TopicArn":"arn:signed","Message":"safe","message":"injected","Timestamp":"t"}`,
		"topic pin swapped":        `{"Type":"Notification","MessageId":"m","TopicArn":"arn:attacker","topicarn":"arn:victim","Message":"m","Timestamp":"t"}`,
		"subscribe url swapped":    `{"Type":"SubscriptionConfirmation","MessageId":"m","TopicArn":"a","Message":"m","SubscribeURL":"https://sns.us-east-1.amazonaws.com/?signed","SUBSCRIBEURL":"https://sns.us-east-1.amazonaws.com/?other","Timestamp":"t"}`,
		"message type swapped":     `{"Type":"Notification","type":"SubscriptionConfirmation","MessageId":"m","TopicArn":"a","Message":"m","Timestamp":"t"}`,
		"signing cert url swapped": `{"Type":"Notification","MessageId":"m","TopicArn":"a","Message":"m","Timestamp":"t","SigningCertURL":"https://sns.us-east-1.amazonaws.com/a.pem","signingcerturl":"https://evil.example/b.pem"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var msg SNSMessage
			if err := decodeSNSMessage([]byte(body), &msg); err == nil {
				t.Fatal("a case-variant duplicate field was accepted, want rejection")
			}
		})
	}
}

// TestDecodeSNSMessage_CaseVariantWouldOtherwiseForgeAValidMessage proves the
// case above is a real bypass and not a theoretical one: the tampered body
// still passes signature verification, because the signature covers the
// exact-case field. Only the decode refuses it.
func TestDecodeSNSMessage_CaseVariantWouldOtherwiseForgeAValidMessage(t *testing.T) {
	resetCertCache(t)
	f := newSigningFixture(t)
	f.serve(t)

	raw := f.sign(t, confirmationFields(), "2")
	var msg SNSMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	fields["message"] = json.RawMessage(`"injected"`)
	tampered, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := verifySNSMessage(tampered, &msg); err != nil {
		t.Fatalf("the tampered body failed verification (%v) — this test no longer proves the decode is what closes the gap", err)
	}
	var decoded SNSMessage
	if err := decodeSNSMessage(tampered, &decoded); err == nil {
		t.Fatalf("decodeSNSMessage accepted a body that verifies while carrying %q, want rejection", "injected")
	}
}

func TestDecodeSNSMessage_AcceptsAGenuineBody(t *testing.T) {
	f := newSigningFixture(t)
	raw := f.sign(t, confirmationFields(), "2")
	var msg SNSMessage
	if err := decodeSNSMessage(raw, &msg); err != nil {
		t.Fatalf("decodeSNSMessage = %v, want success", err)
	}
	if msg.Type != "SubscriptionConfirmation" {
		t.Fatalf("Type = %q, want SubscriptionConfirmation", msg.Type)
	}
	// A field AWS may add that this struct does not model must not be a reason
	// to refuse an otherwise well-formed body.
	withExtra := mustRepack(raw, "UnsubscribeURL", "https://sns.us-east-1.amazonaws.com/?Action=Unsubscribe")
	if err := decodeSNSMessage(withExtra, &msg); err != nil {
		t.Fatalf("decodeSNSMessage rejected an unmodelled field: %v", err)
	}
}

func TestSNSPublicKeyFromPEM_RejectsNonCertificate(t *testing.T) {
	for _, body := range [][]byte{
		[]byte("not pem at all"),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a certificate")}),
	} {
		if _, err := snsPublicKeyFromPEM(body); err == nil {
			t.Errorf("snsPublicKeyFromPEM(%q) succeeded, want failure", body)
		}
	}
}

// mustRepack rewrites one top-level string field in a JSON body, leaving the
// rest of the bytes semantically unchanged.
func mustRepack(raw []byte, key, value string) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		panic(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	fields[key] = encoded
	out, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return out
}

func mustDelete(raw []byte, key string) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		panic(err)
	}
	delete(fields, key)
	out, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return out
}
