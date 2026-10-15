package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const maxProjectedPositiveFreshness = 30 * time.Second

type projectedTokenVerifier struct {
	mu         sync.Mutex
	client     *Client
	digest     [32]byte
	principal  string
	until      time.Time
	generation uint64
	status     projectedVerificationStatus
}

type projectedVerificationStatus = kubeindex.CaptureVerificationStatus

func (verifier *projectedTokenVerifier) verificationStatus() projectedVerificationStatus {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	return verifier.status
}

type projectedTokenClaims struct {
	jwt.Claims
	Kubernetes struct {
		Namespace      string `json:"namespace"`
		ServiceAccount struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"serviceaccount"`
	} `json:"kubernetes.io"`
}

func (verifier *projectedTokenVerifier) verify(ctx context.Context, raw string) (principal string, err error) {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	now := time.Now().UTC()
	freshUntil := now.Add(maxProjectedPositiveFreshness)
	verifier.client.authMu.Lock()
	generation := verifier.client.authGeneration.Load()
	digest := sha256.Sum256([]byte(raw))
	if digest == verifier.digest && generation == verifier.generation && verifier.principal != "" && now.Before(verifier.until) {
		verifier.client.authMu.Unlock()
		return verifier.principal, nil
	}
	verifier.client.authMu.Unlock()
	defer func() {
		verifier.client.authMu.Lock()
		defer verifier.client.authMu.Unlock()
		if verifier.client.authGeneration.Load() != generation {
			principal, err = "", ErrUnauthorized
		}
		if err == nil && principal != "" {
			verifier.digest, verifier.principal, verifier.generation = digest, principal, generation
		}
	}()
	verifier.status = projectedVerificationStatus{Stage: "endpoint_scheme"}
	if verifier.client.base.Scheme != "https" {
		return "", nil
	}
	verifier.status.Stage = "token_header"
	token, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256, jose.ES256})
	if err != nil || len(token.Headers) != 1 || token.Headers[0].KeyID == "" {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var discovery struct {
		Issuer string `json:"issuer"`
	}
	verifier.status.Stage = "discovery_response"
	if valid, err := verifier.get(ctx, "/.well-known/openid-configuration", raw, &discovery); err != nil || !valid {
		return "", err
	}
	verifier.status = projectedVerificationStatus{Stage: "discovery_issuer"}
	issuer, err := url.Parse(discovery.Issuer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" {
		return "", nil
	}
	var keys jose.JSONWebKeySet
	verifier.status.Stage = "jwks_response"
	if valid, err := verifier.get(ctx, "/openid/v1/jwks", raw, &keys); err != nil || !valid {
		return "", err
	}
	verifier.status = projectedVerificationStatus{Stage: "key_count"}
	if len(keys.Keys) == 0 || len(keys.Keys) > 32 {
		return "", nil
	}
	verifier.status.Stage = "key_selection"
	matches := keys.Key(token.Headers[0].KeyID)
	if len(matches) != 1 || !matches[0].Valid() || !matches[0].IsPublic() || (matches[0].Use != "" && matches[0].Use != "sig") || (matches[0].Algorithm != "" && matches[0].Algorithm != token.Headers[0].Algorithm) {
		return "", nil
	}
	var claims projectedTokenClaims
	now = time.Now().UTC()
	verifier.status.Stage = "signature_claims_decode"
	if token.Claims(matches[0].Key, &claims) != nil {
		return "", nil
	}
	verifier.status.Stage = "claims_validation"
	if claims.Expiry == nil || claims.IssuedAt == nil || claims.NotBefore == nil || !claims.Expiry.Time().After(now) || claims.ValidateWithLeeway(jwt.Expected{Issuer: discovery.Issuer, AnyAudience: jwt.Audience{discovery.Issuer}, Time: now}, 0) != nil {
		return "", nil
	}
	verifier.status.Stage = "service_account_binding"
	account := claims.Kubernetes.ServiceAccount
	namespace := claims.Kubernetes.Namespace
	if !validPodLogName(namespace, 63, false) || !validPodLogName(account.Name, 253, true) || account.UID == "" || len(account.UID) > 128 || strings.ContainsAny(account.UID, "\x00\r\n") || claims.Subject != "system:serviceaccount:"+namespace+":"+account.Name {
		return "", nil
	}
	encoded, _ := json.Marshal([]string{"verified-service-account", discovery.Issuer, namespace, account.Name, account.UID})
	verifier.until = claims.Expiry.Time()
	if freshUntil.Before(verifier.until) {
		verifier.until = freshUntil
	}
	verifier.status.Stage = "verified"
	return string(encoded), nil
}

func (verifier *projectedTokenVerifier) get(ctx context.Context, apiPath, token string, output any) (bool, error) {
	if err := verifier.client.waitBackoff(ctx); err != nil {
		return false, err
	}
	address, err := verifier.client.requestURL(apiPath)
	if err != nil {
		return false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return false, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if apiPath == "/openid/v1/jwks" {
		request.Header.Set("Accept", "application/jwk-set+json")
	}
	response, err := verifier.client.http.Do(request)
	if err != nil {
		if errors.Is(err, ErrRedirect) {
			return false, nil
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return false, context.DeadlineExceeded
		}
		if errors.Is(err, context.Canceled) {
			return false, context.Canceled
		}
		return false, errors.New("kubernetes: projected verification unavailable")
	}
	defer response.Body.Close()
	verifier.status.HTTPStatus = response.StatusCode
	if response.StatusCode == http.StatusUnauthorized {
		verifier.client.rejectCredential()
		return false, ErrUnauthorized
	}
	if response.StatusCode == http.StatusTooManyRequests {
		delay := parseRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC())
		verifier.client.backoffMu.Lock()
		verifier.client.backoffUntil = maxTime(verifier.client.backoffUntil, time.Now().UTC().Add(delay))
		verifier.client.backoffMu.Unlock()
		return false, &ErrThrottled{RetryAfter: delay}
	}
	if response.StatusCode >= http.StatusInternalServerError {
		return false, classifyStatus(response.StatusCode)
	}
	if response.StatusCode != http.StatusOK {
		return false, nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, errors.New("kubernetes: projected verification response unavailable")
	}
	return len(data) <= 64<<10 && json.Unmarshal(data, output) == nil, nil
}
