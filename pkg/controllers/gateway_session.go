package controllers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/weborigin"

	"github.com/gofiber/fiber/v2"
)

const (
	gatewaySessionCookieName = "versus_gateway_session"
	gatewaySessionVersion    = "v1"
	gatewaySessionTTL        = 8 * time.Hour
	gatewaySessionKeyDomain  = "versus-incident/gateway-session/signing-key/v1"
)

// RegisterGatewaySessionRoutes mounts the public gateway-secret exchange and
// logout endpoints. Call it before any optional upstream auth middleware so an
// OSS operator can establish or clear this independent session.
func RegisterGatewaySessionRoutes(router fiber.Router) {
	router.Post("/auth/gateway-session", createGatewaySession)
	router.Delete("/auth/gateway-session", deleteGatewaySession)
}

func createGatewaySession(c *fiber.Ctx) error {
	if !middleware.GatewayAuthEnabled() {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	secret := config.GetConfig().GatewaySecret
	if secret == "" || !secureEqual(c.Get("X-Gateway-Secret"), secret) {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}

	now := time.Now()
	expires := now.Add(gatewaySessionTTL)
	c.Cookie(&fiber.Cookie{
		Name:     gatewaySessionCookieName,
		Value:    issueGatewaySession(secret, expires),
		Path:     "/",
		Expires:  expires,
		HTTPOnly: true,
		Secure:   gatewayCookieSecure(c),
		SameSite: fiber.CookieSameSiteStrictMode,
	})
	return c.SendStatus(fiber.StatusNoContent)
}

func deleteGatewaySession(c *fiber.Ctx) error {
	secret := config.GetConfig().GatewaySecret
	if token := c.Cookies(gatewaySessionCookieName); token != "" && validGatewaySession(token, secret, time.Now()) {
		if !requestHasSameOrigin(c) {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "invalid request origin"})
		}
	}
	c.Cookie(&fiber.Cookie{
		Name:     gatewaySessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
		HTTPOnly: true,
		Secure:   gatewayCookieSecure(c),
		SameSite: fiber.CookieSameSiteStrictMode,
	})
	return c.SendStatus(fiber.StatusNoContent)
}

func issueGatewaySession(secret string, expires time.Time) string {
	payload := gatewaySessionVersion + "." + strconv.FormatInt(expires.Unix(), 10)
	return payload + "." + base64.RawURLEncoding.EncodeToString(signGatewaySession(secret, payload))
}

func validGatewaySession(token, secret string, now time.Time) bool {
	if secret == "" {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != gatewaySessionVersion {
		return false
	}
	expiresUnix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}
	expires := time.Unix(expiresUnix, 0)
	if !expires.After(now) || expires.After(now.Add(gatewaySessionTTL)) {
		return false
	}
	provided, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	payload := parts[0] + "." + parts[1]
	return hmac.Equal(provided, signGatewaySession(secret, payload))
}

func signGatewaySession(secret, payload string) []byte {
	derive := hmac.New(sha256.New, []byte(secret))
	_, _ = derive.Write([]byte(gatewaySessionKeyDomain))
	key := derive.Sum(nil)

	sign := hmac.New(sha256.New, key)
	_, _ = sign.Write([]byte(payload))
	return sign.Sum(nil)
}

func requestIsSafe(method string) bool {
	return method == fiber.MethodGet || method == fiber.MethodHead || method == fiber.MethodOptions
}

func requestHasSameOrigin(c *fiber.Ctx) bool {
	expected := config.GetConfig().PublicHost
	if expected == "" {
		expected = directRequestScheme(c.Context().IsTLS()) + "://" + string(c.Context().Host())
	}
	if origin := c.Get("Origin"); origin != "" {
		return weborigin.Same(origin, expected, false)
	}
	referer := c.Get("Referer")
	return referer != "" && weborigin.Same(referer, expected, true)
}

func gatewayCookieSecure(c *fiber.Ctx) bool {
	return cookieSecure(config.GetConfig().PublicHost, c.Context().IsTLS())
}

func cookieSecure(publicHost string, directTLS bool) bool {
	if publicHost != "" {
		return strings.HasPrefix(publicHost, "https://")
	}
	return directTLS
}

func directRequestScheme(directTLS bool) string {
	if directTLS {
		return "https"
	}
	return "http"
}
