package services

import (
	"bytes"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

var testAckKey = []byte("unit-test-signing-key-0123456789ab")

func TestSignAckToken_Deterministic(t *testing.T) {
	a := SignAckToken(testAckKey, "inc-1", 1000)
	b := SignAckToken(testAckKey, "inc-1", 1000)
	if a != b {
		t.Fatalf("signature not deterministic: %q != %q", a, b)
	}
	// A different id or exp yields a different signature.
	if SignAckToken(testAckKey, "inc-2", 1000) == a {
		t.Fatal("signature did not change with a different incident id")
	}
	if SignAckToken(testAckKey, "inc-1", 1001) == a {
		t.Fatal("signature did not change with a different exp")
	}
	if SignAckToken([]byte("other-key-0123456789abcdef012345"), "inc-1", 1000) == a {
		t.Fatal("signature did not change with a different key")
	}
}

func TestVerifyAckToken_Valid(t *testing.T) {
	now := time.Unix(1000, 0)
	exp := now.Add(time.Hour).Unix()
	sig := SignAckToken(testAckKey, "inc-1", exp)
	if err := VerifyAckToken(testAckKey, "inc-1", exp, sig, now); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
}

func TestVerifyAckToken_Missing(t *testing.T) {
	now := time.Unix(1000, 0)
	exp := now.Add(time.Hour).Unix()
	if err := VerifyAckToken(testAckKey, "inc-1", exp, "", now); err != ErrAckTokenMissing {
		t.Fatalf("empty sig: got %v, want ErrAckTokenMissing", err)
	}
	sig := SignAckToken(testAckKey, "inc-1", exp)
	if err := VerifyAckToken(nil, "inc-1", exp, sig, now); err != ErrAckTokenMissing {
		t.Fatalf("nil key: got %v, want ErrAckTokenMissing", err)
	}
}

func TestVerifyAckToken_Expired(t *testing.T) {
	now := time.Unix(1000, 0)
	exp := now.Add(-time.Second).Unix()
	sig := SignAckToken(testAckKey, "inc-1", exp)
	if err := VerifyAckToken(testAckKey, "inc-1", exp, sig, now); err != ErrAckTokenExpired {
		t.Fatalf("expired token: got %v, want ErrAckTokenExpired", err)
	}
	// Exactly at now is treated as expired (exp <= now).
	expNow := now.Unix()
	sigNow := SignAckToken(testAckKey, "inc-1", expNow)
	if err := VerifyAckToken(testAckKey, "inc-1", expNow, sigNow, now); err != ErrAckTokenExpired {
		t.Fatalf("token expiring now: got %v, want ErrAckTokenExpired", err)
	}
}

func TestVerifyAckToken_Tampered(t *testing.T) {
	now := time.Unix(1000, 0)
	exp := now.Add(time.Hour).Unix()
	sig := SignAckToken(testAckKey, "inc-1", exp)

	// Swapped incident id (the IDOR attempt): the signature no longer matches.
	if err := VerifyAckToken(testAckKey, "inc-2", exp, sig, now); err != ErrAckTokenInvalid {
		t.Fatalf("swapped id: got %v, want ErrAckTokenInvalid", err)
	}
	// Extended exp with the original signature: rejected before expiry is trusted.
	if err := VerifyAckToken(testAckKey, "inc-1", exp+3600, sig, now); err != ErrAckTokenInvalid {
		t.Fatalf("extended exp: got %v, want ErrAckTokenInvalid", err)
	}
	// Garbled signature.
	if err := VerifyAckToken(testAckKey, "inc-1", exp, sig+"x", now); err != ErrAckTokenInvalid {
		t.Fatalf("garbled sig: got %v, want ErrAckTokenInvalid", err)
	}
}

func TestAckURL_SignedWhenKeyPresent(t *testing.T) {
	prev := AckSigningKey()
	SetAckSigningKey(testAckKey)
	t.Cleanup(func() { SetAckSigningKey(prev) })

	cfg := &config.Config{PublicHost: "https://versus.example"}
	url := AckURL(cfg, "inc-1", 30*time.Minute)
	// The URL must carry both query params so the endpoint can verify it.
	if !bytes.Contains([]byte(url), []byte("https://versus.example/api/ack/inc-1?exp=")) {
		t.Fatalf("unexpected ack url: %q", url)
	}
	if !bytes.Contains([]byte(url), []byte("&sig=")) {
		t.Fatalf("ack url missing signature: %q", url)
	}
}

func TestAckURL_BarePathWhenNoKey(t *testing.T) {
	prev := AckSigningKey()
	SetAckSigningKey(nil)
	t.Cleanup(func() { SetAckSigningKey(prev) })

	cfg := &config.Config{PublicHost: "https://versus.example"}
	if got, want := AckURL(cfg, "inc-1", 30*time.Minute), "https://versus.example/api/ack/inc-1"; got != want {
		t.Fatalf("bare ack url = %q, want %q", got, want)
	}
}

func TestAckURL_ExpReflectsTTL(t *testing.T) {
	prev := AckSigningKey()
	SetAckSigningKey(testAckKey)
	t.Cleanup(func() { SetAckSigningKey(prev) })

	cfg := &config.Config{PublicHost: "https://versus.example"}
	ttl := 3 * time.Minute
	before := time.Now().Add(ttl).Unix()
	url := AckURL(cfg, "inc-1", ttl)
	after := time.Now().Add(ttl).Unix()

	u, err := neturl.Parse(url)
	if err != nil {
		t.Fatalf("parse ack url: %v", err)
	}
	exp, err := strconv.ParseInt(u.Query().Get("exp"), 10, 64)
	if err != nil {
		t.Fatalf("parse exp: %v", err)
	}
	if exp < before || exp > after {
		t.Fatalf("exp = %d, want within [%d,%d] (now + ttl)", exp, before, after)
	}
	// The embedded exp must verify under the same key.
	if err := VerifyAckToken(testAckKey, "inc-1", exp, u.Query().Get("sig"), time.Now()); err != nil {
		t.Fatalf("VerifyAckToken on generated url: %v", err)
	}
}

func TestAckURL_FromLoadedPublicHost(t *testing.T) {
	const scenarioEnv = "VERSUS_ACK_URL_CONFIG_SCENARIO"
	if scenario := os.Getenv(scenarioEnv); scenario != "" {
		testAckURLFromLoadedPublicHost(t, scenario)
		return
	}

	for _, scenario := range []string{"normalized-origin", "custom-port", "empty-origin"} {
		t.Run(scenario, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestAckURL_FromLoadedPublicHost$")
			cmd.Env = append(os.Environ(), scenarioEnv+"="+scenario)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("config-loaded ack URL subprocess failed: %v\n%s", err, output)
			}
		})
	}
}

func testAckURLFromLoadedPublicHost(t *testing.T, scenario string) {
	publicHost := map[string]string{
		"normalized-origin": "HTTPS://Console.Example:443/",
		"custom-port":       "https://Console.Example:8443/",
		"empty-origin":      "",
	}[scenario]
	if _, ok := map[string]bool{"normalized-origin": true, "custom-port": true, "empty-origin": true}[scenario]; !ok {
		t.Fatalf("unknown scenario %q", scenario)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("public_host: '"+publicHost+"'\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.LoadConfig(path); err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg := config.GetConfig()

	SetAckSigningKey(nil)
	wantBare := map[string]string{
		"normalized-origin": "https://console.example/api/ack/inc-1",
		"custom-port":       "https://console.example:8443/api/ack/inc-1",
		"empty-origin":      "/api/ack/inc-1",
	}[scenario]
	if got := AckURL(cfg, "inc-1", 30*time.Minute); got != wantBare {
		t.Fatalf("bare ack URL = %q, want %q", got, wantBare)
	}

	if scenario != "normalized-origin" {
		return
	}
	SetAckSigningKey(testAckKey)
	signed := AckURL(cfg, "inc-1", 30*time.Minute)
	parsed, err := neturl.Parse(signed)
	if err != nil {
		t.Fatalf("parse signed ack URL: %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "console.example" || parsed.Path != "/api/ack/inc-1" {
		t.Fatalf("signed ack URL target = %s://%s%s, want https://console.example/api/ack/inc-1", parsed.Scheme, parsed.Host, parsed.Path)
	}
	query := parsed.Query()
	if len(query) != 2 || query.Get("exp") == "" || query.Get("sig") == "" {
		t.Fatalf("signed ack URL query = %q, want exactly exp and sig", parsed.RawQuery)
	}
	exp, err := strconv.ParseInt(query.Get("exp"), 10, 64)
	if err != nil {
		t.Fatalf("parse signed ack URL expiry: %v", err)
	}
	if err := VerifyAckToken(testAckKey, "inc-1", exp, query.Get("sig"), time.Now()); err != nil {
		t.Fatalf("verify signed ack URL: %v", err)
	}
}

func TestInitAckSigningKey_GeneratesAndPersists(t *testing.T) {
	prev := AckSigningKey()
	t.Cleanup(func() { SetAckSigningKey(prev) })

	store := storage.NewMemory()
	key := InitAckSigningKey(store, &config.Config{})
	if len(key) != ackSigningKeyLen {
		t.Fatalf("generated key len = %d, want %d", len(key), ackSigningKeyLen)
	}
	// It was persisted, so a second init (a restart / another replica) adopts
	// the SAME surviving key rather than generating a fresh one.
	key2 := InitAckSigningKey(store, &config.Config{})
	if !bytes.Equal(key, key2) {
		t.Fatal("second init did not adopt the persisted key")
	}
	if !bytes.Equal(AckSigningKey(), key) {
		t.Fatal("InitAckSigningKey did not install the key process-wide")
	}
}

func TestInitAckSigningKey_FallsBackToGatewaySecret(t *testing.T) {
	prev := AckSigningKey()
	t.Cleanup(func() { SetAckSigningKey(prev) })

	// A backend that does not implement BlobCreator forces the gateway-secret
	// fallback. noBlobCreator wraps memory but hides the capability.
	store := noBlobCreator{storage.NewMemory()}
	key := InitAckSigningKey(store, &config.Config{GatewaySecret: "shared-secret"})
	if string(key) != "shared-secret" {
		t.Fatalf("fallback key = %q, want the gateway secret", string(key))
	}
}

// noBlobCreator wraps a Provider while hiding BlobCreator so the generate-once
// path is skipped, forcing the gateway-secret fallback.
type noBlobCreator struct{ inner storage.Provider }

func (n noBlobCreator) ReadBlob(name string) ([]byte, error) { return n.inner.ReadBlob(name) }
func (n noBlobCreator) WriteBlob(name string, data []byte) error {
	return n.inner.WriteBlob(name, data)
}
func (n noBlobCreator) ListBlobs(prefix string) ([]storage.Blob, error) {
	return n.inner.ListBlobs(prefix)
}
func (n noBlobCreator) SaveIncident(rec *storage.IncidentRecord) error {
	return n.inner.SaveIncident(rec)
}
func (n noBlobCreator) UpdateIncidentAck(id string, ackedAt time.Time) error {
	return n.inner.UpdateIncidentAck(id, ackedAt)
}
func (n noBlobCreator) GetIncident(id string) (*storage.IncidentRecord, error) {
	return n.inner.GetIncident(id)
}
func (n noBlobCreator) ListIncidents(limit int) ([]*storage.IncidentRecord, error) {
	return n.inner.ListIncidents(limit)
}
func (n noBlobCreator) SaveAnalysis(rec *storage.AnalysisRecord) error {
	return n.inner.SaveAnalysis(rec)
}
func (n noBlobCreator) GetAnalysis(id string) (*storage.AnalysisRecord, error) {
	return n.inner.GetAnalysis(id)
}
func (n noBlobCreator) ListAnalysesByIncident(incidentID string, limit int) ([]*storage.AnalysisRecord, error) {
	return n.inner.ListAnalysesByIncident(incidentID, limit)
}
func (n noBlobCreator) ListAnalyses(limit int) ([]*storage.AnalysisRecord, error) {
	return n.inner.ListAnalyses(limit)
}
func (n noBlobCreator) DeleteAnalysis(id string) error { return n.inner.DeleteAnalysis(id) }
func (n noBlobCreator) Close() error                   { return n.inner.Close() }
