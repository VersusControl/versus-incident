# Security Policy

Versus Incident handles incident data, webhook secrets, and on-call routing
keys. We take security reports seriously and aim to acknowledge every report
quickly.

## Supported versions

Security fixes are issued against the latest minor release. Older minor
versions are not patched.

| Version | Supported |
|---------|-----------|
| latest `1.x` | ✅ |
| older `1.x` | ❌ — please upgrade |
| `0.x` | ❌ |

## Reporting a vulnerability

**Please do not open a public GitHub issue for security reports.**

Use one of the following private channels:

1. **GitHub Security Advisory** (preferred):
   https://github.com/VersusControl/versus-incident/security/advisories/new
2. **Email:** `admin@versusincident.com` with the subject line
   `[SECURITY] versus-incident: <short summary>`.

When reporting, please include:

- Affected version(s) and deployment mode (Docker, Helm, source build)
- Reproduction steps or proof-of-concept
- Impact (information disclosure, RCE, privilege escalation, etc.)
- Any suggested fix or mitigation

You should expect:

- **Acknowledgement** within 3 business days.
- **Initial triage and severity assessment** within 7 business days.
- **Fix or mitigation plan** depending on severity:
  - Critical / High → patch release as soon as a fix is verified.
  - Medium → next scheduled release.
  - Low → tracked publicly once a fix is available.
- A **CVE / GHSA advisory** for any vulnerability that affects published
  releases.

We are happy to credit reporters in the advisory unless you prefer to remain
anonymous.

## Out of scope

- Reports against unsupported versions (please upgrade and re-test).
- Issues that require an attacker to already control the host running
  Versus Incident, the Redis instance, or the configured AI provider.
- Findings that depend on misconfiguration explicitly documented as
  development-only (e.g. `redis.insecure_skip_verify: true`).
- Denial-of-service via unbounded request volume to the public
  `/api/incidents` endpoint when no rate-limiting/proxy is deployed in
  front. Operators are expected to terminate TLS and rate-limit at the
  edge.
- Denial-of-service via unbounded request volume to the hosted control
  plane's per-org ingestion edge (`POST /o/{org-id}/api/incidents`). As with
  `/api/incidents`, operators terminate TLS and rate-limit at the edge. The
  edge now requires a per-org ingest key (see hardening notes below); built-in
  per-org/per-IP edge rate limiting remains a tracked follow-up.

## Hardening recommendations for operators

- Run Versus Incident behind an authenticated reverse proxy. The
  `/api/incidents` endpoint is intentionally unauthenticated so monitoring
  tools can post directly; restrict it at the network layer.
- Set a strong root-level `gateway_secret` if any admin endpoints
  (`/api/admin/*` or `/api/agent/*`) are exposed. Empty `gateway_secret`
  means the admin routes are not registered at all — never set it to an
  empty string and assume the endpoints are protected by something else.
- Use TLS to Redis in production. Do **not** set
  `redis.insecure_skip_verify: true` outside development.
- Provide all tokens, webhook URLs, and routing keys via environment
  variables — never commit them to YAML.
- Enable redaction (`agent.redaction.enable: true`, default) before
  pointing the agent at production logs.
- The hosted control plane's per-org ingestion edge
  (`POST /o/{org-id}/api/incidents`) is gated by a valid, active org id **and**
  a per-org ingest key: the key is verified with a constant-time keyed-HMAC
  compare before the request is forwarded, so a missing or wrong key is
  rejected with `401` and never reaches a tenant's data plane. The key is
  256-bit, shown to the customer exactly once at provisioning/rotation, and
  stored only as a keyed HMAC-SHA256 under `INGEST_KEY_PEPPER` — a database
  leak cannot recover raw keys without the pepper. Keep the edge behind a rate
  limiter until built-in per-org/per-IP rate limiting ships. It exposes no read
  surface — cross-tenant **read** isolation is enforced server-side (epic X3).
  Persistent deployments **must** set `INGEST_KEY_PEPPER`; otherwise an
  ephemeral pepper is used and issued keys stop verifying after a restart.

## Enterprise RBAC and audit trail (epic X5)

The private enterprise build (`versus-enterprise/`) adds role-based access
control and an append-only audit trail. Both are license-gated and dormant in
the community build; the OSS tree never imports the enterprise module.

### RBAC (`versus-enterprise/pkg/rbac`)

- **Data-driven roles.** Four roles — `viewer` < `responder` < `admin` <
  `owner` — map to permissions (`incidents:view`, `incidents:act`,
  `tenants:manage`, `sso:manage`, `roles:manage`, `audit:view`) via a single
  authoritative map. Handlers declare the permission they need
  (`Require(permission)`); they never branch on role.
- **Fail closed.** An unknown/empty role grants nothing; a subject with no
  assignment is denied (`403`); no session is `401`. There is no implicit
  access.
- **Per-org isolation.** Subject→role assignments persist through the X3
  `tenancy.NewOrgScoped` storage seam, so a role in org A can never be seen or
  used in org B. The role-assignment API is guarded by the `roles:manage`
  permission, resolved from the caller's org-bound SSO session.

### Audit trail (`versus-enterprise/pkg/audit`)

- **Append-only.** The API surface exposes only append (in-process emitters)
  and read-only query/verify — there is no update or delete route. Entries
  are per-org scoped and persisted through the same `NewOrgScoped` seam.
- **Tamper-evident.** Each entry carries a SHA-256 hash chained to the prior
  entry (length-prefixed fields, no concatenation collisions). `Verify` /
  `VerifyPersisted` detect any out-of-band edit of the stored log.
- **No secrets in entries.** Security-relevant events are recorded by name
  (e.g. `sso.config.changed`) — never the secret value. The admin read API
  `GET /enterprise/api/audit/{org}` is gated on the `audit:view` permission
  resolved from the caller's org-bound SSO session.
- **Catalog:** `sso.login.success` / `sso.login.failure`,
  `sso.config.changed`, `rbac.denied`, `rbac.role.assigned` /
  `rbac.role.unassigned`, `tenant.created` / `tenant.suspended` /
  `tenant.activated` / `tenant.deleted`.

### Golden rule #11 (Fiber request-buffer safety)

Audit entries OUTLIVE the request that produced them, so every request-derived
field (org, actor, target, request id, IP) is `strings.Clone`d at `Log.Append`
before it is retained. The regression guard
`TestBufferAliasing_AuditEntryOrgNotRewritten` builds a Fiber app **without**
`Immutable:true` so the clone is load-bearing, and was verified to FAIL when
the clone is removed and PASS once restored.

### Environment variables

X5 introduces **no new environment variables**. It reuses the auto-generated SSO
session pepper (the X4 session the RBAC guard resolves): the role-assignment and
audit read APIs are gated on the caller's RBAC permission carried by that
session. The pepper is generated and persisted on first licensed boot rather
than env-configured, never hardcoded; a request with no session fails the admin
APIs closed (`401`), and an insufficient role is denied (`403`).

### X5 gate evidence

`go build ./...`, `go vet ./...`, and `go test -race ./...` are green in
`versus-enterprise/`. Audit immutability (no delete/update API + hash chain),
per-org isolation (RBAC `RoleOf` and audit `Query` proven not to cross orgs),
constant-time secret compares, and the buffer-aliasing guard were all
verified at the X5 gate.

## Supply chain and dependency provenance

We hash-pin every dependency in `go.sum` and record provenance for any
dependency that warrants explanation in our audit evidence (epic X7).

### Reviewed transitive dependencies

| Dependency | Type | Status | Provenance |
|---|---|---|---|
| `github.com/meguminnnnnnnnn/go-openai v0.1.2` | indirect | Accepted | Transitive via cloudwego/eino-ext ACL layer; maintained fork of sashabaranov/go-openai (de-facto Go OpenAI client); hash-pinned in go.sum; no first-party import; reviewed and accepted by CEO 2026-06-08. |
| `github.com/cloudwego/eino-ext/components/embedding/openai v0.0.0-20260608142157-58d993d5cdff` | direct | Accepted | Added by epic E12 (runbook-RAG `find_runbook` tool). Same vendor family as the already-trusted `cloudwego/eino-ext/components/model/openai` (chat-model seam); rides the identical `eino-ext/libs/acl/openai` → `meguminnnnnnnnn/go-openai` chain — introduces **no new third-party vendor** at the leaf. Hash-pinned in go.sum; reviewed and accepted at the E12 security gate 2026-06-09. |

Notes for `eino-ext/components/embedding/openai`:

- **Why it is here:** the embeddings seam for the `find_runbook` tool —
  `pkg/agent/ai/eino/embedder.go` → `cloudwego/eino-ext/components/embedding/openai`
  → `cloudwego/eino-ext/libs/acl/openai` → `meguminnnnnnnnn/go-openai`. It is
  the embeddings sibling of the chat-model component already in use; its full
  transitive closure was already present in the module graph via the chat path
  (verified with `go list -deps`), so no new third-party vendor entered the tree.
- **Pseudo-version note:** pinned to an untagged commit pseudo-version
  (`v0.0.0-20260608142157-…`) rather than a semver tag like the `model/openai`
  sibling (`v0.1.13`). The `h1:`/`go.mod h1:` hashes are present and identical
  in both `versus-incident/go.sum` and `versus-enterprise/go.sum`, so the build
  remains reproducible. Flagged for the Core Engineer to bump to a tagged
  release when one is published (non-blocking).
- **Decision:** accept at the E12 gate — same vendor/provenance as the existing
  Eino model seam, no new attack surface beyond the already-reviewed
  chat-completion egress.

Notes for `meguminnnnnnnnn/go-openai`:

- **Why it is here:** pulled in by our Eino-based agent framework —
  `pkg/agent/ai/eino/chatmodel.go` → `cloudwego/eino-ext/components/model/openai`
  → `cloudwego/eino-ext/libs/acl/openai` → `meguminnnnnnnnn/go-openai`.
  No first-party code imports it directly; it is marked `// indirect` in
  both `versus-incident/go.mod` and `versus-enterprise/go.mod`.
- **Hash pin (verified):** present and identical in both
  `versus-incident/go.sum` and `versus-enterprise/go.sum`:
  - `h1:iXombGGjqjBrmE9WaSidUhhi3YQhf42QTHvHLMkgvCA=`
  - `go.mod h1:qs96ysDmxhE4BZoU45I43zcyfnaYxU3X+aRzLko/htY=`
- **Decision:** keep rather than evict — removing a transitive dependency of
  the CloudWeGo Eino ACL layer risks breaking the agent framework. Provenance
  is recorded here so the dependency is auditable and explained.

## Multi-tenant isolation and control-plane secret hygiene (epics X3, M4)

Reviewed and passed at the Security release gate on 2026-06-10
(`go test ./... -race`, both modules green).

- **Tenant read isolation (X3):** every read and write through the enterprise
  data plane is org-scoped; a cross-org read or delete returns "not found"
  with no existence leak, and per-org blobs are namespaced so two tenants never
  share storage. A `strings.Clone` guard in both the org resolver and the
  org-scoped storage wrapper stops fiber's reused request buffer from rewriting
  a stored org id. This is the confidentiality control behind the SOC 2
  multi-tenancy evidence (epics G7 / X7).
- **Secret hygiene (X3, M4):** all credentials — session key, gateway secret,
  Postgres DSN, Google OAuth client secret — are read
  from environment variables only (no hardcoded literals), compared in constant
  time where applicable, and never written to logs (startup logs print
  non-secret configuration only). Session tokens are 256-bit and stored only as
  an HMAC-SHA256 digest keyed by the session key, so a leak of the session
  table is not replayable; passwords are bcrypt (`DefaultCost`) and never
  logged.
- **Outbound TLS:** every control-plane outbound call (enterprise API, Google
  OIDC) uses the shared client with `MinVersion: TLS 1.2`; `http.DefaultClient`
  is never used.
- **Per-org ingestion edge — authentication closed, rate limiting deferred:**
  `POST /o/{org-id}/api/incidents` now requires a **per-org ingest key**
  (epic M4-FU1). The key is 256-bit, returned to the customer once at
  provisioning/rotation, and stored only as a keyed HMAC-SHA256 under
  `INGEST_KEY_PEPPER` (never plaintext, never bcrypt); verification is a
  constant-time `hmac.Equal` performed **before** the request is proxied, so a
  missing/wrong key is rejected `401` and never reaches a tenant data plane,
  and rotation invalidates the previous key immediately. This closes the
  authentication half of the follow-up flagged at the M4 gate. It remains a
  write/abuse surface only — cross-tenant read isolation is unaffected. One
  follow-up remains before the edge is exposed to the public internet with
  real tenants: built-in per-org and per-IP **rate limiting** (operators
  rate-limit at the edge until then).

## Control-plane billing and secret hygiene (founder-demo slice, epics M4-FU1 / M5 / M6)

Reviewed and passed at the Security release gate on 2026-06-10
(`go test ./...` plus `-race` on `internal/server`, `internal/billing`,
`internal/ingestkey` — all green). This slice adds server-rendered signup ->
Stripe Checkout (TEST) -> customer dashboard -> internal admin to the existing
M4 control plane.

- **Stripe secrets are env-only and never logged:** `STRIPE_SECRET_KEY`,
  `STRIPE_WEBHOOK_SECRET` and the `STRIPE_PRICE_*` ids are read from the
  environment in `internal/config`; the Stripe client returns sanitised errors
  (the Stripe `error.message` only, never the `Authorization: Bearer` secret),
  and startup logs print only a `billing=stripe|fake` label. With no
  `STRIPE_SECRET_KEY` the in-process `Fake` backend is wired so tests and the
  local demo run with no Stripe account.
- **Webhook signature verified, fail-closed:** the webhook handler recomputes
  the Stripe `HMAC-SHA256` over `timestamp.payload`, enforces a 5-minute
  tolerance window, compares in constant time, and **rejects when no webhook
  secret is configured** (never fail-open). Payment is confirmed by a
  server-side Checkout-Session retrieve (server-to-Stripe), never from a
  browser-supplied redirect parameter.
- **Per-org ingest key (M4-FU1):** see the edge note above — 256-bit, shown
  once, keyed-HMAC at rest under `INGEST_KEY_PEPPER`, constant-time verified,
  rotation invalidates the old key, enforced with `401` on the ingest edge.
  The raw key is never stored or logged (`Org.APIKeyHash` is `json:"-"`).
- **Admin/customer auth separation:** the internal `/admin` view accepts only
  a dedicated `vmp_admin` cookie keyed by `ADMIN_API_TOKEN` (constant-time);
  a customer `vmp_session` can never reach it, and an empty `ADMIN_API_TOKEN`
  disables admin entirely (fails closed).
- **CSRF + output encoding:** every state-changing browser POST carries a
  synchroniser token in an HttpOnly cookie, matched in constant time;
  templates use `html/template` auto-escaping with no `template.HTML` on user
  data.
- **Tracked follow-up (does not affect read isolation or secrets):** the
  success-redirect confirmation records the plan from a client cookie and
  binds the retrieved Checkout Session to the current session's account rather
  than to the session's `client_reference_id` org. This must be hardened
  before the recorded plan drives entitlements, usage limits, or real billing.
  **Status: CLOSED 2026-06-10 — see the F1 section below.**

## Control-plane secure checkout (F1) and React SPA posture (epics M4 / M5 / M6)

Reviewed and passed at the Security release gate on 2026-06-10
(`go test ./...` plus `-race` on `internal/server` and `internal/billing` —
all green). This change (A) closes the F1 checkout follow-up flagged above and
(B) replaces the server-rendered HTML control plane with a React SPA served
from an embedded `ui/dist/` plus a JSON API under `/api/*` and `/admin/api/*`.

- **F1 — checkout confirmation is authoritative and server-side (CLOSED):**
  `internal/server/checkout.go` `confirmCheckoutPlan` is the single source of
  truth shared by `POST /api/checkout/confirm` and the Stripe webhook. It (1)
  requires `Session.Paid()` (Stripe `payment_status`+`status`, never a browser
  redirect parameter); (2) binds the paid session to the caller by requiring
  `session.client_reference_id == org.ID` — a cross-tenant `cs_…` replay is
  rejected with `403` and grants **no** upgrade; and (3) derives the plan from
  Stripe via `Catalog.PlanByPriceID(session line-item price id)` with the
  server-stamped `metadata[plan]` (paid-only) as the offline fallback —
  **never** from a client cookie or form field. `RetrieveSession` expands
  `line_items` so the price id is the real purchased price. Proven by
  `TestConfirmCheckoutPlanF1`, `TestConfirmCheckoutPlanDerivesPlanFromStripe`
  and the end-to-end `TestSPAConfirmRejectsCrossTenantSession`.
- **CSRF — double-submit cookie + custom header on every mutation:**
  `internal/server/csrf.go` is applied at the group level on `/api` and
  `/admin/api`, so **no** state-changing route is exempt. A safe `GET` mints a
  readable `vmp_csrf` cookie (`SameSite=Strict`, `Secure` in production, and
  intentionally **not** `HttpOnly` so the same-origin SPA can echo it); every
  unsafe method must present a matching `X-CSRF-Token` header, compared in
  constant time, else `403`. A cross-site page can neither read the
  origin-bound cookie nor set the custom header, so it cannot forge the pair.
  `TestCSRFRejectsUnsafeRequestWithoutHeader` proves a header-less POST is
  rejected before any work. The Stripe webhook (`/webhooks/stripe`) sits
  outside `/api` and is gated by HMAC signature verification instead of CSRF —
  correct for a server-to-server call.
- **Session/cookie posture:** the customer `vmp_session` and admin `vmp_admin`
  cookies remain `HttpOnly` with `SameSite=Lax` (not `None`) so the top-level
  Stripe redirect-back keeps the user signed in; this is safe given the
  double-submit CSRF guard. Sessions stay opaque and HMAC-hashed at rest
  (carried from M4), expiry is enforced, and logout deletes the server-side
  session.
- **Admin/customer separation (fails closed):** `/admin` and `/admin/api/*`
  accept **only** the `vmp_admin` cookie (HMAC of `ADMIN_API_TOKEN`); a
  customer `vmp_session` can never reach admin data and an empty
  `ADMIN_API_TOKEN` disables admin entirely. Proven by
  `TestSPAAdminSeparateCredential`.
- **SPA static mount does not shadow APIs:** `internal/server/static.go` is
  mounted **last** and its catch-all fallback defers `/api/`, `/admin/api/`,
  `/o/`, `/webhooks/`, `/auth/`, `/healthz` and `/readyz` back to their
  handlers, so `index.html` is never served over an API/ingest/webhook route.
  The embed compiles with only `dist/.gitkeep`, so `go test` needs no UI build.
- **Per-org ingest key and Fiber hardening carried over:** the ingest edge is
  still keyed-HMAC at rest under `INGEST_KEY_PEPPER`, constant-time verified
  and `401` before any proxy hop; `fiber.Config{Immutable:true}` is set so
  persisted request values are safe; Stripe secrets stay env-only and Stripe
  errors are sanitised. React auto-escaping is intact — no
  `dangerouslySetInnerHTML` on server/user data in `ui/src`.
- **Tracked follow-up (LOW, attacker-unreachable):** `handleStripeWebhook`
  defaults the recorded plan to `team` when a validly-signed session carries
  no catalog price id and no `metadata[plan]`. Reaching this requires a
  genuine Stripe signature, so it is not exploitable by a customer; prefer
  skipping the record over a silent Team grant before entitlements depend on
  it.

## Fiber request-buffer aliasing bug class — CLOSED and GUARDED (golden rule #11)

Reviewed and passed at the Security release gate on 2026-06-10 (GATE-ALIASING).
This is the tenant-isolation bug class flagged twice from the gate (GATE-X3
org-id rewrite, GATE-PLATDEMO signup-email rewrite): Fiber's `c.Get`,
`c.FormValue`, `c.Params`, `c.Query` and `c.Cookies` return strings that alias a
pooled request buffer Fiber reuses across requests, so any such value retained
past its handler (a stamped `OrgID`, a persisted email) is silently rewritten in
place by a later request — collapsing tenant isolation. The standard enforced is
golden rule #11 in `AGENTS.md`: **build every Fiber app with
`fiber.Config{Immutable: true}` (belt) AND `strings.Clone` request-derived
strings persisted in shared library/middleware code (suspenders).** The CTO's
hardening sweep applied it across all three modules and each guard now has a
deterministic regression test that **bites** (verified by removing the guard and
observing the test fail):

- **OSS (`versus-incident`):** `cmd/main.go` builds the Fiber app with
  `Immutable: true`; `pkg/middleware/extension.go` `OrgInjector` `strings.Clone`s
  the resolved org at the `c.Locals` persistence boundary (this seam is reused by
  the enterprise binary, which cannot assume the host set `Immutable`). Guarded
  by `pkg/middleware/extension_buffer_test.go`
  (`TestOrgResolverSurvivesBufferReuse`) — built without `Immutable` so the
  clone is load-bearing; fails if the clone is removed.
- **Enterprise (`versus-enterprise`):** `cmd/versus-enterprise/main.go` builds
  the app with `Immutable: true`; both X3 clones remain present —
  `pkg/tenancy/middleware.go` `OrgResolver`
  (`strings.Clone(strings.TrimSpace(c.Get(HeaderOrg)))`) and
  `pkg/tenancy/orgstore.go` `NewOrgScoped`
  (`NormalizeOrgID(strings.Clone(org))`). Guarded by
  `pkg/tenancy/buffer_aliasing_test.go`, which drives `NewOrgScoped` and
  `OrgResolver` directly (not via the OSS `OrgInjector`, whose own clone would
  otherwise mask a missing seam guard) with two subtests that each fail if their
  respective clone is removed — confirmed to bite per-clone under `-race`.
- **Platform (`versus-management-platform`):** `internal/server/server.go`
  `New` builds the app with `Immutable: true`, the belt the control plane relies
  on (it has no per-field clone on the signup→account email path). Guarded by
  `internal/server/buffer_aliasing_test.go`
  (`TestSignupEmailNotAliasedAcrossRequests`), which drives the real app via a
  **form-encoded** signup (a JSON body would not exercise Fiber's buffer-aliasing
  accessor) and fails when `Immutable: true` is removed from `New`. The two
  `c.Body()` reads (ingest-proxy forward, Stripe-webhook verify) are consumed
  synchronously within their handlers and retain no sub-slice past return, so
  they are unaffected.

**Ruling:** the bug class is CLOSED and GUARDED across all three modules — a
future removal of any belt or clone is caught by a test. Tracked follow-ups, not
blockers now: OSS teams/members `c.Params("id")` persistence relies on the belt
only (no per-field clone); future enterprise X4 SSO subject/email and X10 source
metadata will need the same `strings.Clone` discipline when those epics land.

## Enterprise X4 SSO (OIDC Authorization Code + PKCE) — GATE-X4SSO

Reviewed and **PASSED** at the Security release gate on 2026-06-11 (GATE-X4SSO),
covering `versus-enterprise/pkg/sso` (`verify.go`, `oidc.go`, `txstore.go`,
`session.go`, `config.go`, `api.go`, `pkce.go`). Ran
`cd versus-enterprise && go test ./... -race` — green.

- **ID-token validation (`verify.go`):** `alg` pinned to `RS256` — anything else
  (notably `none` and an HS256 forgery using the RSA public key as the HMAC
  secret) is rejected before any crypto. Signature is `RSA-PKCS1v15`/SHA-256
  verified against the JWKS key addressed by the token's `kid` (unknown `kid`
  triggers one rate-limited JWKS refetch for key rotation, then fails). `iss`
  compared exactly (trailing-slash normalised), `aud` must contain the
  configured `client_id`, `exp`/`iat`/`nbf` enforced with a 2-minute skew, and a
  non-empty `email` claim is required. JWKS/discovery bodies are `LimitReader`-
  capped at 1 MiB.
- **CSRF / replay:** server-side single-use `state` (`txStore.consume` deletes on
  read, so a replayed/forged `state` → 401); `nonce` minted server-side, sent to
  the IdP and required to match the ID token's `nonce`; PKCE `code_verifier`
  (48 bytes `crypto/rand`) held only server-side, only its S256 `code_challenge`
  leaves the process. Transactions expire after 10 minutes.
- **Session binding (`session.go`):** opaque 32-byte random handle; the cookie is
  `<id>.<base64url(HMAC-SHA256(id))>` verified in constant time
  (`subtle.ConstantTimeCompare`) before any store lookup — bad-tag / unknown /
  expired are indistinguishable (no oracle). `OrgID` is stamped onto the session
  and `strings.Clone`d off the request buffer (golden rule #11). Cookie is
  `HttpOnly`, `Secure` (prod), `SameSite=Lax` (so the top-level IdP redirect
  carries it). The session pepper is auto-generated and persisted on first
  licensed boot (stable across restarts) — never a hardcoded default.
- **Per-org isolation (`config.go`):** IdP config persists through
  `tenancy.NewOrgScoped(base, org)`, namespaced per tenant so one org's IdP
  settings can never resolve for another. The callback enforces
  `secureEqual(tx.Org, org)` (constant-time) — a flow begun for org A cannot be
  completed on org B's callback. `resolveOrg` validates `tenancy.ValidOrgID` and
  active-tenant status, and `strings.Clone`s the path org.
- **Secret handling:** `client_secret` accepts an `${ENV}` reference expanded only
  at exchange time; `IdPConfig.MarshalJSON` redacts it (`***redacted***`) so it
  never leaves over the API, and the stored blob uses a separate non-redacting
  marshaller. `state`/`nonce`/`verifier`/session-handle all from `crypto/rand`;
  the org binding is compared via `secureEqual` (sha256 + `subtle`). The
  token-exchange error path never echoes the IdP response body.
- **SSRF:** the OIDC `issuer` (hence discovery URL) is **config-pinned per org**
  via the `sso:manage`-gated `PUT /:org/config`; it is never taken from request
  input at login/callback time. Discovery requires the document's own `issuer` to
  equal the configured issuer exactly, and `jwks_uri` is taken from that verified
  document — an attacker cannot redirect the JWKS fetch at request time.
- **Golden rule #11 (verified by neutralisation):** with the SSO app built
  *without* the `Immutable` belt, `SessionStore.New`'s `strings.Clone(org)` is the
  only guard. Removing it makes
  `TestBufferAliasing_SessionOrgNotRewrittenAcrossRequests` **FAIL** (org-a
  session `OrgID` rewritten to `org-b` after pooled-buffer reuse); restored, it
  **PASSES** and the full suite is green under `-race`. Confirmed this gate, then
  restored.

**Ruling:** **PASS.** Non-blocking follow-up (LOW): the per-org `issuer` is
admin-pinned but not range-restricted, so a privileged admin could point it at an
internal address — consider an egress allow/deny list (or SSRF guard on the
shared HTTP client) for the discovery/JWKS/token fetches when X4 hardens.

## Platform waitlist `/api/waitlist` (M1-T3) — GATE-WAITLIST

Reviewed at the Security release gate on 2026-06-11 (GATE-WAITLIST), covering
`versus-management-platform/internal/server/{waitlist.go,server.go,csrf.go}`, the
store layer (`internal/store/{memory.go,postgres.go,store.go}`) and migration
`internal/store/migrations/0003_waitlist.sql`.

**Verdict: BLOCK on arrival → resolved.** The build shipped with
`fiber.Config{Immutable: false}` in `server.New` — the app-wide belt the control
plane relies on (it carries no per-field clone on its persistence seams). With
it disabled, **both** golden-rule-#11 regression tests failed as committed:
`TestWaitlistEmailNotAliasedAcrossRequests` (re-add of `emailA` returned `<nil>`
instead of `ErrConflict` — the stored email had been rewritten to a later
request's value) and `TestSignupEmailNotAliasedAcrossRequests` (account A's
persisted email rewritten to `eeee@versus.io`). This is a live production
data-integrity / tenant-data-corruption defect on the form-encoded write path and
a regression of the GATE-ALIASING ruling above. **Fix applied at the gate:**
restored `Immutable: true` in `internal/server/server.go`. After the fix,
`go test ./... -race` is green across the module (both biting tests PASS).
Routed back to the Platform Engineer: do not ship `server.New` with the belt off.

With the belt restored, the rest of the surface is sound:

- **CSRF:** the route lives inside the `/api` group whose `csrfGuard` is applied
  via `api.Use` (double-submit `vmp_csrf` cookie + `X-CSRF-Token` header,
  constant-time compare, `SameSite=Strict`). A public *unauthenticated* form
  inside the CSRF group is the correct posture — it blocks cross-site forced
  submissions while still allowing the same-origin SPA to post.
  `TestWaitlistRequiresCSRF` proves a token-less POST → 403 before any work.
- **Rate limit:** `waitlistLimiter()` (per-IP, 5/min, in-memory) is wired as
  route middleware *before* `handleWaitlist`, inside the CSRF group, so the order
  is csrfGuard → limiter → handler. `TestWaitlistRateLimited` proves the 6th
  same-IP POST → 429. **Follow-up (recorded, acceptable for now):** the key is
  `c.IP()` with no trusted-proxy config, so behind a reverse proxy/LB all clients
  may share the proxy IP (over-throttle) or, if `ProxyHeader`/`X-Forwarded-For`
  is later trusted naively, be spoofable — set Fiber's trusted-proxy config and
  derive the client IP from it before this is load-bearing in production.
- **Input validation:** `net/mail.ParseAddress` with `addr.Address == email`
  (rejects `Name <addr>` display-name smuggling), trim+lower normalisation, a
  254-octet cap (RFC 5321) ahead of the 4 MiB app `BodyLimit`, and dedupe on the
  normalized form → `ErrConflict` → `"already"`. `TestWaitlistRejectsInvalidEmail`
  / `…QueuesNewEmail` / `…DuplicateReportsAlready` cover these.
- **PII / enumeration:** the email is never logged anywhere on the path; the
  response contract is exactly `queued` / `already` (the address is only echoed
  back to the same submitter). The migration stores only `email/source/
  created_at` — no secret, no account linkage.
- **SQL injection:** the Postgres writer is fully parameterised
  (`INSERT … VALUES (lower($1), $2, $3)`) with a defence-in-depth
  `UNIQUE INDEX … (lower(email))`; no string concatenation.
- **Buffer safety:** the persisted email comes from `c.BodyParser` (JSON
  allocates fresh strings; the form path is covered by the restored belt).
  Nothing from `c.Get`/`c.IP`/`c.Params` is persisted — `c.IP()` is used only as
  an in-memory limiter map key for the request's lifetime (the `waitlist.go`
  comment crediting the belt for `c.IP()` safety is imprecise but harmless:
  default `c.IP()` returns a freshly-allocated `RemoteIP().String()`).

**Ruling:** **PASS once `Immutable: true` is restored** (done at the gate).
Follow-ups (non-blocking): trusted-proxy/`X-Forwarded-For` handling for the
rate-limit key; correct the `c.IP()` belt comment in `waitlist.go`.
