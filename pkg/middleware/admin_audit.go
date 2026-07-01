// admin_audit.go — a generic, unopinionated admin-mutation audit seam
// (companion to the extension.go org/auth seams).
//
// State-changing admin routes (e.g. the agent catalog reset, shadow/detect
// clears, and the manual service / attribution-override mutations) call
// RecordAdminAudit after they succeed OR when they reject a request. OSS ships
// NO hook, so every call is a community-mode no-op and an untouched OSS binary
// behaves exactly as before — there is no audit backend in the open-core tree.
// An external wrapper (the enterprise build) registers a hook that writes the
// event into its append-only, per-org audit trail.
//
// The hook is handed the request ctx (not a pre-extracted actor/org) so the
// implementation can derive the operator identity and tenant the way it
// authenticates requests — OSS has no concept of an SSO principal or a tenant,
// so resolving those belongs entirely to the wrapper.
//
// Registration is process-wide and expected to happen once at boot, before the
// server starts accepting connections.
package middleware

import (
	"sync/atomic"

	"github.com/gofiber/fiber/v2"
)

// Admin-audit outcome strings. A destructive admin action records exactly one
// of these; they mirror the wrapper's audit-result vocabulary without the OSS
// tree depending on it.
const (
	// AdminAuditSuccess — the mutation completed.
	AdminAuditSuccess = "success"
	// AdminAuditDenied — the mutation was rejected (validation, precondition,
	// conflict, or a gate).
	AdminAuditDenied = "denied"
)

// AdminAuditEvent describes one state-changing admin action for the audit
// seam. Action is a stable, namespaced verb (e.g. "agent.catalog.reset");
// Target is the non-secret object acted upon (a service name, override id, or
// a reset summary); Result is one of the AdminAudit* outcome strings.
type AdminAuditEvent struct {
	Action string
	Target string
	Result string
}

// AdminAuditHook records one admin mutation. It receives the request ctx so an
// implementation can derive the actor, org and request provenance. OSS ships
// none (community no-op).
type AdminAuditHook func(c *fiber.Ctx, ev AdminAuditEvent)

// adminAuditSlot holds the registered hook. It uses atomic.Value so a boot-time
// registration is safely visible to request goroutines without a data race.
var adminAuditSlot atomic.Value // AdminAuditHook

// SetAdminAuditHook registers the hook invoked for every state-changing admin
// action. OSS ships none (the slot is a no-op). Passing nil clears it (back to
// the community no-op). Call at boot.
func SetAdminAuditHook(h AdminAuditHook) {
	if h == nil {
		adminAuditSlot = atomic.Value{}
		return
	}
	adminAuditSlot.Store(h)
}

// RecordAdminAudit emits one admin-mutation audit event to the registered hook.
// With no hook registered (community mode) it is a no-op, so an OSS binary is
// byte-inert. Callers pass a freshly built, non-secret target string.
func RecordAdminAudit(c *fiber.Ctx, action, target, result string) {
	if h, ok := adminAuditSlot.Load().(AdminAuditHook); ok && h != nil {
		h(c, AdminAuditEvent{Action: action, Target: target, Result: result})
	}
}
