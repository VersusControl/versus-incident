package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestChatAuditHookNilIsInertAndRegisteredHookRecords(t *testing.T) {
	SetChatAuditHook(nil)
	t.Cleanup(func() { SetChatAuditHook(nil) })
	app := fiber.New()
	count := 0
	app.Get("/", func(c *fiber.Ctx) error {
		ChatAuditor(c)(ChatAuditEvent{Action: "chat.session.created", Result: ChatAuditSuccess})
		return c.SendStatus(fiber.StatusNoContent)
	})
	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil), -1); err != nil {
		t.Fatal(err)
	}
	SetChatAuditHook(func(*fiber.Ctx) ChatAuditRecorder {
		return func(ChatAuditEvent) { count++ }
	})
	if _, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil), -1); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("recorded events = %d, want 1", count)
	}
}
