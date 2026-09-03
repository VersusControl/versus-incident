package kubernetes

import (
	"context"
	"crypto/tls"
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
	lockOnce sync.Once
	lock     chan struct{}
	refresh  func(context.Context) (string, time.Time, error)
	now      func() time.Time
	skew     time.Duration
	header   string
	expires  time.Time
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
		return "", err
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
	return &credentialSource{authorization: func(context.Context) (string, error) {
		data, err := readBoundedFile(path, maxTokenBytes)
		if err != nil {
			return "", errors.New("kubernetes: credential unavailable")
		}
		return bearerHeader(string(data))
	}}
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
		return "", errors.New("kubernetes: credential unavailable")
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
