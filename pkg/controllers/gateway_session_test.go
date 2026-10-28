package controllers

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/middleware"

	"github.com/gofiber/fiber/v2"
)

const gatewaySessionTestSecret = "gateway-session-test-secret"

func gatewaySessionTestApp(t *testing.T) *fiber.App {
	t.Helper()
	middleware.SetGatewayAuthEnabled(true)
	t.Cleanup(func() { middleware.SetGatewayAuthEnabled(true) })
	loadGatewayConfig(t, gatewaySessionTestSecret)
	cfg := config.GetConfig()
	previous := cfg.GatewaySecret
	previousPublicHost := cfg.PublicHost
	cfg.GatewaySecret = gatewaySessionTestSecret
	cfg.PublicHost = ""
	t.Cleanup(func() {
		cfg.GatewaySecret = previous
		cfg.PublicHost = previousPublicHost
	})

	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		if c.Get("X-Test-Upstream-Authorized") == "true" {
			middleware.MarkAuthorized(c)
		}
		return c.Next()
	})
	api := app.Group("/api")
	RegisterGatewaySessionRoutes(api)
	api.Get("/protected", adminGatewayGuard, func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})
	api.Post("/protected", adminGatewayGuard, func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})
	return app
}

func newGatewayRequest(t *testing.T, method, target string) *http.Request {
	t.Helper()
	if strings.HasPrefix(target, "/") {
		target = "http://console.example" + target
	}
	request, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func exchangeGatewaySession(t *testing.T, app *fiber.App, secret string, secure bool) *http.Cookie {
	t.Helper()
	scheme := "http"
	if secure {
		scheme = "https"
	}
	req := newGatewayRequest(t, http.MethodPost, scheme+"://console.example/api/auth/gateway-session")
	if secure {
		req.Header.Set("X-Forwarded-Proto", "https")
	}
	req.Header.Set("X-Gateway-Secret", secret)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("exchange request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("exchange status = %d, want 204", resp.StatusCode)
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("exchange cookies = %d, want 1", len(cookies))
	}
	return cookies[0]
}

func TestRegisterGatewaySessionRoutes_IssuesOpaqueCookieWithoutTrustingForwardedProto(t *testing.T) {
	app := gatewaySessionTestApp(t)
	req, err := http.NewRequest(http.MethodPost, "http://console.example/api/auth/gateway-session", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Gateway-Secret", gatewaySessionTestSecret)
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("exchange request: %v", err)
	}
	defer resp.Body.Close()
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("exchange cookies = %d, want 1", len(cookies))
	}
	cookie := cookies[0]

	if cookie.Name != gatewaySessionCookieName || cookie.Name == "versus_enterprise_session" {
		t.Errorf("cookie name = %q", cookie.Name)
	}
	if strings.Contains(cookie.Value, gatewaySessionTestSecret) {
		t.Error("cookie contains the gateway secret")
	}
	if !cookie.HttpOnly || cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
		t.Errorf("cookie attributes = HttpOnly:%v Secure:%v SameSite:%v Path:%q", cookie.HttpOnly, cookie.Secure, cookie.SameSite, cookie.Path)
	}
	parts := strings.Split(cookie.Value, ".")
	expiresUnix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		t.Fatalf("parse token expiry: %v", err)
	}
	remaining := time.Until(time.Unix(expiresUnix, 0))
	if remaining < gatewaySessionTTL-time.Minute || remaining > gatewaySessionTTL+time.Minute {
		t.Errorf("cookie TTL = %v, want about %v", remaining, gatewaySessionTTL)
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Set-Cookie")), "expires=") {
		t.Errorf("Set-Cookie lacks absolute Expires: %q", resp.Header.Get("Set-Cookie"))
	}
}

func TestGatewaySessionCookieSecureFromDirectOrConfiguredHTTPS(t *testing.T) {
	tests := []struct {
		name, requestURL, publicHost string
		forwarded                    bool
		wantSecure                   bool
	}{
		{name: "direct http", requestURL: "http://console.example/api/auth/gateway-session"},
		{name: "configured proxy https", requestURL: "http://internal:3000/api/auth/gateway-session", publicHost: "https://console.example", wantSecure: true},
		{name: "configured proxy http", requestURL: "https://internal:3000/api/auth/gateway-session", publicHost: "http://console.example"},
		{name: "spoofed forwarded proto", requestURL: "http://console.example/api/auth/gateway-session", forwarded: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := gatewaySessionTestApp(t)
			config.GetConfig().PublicHost = tt.publicHost
			req := newGatewayRequest(t, http.MethodPost, tt.requestURL)
			req.Header.Set("X-Gateway-Secret", gatewaySessionTestSecret)
			if tt.forwarded {
				req.Header.Set("X-Forwarded-Proto", "https")
			}
			resp, err := app.Test(req, -1)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if cookies := resp.Cookies(); len(cookies) != 1 || cookies[0].Secure != tt.wantSecure {
				t.Fatalf("cookies = %#v, want Secure=%v", cookies, tt.wantSecure)
			}
		})
	}
}

func TestGatewaySessionCookieSecureForDirectTLS(t *testing.T) {
	if !cookieSecure("", true) {
		t.Fatal("cookieSecure() = false for direct TLS, want true")
	}
	if directRequestScheme(true) != "https" {
		t.Fatal("directRequestScheme() did not report https for direct TLS")
	}
}

func TestGatewaySessionOriginUsesConfiguredOrDirectOrigin(t *testing.T) {
	app := gatewaySessionTestApp(t)
	cookie := exchangeGatewaySession(t, app, gatewaySessionTestSecret, false)
	tests := []struct {
		name, origin, referer, publicHost string
		forwardedHost                     string
		want                              int
	}{
		{name: "direct exact", origin: "http://console.example:80", want: http.StatusNoContent},
		{name: "direct mismatch", origin: "https://console.example", want: http.StatusForbidden},
		{name: "referer prefix attack", referer: "http://console.example.attacker.test/admin", want: http.StatusForbidden},
		{name: "configured proxy exact", origin: "https://public.example:443", publicHost: "https://public.example", want: http.StatusNoContent},
		{name: "configured proxy mismatch", origin: "http://internal.example", publicHost: "https://public.example", want: http.StatusForbidden},
		{name: "spoofed forwarded host ignored", origin: "http://attacker.example", forwardedHost: "attacker.example", want: http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config.GetConfig().PublicHost = tt.publicHost
			req := newGatewayRequest(t, http.MethodPost, "http://console.example/api/protected")
			req.Header.Set("Origin", tt.origin)
			if tt.origin == "" {
				req.Header.Del("Origin")
			}
			if tt.referer != "" {
				req.Header.Set("Referer", tt.referer)
			}
			if tt.forwardedHost != "" {
				req.Header.Set("X-Forwarded-Host", tt.forwardedHost)
			}
			req.AddCookie(cookie)
			resp, err := app.Test(req, -1)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}

func TestGatewaySessionTokenValidation(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	valid := issueGatewaySession(gatewaySessionTestSecret, now.Add(time.Hour))
	tamperedParts := strings.Split(valid, ".")
	tamperedSignature, err := base64.RawURLEncoding.DecodeString(tamperedParts[2])
	if err != nil {
		t.Fatalf("decode test signature: %v", err)
	}
	tamperedSignature[0] ^= 0xff
	tampered := tamperedParts[0] + "." + tamperedParts[1] + "." + base64.RawURLEncoding.EncodeToString(tamperedSignature)

	tests := []struct {
		name   string
		token  string
		secret string
		want   bool
	}{
		{name: "valid", token: valid, secret: gatewaySessionTestSecret, want: true},
		{name: "malformed", token: "v1.not-a-time.bad", secret: gatewaySessionTestSecret},
		{name: "tampered", token: tampered, secret: gatewaySessionTestSecret},
		{name: "rotated secret", token: valid, secret: "rotated-secret"},
		{name: "empty secret", token: valid, secret: ""},
		{name: "expired", token: issueGatewaySession(gatewaySessionTestSecret, now), secret: gatewaySessionTestSecret},
		{name: "exceeds absolute ttl", token: issueGatewaySession(gatewaySessionTestSecret, now.Add(gatewaySessionTTL+time.Second)), secret: gatewaySessionTestSecret},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validGatewaySession(tt.token, tt.secret, now); got != tt.want {
				t.Errorf("validGatewaySession() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGatewaySessionExchangeFailsClosed(t *testing.T) {
	app := gatewaySessionTestApp(t)
	for _, secret := range []string{"", "wrong-secret"} {
		req := newGatewayRequest(t, http.MethodPost, "/api/auth/gateway-session")
		req.Header.Set("X-Gateway-Secret", secret)
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatalf("exchange request: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("secret %q status = %d, want 401", secret, resp.StatusCode)
		}
	}

	config.GetConfig().GatewaySecret = ""
	req := newGatewayRequest(t, http.MethodPost, "/api/auth/gateway-session")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("empty-config exchange request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("empty-config status = %d, want 401", resp.StatusCode)
	}
}

func TestAdminGatewayGuardAuthenticationAndOrigin(t *testing.T) {
	app := gatewaySessionTestApp(t)
	cookie := exchangeGatewaySession(t, app, gatewaySessionTestSecret, false)

	drive := func(method, origin string, withCookie, withHeader bool) int {
		req := newGatewayRequest(t, method, "http://console.example/api/protected")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if withCookie {
			req.AddCookie(cookie)
		}
		if withHeader {
			req.Header.Set("X-Gateway-Secret", gatewaySessionTestSecret)
		}
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatalf("%s protected request: %v", method, err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := drive(http.MethodGet, "", true, false); got != http.StatusNoContent {
		t.Errorf("cookie GET status = %d, want 204", got)
	}
	if got := drive(http.MethodPost, "", true, false); got != http.StatusForbidden {
		t.Errorf("cookie POST without origin status = %d, want 403", got)
	}
	if got := drive(http.MethodPost, "https://attacker.example", true, false); got != http.StatusForbidden {
		t.Errorf("cookie POST cross-origin status = %d, want 403", got)
	}
	if got := drive(http.MethodPost, "http://console.example", true, false); got != http.StatusNoContent {
		t.Errorf("cookie POST same-origin status = %d, want 204", got)
	}
	refererReq := newGatewayRequest(t, http.MethodPost, "http://console.example/api/protected")
	refererReq.Header.Set("Referer", "http://console.example/agent")
	refererReq.AddCookie(cookie)
	refererResp, err := app.Test(refererReq, -1)
	if err != nil {
		t.Fatalf("cookie POST with referer: %v", err)
	}
	refererResp.Body.Close()
	if refererResp.StatusCode != http.StatusNoContent {
		t.Errorf("cookie POST same-origin referer status = %d, want 204", refererResp.StatusCode)
	}
	if got := drive(http.MethodPost, "", false, true); got != http.StatusNoContent {
		t.Errorf("header POST without origin status = %d, want 204", got)
	}
	if got := drive(http.MethodPost, "https://attacker.example", true, true); got != http.StatusNoContent {
		t.Errorf("header-preferred POST status = %d, want 204", got)
	}
}

func TestAdminGatewayGuardRejectsInvalidCookies(t *testing.T) {
	app := gatewaySessionTestApp(t)
	now := time.Now()
	tests := []struct {
		name  string
		value string
	}{
		{name: "expired", value: issueGatewaySession(gatewaySessionTestSecret, now.Add(-time.Second))},
		{name: "tampered", value: issueGatewaySession(gatewaySessionTestSecret, now.Add(time.Hour)) + "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newGatewayRequest(t, http.MethodGet, "/api/protected")
			req.AddCookie(&http.Cookie{Name: gatewaySessionCookieName, Value: tt.value})
			resp, err := app.Test(req, -1)
			if err != nil {
				t.Fatalf("protected request: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}

	validBeforeRotation := issueGatewaySession(gatewaySessionTestSecret, now.Add(time.Hour))
	config.GetConfig().GatewaySecret = "rotated-secret"
	req := newGatewayRequest(t, http.MethodGet, "/api/protected")
	req.AddCookie(&http.Cookie{Name: gatewaySessionCookieName, Value: validBeforeRotation})
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("rotated-secret request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("rotated-secret status = %d, want 401", resp.StatusCode)
	}
}

func TestAdminGatewayGuardHonorsUpstreamAuthorization(t *testing.T) {
	loadGatewayConfig(t, "")
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		middleware.MarkAuthorized(c)
		return c.Next()
	})
	app.Post("/protected", adminGatewayGuard, func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})
	req := newGatewayRequest(t, http.MethodPost, "/protected")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("upstream-authorized request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
}

func TestGatewayAuthDisabledAllowsOnlyUpstreamAuthorization(t *testing.T) {
	app := gatewaySessionTestApp(t)
	oldCookie := exchangeGatewaySession(t, app, gatewaySessionTestSecret, false)
	middleware.SetGatewayAuthEnabled(false)

	tests := []struct {
		name       string
		path       string
		header     bool
		cookie     bool
		authorized bool
		want       int
	}{
		{name: "exchange", path: "/api/auth/gateway-session", header: true, want: http.StatusUnauthorized},
		{name: "direct header", path: "/api/protected", header: true, want: http.StatusUnauthorized},
		{name: "previous cookie", path: "/api/protected", cookie: true, want: http.StatusUnauthorized},
		{name: "upstream authorization", path: "/api/protected", authorized: true, want: http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newGatewayRequest(t, http.MethodPost, "http://console.example"+tt.path)
			if tt.header {
				req.Header.Set("X-Gateway-Secret", gatewaySessionTestSecret)
			}
			if tt.cookie {
				req.AddCookie(oldCookie)
			}
			if tt.authorized {
				req.Header.Set("X-Test-Upstream-Authorized", "true")
			}
			resp, err := app.Test(req, -1)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}

func TestGatewaySessionLogoutExpiresCookie(t *testing.T) {
	app := gatewaySessionTestApp(t)
	cookie := exchangeGatewaySession(t, app, gatewaySessionTestSecret, false)
	req := newGatewayRequest(t, http.MethodDelete, "http://console.example/api/auth/gateway-session")
	req.Header.Set("Origin", "http://console.example")
	req.AddCookie(cookie)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("logout request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", resp.StatusCode)
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 || cookies[0].Name != gatewaySessionCookieName || cookies[0].Value != "" || !cookies[0].Expires.Before(time.Now()) {
		t.Errorf("logout cookie = %#v, want expired %q", cookies, gatewaySessionCookieName)
	}
}
