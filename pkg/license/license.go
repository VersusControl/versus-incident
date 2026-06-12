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
	"strings"
	"time"
)

// EnvVar is the environment variable the license token is read from.
const EnvVar = "LICENSE_KEY"

// embeddedPublicKeyB64 is the standard-base64 Ed25519 public key that
// every Versus binary trusts. It is a throwaway development key; the
// matching private key lives outside this repo (the license service).
// Rotating it is a code change + redeploy, by design — the OSS tree must
// never be able to mint a license.
const embeddedPublicKeyB64 = "42+RPOGr+BTpmm7374kSWIz0cviocQi9Os45/0IdgRI="

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
type License struct {
	enterprise bool
	claims     Claims
}

// header is the JWT header. Only EdDSA is accepted.
type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// Load reads LICENSE_KEY from the environment and validates it. A missing
// key returns a community License with ErrNoLicense (callers usually
// ignore the error and run in community mode). A present-but-invalid key
// returns a community License together with the validation error so the
// caller can log/refuse, never silently granting enterprise on a bad
// token.
func Load() (*License, error) {
	return loadFrom(os.Getenv(EnvVar), embeddedPublicKey())
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
	return &License{enterprise: true, claims: claims}, nil
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

// IsEnterpriseEnabled reports whether a valid enterprise license is in
// force. Always false in community mode.
func (l *License) IsEnterpriseEnabled() bool {
	if l == nil {
		return false
	}
	return l.enterprise
}

// Features returns the entitlements granted by the license. Empty in
// community mode. The returned slice is a copy callers may mutate freely.
func (l *License) Features() []string {
	if l == nil || !l.enterprise {
		return nil
	}
	return append([]string(nil), l.claims.FeatureList...)
}

// HasFeature reports whether a specific entitlement is granted.
func (l *License) HasFeature(name string) bool {
	if l == nil || !l.enterprise {
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
// community mode.
func (l *License) Claims() Claims {
	if l == nil || !l.enterprise {
		return Claims{}
	}
	c := l.claims
	c.FeatureList = append([]string(nil), l.claims.FeatureList...)
	return c
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
