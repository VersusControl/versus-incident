package fakekube

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const projectedIssuer = "https://kubernetes.default.svc"

func (server *Server) projectedTokenControl(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		ID              string `json:"id"`
		UID             string `json:"uid"`
		Mismatch        string `json:"mismatch"`
		Rotate          bool   `json:"rotate"`
		LifetimeSeconds int64  `json:"lifetime_seconds"`
	}
	if request.URL.Path == "/_fake/projected-token" {
		if request.Method != http.MethodPost || json.NewDecoder(http.MaxBytesReader(writer, request.Body, 4096)).Decode(&input) != nil {
			http.Error(writer, "invalid token control", http.StatusBadRequest)
			return
		}
	} else if request.Method != http.MethodGet {
		http.Error(writer, "GET required", http.StatusMethodNotAllowed)
		return
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.projectedKey == nil || input.Rotate {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			http.Error(writer, "signer unavailable", http.StatusInternalServerError)
			return
		}
		server.projectedKey = key
		server.projectedKeyID = time.Now().UTC().Format("150405.000000000")
	}
	if request.URL.Path == "/openid/v1/jwks" {
		writeJSON(writer, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &server.projectedKey.PublicKey, KeyID: server.projectedKeyID, Algorithm: "ES256", Use: "sig"}}})
		return
	}
	if input.UID == "" {
		input.UID = "harness-reader-uid"
	}
	if input.LifetimeSeconds < 0 || input.LifetimeSeconds > 2*366*24*60*60 {
		http.Error(writer, "invalid token lifetime", http.StatusBadRequest)
		return
	}
	lifetime := time.Hour
	if input.LifetimeSeconds > 0 {
		lifetime = time.Duration(input.LifetimeSeconds) * time.Second
	}
	now := time.Now().UTC()
	claims := jwt.Claims{Issuer: projectedIssuer, Subject: "system:serviceaccount:versus-system:reader", Audience: jwt.Audience{projectedIssuer}, ID: input.ID, Expiry: jwt.NewNumericDate(now.Add(lifetime)), NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)), IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute))}
	key, keyID := server.projectedKey, server.projectedKeyID
	switch input.Mismatch {
	case "":
	case "issuer":
		claims.Issuer = "https://untrusted.invalid"
	case "audience":
		claims.Audience = jwt.Audience{"wrong-audience"}
	case "expired":
		claims.Expiry = jwt.NewNumericDate(now.Add(-time.Minute))
	case "future":
		claims.NotBefore = jwt.NewNumericDate(now.Add(time.Hour))
	case "issued_future":
		claims.IssuedAt = jwt.NewNumericDate(now.Add(time.Hour))
	case "key":
		keyID = "unknown-key"
	case "signature":
		var err error
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			http.Error(writer, "signer unavailable", http.StatusInternalServerError)
			return
		}
	default:
		http.Error(writer, "unknown mismatch", http.StatusBadRequest)
		return
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", keyID))
	if err != nil {
		http.Error(writer, "signer unavailable", http.StatusInternalServerError)
		return
	}
	principal := map[string]any{"kubernetes.io": map[string]any{"namespace": "versus-system", "serviceaccount": map[string]string{"name": "reader", "uid": input.UID}, "pod": map[string]string{"name": input.ID, "uid": input.ID}}}
	token, err := jwt.Signed(signer).Claims(claims).Claims(principal).Serialize()
	if err != nil {
		http.Error(writer, "token unavailable", http.StatusInternalServerError)
		return
	}
	writeJSON(writer, map[string]string{"token": token, "key_id": keyID})
}
