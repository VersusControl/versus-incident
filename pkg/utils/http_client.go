package utils

import (
	"crypto/tls"
	"net/http"
	"net/url"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
)

// CreateHTTPClient creates an HTTP client with optional proxy support
func CreateHTTPClient(proxyConfig config.ProxyConfig, useProxy bool) *http.Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: false,
		},
	}

	// Configure proxy if enabled and URL is provided
	if useProxy && proxyConfig.URL != "" {
		proxyURL, err := url.Parse(proxyConfig.URL)
		if err == nil {
			// Set proxy authentication if provided
			if proxyConfig.Username != "" && proxyConfig.Password != "" {
				proxyURL.User = url.UserPassword(proxyConfig.Username, proxyConfig.Password)
			}
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}

	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
}
