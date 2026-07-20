package controllers

import (
	"net/http/httptest"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"

	"github.com/gofiber/fiber/v2"
)

// ackTestApp mounts HandleAck on the public ack route the same way the router
// registers it, so requests exercise the real handler path.
func ackTestApp() *fiber.App {
	app := fiber.New()
	app.Get("/api/ack/:incidentID", HandleAck)
	return app
}

// TestHandleAck_UninitializedReturns503 proves the DoS fix: an unauthenticated
// ack request with on-call never initialized (the default) returns a clean
// 503 instead of panicking and taking down the process.
func TestHandleAck_UninitializedReturns503(t *testing.T) {
	core.SetOnCallWorkflow(nil)

	app := ackTestApp()
	resp, err := app.Test(httptest.NewRequest("GET", "/api/ack/anything", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("expected %d when on-call uninitialized, got %d", fiber.StatusServiceUnavailable, resp.StatusCode)
	}
}

// TestHandleAck_InitializedProceeds proves that when the workflow IS installed
// the handler skips the 503 guard and delegates to Ack. With a workflow that
// has no Redis client, Ack reports it isn't fully wired, so the handler returns
// the existing 500 error path — the key point being it is NOT the 503 guard and
// it does NOT panic.
func TestHandleAck_InitializedProceeds(t *testing.T) {
	core.SetOnCallWorkflow(core.NewOnCallWorkflow(nil, nil))
	t.Cleanup(func() { core.SetOnCallWorkflow(nil) })

	app := ackTestApp()
	resp, err := app.Test(httptest.NewRequest("GET", "/api/ack/i-123", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == fiber.StatusServiceUnavailable {
		t.Fatalf("did not expect the 503 uninitialized guard once the workflow is installed")
	}
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("expected %d from the Ack error path, got %d", fiber.StatusInternalServerError, resp.StatusCode)
	}
}
