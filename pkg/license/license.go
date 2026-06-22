// Package license is the OSS base of the Versus license shim (X2-T2).
//
// It reads a signed license token from the LICENSE_KEY environment
// variable and validates it OFFLINE against an embedded Ed25519 public
// key. There is no phone-home and no outbound network call — validation
// is pure local crypto, so an air-gapped deployment validates exactly
// like a connected one.
//
// Two modes:
//
//   - community — LICENSE_KEY is absent or empty. IsEnterpriseEnabled()
//     returns false and Features() is empty. This is the default for
//     every OSS deployment and changes no behaviour.
//   - enterprise — LICENSE_KEY holds a valid, unexpired token signed by
//     the Versus license private key. IsEnterpriseEnabled() returns true
//     and Features() lists the entitlements baked into the token.
//
// The token format is a compact EdDSA JWT (header.payload.signature, all
// base64url). The private signing key is held OUTSIDE this repository by
// the Versus license service; only the public verification key is
// embedded below, so the OSS tree can verify but never mint licenses.
package license

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// EnvVar is the environment variable the license token is read from.
const EnvVar = "LICENSE_KEY"

// CacheEnvVar is the environment variable holding the path to the on-disk
// lease cache. When set, Load() prefers a valid cached token over the
// LICENSE_KEY seed, and a long-running process that renews its lease can
// persist the fresh token here so it survives a restart. The cache is
// pure local file IO — no network, no enterprise coupling — so the OSS
// tree stays offline-only. An empty value disables the cache entirely.
const CacheEnvVar = "LICENSE_CACHE_PATH"

// embeddedPublicKeyB64 is the standard-base64 Ed25519 public key that
// every Versus binary trusts. It is a throwaway development key; the
// matching private key lives outside this repo (the license service).
// Rotating it is a code change + redeploy, by design — the OSS tree must
// never be able to mint a license.
const embeddedPublicKeyB64 = "gmlgNdAtvC8UTyk5uOg/ZowKvLzPqAwWzi8uCZW9W9Q="

var (
	// ErrNoLicense indicates LICENSE_KEY was absent/empty — community mode.
	ErrNoLicense = errors.New("license: no LICENSE_KEY set (community mode)")
	// ErrMalformed indicates the token is not a well-formed EdDSA JWT.
	ErrMalformed = errors.New("license: malformed token")
	// ErrBadSignature indicates the signature did not verify against the
	// embedded public key.
	ErrBadSignature = errors.New("license: signature verification failed")
	// ErrExpired indicates the token's exp claim is in the past.
	ErrExpired = errors.New("license: token expired")
)

// Claims is the validated payload of a license token. Only the fields
// Versus understands are decoded; unknown fields are ignored.
type Claims struct {
	// Org is the organization the license was issued to.
	Org string `json:"org,omitempty"`
	// Subject mirrors the JWT "sub" claim (customer / account id).
	Subject string `json:"sub,omitempty"`
	// FeatureList is the set of enabled enterprise entitlements.
	FeatureList []string `json:"features,omitempty"`
	// IssuedAt is the JWT "iat" claim (unix seconds).
	IssuedAt int64 `json:"iat,omitempty"`
	// ExpiresAt is the JWT "exp" claim (unix seconds). 0 means no expiry.
	ExpiresAt int64 `json:"exp,omitempty"`
}

// License is the result of evaluating LICENSE_KEY. The zero value is a
// valid community license (no enterprise features).
//
// A License is safe for concurrent use: every read goes through an RWMutex
// and Reload swaps the validated claims atomically under the write lock, so
// a long-running process can adopt a renewed lease without a restart while
// other goroutines keep calling HasFeature/IsEnterpriseEnabled. Because it
// carries a mutex a License must never be copied after first use — always
// pass *License.
type License struct {
	mu         sync.RWMutex
	enterprise bool
	claims     Claims
	// raw is the original compact token this license was loaded/reloaded
	// from. Exposed via Raw() so the enterprise renewer can present the
	// current lease as its renewal credential.
	raw string
}

// header is the JWT header. Only EdDSA is accepted.
type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// Load reads the license token and validates it OFFLINE. A cached token at
// LICENSE_CACHE_PATH (if set and valid) takes precedence over the
// LICENSE_KEY environment seed, so a lease renewed by a long-running
// process survives a restart; otherwise the LICENSE_KEY seed is used. A
// missing/empty token returns a community License with ErrNoLicense
// (callers usually ignore the error and run in community mode). A
// present-but-invalid token returns a community License together with the
// validation error so the caller can log/refuse, never silently granting
// enterprise on a bad token.
func Load() (*License, error) {
	pub := embeddedPublicKey()
	if path := strings.TrimSpace(os.Getenv(CacheEnvVar)); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if lic, err := loadFrom(string(data), pub); err == nil {
				return lic, nil
			}
			// A stale/invalid cache falls through to the env seed below;
			// the renewer overwrites it on the next successful renewal.
		}
	}
	return loadFrom(os.Getenv(EnvVar), pub)
}

// Parse validates an explicit token string against the embedded public
// key. Useful for tooling and tests that do not want to touch the
// environment.
func Parse(token string) (*License, error) {
	return loadFrom(token, embeddedPublicKey())
}

// loadFrom is the testable core: it validates token against pub.
func loadFrom(token string, pub ed25519.PublicKey) (*License, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return &License{}, ErrNoLicense
	}
	claims, err := verify(token, pub)
	if err != nil {
		return &License{}, err
	}
	return &License{enterprise: true, claims: claims, raw: token}, nil
}

// verify checks the compact EdDSA JWT signature and expiry, returning the
// decoded claims on success.
func verify(token string, pub ed25519.PublicKey) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, ErrMalformed
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var h header
	if err := json.Unmarshal(headerBytes, &h); err != nil {
		return Claims{}, ErrMalformed
	}
	if h.Alg != "EdDSA" {
		return Claims{}, ErrMalformed
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrMalformed
	}

	signingInput := []byte(parts[0] + "." + parts[1])
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, signingInput, sig) {
		return Claims{}, ErrBadSignature
	}

	var claims Claims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return Claims{}, ErrMalformed
	}
	if claims.ExpiresAt != 0 && time.Now().Unix() >= claims.ExpiresAt {
		return Claims{}, ErrExpired
	}
	return claims, nil
}

// activeLocked reports whether the license currently grants enterprise.
// The caller must hold at least the read lock. A license whose exp has
// passed is treated as inactive even though it verified at load time \u2014 the
// lease model means features turn OFF the moment the lease lapses, so a
// process that fails to renew naturally drops to community at the next read.
func (l *License) activeLocked() bool {
	if !l.enterprise {
		return false
	}
	if l.claims.ExpiresAt != 0 && time.Now().Unix() >= l.claims.ExpiresAt {
		return false
	}
	return true
}

// IsEnterpriseEnabled reports whether a valid, unexpired enterprise license
// is in force. Always false in community mode, and false once the current
// lease's exp has passed without a renewal.
func (l *License) IsEnterpriseEnabled() bool {
	if l == nil {
		return false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.activeLocked()
}

// Features returns the entitlements granted by the license. Empty in
// community mode or once the lease has expired. The returned slice is a
// copy callers may mutate freely.
func (l *License) Features() []string {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if !l.activeLocked() {
		return nil
	}
	return append([]string(nil), l.claims.FeatureList...)
}

// HasFeature reports whether a specific entitlement is granted by an
// in-force lease. False in community mode and once the lease has expired.
func (l *License) HasFeature(name string) bool {
	if l == nil {
		return false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if !l.activeLocked() {
		return false
	}
	for _, f := range l.claims.FeatureList {
		if f == name {
			return true
		}
	}
	return false
}

// Claims returns a copy of the validated license claims. Zero value in
// community mode or once the lease has expired.
func (l *License) Claims() Claims {
	if l == nil {
		return Claims{}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if !l.activeLocked() {
		return Claims{}
	}
	c := l.claims
	c.FeatureList = append([]string(nil), l.claims.FeatureList...)
	return c
}

// Raw returns the original compact token this license currently holds, or
// "" in community mode. The enterprise renewer presents it as the Bearer
// credential when asking the platform for a fresh lease.
func (l *License) Raw() string {
	if l == nil {
		return ""
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.raw
}

// ExpiresAt returns the current lease's expiry as a time.Time, or the zero
// time when there is no expiry (community mode, or a never-expiring token).
// The renewer reads it to decide how much lease time remains before it must
// refresh.
func (l *License) ExpiresAt() time.Time {
	if l == nil {
		return time.Time{}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if !l.enterprise || l.claims.ExpiresAt == 0 {
		return time.Time{}
	}
	return time.Unix(l.claims.ExpiresAt, 0)
}

// Reload verifies newToken OFFLINE against the embedded public key and, on
// success, atomically swaps the in-memory claims so a long-running process
// adopts a renewed lease without a restart. On any verification error the
// existing claims are retained unchanged and the error is returned — a bad
// or foreign-signed token can never downgrade or hijack a live license. It
// is pure local crypto: it verifies and swaps, it never fetches anything.
func (l *License) Reload(newToken string) error {
	return l.reloadWith(newToken, embeddedPublicKey())
}

// reloadWith is the testable core of Reload: it verifies newToken against
// pub and swaps the claims under the write lock on success.
func (l *License) reloadWith(newToken string, pub ed25519.PublicKey) error {
	if l == nil {
		return errors.New("license: Reload on nil License")
	}
	newToken = strings.TrimSpace(newToken)
	if newToken == "" {
		return ErrMalformed
	}
	claims, err := verify(newToken, pub)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.enterprise = true
	l.claims = claims
	l.raw = newToken
	l.mu.Unlock()
	return nil
}

// WriteCache atomically persists token to path with 0600 permissions so a
// renewed lease survives a restart (Load prefers it over the LICENSE_KEY
// seed). It is pure local file IO — no network — keeping the OSS tree
// offline-only. An empty path is a no-op (caching disabled). The write goes
// through a temp file + rename so a crash mid-write can never leave a
// truncated lease on disk.
func WriteCache(path, token string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".license-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(strings.TrimSpace(token)); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// embeddedPublicKey decodes the compiled-in verification key. It panics
// on a malformed constant because that is a build-time programming error,
// never a runtime input.
func embeddedPublicKey() ed25519.PublicKey {
	raw, err := base64.StdEncoding.DecodeString(embeddedPublicKeyB64)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		panic("license: invalid embedded public key")
	}
	return ed25519.PublicKey(raw)
}
