package controllers

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The SNS HTTPS endpoint is unauthenticated by design: anyone who can reach it
// can post a body. Every URL in that body (SubscribeURL, SigningCertURL) is
// therefore attacker-controlled until proven otherwise, and fetching one
// unchecked turns this process into a request proxy for whatever the network
// can reach — internal services, the cloud instance metadata endpoint,
// localhost admin ports. Nothing in a message is acted on before the RSA
// signature over its canonical form verifies against an Amazon-served
// certificate, and both fetches are pinned to public AWS SNS hosts, denied
// redirects, denied non-public destination addresses, bounded in time and
// bounded in body size.

const (
	// snsFetchTimeout bounds each outbound fetch end to end.
	snsFetchTimeout = 10 * time.Second
	// snsMaxCertBytes bounds a signing certificate download. A real SNS PEM is
	// ~1.5 KB.
	snsMaxCertBytes = 32 << 10
	// snsMaxConfirmBytes bounds the subscription-confirmation response. The
	// body is never read for content — only drained so the connection can be
	// reused — so this only has to stop an unbounded stream.
	snsMaxConfirmBytes = 64 << 10
	// snsMaxSignatureBytes bounds the base64 signature before it is decoded.
	snsMaxSignatureBytes = 4 << 10
	// snsMaxSkew is how far a message timestamp may sit from now. A captured
	// message replayed later than this is refused.
	snsMaxSkew = time.Hour
	// snsCertCacheMax bounds the signing-certificate cache. AWS rotates through
	// a small number of certificates; the cap stops an attacker who can pass
	// host validation from growing the map without limit.
	snsCertCacheMax = 32
	// snsMaxCertFetches bounds how many signing-certificate downloads may be in
	// flight at once. The key is needed before a signature can be judged, so
	// every unverified POST naming an uncached certificate would otherwise open
	// its own outbound connection.
	snsMaxCertFetches = 4
)

// snsHostPattern is the only host shape either URL in an SNS message may name:
// sns.<region>.amazonaws.com, plus the .cn partition. Anchored, lowercase, and
// with a region charset that cannot express a dotted-quad, so no IP literal and
// no attacker-chosen subdomain can match.
var snsHostPattern = regexp.MustCompile(`^sns\.[a-z0-9][a-z0-9-]{0,30}[a-z0-9]\.amazonaws\.com(\.cn)?$`)

// snsCertPathPattern is the only path shape SNS serves a signing certificate
// from. Matched against the escaped path, so a percent-encoded spelling of the
// same name is refused rather than validated in decoded form and fetched in
// encoded form.
var snsCertPathPattern = regexp.MustCompile(`^/SimpleNotificationService-[A-Za-z0-9]{8,64}\.pem$`)

var (
	errSNSNotAWSHost      = errors.New("url host is not an AWS SNS endpoint")
	errSNSNotHTTPS        = errors.New("url scheme is not https")
	errSNSURLUserinfo     = errors.New("url carries userinfo")
	errSNSURLPort         = errors.New("url carries an explicit port")
	errSNSURLOpaque       = errors.New("url is not an absolute http url")
	errSNSBlockedAddress  = errors.New("destination address is not a public address")
	errSNSRedirect        = errors.New("redirect refused")
	errSNSBadSignature    = errors.New("sns message signature is not valid")
	errSNSUnsupportedSigV = errors.New("unsupported sns signature version")
	errSNSStaleMessage    = errors.New("sns message timestamp is outside the accepted window")
	errSNSNoCert          = errors.New("signing certificate did not contain an rsa public key")
	errSNSAmbiguousField  = errors.New("sns body carries a field name that differs only by case")
	errSNSCertFetchBusy   = errors.New("too many signing certificate fetches in flight")
)

// snsModelledFields are the JSON names SNSMessage binds. encoding/json matches
// them case-insensitively and lets the last matching key win, while the
// canonical string is rebuilt by exact-key lookup. A body carrying both
// "Message" and "message" would therefore be signed over one value and acted on
// with the other — a signature bypass. Refuse the divergence instead of
// parsing it.
var snsModelledFields = []string{
	"Type", "MessageId", "Token", "TopicArn", "Subject", "Message",
	"SubscribeURL", "Timestamp", "SignatureVersion", "Signature", "SigningCertURL",
}

// decodeSNSMessage is the only way this package turns a request body into an
// SNSMessage. It admits the body only when every top-level key is unambiguous
// case-insensitively and every modelled field is spelled exactly as the struct
// spells it, so the bytes the signature covers and the bytes the handler acts
// on are the same bytes.
func decodeSNSMessage(rawBody []byte, msg *SNSMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &fields); err != nil {
		return fmt.Errorf("parse sns body: %w", err)
	}
	seen := make(map[string]string, len(fields))
	for k := range fields {
		lower := strings.ToLower(k)
		if prev, dup := seen[lower]; dup {
			return fmt.Errorf("%w: %q, %q", errSNSAmbiguousField, prev, k)
		}
		seen[lower] = k
	}
	for _, name := range snsModelledFields {
		if got, ok := seen[strings.ToLower(name)]; ok && got != name {
			return fmt.Errorf("%w: %q", errSNSAmbiguousField, got)
		}
	}
	return json.Unmarshal(rawBody, msg)
}

// validateSNSURL accepts only an absolute HTTPS URL served by a public AWS SNS
// host on the default port, with no userinfo. Everything else — http, a
// non-AWS host, an IP literal, an embedded credential, an alternate port — is
// refused before any socket is opened.
func validateSNSURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	if u.Opaque != "" || !u.IsAbs() {
		return nil, errSNSURLOpaque
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, errSNSNotHTTPS
	}
	if u.User != nil {
		return nil, errSNSURLUserinfo
	}
	if u.Port() != "" {
		return nil, errSNSURLPort
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if _, err := netip.ParseAddr(host); err == nil {
		return nil, errSNSNotAWSHost
	}
	if !snsHostPattern.MatchString(host) {
		return nil, errSNSNotAWSHost
	}
	// Collapse the spellings DNS treats as one name — mixed case, a trailing
	// root dot, an empty ":" port — onto the form that was validated, so the URL
	// that is fetched and cached cannot vary while naming the same host.
	u.Host = host
	return u, nil
}

// snsAddrAllowed reports whether a resolved destination address may be
// connected to. Host validation alone is not enough: DNS for a name that passes
// the pattern still resolves at connect time, so the address itself is checked
// after resolution and immediately before the connect syscall, which leaves no
// window to rebind it.
func snsAddrAllowed(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return errSNSBlockedAddress
	}
	// An IPv4 address tunnelled inside IPv6 (::ffff:169.254.169.254) is the
	// same destination as the bare v4 address, so judge the unwrapped form.
	addr = addr.Unmap()
	if snsBlockedAddr(addr) {
		return fmt.Errorf("%w: %s", errSNSBlockedAddress, addr)
	}
	return nil
}

// nat64Prefix is the well-known NAT64 range: a v6 address here embeds a v4
// destination in its low 32 bits, so it can carry a private target past a
// naive v6 check. Refused outright rather than unwrapped.
var nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")

// snsBlockedAddr reports whether an address names something other than a public
// internet destination. Loopback, link-local (which is where the 169.254.169.254
// instance metadata service lives), RFC1918 and IPv6 unique-local, carrier NAT,
// multicast, and the reserved/benchmark ranges are all refused.
func snsBlockedAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	if addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsMulticast() {
		return true
	}
	if nat64Prefix.Contains(addr) {
		return true
	}
	for _, p := range snsReservedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// snsReservedPrefixes are the non-public ranges Go's netip predicates do not
// already cover.
var snsReservedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),          // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),      // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),       // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),       // documentation
	netip.MustParsePrefix("192.88.99.0/24"),     // 6to4 relay anycast
	netip.MustParsePrefix("198.18.0.0/15"),      // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"),    // documentation
	netip.MustParsePrefix("203.0.113.0/24"),     // documentation
	netip.MustParsePrefix("240.0.0.0/4"),        // reserved
	netip.MustParsePrefix("255.255.255.255/32"), // broadcast
	netip.MustParsePrefix("::/96"),              // unspecified and IPv4-compatible
	netip.MustParsePrefix("64:ff9b:1::/48"),     // local-use NAT64
	netip.MustParsePrefix("100::/64"),           // discard-only
	netip.MustParsePrefix("2001::/32"),          // Teredo, embeds a v4 destination
	netip.MustParsePrefix("2001:db8::/32"),      // documentation
	netip.MustParsePrefix("2002::/16"),          // 6to4, embeds a v4 destination
}

// snsHTTPClient is the only client used for message-driven fetches. Package
// level so a test can substitute a transport; production never reassigns it.
var snsHTTPClient = newSNSHTTPClient()

func newSNSHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			return snsAddrAllowed(address)
		},
	}
	return &http.Client{
		Timeout: snsFetchTimeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 5 * time.Second,
		},
		// A redirect is a second attacker-chosen destination. Refuse rather
		// than re-validate: SNS never redirects its own endpoints.
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			return fmt.Errorf("%w: %s", errSNSRedirect, req.URL.Host)
		},
	}
}

// snsFetch issues the one bounded GET this package is allowed to make and
// returns at most limit bytes of the body. The body is returned only to the
// certificate parser; it is never logged and never written to a response.
func snsFetch(ctx context.Context, u *url.URL, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := snsHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, limit))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, err
	}
	return body, nil
}

// confirmSNSSubscription validates SubscribeURL and, only if it passes, issues
// the bounded confirmation GET. The response body is discarded — echoing it
// into a log or a reply would hand back whatever the fetch reached.
func confirmSNSSubscription(ctx context.Context, subscribeURL string) error {
	u, err := validateSNSURL(subscribeURL)
	if err != nil {
		return fmt.Errorf("subscribe url rejected: %w", err)
	}
	if _, err := snsFetch(ctx, u, snsMaxConfirmBytes); err != nil {
		return fmt.Errorf("subscription confirmation failed: %w", err)
	}
	return nil
}

// snsSignableKeys lists, per message type, the fields AWS signs — in the sorted
// order the canonical string requires. A field absent from the message is
// skipped; a field present with an empty value is still signed.
var snsSignableKeys = map[string][]string{
	"Notification":             {"Message", "MessageId", "Subject", "Timestamp", "TopicArn", "Type"},
	"SubscriptionConfirmation": {"Message", "MessageId", "SubscribeURL", "Timestamp", "Token", "TopicArn", "Type"},
	"UnsubscribeConfirmation":  {"Message", "MessageId", "SubscribeURL", "Timestamp", "Token", "TopicArn", "Type"},
}

// verifySNSMessage is the gate every SNS request passes before any field of it
// is used. It rebuilds AWS's canonical string from the raw body — not from the
// decoded struct, so a field this service does not model cannot be dropped from
// the signed form — and verifies the RSA signature against the certificate AWS
// serves at SigningCertURL.
func verifySNSMessage(rawBody []byte, msg *SNSMessage) error {
	keys, ok := snsSignableKeys[msg.Type]
	if !ok {
		return fmt.Errorf("unsupported sns message type %q", msg.Type)
	}

	var hash crypto.Hash
	switch msg.SignatureVersion {
	case "1":
		hash = crypto.SHA1
	case "2":
		hash = crypto.SHA256
	default:
		return fmt.Errorf("%w: %q", errSNSUnsupportedSigV, msg.SignatureVersion)
	}

	if err := snsTimestampFresh(msg.Timestamp); err != nil {
		return err
	}

	if len(msg.Signature) > snsMaxSignatureBytes {
		return errSNSBadSignature
	}
	sig, err := base64.StdEncoding.DecodeString(msg.Signature)
	if err != nil || len(sig) == 0 {
		return errSNSBadSignature
	}

	canonical, err := snsCanonicalString(rawBody, keys)
	if err != nil {
		return err
	}

	key, err := snsSigningKey(msg.SigningCertURL)
	if err != nil {
		return err
	}

	digest := snsDigest(hash, canonical)
	if err := rsa.VerifyPKCS1v15(key, hash, digest, sig); err != nil {
		return errSNSBadSignature
	}
	return nil
}

func snsDigest(hash crypto.Hash, canonical []byte) []byte {
	if hash == crypto.SHA1 {
		sum := sha1.Sum(canonical) // #nosec G401 -- AWS SignatureVersion 1 is defined as RSA-SHA1.
		return sum[:]
	}
	sum := sha256.Sum256(canonical)
	return sum[:]
}

// snsCanonicalString rebuilds "<name>\n<value>\n" for each signed field that is
// actually present in the body, reading the raw JSON so an unmodelled or
// optional field (Subject) is included exactly when AWS included it.
func snsCanonicalString(rawBody []byte, keys []string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &fields); err != nil {
		return nil, fmt.Errorf("parse sns body: %w", err)
	}
	var b strings.Builder
	for _, k := range keys {
		raw, ok := fields[k]
		if !ok {
			continue
		}
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			// A signed field that is not a JSON string is not something AWS
			// produces; refuse rather than coerce it into the canonical form.
			return nil, fmt.Errorf("sns field %q is not a string", k)
		}
		b.WriteString(k)
		b.WriteByte('\n')
		b.WriteString(v)
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}

// snsTimestampFresh bounds replay of a captured message.
func snsTimestampFresh(ts string) error {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(ts))
	if err != nil {
		return fmt.Errorf("%w: unparseable timestamp", errSNSStaleMessage)
	}
	if d := time.Since(t); d > snsMaxSkew || d < -snsMaxSkew {
		return errSNSStaleMessage
	}
	return nil
}

var (
	snsCertMu    sync.Mutex
	snsCertCache = map[string]*rsa.PublicKey{}
	// snsCertFetchSlots admits a bounded number of concurrent downloads.
	snsCertFetchSlots = make(chan struct{}, snsMaxCertFetches)
)

// snsSigningKey resolves SigningCertURL to an RSA public key. The URL is held
// to the same allowlist as SubscribeURL — an unchecked certificate URL is the
// identical SSRF one level down — and the fetched PEM is bounded and parsed as
// a certificate before its key is used.
func snsSigningKey(certURL string) (*rsa.PublicKey, error) {
	u, err := validateSNSURL(certURL)
	if err != nil {
		return nil, fmt.Errorf("signing cert url rejected: %w", err)
	}
	if !snsCertPathPattern.MatchString(u.EscapedPath()) {
		return nil, fmt.Errorf("signing cert url rejected: not an sns certificate path")
	}
	// A certificate is served from its path alone. Dropping the query and
	// fragment leaves one spelling per certificate, so a body cannot name the
	// same file in unlimited ways to miss the cache on every request.
	u.RawQuery, u.Fragment, u.RawFragment = "", "", ""
	canonical := u.String()

	snsCertMu.Lock()
	cached, ok := snsCertCache[canonical]
	snsCertMu.Unlock()
	if ok {
		return cached, nil
	}

	// The signature cannot be judged without this key, so an unverified body
	// reaches this fetch by design. Cap how many may be outstanding rather
	// than letting inbound request rate set the outbound connection count.
	select {
	case snsCertFetchSlots <- struct{}{}:
		defer func() { <-snsCertFetchSlots }()
	default:
		return nil, errSNSCertFetchBusy
	}

	ctx, cancel := context.WithTimeout(context.Background(), snsFetchTimeout)
	defer cancel()
	body, err := snsFetch(ctx, u, snsMaxCertBytes)
	if err != nil {
		return nil, fmt.Errorf("fetch signing cert: %w", err)
	}

	key, err := snsPublicKeyFromPEM(body)
	if err != nil {
		return nil, err
	}

	snsCertMu.Lock()
	if len(snsCertCache) >= snsCertCacheMax {
		snsCertCache = map[string]*rsa.PublicKey{}
	}
	snsCertCache[canonical] = key
	snsCertMu.Unlock()
	return key, nil
}

func snsPublicKeyFromPEM(body []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(body)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errSNSNoCert
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse signing cert: %w", err)
	}
	key, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, errSNSNoCert
	}
	return key, nil
}
