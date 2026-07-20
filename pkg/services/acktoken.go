package services

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// The ack link delivered through an alert channel authorizes acknowledging an
// incident, so it must not be forgeable from the incident id alone. Each link
// carries an expiry and an HMAC signature over (incidentID, exp); the ack
// endpoint recomputes the HMAC with the same key and rejects anything that is
// missing, expired, or tampered. This closes the ack IDOR: knowing an id is no
// longer enough to ack it.

var (
	// ErrAckTokenMissing is returned when no signature (or no signing key) is
	// present, so the request carries nothing to verify.
	ErrAckTokenMissing = errors.New("ack token missing")
	// ErrAckTokenExpired is returned when exp is at or before now.
	ErrAckTokenExpired = errors.New("ack token expired")
	// ErrAckTokenInvalid is returned when the HMAC does not match.
	ErrAckTokenInvalid = errors.New("ack token invalid")
)

const (
	// ackSigningKeyBlob is the fixed blob key the generate-once ack signing key
	// is persisted under. Lives beside any other generated secrets under the
	// secrets/ namespace.
	ackSigningKeyBlob = "secrets/ack-signing-key"
	// ackSigningKeyLen is the length in bytes of the generated HMAC key.
	ackSigningKeyLen = 32
)

// ackSigningKey is the process-wide HMAC key used to sign and verify ack
// tokens. Set once at startup by InitAckSigningKey. Never logged.
var ackSigningKey []byte

// SetAckSigningKey installs the HMAC key used to sign and verify ack tokens.
// Called once from main after storage is wired; tests set it directly.
func SetAckSigningKey(k []byte) { ackSigningKey = k }

// AckSigningKey returns the installed ack signing key, or nil when none has
// been configured (in which case ack verification fails closed).
func AckSigningKey() []byte { return ackSigningKey }

// SignAckToken returns base64url(HMAC-SHA256(key, incidentID + "." + exp)).
// It is pure — the signature depends only on its arguments — so it is the
// single point both the URL builder and the verifier compute the MAC.
func SignAckToken(key []byte, incidentID string, exp int64) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(incidentID + "." + strconv.FormatInt(exp, 10)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyAckToken checks that sig is a valid, unexpired signature over
// (incidentID, exp) under key, relative to now. It fails closed:
//   - no key configured or an empty signature ⇒ ErrAckTokenMissing
//   - HMAC mismatch (tampered id/exp/sig)      ⇒ ErrAckTokenInvalid
//   - exp at or before now                     ⇒ ErrAckTokenExpired
//
// The signature is verified BEFORE the expiry is trusted, so a forged exp
// cannot extend a link's life — a changed exp changes the MAC and is rejected.
// The comparison is constant-time (hmac.Equal) to avoid a timing oracle.
func VerifyAckToken(key []byte, incidentID string, exp int64, sig string, now time.Time) error {
	if len(key) == 0 || sig == "" {
		return ErrAckTokenMissing
	}
	expected := SignAckToken(key, incidentID, exp)
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return ErrAckTokenInvalid
	}
	if exp <= now.Unix() {
		return ErrAckTokenExpired
	}
	return nil
}

// AckURL builds the acknowledgment link injected into an alert's content. When
// a signing key is configured it embeds an expiry and signature so the endpoint
// can authenticate the request; without a key (verification cannot succeed
// anyway) it emits the bare path so the shape is unchanged. The URL is rendered
// verbatim by channel templates, so no template change is needed.
//
// ttl is the link's lifetime, chosen by the caller — for an incident that is
// the effective on-call acknowledgment wait window, so the link stays valid
// exactly as long as an ack can still forestall escalation.
func AckURL(cfg *config.Config, incidentID string, ttl time.Duration) string {
	key := AckSigningKey()
	if len(key) == 0 {
		return fmt.Sprintf("%s/api/ack/%s", cfg.PublicHost, incidentID)
	}
	exp := time.Now().Add(ttl).Unix()
	sig := SignAckToken(key, incidentID, exp)
	return fmt.Sprintf("%s/api/ack/%s?exp=%d&sig=%s", cfg.PublicHost, incidentID, exp, sig)
}

// InitAckSigningKey resolves the HMAC key used to sign and verify ack tokens,
// installs it process-wide, and returns it. Resolution order, HA-consistent:
//
//  1. Generate a random 32-byte key and persist it generate-once via the
//     storage.BlobCreator seam under ackSigningKeyBlob. Exactly one instance
//     wins the create; every instance then adopts the surviving bytes, so all
//     replicas sign with the same key and links survive restarts.
//  2. If the backend does not implement BlobCreator, fall back to
//     config.GatewaySecret when set (stable across restarts, operator-owned).
//  3. Otherwise generate an ephemeral in-memory key and warn that ack links
//     will not survive a restart.
//
// The key is never logged.
func InitAckSigningKey(store storage.Provider, cfg *config.Config) []byte {
	if key := generateAndPersistAckKey(store); key != nil {
		SetAckSigningKey(key)
		return key
	}
	if cfg != nil {
		if s := strings.TrimSpace(cfg.GatewaySecret); s != "" {
			SetAckSigningKey([]byte(s))
			return []byte(s)
		}
	}
	key := make([]byte, ackSigningKeyLen)
	if _, err := rand.Read(key); err != nil {
		log.Printf("ack: failed to generate an ephemeral signing key: %v", err)
	}
	log.Printf("ack: WARNING using an in-memory ack signing key — ack links will not survive a restart; configure a database storage backend or gateway_secret for a stable key")
	SetAckSigningKey(key)
	return key
}

// generateAndPersistAckKey elects a single generated key across every instance
// sharing the store via CreateBlobIfAbsent, returning the surviving bytes. It
// returns nil when the backend does not implement BlobCreator or the
// create/read path fails, so the caller can fall back.
func generateAndPersistAckKey(store storage.Provider) []byte {
	bc, ok := store.(storage.BlobCreator)
	if !ok {
		return nil
	}
	candidate := make([]byte, ackSigningKeyLen)
	if _, err := rand.Read(candidate); err != nil {
		log.Printf("ack: failed to generate a signing key: %v", err)
		return nil
	}
	written, err := bc.CreateBlobIfAbsent(ackSigningKeyBlob, candidate)
	if err != nil {
		log.Printf("ack: failed to persist the signing key: %v", err)
		return nil
	}
	if written {
		return candidate
	}
	// Another instance (or a prior boot) already wrote the key; adopt the
	// surviving bytes so every replica signs identically.
	surviving, err := store.ReadBlob(ackSigningKeyBlob)
	if err != nil || len(surviving) == 0 {
		log.Printf("ack: failed to read the persisted signing key: %v", err)
		return nil
	}
	return surviving
}
