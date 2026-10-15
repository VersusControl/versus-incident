package kubernetes

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

const maxCredentialFileBytes = int64(1 << 20)

// CredentialSource supplies request and TLS credentials without exposing them
// through connector configuration or status DTOs.
type CredentialSource interface {
	Authorization(context.Context) (string, error)
	ClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error)
}

type credentialSource struct {
	authorization func(context.Context) (string, error)
	certificate   func(*tls.CertificateRequestInfo) (*tls.Certificate, error)
}

type expiringBearerSource struct {
	lockOnce  sync.Once
	lock      chan struct{}
	refresh   func(context.Context) (string, time.Time, error)
	now       func() time.Time
	skew      time.Duration
	header    string
	expires   time.Time
	principal string
}

func (source *expiringBearerSource) credentialPrincipal() string { return source.principal }

type credentialEpoch struct {
	mu          sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	initialized bool
	fingerprint [32]byte
	principal   [32]byte
	known       bool
	retired     bool
}

type credentialEpochContextKey struct{}

func (service *Service) captureCredentialID() string {
	epoch := service.identity
	epoch.mu.Lock()
	defer epoch.mu.Unlock()
	identity := epoch.fingerprint
	mode := "credential"
	if service.client.credentials == nil {
		mode = "anonymous"
	} else if epoch.known {
		identity = epoch.principal
		mode = "provider-principal"
	}
	endpoint := ""
	if service.client.base != nil {
		endpoint = service.client.base.String()
	}
	encoded, _ := json.Marshal([]string{"capture-scope-v1", endpoint, service.configuredCredentialID, mode, hex.EncodeToString(identity[:])})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (service *Service) credentialContext(ctx context.Context) (context.Context, context.CancelFunc) {
	bound, cancel := context.WithCancel(context.WithValue(ctx, credentialEpochContextKey{}, service))
	stop := context.AfterFunc(service.identity.ctx, cancel)
	return bound, func() { stop(); cancel() }
}

func (service *Service) validateCredential(ctx context.Context) error {
	if service == nil {
		return ErrInvalidArguments
	}
	if service.changes != nil {
		service.changes.mu.Lock()
		stopped := service.changes.stopped[service.indexScope()]
		service.changes.mu.Unlock()
		if stopped {
			return ErrClusterUnavailable
		}
	}
	if service.identity == nil || service.client == nil || service.client.credentials == nil {
		return ctx.Err()
	}
	epoch := service.identity
	epoch.mu.Lock()
	defer epoch.mu.Unlock()
	if epoch.retired || epoch.ctx.Err() != nil {
		epoch.retired = true
		return ErrCredentialUnavailable
	}
	header, err := service.client.credentials.Authorization(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		epoch.retired = true
		epoch.cancel()
		return ErrCredentialUnavailable
	}
	return service.acceptCredentialLocked(ctx, header)
}

func (service *Service) acceptCredential(ctx context.Context, header string) error {
	epoch := service.identity
	epoch.mu.Lock()
	defer epoch.mu.Unlock()
	return service.acceptCredentialLocked(ctx, header)
}

func (service *Service) commitCredential(ctx context.Context, commit func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	epoch := service.identity
	epoch.mu.Lock()
	defer epoch.mu.Unlock()
	if epoch.retired || epoch.ctx.Err() != nil {
		return ErrCredentialUnavailable
	}
	if service.client != nil && service.client.credentials != nil {
		header, err := service.client.credentials.Authorization(ctx)
		if err != nil {
			return err
		}
		if err := service.acceptCredentialLocked(ctx, header); err != nil {
			return err
		}
	}
	if service.client != nil {
		service.client.authMu.Lock()
		defer service.client.authMu.Unlock()
		if epoch.ctx.Err() != nil {
			return ErrCredentialUnavailable
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return commit()
}

func (service *Service) acceptCredentialLocked(ctx context.Context, header string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	epoch := service.identity
	if epoch.retired || epoch.ctx.Err() != nil {
		return ErrCredentialUnavailable
	}
	certificate, err := service.client.credentials.ClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		epoch.retired = true
		epoch.cancel()
		return ErrCredentialUnavailable
	}
	digest := sha256.New()
	digest.Write([]byte(header))
	if certificate != nil {
		for _, encoded := range certificate.Certificate {
			digest.Write(encoded)
		}
	}
	var fingerprint [32]byte
	copy(fingerprint[:], digest.Sum(nil))
	principal := ""
	if source, ok := service.client.credentials.(interface {
		verifiedCredentialPrincipal(context.Context, string) (string, error)
	}); ok {
		principal, err = source.verifiedCredentialPrincipal(ctx, header)
		if err != nil {
			if errors.Is(err, ErrUnauthorized) {
				epoch.retired = true
				epoch.cancel()
			}
			return err
		}
	} else if source, ok := service.client.credentials.(interface{ credentialPrincipal() string }); ok {
		principal = source.credentialPrincipal()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if epoch.ctx.Err() != nil {
		epoch.retired = true
		return ErrCredentialUnavailable
	}
	principalHash := sha256.Sum256([]byte(principal))
	known := principal != ""
	sameFingerprint := subtle.ConstantTimeCompare(fingerprint[:], epoch.fingerprint[:]) == 1
	samePrincipal := subtle.ConstantTimeCompare(principalHash[:], epoch.principal[:]) == 1
	if epoch.initialized && ((!sameFingerprint && !(known && epoch.known && samePrincipal)) || known != epoch.known || !samePrincipal) {
		epoch.retired = true
		epoch.cancel()
		return ErrCredentialUnavailable
	}
	epoch.initialized, epoch.fingerprint, epoch.principal, epoch.known = true, fingerprint, principalHash, known
	return ctx.Err()
}

func (source *expiringBearerSource) Authorization(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	source.lockOnce.Do(func() {
		source.lock = make(chan struct{}, 1)
		source.lock <- struct{}{}
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-source.lock:
	}
	defer func() { source.lock <- struct{}{} }()
	now := source.now()
	if source.header != "" && now.Add(source.skew).Before(source.expires) {
		return source.header, nil
	}
	token, expires, err := source.refresh(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		return "", ErrCredentialUnavailable
	}
	header, err := bearerHeader(token)
	if err != nil || !expires.After(now) {
		return "", errors.New("kubernetes: credential provider returned an invalid token")
	}
	source.header = header
	source.expires = expires
	return header, nil
}

func (source *expiringBearerSource) ClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return new(tls.Certificate), nil
}

func (source *credentialSource) Authorization(ctx context.Context) (string, error) {
	if source == nil || source.authorization == nil {
		return "", nil
	}
	return source.authorization(ctx)
}

func (source *credentialSource) ClientCertificate(info *tls.CertificateRequestInfo) (*tls.Certificate, error) {
	if source == nil || source.certificate == nil {
		return new(tls.Certificate), nil
	}
	return source.certificate(info)
}

func staticTokenCredential(token string) (CredentialSource, error) {
	header, err := bearerHeader(token)
	if err != nil {
		return nil, err
	}
	return &credentialSource{authorization: func(context.Context) (string, error) { return header, nil }}, nil
}

func tokenFileCredential(path string) CredentialSource {
	return &tokenFileSource{path: path}
}

type tokenFileSource struct {
	path     string
	verifier *projectedTokenVerifier
}

func (source *tokenFileSource) Authorization(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, err := readBoundedFile(source.path, maxTokenBytes)
	if err != nil {
		return "", ErrCredentialUnavailable
	}
	return bearerHeader(string(data))
}

func (source *tokenFileSource) ClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return new(tls.Certificate), nil
}

func (source *tokenFileSource) verifiedCredentialPrincipal(ctx context.Context, header string) (string, error) {
	if source.verifier == nil {
		return "", nil
	}
	return source.verifier.verify(ctx, strings.TrimPrefix(header, "Bearer "))
}

func clientCertificateCredential(certificateFile, keyFile string, certificateData, keyData []byte) (CredentialSource, error) {
	if len(certificateData) > 0 || len(keyData) > 0 {
		if len(certificateData) == 0 || len(keyData) == 0 || certificateFile != "" || keyFile != "" {
			return nil, errors.New("kubernetes: invalid client certificate configuration")
		}
		certificate, err := tls.X509KeyPair(certificateData, keyData)
		if err != nil {
			return nil, errors.New("kubernetes: invalid client certificate")
		}
		return &credentialSource{certificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &certificate, nil }}, nil
	}
	if certificateFile == "" || keyFile == "" {
		return nil, errors.New("kubernetes: invalid client certificate configuration")
	}
	cache := &certificateFileCache{certificateFile: certificateFile, keyFile: keyFile}
	if _, err := cache.load(); err != nil {
		return nil, err
	}
	return &credentialSource{certificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return cache.load() }}, nil
}

type certificateFileCache struct {
	mu              sync.Mutex
	certificateFile string
	keyFile         string
	certificate     *tls.Certificate
	certificateMod  int64
	keyMod          int64
}

func (cache *certificateFileCache) load() (*tls.Certificate, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	certificateInfo, err := os.Stat(cache.certificateFile)
	if err != nil {
		return nil, errors.New("kubernetes: client certificate unavailable")
	}
	keyInfo, err := os.Stat(cache.keyFile)
	if err != nil || keyInfo.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("kubernetes: client certificate unavailable")
	}
	certificateMod := certificateInfo.ModTime().UnixNano()
	keyMod := keyInfo.ModTime().UnixNano()
	if cache.certificate != nil && cache.certificateMod == certificateMod && cache.keyMod == keyMod {
		return cache.certificate, nil
	}
	certificatePEM, err := readBoundedFile(cache.certificateFile, maxCredentialFileBytes)
	if err != nil {
		return nil, errors.New("kubernetes: client certificate unavailable")
	}
	keyPEM, err := readBoundedFile(cache.keyFile, maxCredentialFileBytes)
	if err != nil {
		return nil, errors.New("kubernetes: client certificate unavailable")
	}
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		return nil, errors.New("kubernetes: invalid client certificate")
	}
	cache.certificate = &certificate
	cache.certificateMod = certificateMod
	cache.keyMod = keyMod
	return cache.certificate, nil
}

func bearerHeader(token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > int(maxTokenBytes) || strings.ContainsAny(token, "\r\n") {
		return "", ErrCredentialUnavailable
	}
	return "Bearer " + token, nil
}

func readBoundedFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		return nil, errors.New("bounded file read failed")
	}
	return data, nil
}

func readSecureBoundedFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("credential file is not private")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		return nil, errors.New("bounded credential file read failed")
	}
	return data, nil
}
