package routes

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/middleware"

	"github.com/gofiber/fiber/v2"
)

func TestSetupRoutes_GatewaySessionExchangePrecedesExtensionAuth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("name: test\nhost: 127.0.0.1\nport: 3000\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.LoadConfig(path); err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg := config.GetConfig()
	previous := cfg.GatewaySecret
	cfg.GatewaySecret = "route-order-secret"
	t.Cleanup(func() {
		cfg.GatewaySecret = previous
		middleware.SetAuthMiddleware(nil)
	})

	middleware.SetAuthMiddleware(func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusTeapot)
	})
	app := fiber.New()
	SetupRoutes(app, nil)

	exchange := httptest.NewRequest(http.MethodPost, "/api/auth/gateway-session", nil)
	exchange.Header.Set("X-Gateway-Secret", "route-order-secret")
	exchangeResp, err := app.Test(exchange, -1)
	if err != nil {
		t.Fatalf("exchange request: %v", err)
	}
	exchangeResp.Body.Close()
	if exchangeResp.StatusCode != http.StatusNoContent {
		t.Errorf("exchange status = %d, want 204", exchangeResp.StatusCode)
	}

	protected := httptest.NewRequest(http.MethodGet, "/api/admin/config/agent", nil)
	protectedResp, err := app.Test(protected, -1)
	if err != nil {
		t.Fatalf("protected request: %v", err)
	}
	protectedResp.Body.Close()
	if protectedResp.StatusCode != fiber.StatusTeapot {
		t.Errorf("protected status = %d, want %d", protectedResp.StatusCode, fiber.StatusTeapot)
	}
}
