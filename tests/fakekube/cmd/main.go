package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/VersusControl/versus-incident/tests/fakekube"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:9443", "TLS listen address")
	stateDir := flag.String("state-dir", "", "directory for CA and readiness state")
	scenario := flag.String("scenario", "triage", "fixture scenario")
	pods := flag.Int("pods", 0, "synthetic pod count (up to 50000)")
	namespaces := flag.Int("namespaces", 0, "synthetic namespace count")
	seed := flag.Int64("seed", 1, "deterministic fixture seed")
	flag.Parse()
	if *stateDir == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*listen, *stateDir, *scenario, *pods, *namespaces, *seed); err != nil {
		_, _ = os.Stderr.WriteString("fakekube: startup failed\n")
		os.Exit(1)
	}
}

func run(address, stateDir, scenario string, pods, namespaces int, seed int64) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return errors.New("state directory unavailable")
	}
	server, err := fakekube.NewServer(fakekube.Config{Scenario: scenario, Pods: pods, Namespaces: namespaces, Seed: seed})
	if err != nil {
		return err
	}
	certificate, privateKey, err := makeCertificate(address)
	if err != nil {
		return errors.New("TLS certificate unavailable")
	}
	certificatePath := filepath.Join(stateDir, "server.crt")
	keyPath := filepath.Join(stateDir, "server.key")
	caPath := filepath.Join(stateDir, "ca.crt")
	for path, data := range map[string][]byte{certificatePath: certificate, keyPath: privateKey, caPath: certificate} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return errors.New("TLS state unavailable")
		}
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return errors.New("listen failed")
	}
	readyData, err := json.Marshal(map[string]any{"pid": os.Getpid(), "url": "https://" + listener.Addr().String(), "ca_file": caPath, "scenario": scenario})
	if err != nil {
		listener.Close()
		return errors.New("readiness state unavailable")
	}
	if err := os.WriteFile(filepath.Join(stateDir, "ready.json"), readyData, 0o600); err != nil {
		listener.Close()
		return errors.New("readiness state unavailable")
	}
	httpServer := &http.Server{Handler: server, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.ServeTLS(listener, certificatePath, keyPath) }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdown)
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("server stopped")
	}
}

func makeCertificate(address string) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "fakekube"},
		NotBefore:    now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		template.IPAddresses = []net.IP{ip}
		template.DNSNames = []string{"localhost"}
	} else {
		template.DNSNames = []string{host, "localhost"}
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	return certificatePEM, privateKeyPEM, nil
}
