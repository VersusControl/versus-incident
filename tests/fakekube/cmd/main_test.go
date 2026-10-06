package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestCertificateTrustsHarnessEndpointWithConfiguredServerName(t *testing.T) {
	certificatePEM, privateKeyPEM, err := makeCertificate("0.0.0.0:19443")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certificatePEM)
	if block == nil {
		t.Fatal("generated certificate is not PEM encoded")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	trustedRoots := x509.NewCertPool()
	trustedRoots.AddCert(certificate)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api" {
			http.NotFound(writer, request)
			return
		}
		writer.WriteHeader(http.StatusOK)
	})}
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- server.Serve(tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}))
	}()
	defer func() {
		_ = server.Close()
		if serveErr := <-serveResult; serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) && !errors.Is(serveErr, net.ErrClosed) {
			t.Errorf("TLS server stopped unexpectedly: %v", serveErr)
		}
	}()

	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: trustedRoots, ServerName: "localhost", MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, listener.Addr().String())
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	response, err := client.Get("https://host.docker.internal:" + port + "/api")
	if err != nil {
		t.Fatalf("verified harness TLS request failed: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("harness API status=%d body=%q", response.StatusCode, body)
	}
}
