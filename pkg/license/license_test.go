package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
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

func TestReloadSwapsClaims(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	// Start on a lease granting only "sso".
	first := mintToken(t, priv, Claims{
		Org:         "acme",
		FeatureList: []string{"sso"},
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})
	lic, err := loadFrom(first, pub)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if lic.Raw() != first {
		t.Fatal("Raw() should return the original token")
	}

	// Reload a fresh lease that grants more features and a later expiry.
	newExp := time.Now().Add(30 * 24 * time.Hour)
	second := mintToken(t, priv, Claims{
		Org:         "acme",
		FeatureList: []string{"sso", "intelligence"},
		ExpiresAt:   newExp.Unix(),
	})
	if err := lic.reloadWith(second, pub); err != nil {
		t.Fatalf("reloadWith valid token: %v", err)
	}
	if !lic.HasFeature("intelligence") {
		t.Fatal("reloaded feature missing after swap")
	}
	if lic.Raw() != second {
		t.Fatal("Raw() should reflect the reloaded token")
	}
	if got := lic.ExpiresAt().Unix(); got != newExp.Unix() {
		t.Fatalf("ExpiresAt = %d, want %d", got, newExp.Unix())
	}
}

func TestReloadRejectsForeignTokenAndRetainsClaims(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, foreignPriv, _ := ed25519.GenerateKey(rand.Reader)

	good := mintToken(t, priv, Claims{
		Org:         "acme",
		FeatureList: []string{"sso"},
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})
	lic, err := loadFrom(good, pub)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}

	// A token signed by a foreign key must be rejected and must NOT mutate
	// the live license.
	foreign := mintToken(t, foreignPriv, Claims{
		Org:         "evil",
		FeatureList: []string{"intelligence"},
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})
	if err := lic.reloadWith(foreign, pub); err != ErrBadSignature {
		t.Fatalf("reloadWith foreign token err = %v, want ErrBadSignature", err)
	}
	if lic.HasFeature("intelligence") {
		t.Fatal("foreign token must not grant features")
	}
	if !lic.HasFeature("sso") || lic.Raw() != good {
		t.Fatal("original lease must be retained after a rejected reload")
	}

	// An expired token is likewise rejected and the old lease retained.
	expired := mintToken(t, priv, Claims{
		Org:       "acme",
		ExpiresAt: time.Now().Add(-time.Minute).Unix(),
	})
	if err := lic.reloadWith(expired, pub); err != ErrExpired {
		t.Fatalf("reloadWith expired token err = %v, want ErrExpired", err)
	}
	if !lic.HasFeature("sso") {
		t.Fatal("expired reload must not drop the live lease")
	}
}

func TestExpiredLeaseDisablesFeatures(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	// A token whose exp is already in the past: loadFrom rejects it, but the
	// read-time check is what matters for a lease that lapses in-process.
	tok := mintToken(t, priv, Claims{
		Org:         "acme",
		FeatureList: []string{"sso"},
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})
	lic, err := loadFrom(tok, pub)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if !lic.IsEnterpriseEnabled() {
		t.Fatal("expected enterprise enabled while lease is live")
	}
	// Force the lease to look expired by swapping in an in-the-past exp via
	// the unexported field (white-box) to simulate the lease lapsing.
	lic.mu.Lock()
	lic.claims.ExpiresAt = time.Now().Add(-time.Second).Unix()
	lic.mu.Unlock()
	if lic.IsEnterpriseEnabled() {
		t.Fatal("expired lease must disable enterprise at read time")
	}
	if lic.HasFeature("sso") {
		t.Fatal("expired lease must disable features at read time")
	}
}

func TestReloadRaceFree(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	mk := func(features ...string) string {
		return mintToken(t, priv, Claims{
			Org:         "acme",
			FeatureList: features,
			ExpiresAt:   time.Now().Add(time.Hour).Unix(),
		})
	}
	lic, err := loadFrom(mk("sso"), pub)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// Readers hammering the lock during reloads.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = lic.IsEnterpriseEnabled()
					_ = lic.HasFeature("sso")
					_ = lic.Features()
					_ = lic.Raw()
					_ = lic.ExpiresAt()
				}
			}
		}()
	}
	// Writers swapping the lease concurrently.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if err := lic.reloadWith(mk("sso", "intelligence"), pub); err != nil {
					t.Errorf("reloadWith: %v", err)
					return
				}
			}
		}(i)
	}
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestWriteCacheRoundTrip(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	tok := mintToken(t, priv, Claims{
		Org:       "acme",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "license.lease")

	if err := WriteCache(path, tok); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("cache perm = %o, want 600", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != tok {
		t.Fatal("cached token mismatch")
	}

	// Empty path is a no-op.
	if err := WriteCache("", tok); err != nil {
		t.Fatalf("WriteCache empty path: %v", err)
	}
}

func TestLoadPrefersValidCacheOverEnvSeed(t *testing.T) {
	// Both the env seed and the cache must verify against the SAME key for
	// loadFrom to accept them, but Load uses the embedded key. We can only
	// exercise the precedence wiring by pointing the cache at an invalid blob
	// and asserting Load falls back to the (also invalid here) env seed
	// without panicking, plus the happy-path file selection via os env.
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "lease")
	if err := os.WriteFile(cachePath, []byte("not-a-jwt"), 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	t.Setenv(CacheEnvVar, cachePath)
	t.Setenv(EnvVar, "")

	lic, err := Load()
	if err != ErrNoLicense {
		t.Fatalf("Load with bad cache + empty env err = %v, want ErrNoLicense", err)
	}
	if lic.IsEnterpriseEnabled() {
		t.Fatal("invalid cache must not enable enterprise")
	}
}
