// extension.go — generic, unopinionated extension seams (X2-T3).
//
// These are registration hooks the OSS server exposes so an external
// module (the enterprise build, or any third-party wrapper) can attach
// behaviour WITHOUT the OSS tree depending on it. Nothing here is
// enterprise-specific: the defaults are community-mode no-ops, so an
// untouched OSS binary behaves exactly as before.
//
// Two seams:
//
//   - Org injection — a request-context org id, resolved per request and
//     defaulting to storage.DefaultOrgID ("default"). Single-tenant OSS
//     never sees it; a multi-tenant wrapper registers a resolver that
//     reads the org from a header/token.
//   - Auth slot — a settable middleware that runs on the API surface
//     ahead of the OSS gateway-secret checks. OSS registers none (the
//     slot is a pass-through); a wrapper registers SSO/JWT enforcement.
//     A registered handler may call MarkAuthorized to tell the OSS
//     gateway-secret guards the request is already authenticated by an
//     alternative credential (e.g. an SSO session), so one enterprise
//     credential can unlock both the data plane and the admin surfaces.
//
// Registration is process-wide and expected to happen once at boot,
// before the server starts accepting connections.
package middleware

import (
	"strings"
	"sync/atomic"

	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

// OrgContextKey is the fiber Locals key under which the resolved org id
// is stored for the lifetime of a request.
const OrgContextKey = "versus.org_id"

// OrgResolver extracts the org id for a request. Returning "" means "no
// explicit org" and falls back to storage.DefaultOrgID.
type OrgResolver func(c *fiber.Ctx) string

// authSlot and orgResolverSlot hold the registered hooks. They use
// atomic.Value so a boot-time registration is safely visible to request
// goroutines without a data race.
var (
	authSlot        atomic.Value // fiber.Handler
	orgResolverSlot atomic.Value // OrgResolver
)

// SetAuthMiddleware registers an extra auth handler that runs on the API
// surface ahead of the OSS gateway-secret checks. OSS ships none. Passing
// nil clears the slot (back to the community pass-through). Call at boot.
func SetAuthMiddleware(h fiber.Handler) {
	if h == nil {
		authSlot = atomic.Value{}
		return
	}
	authSlot.Store(h)
}

// AuthMiddleware returns the registered auth handler, or a no-op
// pass-through when none is registered (community mode).
func AuthMiddleware() fiber.Handler {
	if v, ok := authSlot.Load().(fiber.Handler); ok && v != nil {
		return v
	}
	return func(c *fiber.Ctx) error { return c.Next() }
}

// AuthorizedContextKey is the fiber Locals key a registered auth handler
// (see SetAuthMiddleware) sets to mark a request as already authenticated
// by an alternative credential — e.g. an enterprise SSO session. The OSS
// gateway-secret guards honour this flag and skip the
// X-Gateway-Secret check when it is set, so a single enterprise credential
// can unlock both the data plane and the admin surfaces.
const AuthorizedContextKey = "versus.authorized"

// MarkAuthorized records that the current request was authenticated upstream
// by a registered auth handler. Community OSS never calls this, so the
// gateway-secret guards behave exactly as before.
func MarkAuthorized(c *fiber.Ctx) {
	c.Locals(AuthorizedContextKey, true)
}

// RequestAuthorized reports whether a prior auth handler marked this request
// authorized. It defaults to false (community mode), leaving the OSS
// gateway-secret checks unchanged.
func RequestAuthorized(c *fiber.Ctx) bool {
	v, _ := c.Locals(AuthorizedContextKey).(bool)
	return v
}

// SetOrgResolver registers the function used to resolve an org id from a
// request. OSS ships none, so every request resolves to the default org.
// Passing nil clears it. Call at boot.
func SetOrgResolver(r OrgResolver) {
	if r == nil {
		orgResolverSlot = atomic.Value{}
		return
	}
	orgResolverSlot.Store(r)
}

// OrgInjector returns middleware that stamps the resolved org id onto the
// request context. With no resolver registered (or a resolver that
// returns ""), every request is scoped to storage.DefaultOrgID, which is
// invisible to single-tenant OSS users.
func OrgInjector() fiber.Handler {
	return func(c *fiber.Ctx) error {
		org := ""
		if r, ok := orgResolverSlot.Load().(OrgResolver); ok && r != nil {
			// A resolver typically reads the org from a request header/token,
			// which Fiber returns as a string backed by the pooled, reused
			// request buffer (golden rule #11). This value is stamped into the
			// request context and may be persisted by a downstream handler, so
			// copy it off the buffer here (suspenders) — shared middleware
			// cannot assume the host built the app with Immutable:true.
			org = strings.Clone(r(c))
		}
		if org == "" {
			org = storage.DefaultOrgID
		}
		c.Locals(OrgContextKey, org)
		return c.Next()
	}
}

// OrgFromContext returns the org id stamped on the request by
// OrgInjector, or storage.DefaultOrgID when none was set.
func OrgFromContext(c *fiber.Ctx) string {
	if v, ok := c.Locals(OrgContextKey).(string); ok && v != "" {
		return v
	}
	return storage.DefaultOrgID
}
