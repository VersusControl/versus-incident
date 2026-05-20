package utils

import (
	"net/http"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
)

func TestCreateHTTPClient_NoProxy(t *testing.T) {
	c := CreateHTTPClient(config.ProxyConfig{}, false)
	if c == nil {
		t.Fatal("nil client")
	}
	if c.Timeout == 0 {
		t.Error("expected non-zero timeout")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", c.Transport)
	}
	if tr.Proxy != nil {
		t.Error("expected nil Proxy when useProxy=false")
	}
}

func TestCreateHTTPClient_ProxyDisabledIgnoresURL(t *testing.T) {
	c := CreateHTTPClient(config.ProxyConfig{URL: "http://proxy.example:8080"}, false)
	tr := c.Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Error("Proxy should be unset when useProxy=false even with URL provided")
	}
}

func TestCreateHTTPClient_ProxyEnabledNoURL(t *testing.T) {
	c := CreateHTTPClient(config.ProxyConfig{}, true)
	tr := c.Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Error("Proxy should remain unset when URL is empty")
	}
}

func TestCreateHTTPClient_ProxyEnabledWithAuth(t *testing.T) {
	c := CreateHTTPClient(config.ProxyConfig{
		URL:      "http://proxy.example:8080",
		Username: "u",
		Password: "p",
	}, true)
	tr := c.Transport.(*http.Transport)
	if tr.Proxy == nil {
		t.Fatal("expected Proxy func to be set")
	}
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	pu, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy returned error: %v", err)
	}
	if pu == nil || pu.Host != "proxy.example:8080" {
		t.Errorf("proxy URL host = %v, want proxy.example:8080", pu)
	}
	if pu.User == nil {
		t.Fatal("expected userinfo on proxy URL")
	}
	if pu.User.Username() != "u" {
		t.Errorf("username = %q, want u", pu.User.Username())
	}
	if pw, _ := pu.User.Password(); pw != "p" {
		t.Errorf("password = %q, want p", pw)
	}
}

func TestCreateHTTPClient_BadProxyURL(t *testing.T) {
	// Malformed URL is silently ignored (Proxy stays nil).
	c := CreateHTTPClient(config.ProxyConfig{URL: "://bad"}, true)
	tr := c.Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Error("malformed proxy URL should result in nil Proxy")
	}
}
