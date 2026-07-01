package controllers

import (
	"sync"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

// capturedAudit is one admin-audit event the test hook recorded.
type capturedAudit struct {
	action string
	target string
	result string
	org    string
}

// auditCapture is a test AdminAuditHook that records every emitted event so the
// route tests can assert the action/target/result (and the ctx-derived org).
type auditCapture struct {
	mu     sync.Mutex
	events []capturedAudit
}

func (a *auditCapture) hook() middleware.AdminAuditHook {
	return func(c *fiber.Ctx, ev middleware.AdminAuditEvent) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.events = append(a.events, capturedAudit{
			action: ev.Action,
			target: ev.Target,
			result: ev.Result,
			org:    middleware.OrgFromContext(c),
		})
	}
}

// only returns the single event with the given action+result, failing if there
// is not exactly one.
func (a *auditCapture) only(t *testing.T, action, result string) capturedAudit {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	var hits []capturedAudit
	for _, e := range a.events {
		if e.action == action && e.result == result {
			hits = append(hits, e)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("want exactly 1 audit event action=%q result=%q, got %d (all=%v)", action, result, len(hits), a.events)
	}
	return hits[0]
}

// reset clears the captured events between route drives.
func (a *auditCapture) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = nil
}

// installCapture registers a fresh capture hook and clears it on cleanup so the
// process-wide slot never leaks into another test.
func installCapture(t *testing.T) *auditCapture {
	t.Helper()
	cap := &auditCapture{}
	middleware.SetAdminAuditHook(cap.hook())
	t.Cleanup(func() { middleware.SetAdminAuditHook(nil) })
	return cap
}

// newAuditApp wires a controller with all stores non-nil and mounts every
// audited destructive route directly (bypassing authMiddleware, covered
// elsewhere).
func newAuditApp(t *testing.T) (*fiber.App, *agent.Catalog) {
	t.Helper()
	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	shadow, err := agent.LoadShadowLog(storage.NewMemory(), 100)
	if err != nil {
		t.Fatalf("LoadShadowLog: %v", err)
	}
	detectLog, err := agent.LoadDetectLog(storage.NewMemory(), 100)
	if err != nil {
		t.Fatalf("LoadDetectLog: %v", err)
	}
	ov, err := agent.LoadServiceOverrideStore(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadServiceOverrideStore: %v", err)
	}
	ctrl := NewAgentController(cat, nil, shadow, detectLog, ov, false)
	app := fiber.New()
	app.Delete("/api/agent/catalog", ctrl.resetCatalog)
	app.Delete("/api/agent/shadow", ctrl.clearShadow)
	app.Delete("/api/agent/detect", ctrl.clearDetect)
	app.Post("/api/agent/services", ctrl.createService)
	app.Put("/api/agent/services/:name", ctrl.renameService)
	app.Delete("/api/agent/services/:name", ctrl.deleteService)
	app.Post("/api/agent/service-overrides", ctrl.createServiceOverride)
	app.Delete("/api/agent/service-overrides/:id", ctrl.deleteServiceOverride)
	return app, cat
}

// TestAdminAudit_ResetCatalog_SuccessScopeAndCount proves the Clear-all reset
// emits agent.catalog.reset:success with a target that reflects the full-reset
// scope AND the cleared pattern/service counts.
func TestAdminAudit_ResetCatalog_SuccessScopeAndCount(t *testing.T) {
	cap := installCapture(t)
	app, cat := newAuditApp(t)

	// Seed some learned state so the cleared counts are non-zero.
	cat.RegisterService("payments")
	cat.Upsert("p1", "boom <*>", "es:prod", 5, 0.2, "default", "payments")

	code, _ := doJSON(t, app, "DELETE", "/api/agent/catalog", nil)
	if code != fiber.StatusOK {
		t.Fatalf("reset status = %d, want 200", code)
	}
	ev := cap.only(t, auditActionCatalogReset, middleware.AdminAuditSuccess)
	if ev.org != storage.DefaultOrgID {
		t.Errorf("org = %q, want %q", ev.org, storage.DefaultOrgID)
	}
	if want := "all patterns + services (patterns=1 services=1)"; ev.target != want {
		t.Errorf("target = %q, want %q", ev.target, want)
	}
}

// TestAdminAudit_ShadowDetect_SuccessAndDenial proves the shadow/detect clears
// emit their action on success (with the cleared count) and a denial when the
// log is disabled.
func TestAdminAudit_ShadowDetect_SuccessAndDenial(t *testing.T) {
	cap := installCapture(t)
	app, _ := newAuditApp(t)

	code, _ := doJSON(t, app, "DELETE", "/api/agent/shadow", nil)
	if code != fiber.StatusOK {
		t.Fatalf("clear shadow status = %d, want 200", code)
	}
	if ev := cap.only(t, auditActionShadowCleared, middleware.AdminAuditSuccess); ev.target != "shadow log (cleared=0)" {
		t.Errorf("shadow target = %q", ev.target)
	}

	cap.reset()
	code, _ = doJSON(t, app, "DELETE", "/api/agent/detect", nil)
	if code != fiber.StatusOK {
		t.Fatalf("clear detect status = %d, want 200", code)
	}
	if ev := cap.only(t, auditActionDetectCleared, middleware.AdminAuditSuccess); ev.target != "detect log (cleared=0)" {
		t.Errorf("detect target = %q", ev.target)
	}

	// Denial: a controller with disabled shadow/detect logs rejects the clear
	// and records a denial row.
	cap.reset()
	catNil, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	nilCtrl := NewAgentController(catNil, nil, nil, nil, nil, false)
	napp := fiber.New()
	napp.Delete("/api/agent/shadow", nilCtrl.clearShadow)
	napp.Delete("/api/agent/detect", nilCtrl.clearDetect)

	if code, _ := doJSON(t, napp, "DELETE", "/api/agent/shadow", nil); code != fiber.StatusServiceUnavailable {
		t.Fatalf("disabled shadow status = %d, want 503", code)
	}
	cap.only(t, auditActionShadowCleared, middleware.AdminAuditDenied)

	cap.reset()
	if code, _ := doJSON(t, napp, "DELETE", "/api/agent/detect", nil); code != fiber.StatusServiceUnavailable {
		t.Fatalf("disabled detect status = %d, want 503", code)
	}
	cap.only(t, auditActionDetectCleared, middleware.AdminAuditDenied)
}

// TestAdminAudit_ServiceMutations_SuccessAndDenial proves each manual-service
// mutation emits a success row (with the service-name target) and a denial row.
func TestAdminAudit_ServiceMutations_SuccessAndDenial(t *testing.T) {
	cap := installCapture(t)
	app, _ := newAuditApp(t)

	// create success
	if code, _ := doJSON(t, app, "POST", "/api/agent/services", map[string]any{"name": "payments"}); code != fiber.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	if ev := cap.only(t, auditActionServiceCreated, middleware.AdminAuditSuccess); ev.target != "payments" {
		t.Errorf("create target = %q, want payments", ev.target)
	}

	// create denial (duplicate)
	cap.reset()
	if code, _ := doJSON(t, app, "POST", "/api/agent/services", map[string]any{"name": "payments"}); code != fiber.StatusConflict {
		t.Fatalf("dup create status = %d, want 409", code)
	}
	if ev := cap.only(t, auditActionServiceCreated, middleware.AdminAuditDenied); ev.target != "payments" {
		t.Errorf("dup target = %q, want payments", ev.target)
	}

	// rename success
	cap.reset()
	if code, _ := doJSON(t, app, "PUT", "/api/agent/services/payments", map[string]any{"name": "billing"}); code != fiber.StatusOK {
		t.Fatalf("rename status = %d, want 200", code)
	}
	if ev := cap.only(t, auditActionServiceRenamed, middleware.AdminAuditSuccess); ev.target != "payments -> billing" {
		t.Errorf("rename target = %q, want 'payments -> billing'", ev.target)
	}

	// rename denial (missing)
	cap.reset()
	if code, _ := doJSON(t, app, "PUT", "/api/agent/services/ghost", map[string]any{"name": "x"}); code != fiber.StatusNotFound {
		t.Fatalf("rename missing status = %d, want 404", code)
	}
	if ev := cap.only(t, auditActionServiceRenamed, middleware.AdminAuditDenied); ev.target != "ghost -> x" {
		t.Errorf("rename denial target = %q, want 'ghost -> x'", ev.target)
	}

	// delete success
	cap.reset()
	if code, _ := doJSON(t, app, "DELETE", "/api/agent/services/billing", nil); code != fiber.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", code)
	}
	if ev := cap.only(t, auditActionServiceDeleted, middleware.AdminAuditSuccess); ev.target != "billing" {
		t.Errorf("delete target = %q, want billing", ev.target)
	}

	// delete denial (missing)
	cap.reset()
	if code, _ := doJSON(t, app, "DELETE", "/api/agent/services/ghost", nil); code != fiber.StatusNotFound {
		t.Fatalf("delete missing status = %d, want 404", code)
	}
	if ev := cap.only(t, auditActionServiceDeleted, middleware.AdminAuditDenied); ev.target != "ghost" {
		t.Errorf("delete denial target = %q, want ghost", ev.target)
	}
}

// TestAdminAudit_OverrideMutations_SuccessAndDenial proves the override set /
// delete routes emit a success row (target carries the override id + service)
// and a denial row.
func TestAdminAudit_OverrideMutations_SuccessAndDenial(t *testing.T) {
	cap := installCapture(t)
	app, cat := newAuditApp(t)
	_ = cat.CreateService("payments")

	// set denial (unknown target service)
	if code, _ := doJSON(t, app, "POST", "/api/agent/service-overrides", map[string]any{
		"source_type": "log", "match": "p-1", "service": "ghost",
	}); code != fiber.StatusBadRequest {
		t.Fatalf("override unknown-target status = %d, want 400", code)
	}
	if ev := cap.only(t, auditActionOverrideSet, middleware.AdminAuditDenied); ev.target != "service ghost" {
		t.Errorf("override denial target = %q, want 'service ghost'", ev.target)
	}

	// set success
	cap.reset()
	_, body := doJSON(t, app, "POST", "/api/agent/service-overrides", map[string]any{
		"source_type": "log", "match": "p-1", "service": "payments",
	})
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("override create returned no id; body=%v", body)
	}
	if ev := cap.only(t, auditActionOverrideSet, middleware.AdminAuditSuccess); ev.target != id+" -> payments" {
		t.Errorf("override set target = %q, want %q", ev.target, id+" -> payments")
	}

	// delete success
	cap.reset()
	if code, _ := doJSON(t, app, "DELETE", "/api/agent/service-overrides/"+id, nil); code != fiber.StatusNoContent {
		t.Fatalf("override delete status = %d, want 204", code)
	}
	if ev := cap.only(t, auditActionOverrideDeleted, middleware.AdminAuditSuccess); ev.target != id {
		t.Errorf("override delete target = %q, want %q", ev.target, id)
	}

	// delete denial (missing id)
	cap.reset()
	if code, _ := doJSON(t, app, "DELETE", "/api/agent/service-overrides/nope", nil); code != fiber.StatusNotFound {
		t.Fatalf("override delete missing status = %d, want 404", code)
	}
	if ev := cap.only(t, auditActionOverrideDeleted, middleware.AdminAuditDenied); ev.target != "nope" {
		t.Errorf("override delete denial target = %q, want nope", ev.target)
	}
}

// TestAdminAudit_CommunityNoHook_NoOp proves that with NO hook registered
// (community mode) the destructive routes still succeed and nothing panics —
// the seam is a clean no-op with no audit backend.
func TestAdminAudit_CommunityNoHook_NoOp(t *testing.T) {
	middleware.SetAdminAuditHook(nil) // ensure the slot is empty (community)
	app, _ := newAuditApp(t)

	if code, _ := doJSON(t, app, "DELETE", "/api/agent/catalog", nil); code != fiber.StatusOK {
		t.Errorf("reset status = %d, want 200", code)
	}
	if code, _ := doJSON(t, app, "DELETE", "/api/agent/shadow", nil); code != fiber.StatusOK {
		t.Errorf("clear shadow status = %d, want 200", code)
	}
	if code, _ := doJSON(t, app, "POST", "/api/agent/services", map[string]any{"name": "svc"}); code != fiber.StatusCreated {
		t.Errorf("create status = %d, want 201", code)
	}
}
