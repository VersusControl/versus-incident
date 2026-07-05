package controllers

import (
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

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
	app.Post("/api/agent/patterns/:id", ctrl.updatePattern)
	app.Delete("/api/agent/patterns", ctrl.clearPatterns)
	app.Delete("/api/agent/services", ctrl.clearServices)
	app.Delete("/api/agent/shadow", ctrl.clearShadow)
	app.Delete("/api/agent/detect", ctrl.clearDetect)
	app.Post("/api/agent/services", ctrl.createService)
	app.Put("/api/agent/services/:name", ctrl.renameService)
	app.Delete("/api/agent/services/:name", ctrl.deleteService)
	app.Post("/api/agent/services/:name/grace", ctrl.controlServiceGrace)
	app.Post("/api/agent/service-overrides", ctrl.createServiceOverride)
	app.Delete("/api/agent/service-overrides/:id", ctrl.deleteServiceOverride)
	return app, cat
}

// TestAdminAudit_ClearPatterns_SuccessScopeAndCount proves the Clear-all-logs
// reset emits agent.patterns.cleared:success with a target that reflects the
// pattern-reset scope AND the cleared pattern count, leaving services intact.
func TestAdminAudit_ClearPatterns_SuccessScopeAndCount(t *testing.T) {
	cap := installCapture(t)
	app, cat := newAuditApp(t)

	// Seed some learned state so the cleared count is non-zero.
	cat.RegisterService("payments")
	cat.Upsert("p1", "boom <*>", "es:prod", 5, 0.2, "default", "payments")

	code, _ := doJSON(t, app, "DELETE", "/api/agent/patterns", nil)
	if code != fiber.StatusOK {
		t.Fatalf("clear patterns status = %d, want 200", code)
	}
	ev := cap.only(t, auditActionPatternsCleared, middleware.AdminAuditSuccess)
	if ev.org != storage.DefaultOrgID {
		t.Errorf("org = %q, want %q", ev.org, storage.DefaultOrgID)
	}
	if want := "all patterns (patterns=1)"; ev.target != want {
		t.Errorf("target = %q, want %q", ev.target, want)
	}
	// Services survive a pattern-only clear.
	if n := len(cat.AllServices()); n != 1 {
		t.Errorf("services after clear-patterns = %d, want 1 (services must survive)", n)
	}
}

// TestAdminAudit_PatternRelabel_SuccessAndDenial proves the pattern relabel/
// clear route emits agent.pattern.relabeled:success (target = the pattern id,
// org from ctx) on a live pattern AND a denial row when the pattern id is
// unknown — the branches the audit-test file had left uncovered.
func TestAdminAudit_PatternRelabel_SuccessAndDenial(t *testing.T) {
	cap := installCapture(t)
	app, cat := newAuditApp(t)

	// Seed a learned pattern so the relabel targets a live row.
	cat.RegisterService("payments")
	cat.Upsert("p1", "boom <*>", "es:prod", 5, 0.2, "default", "payments")

	// relabel success (set verdict "known")
	if code, _ := doJSON(t, app, "POST", "/api/agent/patterns/p1", map[string]any{"verdict": "known"}); code != fiber.StatusOK {
		t.Fatalf("relabel status = %d, want 200", code)
	}
	ev := cap.only(t, auditActionPatternRelabeled, middleware.AdminAuditSuccess)
	if ev.org != storage.DefaultOrgID {
		t.Errorf("org = %q, want %q", ev.org, storage.DefaultOrgID)
	}
	if ev.target != "p1" {
		t.Errorf("relabel target = %q, want p1", ev.target)
	}

	// relabel denial (unknown pattern id)
	cap.reset()
	if code, _ := doJSON(t, app, "POST", "/api/agent/patterns/ghost", map[string]any{"verdict": "known"}); code != fiber.StatusNotFound {
		t.Fatalf("relabel missing status = %d, want 404", code)
	}
	if ev := cap.only(t, auditActionPatternRelabeled, middleware.AdminAuditDenied); ev.target != "ghost" {
		t.Errorf("relabel denial target = %q, want ghost", ev.target)
	}
}

// TestAdminAudit_ClearServices_SuccessScopeAndCount proves the Clear-all-
// services reset emits agent.services.cleared:success with a target that
// reflects the service-reset scope AND the cleared service count, leaving
// patterns intact.
func TestAdminAudit_ClearServices_SuccessScopeAndCount(t *testing.T) {
	cap := installCapture(t)
	app, cat := newAuditApp(t)

	// Seed some learned state so the cleared count is non-zero.
	cat.RegisterService("payments")
	cat.Upsert("p1", "boom <*>", "es:prod", 5, 0.2, "default", "payments")

	code, _ := doJSON(t, app, "DELETE", "/api/agent/services", nil)
	if code != fiber.StatusOK {
		t.Fatalf("clear services status = %d, want 200", code)
	}
	ev := cap.only(t, auditActionServicesCleared, middleware.AdminAuditSuccess)
	if ev.org != storage.DefaultOrgID {
		t.Errorf("org = %q, want %q", ev.org, storage.DefaultOrgID)
	}
	if want := "all services (services=1)"; ev.target != want {
		t.Errorf("target = %q, want %q", ev.target, want)
	}
	// Patterns survive a service-only clear.
	if n := cat.Len(); n != 1 {
		t.Errorf("patterns after clear-services = %d, want 1 (patterns must survive)", n)
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

// TestAdminAudit_GraceControl_ValidActionsAndBoundedInvalidTarget proves the
// grace-control route records `<name> (action=end|restart)` on the valid paths
// (unchanged) and, on an INVALID action, records a bounded, control-char-
// stripped target — never the raw client string (B52 log-injection guard).
func TestAdminAudit_GraceControl_ValidActionsAndBoundedInvalidTarget(t *testing.T) {
	cap := installCapture(t)
	app, cat := newAuditApp(t)
	cat.RegisterService("payments")

	// valid: end → success target unchanged.
	if code, _ := doJSON(t, app, "POST", "/api/agent/services/payments/grace", map[string]any{"action": "end"}); code != fiber.StatusOK {
		t.Fatalf("end status = %d, want 200", code)
	}
	if ev := cap.only(t, auditActionServiceGraceControlled, middleware.AdminAuditSuccess); ev.target != "payments (action=end)" {
		t.Errorf("end target = %q, want 'payments (action=end)'", ev.target)
	}

	// valid: restart → success target unchanged.
	cap.reset()
	if code, _ := doJSON(t, app, "POST", "/api/agent/services/payments/grace", map[string]any{"action": "restart"}); code != fiber.StatusOK {
		t.Fatalf("restart status = %d, want 200", code)
	}
	if ev := cap.only(t, auditActionServiceGraceControlled, middleware.AdminAuditSuccess); ev.target != "payments (action=restart)" {
		t.Errorf("restart target = %q, want 'payments (action=restart)'", ev.target)
	}

	// invalid: an oversized, control-char-laden client action must be rejected
	// (400) AND must never reach the audit target verbatim/unbounded.
	cap.reset()
	evil := "delete\n\rDROP TABLE audit;" + strings.Repeat("A", 500)
	if code, _ := doJSON(t, app, "POST", "/api/agent/services/payments/grace", map[string]any{"action": evil}); code != fiber.StatusBadRequest {
		t.Fatalf("invalid action status = %d, want 400", code)
	}
	ev := cap.only(t, auditActionServiceGraceControlled, middleware.AdminAuditDenied)
	if strings.Contains(ev.target, evil) {
		t.Fatalf("audit target echoed the raw client action verbatim: %q", ev.target)
	}
	if strings.ContainsAny(ev.target, "\n\r") {
		t.Errorf("audit target contains control/newline chars (log-injection surface): %q", ev.target)
	}
	if got, max := len(ev.target), len("payments (action=invalid:)")+maxAuditActionLen; got > max {
		t.Errorf("audit target not bounded: len=%d > %d target=%q", got, max, ev.target)
	}
	if !strings.HasPrefix(ev.target, "payments (action=invalid:") {
		t.Errorf("target = %q, want it to name the service + a bounded action=invalid token", ev.target)
	}

	// invalid on a non-existent service still audits a denial with a bounded
	// target (the raw action never lands verbatim regardless of service state).
	cap.reset()
	if code, _ := doJSON(t, app, "POST", "/api/agent/services/ghost/grace", map[string]any{"action": strings.Repeat("z", 300)}); code != fiber.StatusBadRequest {
		t.Fatalf("invalid action (missing svc) status = %d, want 400", code)
	}
	if ev := cap.only(t, auditActionServiceGraceControlled, middleware.AdminAuditDenied); len(ev.target) > len("ghost (action=invalid:)")+maxAuditActionLen {
		t.Errorf("audit target not bounded: len=%d target=%q", len(ev.target), ev.target)
	}
}

// TestAdminAudit_CreateServiceOverride_BoundedDenialTarget proves the override-
// create route (B53 / QA-031) never echoes a raw, unbounded client `service`
// into the audit target on EITHER denial branch — the unknown-target-service
// 400 and the overrides.Put-error 400 — while the valid create target stays
// unchanged. Same class + guard as the B52 grace-control fix.
func TestAdminAudit_CreateServiceOverride_BoundedDenialTarget(t *testing.T) {
	overrideMaxTargetLen := len("service ") + maxAuditActionLen
	evil := "ghost\n\rDROP TABLE audit;" + strings.Repeat("A", 500)

	// (1) unknown-target-service denial: catalog non-nil ⇒ the existence check
	// fires and rejects the arbitrary client service. The denial target must be
	// bounded + control-char-stripped, never the raw client string.
	t.Run("unknown target service branch", func(t *testing.T) {
		cap := installCapture(t)
		app, _ := newAuditApp(t) // catalog non-nil
		code, _ := doJSON(t, app, "POST", "/api/agent/service-overrides", map[string]any{
			"source_type": "log", "match": "p-1", "service": evil,
		})
		if code != fiber.StatusBadRequest {
			t.Fatalf("unknown-target status = %d, want 400", code)
		}
		ev := cap.only(t, auditActionOverrideSet, middleware.AdminAuditDenied)
		assertBoundedOverrideTarget(t, ev.target, evil, overrideMaxTargetLen)
	})

	// (2) overrides.Put-error denial: catalog nil ⇒ the existence check is
	// skipped, so an oversized service reaches Put, which rejects it ("entry
	// exceeds maximum length"). The denial target must still be bounded, so an
	// arbitrary client service can never reach the trail verbatim/unbounded even
	// when no catalog gates it.
	t.Run("overrides.Put error branch", func(t *testing.T) {
		cap := installCapture(t)
		ov, err := agent.LoadServiceOverrideStore(storage.NewMemory())
		if err != nil {
			t.Fatalf("LoadServiceOverrideStore: %v", err)
		}
		ctrl := NewAgentController(nil, nil, nil, nil, ov, false) // nil catalog
		app := fiber.New()
		app.Post("/api/agent/service-overrides", ctrl.createServiceOverride)
		code, _ := doJSON(t, app, "POST", "/api/agent/service-overrides", map[string]any{
			"source_type": "log", "match": "p-1", "service": evil,
		})
		if code != fiber.StatusBadRequest {
			t.Fatalf("put-error status = %d, want 400", code)
		}
		ev := cap.only(t, auditActionOverrideSet, middleware.AdminAuditDenied)
		assertBoundedOverrideTarget(t, ev.target, evil, overrideMaxTargetLen)
	})

	// (3) valid create target unchanged: an existing, in-bounds service still
	// records `<id> -> <service>` verbatim (never sanitized/truncated).
	t.Run("valid create target unchanged", func(t *testing.T) {
		cap := installCapture(t)
		app, cat := newAuditApp(t)
		_ = cat.CreateService("payments")
		_, body := doJSON(t, app, "POST", "/api/agent/service-overrides", map[string]any{
			"source_type": "log", "match": "p-1", "service": "payments",
		})
		id, _ := body["id"].(string)
		if id == "" {
			t.Fatalf("override create returned no id; body=%v", body)
		}
		if ev := cap.only(t, auditActionOverrideSet, middleware.AdminAuditSuccess); ev.target != id+" -> payments" {
			t.Errorf("valid create target = %q, want %q", ev.target, id+" -> payments")
		}
	})
}

// assertBoundedOverrideTarget checks a createServiceOverride denial target is a
// bounded, control-char-stripped "service <token>" — never the raw client
// string, no newline/CR, length-capped (B53 log-injection guard).
func assertBoundedOverrideTarget(t *testing.T, target, raw string, max int) {
	t.Helper()
	if strings.Contains(target, raw) {
		t.Fatalf("audit target echoed the raw client service verbatim: %q", target)
	}
	if strings.ContainsAny(target, "\n\r") {
		t.Errorf("audit target contains control/newline chars (log-injection surface): %q", target)
	}
	if len(target) > max {
		t.Errorf("audit target not bounded: len=%d > %d target=%q", len(target), max, target)
	}
	if !strings.HasPrefix(target, "service ") {
		t.Errorf("target = %q, want a 'service <bounded token>' shape", target)
	}
}

// TestSanitizeAuditToken exercises the B52 helper directly: it strips
// control/newline runes, caps the result at maxAuditActionLen bytes without
// ever exceeding it (or splitting a multi-byte rune), and is a clean pass-
// through for empty/normal input.
func TestSanitizeAuditToken(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"normal short", "end", "end"},
		{"normal restart", "restart", "restart"},
		{"strips newline+cr", "de\nle\rte", "delete"},
		{"strips tab+control", "a\tb\x00c\x1bd", "abcd"},
		{"caps oversized", strings.Repeat("A", 500), strings.Repeat("A", maxAuditActionLen)},
		{"strip then cap", "x\n\r" + strings.Repeat("B", 500), "x" + strings.Repeat("B", maxAuditActionLen-1)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeAuditToken(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeAuditToken(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if len(got) > maxAuditActionLen {
				t.Errorf("sanitizeAuditToken(%q) len=%d exceeds cap %d", tc.in, len(got), maxAuditActionLen)
			}
			if strings.ContainsAny(got, "\n\r") {
				t.Errorf("sanitizeAuditToken(%q) leaked a newline/CR: %q", tc.in, got)
			}
		})
	}

	// Multi-byte runes: the byte cap must never be exceeded nor split a rune.
	multi := sanitizeAuditToken(strings.Repeat("€", 40)) // 3 bytes each
	if len(multi) > maxAuditActionLen {
		t.Errorf("multibyte cap exceeded: len=%d > %d", len(multi), maxAuditActionLen)
	}
	if !utf8.ValidString(multi) {
		t.Errorf("multibyte result split a rune (invalid UTF-8): %q", multi)
	}
}

// TestAdminAudit_CommunityNoHook_NoOp proves that with NO hook registered
// (community mode) the destructive routes still succeed and nothing panics —
// the seam is a clean no-op with no audit backend.
func TestAdminAudit_CommunityNoHook_NoOp(t *testing.T) {
	middleware.SetAdminAuditHook(nil) // ensure the slot is empty (community)
	app, _ := newAuditApp(t)

	if code, _ := doJSON(t, app, "DELETE", "/api/agent/patterns", nil); code != fiber.StatusOK {
		t.Errorf("clear patterns status = %d, want 200", code)
	}
	if code, _ := doJSON(t, app, "DELETE", "/api/agent/shadow", nil); code != fiber.StatusOK {
		t.Errorf("clear shadow status = %d, want 200", code)
	}
	if code, _ := doJSON(t, app, "POST", "/api/agent/services", map[string]any{"name": "svc"}); code != fiber.StatusCreated {
		t.Errorf("create status = %d, want 201", code)
	}
}
