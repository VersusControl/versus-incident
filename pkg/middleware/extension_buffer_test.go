package middleware

import (
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// TestOrgResolverSurvivesBufferReuse is the regression guard for golden
// rule #11: Fiber request strings (c.Get/Params/Query/Cookies/FormValue) are
// backed by a pooled, reused request buffer, so a value persisted beyond the
// request is silently corrupted when a later request reuses that buffer.
//
// The OrgResolver→OrgInjector seam is the load-bearing path here: it is reused
// by the enterprise binary, which means OrgInjector cannot assume the host
// built the app with fiber.Config{Immutable:true}. It must therefore copy the
// resolved org off the request buffer itself (the suspenders strings.Clone).
//
// This test deliberately builds the app WITHOUT Immutable so the pooled buffer
// is genuinely reused between requests, exercising that clone. Two sequential
// requests are fired through the real Fiber router: request A persists its
// resolved org, request B sends a DIFFERENT org of the same length in the same
// header. The guard's job is that B's request does not corrupt A's persisted
// value. Remove the strings.Clone in OrgInjector and this test fails.
func TestOrgResolverSurvivesBufferReuse(t *testing.T) {
	resetSlots()
	defer resetSlots()

	// Resolve the org straight from the buffer-backed request header.
	SetOrgResolver(func(c *fiber.Ctx) string {
		return c.Get("X-Org-Id")
	})

	// persisted holds the org a handler keeps beyond the request lifetime —
	// exactly the lifetime the buffer-aliasing bug corrupts.
	var persisted []string

	// Default config (Immutable:false) on purpose: this asserts the seam is
	// safe even when the host forgot the app-wide belt.
	app := fiber.New()
	app.Use(OrgInjector())
	app.Get("/whoami", func(c *fiber.Ctx) error {
		org := OrgFromContext(c)
		persisted = append(persisted, org)
		return c.SendString(org)
	})

	// Same length, different bytes — so request B reuses request A's buffer
	// slot in place, which is what makes the corruption deterministic.
	const orgA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const orgB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// Request A persists orgA.
	reqA := httptest.NewRequest("GET", "/whoami", nil)
	reqA.Header.Set("X-Org-Id", orgA)
	respA, err := app.Test(reqA)
	if err != nil {
		t.Fatalf("app.Test(A): %v", err)
	}
	bodyA, _ := io.ReadAll(respA.Body)
	if string(bodyA) != orgA {
		t.Fatalf("request A org = %q, want %q", string(bodyA), orgA)
	}

	// Request B sends a different org, reusing the pooled request buffer that
	// backed request A's header.
	reqB := httptest.NewRequest("GET", "/whoami", nil)
	reqB.Header.Set("X-Org-Id", orgB)
	respB, err := app.Test(reqB)
	if err != nil {
		t.Fatalf("app.Test(B): %v", err)
	}
	bodyB, _ := io.ReadAll(respB.Body)
	if string(bodyB) != orgB {
		t.Fatalf("request B org = %q, want %q", string(bodyB), orgB)
	}

	// The whole point: request A's persisted org must survive request B.
	if got := persisted[0]; got != orgA {
		t.Fatalf("request A's persisted org was corrupted by request B: "+
			"got %q, want %q (buffer-aliasing guard missing in OrgInjector)", got, orgA)
	}
}
