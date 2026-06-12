package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// mintToken builds a compact EdDSA JWT signed with priv. Used only by the
// tests — the real signer lives outside this repo.
func mintToken(t *testing.T, priv ed25519.PrivateKey, claims Claims) string {
	t.Helper()
	h := header{Alg: "EdDSA", Typ: "JWT"}
	hb, _ := json.Marshal(h)
	pb, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(hb) + "." + enc.EncodeToString(pb)
	sig := ed25519.Sign(priv, []byte(signingInput))
	return signingInput + "." + enc.EncodeToString(sig)
}

func TestValidSelfSignedToken(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	tok := mintToken(t, priv, Claims{
		Org:         "acme",
		Subject:     "cust-123",
		FeatureList: []string{"sso", "multi-tenant"},
		IssuedAt:    time.Now().Unix(),
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})

	lic, err := loadFrom(tok, pub)
	if err != nil {
		t.Fatalf("loadFrom valid token: %v", err)
	}
	if !lic.IsEnterpriseEnabled() {
		t.Fatal("expected enterprise enabled for valid token")
	}
	if !lic.HasFeature("sso") || !lic.HasFeature("multi-tenant") {
		t.Fatalf("missing features: %v", lic.Features())
	}
	if lic.HasFeature("nope") {
		t.Fatal("unexpected feature")
	}
	if got := lic.Claims().Org; got != "acme" {
		t.Fatalf("org = %q, want acme", got)
	}
}

func TestCommunityModeWhenAbsent(t *testing.T) {
	lic, err := loadFrom("", embeddedPublicKey())
	if err != ErrNoLicense {
		t.Fatalf("err = %v, want ErrNoLicense", err)
	}
	if lic.IsEnterpriseEnabled() {
		t.Fatal("community mode must not enable enterprise")
	}
	if lic.Features() != nil {
		t.Fatalf("community Features() = %v, want nil", lic.Features())
	}
}

func TestRejectsForeignSignature(t *testing.T) {
	// Token signed by an attacker key must NOT validate against a
	// different (e.g. embedded) public key.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)

	tok := mintToken(t, priv, Claims{Org: "evil"})
	lic, err := loadFrom(tok, otherPub)
	if err != ErrBadSignature {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
	if lic.IsEnterpriseEnabled() {
		t.Fatal("bad signature must not enable enterprise")
	}
}

func TestRejectsExpiredToken(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tok := mintToken(t, priv, Claims{
		Org:       "acme",
		ExpiresAt: time.Now().Add(-time.Minute).Unix(),
	})
	if _, err := loadFrom(tok, pub); err != ErrExpired {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
}

func TestRejectsMalformedToken(t *testing.T) {
	pub := embeddedPublicKey()
	for _, tok := range []string{"not-a-jwt", "a.b", "a.b.c.d"} {
		if _, err := loadFrom(tok, pub); err != ErrMalformed {
			t.Fatalf("loadFrom(%q) err = %v, want ErrMalformed", tok, err)
		}
	}
}

func TestEmbeddedPublicKeyValid(t *testing.T) {
	// The compiled-in key must decode to a proper Ed25519 public key.
	pub := embeddedPublicKey()
	if len(pub) != ed25519.PublicKeySize {
		t.Fatalf("embedded key size = %d, want %d", len(pub), ed25519.PublicKeySize)
	}
}
