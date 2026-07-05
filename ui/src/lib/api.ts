// Centralized API client for the agent admin endpoints.
//
// All requests are authenticated with the X-Gateway-Secret header. The secret
// is read from localStorage; AuthGate prompts for it on first visit.

import type { LearnExclusions } from "@/lib/learnExclude";

// LearnExclusionsWire is the raw enterprise learn-exclusion policy shape ON THE
// WIRE. It differs from the UI's LearnExclusions in ONE field name: the
// per-log-pattern grain is `log_patterns` on the wire (the UI models it as
// `patterns`). The api client maps across this seam so a caller never has to
// know the wire name — and never silently reads the wrong field, which is what
// left an ignored log pattern stranded in the Active tab.
interface LearnExclusionsWire {
  services?: string[];
  metrics?: string[];
  log_patterns?: string[];
}

const SECRET_KEY = "versus.gatewaySecret";
const API_BASE = import.meta.env.VITE_API_BASE_URL || ""; // empty → uses Vite proxy

export class ApiError extends Error {
  status: number;
  body?: unknown;
  constructor(status: number, message: string, body?: unknown) {
    super(message);
    this.status = status;
    this.body = body;
  }
}

export function getSecret(): string | null {
  return localStorage.getItem(SECRET_KEY);
}

export function setSecret(value: string) {
  localStorage.setItem(SECRET_KEY, value);
}

export function clearSecret() {
  localStorage.removeItem(SECRET_KEY);
}

// signIn verifies the gateway secret against the data plane and persists it.
// The gateway secret is the read-only machine/data-plane credential (the OSS
// path and the enterprise read path). Privileged enterprise management is no
// longer unlocked by a static token here — it rides the SSO session (the RBAC
// admin user). Throws ApiError(401) when the secret is rejected; other errors
// propagate unchanged.
//
// We verify against /api/admin/config/agent (getAgentConfig), NOT
// /api/agent/status: the config endpoint is always mounted and gateway-secret
// gated, whereas the agent status route only exists when agent.enable=true.
// Verifying against status coupled sign-in to the agent being on, so an
// alert-router deployment with a gateway secret but no agent could not log in.
export async function signIn(value: string): Promise<void> {
  setSecret(value.trim());
  await api.getAgentConfig();
}

// AUTH_EXPIRED_EVENT fires when a request that carried a secret comes back
// 401 — i.e. the secret was rotated server-side mid-session. AppShell's
// ReauthModal listens and re-prompts over the current page instead of
// letting every view collapse into bare "HTTP 401" walls (audit finding).
export const AUTH_EXPIRED_EVENT = "versus:auth-expired";

function notifyAuthExpired() {
  window.dispatchEvent(new Event(AUTH_EXPIRED_EVENT));
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const secret = getSecret() ?? "";
  const headers = new Headers(init.headers);
  // OSS/community authenticates the data plane with the gateway secret; the
  // enterprise console authenticates with the HttpOnly session cookie
  // (versus_enterprise_session) carried via credentials: same-origin and holds
  // no secret. Attach the header ONLY when a secret is actually held, so a
  // licensed binary never sends X-Gateway-Secret — session-only, no fallback.
  if (secret) headers.set("X-Gateway-Secret", secret);
  if (init.body && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }

  // credentials: "same-origin" so an established session cookie
  // (versus_enterprise_session) rides along and authenticates the data plane
  // on the enterprise path (built-in default admin or SSO).
  //
  // cache: "no-store" — the admin surfaces show live agent state (patterns,
  // shadow/detect events, incidents). Without it the browser HTTP cache can
  // hand back a stale GET body on reload, so an operator hits F5 and sees old
  // numbers. no-store forces every request to the network for fresh data.
  const res = await fetch(`${API_BASE}${path}`, {
    ...init,
    headers,
    credentials: "same-origin",
    cache: "no-store",
  });

  if (res.status === 204) return undefined as T;

  let body: unknown = null;
  const text = await res.text();
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      body = text;
    }
  }
  if (!res.ok) {
    if (res.status === 401 && secret) notifyAuthExpired();
    const msg =
      (body && typeof body === "object" && "error" in body
        ? String((body as { error: unknown }).error)
        : null) || `HTTP ${res.status}`;
    throw new ApiError(res.status, msg, body);
  }
  return body as T;
}

// ---------- Types matching pkg/agent shapes ----------

// Readiness mirrors the OSS core.Readiness shape — how close a signal is to its
// settled/known state. It is present on every pattern and baseline row (logs
// always; metrics/traces only where the enterprise brain runs). Presentation
// (remaining, ETA, progress bar) is DERIVED by the UI from these facts — see
// lib/readiness.ts. Sentinels: needed === 0 ⇒ indeterminate (no count gate,
// e.g. logs with auto-promotion disabled); rate_per_min === 0 ⇒ no honest ETA
// (no rate yet / stalled / already ready).
export interface Readiness {
  ready: boolean;
  seen: number;
  needed: number; // 0 ⇒ indeterminate (manual-only promotion)
  rate_per_min: number; // 0 ⇒ unknown/stalled ⇒ no ETA
}

export interface Pattern {
  id: string;
  template: string;
  first_seen: string;
  last_seen: string;
  count: number;
  baseline_frequency: number;
  verdict: string; // "" | "known" | operator-set
  rule_name: string;
  source: string;
  service?: string;
  tags?: string[];
  readiness: Readiness; // learning-readiness / time-to-known (always present)
  // samples is the bounded ring of the most recent POST-REDACTION example log
  // lines this pattern was learned from (oldest→newest, latest last). Present
  // only on the pattern detail read (getPattern) — the list rows strip it.
  samples?: string[];
}

export interface Status {
  patterns: number;
  dirty: boolean;
  shadow_events?: number;
  shadow_dirty?: boolean;
  detect_events?: number;
  detect_dirty?: boolean;
  runbooks_available?: boolean;
}

// BaselineRow mirrors the enterprise pkg/intel BaselineRow — one learned
// metric or trace signal as the Metrics / Traces views render it. The endpoint
// is Enterprise-gated (403 without an `intelligence` license; absent entirely
// on an OSS binary) — the page renders the locked upsell state in that case.
// The server carries the display `unit` plus already-converted `display_mean`
// /`display_std`, so the UI formats numbers but never converts a wire unit.
export interface BaselineRow {
  type: "metric" | "trace";
  source: string; // "prometheus" | "traces"
  service: string;
  signal: string;
  operation?: string; // trace rows only
  kind: string; // traffic | errors | latency | saturation | other
  expected_mean: number; // raw learned value, in the wire unit
  expected_std: number;
  unit: string; // display unit: "req/s" | "ms" | "%" | "" (raw)
  display_mean: number; // expected_mean converted into `unit`
  display_std: number; // expected_std converted into `unit`
  confident: boolean; // still-learning (false) vs ready-to-detect (true)
  observations: number; // samples folded so far
  threshold: number; // samples needed before the signal is ready
  last_updated: string;
  readiness: Readiness; // same shape as logs; ready === confident
  // latest_sample is the most recent POST-REDACTION compact example this
  // signal was learned from — the metric/trace parity of the log pattern's
  // "Example log line". The peek renders it as "Example metric" / "Example
  // trace". Enterprise-gated with the rest of the row: absent (omitempty) on a
  // community/OSS build or until the signal has folded at least one sample, so
  // the peek degrades to "No example captured yet".
  latest_sample?: string;
}

export interface BaselinesResponse {
  org: string;
  count: number;
  baselines: BaselineRow[];
}

// --- SLI/SLO auto-define (epic X29) -----------------------------------------
// The "SLO Advisor" recommends SLIs/SLOs per service. The read endpoint is
// Enterprise-gated (403 without an `intelligence` license; absent on an OSS
// binary) and carries an AI-gate status so the page can show a clear OFF
// reason when AI is disabled. Advisory only — adopting an objective is a human
// action; the page never mutates cluster state.

export interface SLORecommendationSLI {
  name: string;
  type: string; // availability | latency | error_rate | throughput | saturation
  signal: string;
  objective: number; // ratio in (0,1), or a latency target in ms
  window_days: number;
  rationale: string;
  confidence: number; // 0..1
}

export interface SLORecommendation {
  service: string;
  generated_at: string;
  version: number;
  run_id?: string;
  model?: string;
  prompt_hash?: string;
  summary: string;
  slis: SLORecommendationSLI[];
}

export interface SLOGateStatus {
  enabled: boolean; // the AI hard gate is OPEN
  off_reason?: string; // the clear reason when the gate is CLOSED
}

export interface SLORecommendationsResponse {
  org: string;
  count: number;
  recommendations: SLORecommendation[];
  status: SLOGateStatus;
}

export interface SLOAutodefineConfig {
  cadence: string; // a Go duration string, e.g. "24h0m0s"
  enabled: boolean; // the per-org feature toggle (DISTINCT from status.enabled)
  updated_at?: string;
  updated_by?: string;
  min_cadence: string;
  status: SLOGateStatus; // the AI hard gate; status.enabled gates the toggle
}

export interface ShadowEvent {
  pattern_id: string;
  template: string;
  source: string;
  service?: string; // attributed service (may be blank/_unknown)
  rule_name?: string;
  verdict: string; // "unknown" | "spike"
  sample_message: string;
  count: number;
  occurrences: number;
  first_seen: string;
  last_seen: string;
}

export interface ShadowStats {
  events: number;
  total_signals: number;
  verdicts: Record<string, number>;
  occurrences: number;
}

export interface ServiceInfo {
  first_seen: string;
  // manual distinguishes an operator-created service (true — selectable as an
  // override target, renameable/deletable through the admin API) from an
  // auto-discovered one (false). The server always sends it, so the Services
  // table renders an explicit "Manual"/"Auto" origin for every row.
  manual: boolean;
  // in_grace + grace_seconds_remaining are the new-service grace status the
  // server computes with the SAME helper the service-detail endpoint uses, so
  // the Services LIST and the service DETAIL page report the same status. A
  // service inside its grace window is learned-but-not-alerted; the list shows
  // "in grace" and the remaining time, else "tracked" and "—".
  in_grace: boolean;
  grace_seconds_remaining: number;
}

// --- Manual-attribution service overrides ------------------------------------
// One durable operator correction that re-labels a mis-attributed signal's
// service. Logs override is an OSS capability; metric/trace rules ride the SAME
// endpoint but only take effect where the enterprise metric/trace brains run.

export type ServiceOverrideSource = "log" | "metric" | "trace";

export interface ServiceOverride {
  id: string;
  source_type: ServiceOverrideSource;
  // match is the source-appropriate key: a log pattern id / message substring,
  // or a metric/trace signal name (exact or `*`/`?` glob).
  match: string;
  service: string;
  created_at: string;
}


// --- Service detail (X30) ----------------------------------------------------
// The OSS half of the service-detail surface: service meta + grace, the
// log-pattern catalog scoped to the service, and a bounded incident summary.
// It carries NO metrics/traces fields — those ride the Enterprise /intel
// endpoint (ServiceIntel) and the page renders them separately.

export interface ServicePattern {
  id: string;
  template: string;
  count: number;
  verdict: string; // "" | "known" | operator-set
  source: string;
  last_seen: string;
  tags?: string[];
}

export interface ServiceIncidentRecent {
  id: string;
  title?: string;
  severity: string;
  created_at: string;
}

export interface ServiceIncidentSummary {
  window_days: number;
  count: number;
  severities: Record<string, number>;
  recent: ServiceIncidentRecent[];
}

export interface ServiceDetail {
  service: string;
  first_seen: string;
  in_grace: boolean;
  grace_seconds_remaining: number;
  patterns: ServicePattern[];
  incidents: ServiceIncidentSummary;
  counts: { patterns: number; incidents: number };
}

// ServiceIntel is the Enterprise metrics/traces half of the service-detail
// surface (X30-T2). The endpoint is Enterprise-gated (403 unlicensed) and
// absent on an OSS binary (404) — the page renders the locked upsell in that
// case, driven purely by HTTP status. No enterprise dependency lives in the OSS
// UI; the shape reuses the OSS-local BaselineRow type.
export interface ServiceIntel {
  org?: string;
  service: string;
  metrics?: BaselineRow[];
  traces?: BaselineRow[];
}

// AIFinding is the structured response parsed out of the model's JSON.
export interface AIFinding {
  Title?: string;
  Summary?: string;
  Severity?: string; // critical | high | medium | low
  Category?: string;
  Confidence?: number; // 0..1
  Suggestions?: string[];
  SampleIDs?: string[];
}

// DetectEvent mirrors pkg/agent.DetectEvent — the audit record for one
// detect-mode handling of a pattern.
export interface DetectEvent {
  id: string;
  timestamp: string;
  source: string;
  pattern_id: string;
  template: string;
  service?: string;
  verdict: string; // unknown | spike | known
  frequency: number;
  baseline: number;
  samples?: string[];
  model?: string;
  user_prompt?: string;
  raw_response?: string;
  duration_ms?: number;
  finding?: AIFinding | null;
  outcome: string; // emitted | cached | dry | quota | ai_error | send_error
  error?: string;
}

// DetectStats is a flat map: keys include `events`, `outcome_<name>`,
// `verdict_<name>`, `severity_<name>`.
export type DetectStats = Record<string, number>;

// Incident shapes — list responses are summaries (no Content blob); the
// detail endpoint returns the full payload.
export interface IncidentSummary {
  id: string;
  team_id?: string;
  title?: string;
  source?: string;
  // origin is the coarse classifier for how the incident entered the
  // system: "ai_detect" (AI detect agent) or "webhook" (inbound alert).
  // The Incidents page separates the two feeds on it. Always present on
  // fresh responses; legacy rows are classified server-side from source.
  origin?: string;
  service?: string;
  resolved: boolean;
  channels_notified?: string[];
  oncall_triggered?: boolean;
  notify_status?: "pending" | "sent" | "failed" | string;
  notify_error?: string;
  created_at: string;
  acked_at?: string | null;
  resolved_at?: string | null;
  assigned_team_id?: string;
  assigned_member_ids?: string[];
}

// OriginCounts is the per-origin tally the list/search endpoints return
// alongside the rows, computed over the FULL result set so the Incidents
// top-bar can show both feeds ("AI: N · Webhook: M") regardless of the
// active tab. total is ai_detect + webhook.
export interface OriginCounts {
  ai_detect: number;
  webhook: number;
  total: number;
}

// IncidentIndex is the full list/search response: one (optionally
// origin-filtered, optionally paginated) window of rows plus the
// whole-set origin counts. `total` is the number of rows matching the
// active origin filter before pagination.
export interface IncidentIndex {
  incidents: IncidentSummary[];
  counts: OriginCounts;
  total: number;
  page?: number;
  page_size?: number;
}

export interface IncidentDetail extends IncidentSummary {
  content?: Record<string, unknown>;
}

// AnalysisRecord mirrors pkg/storage.AnalysisRecord. The analyze
// agent's upper-block fields (Title/Summary/Severity/...) ship
// PascalCase because pkg/core.AIFinding declares them without json
// tags; analyze-only fields use snake_case via explicit tags.
export interface RootCauseHypothesis {
  hypothesis: string;
  confidence: number;
  rationale?: string;
}

export interface EvidenceItem {
  source: string;
  summary: string;
  detail?: string;
}

export interface AIFinding {
  Title?: string;
  Summary?: string;
  Severity?: string;
  Category?: string;
  Confidence?: number;
  Suggestions?: string[];
  SampleIDs?: string[];
  root_cause_hypotheses?: RootCauseHypothesis[];
  evidence?: EvidenceItem[];
  related_pattern_ids?: string[];
  next_steps?: string[];
}

export interface AnalysisToolCall {
  name: string;
  args?: unknown;
  output?: unknown;
  duration_ms?: number;
  error?: string;
}

export interface AnalysisRecord {
  id: string;
  incident_id: string;
  requested_at: string;
  requested_by?: string;
  duration_ms?: number;
  model?: string;
  tool_calls?: AnalysisToolCall[];
  finding?: AIFinding;
  raw_response?: string;
  status: "ok" | "error" | "rate_limited" | string;
  error?: string;
}

// ---------- Team / member management ----------

// MemberMeta mirrors pkg/teams.MemberMeta — typed per-channel ids.
export interface MemberMeta {
  email?: string;
  slack_id?: string;
  telegram_id?: string;
  msteams_upn?: string;
  viber_id?: string;
  lark_id?: string;
  pagerduty_user_id?: string;
  awsim_contact_arn?: string;
  phone?: string;
}

export interface Member {
  id: string;
  name: string;
  alias: string;
  meta: MemberMeta;
  created_at: string;
  updated_at: string;
}

export interface Team {
  id: string;
  name: string;
  alias: string;
  description?: string;
  member_ids: string[];
  created_at: string;
  updated_at: string;
}

// MemberInput / TeamInput are the bodies sent on create/update.
// `meta`/`member_ids` use `null` to mean "field omitted" (leave alone).
export interface MemberInput {
  name?: string;
  alias?: string;
  meta?: MemberMeta | null;
}

export interface TeamInput {
  name?: string;
  alias?: string;
  description?: string;
  member_ids?: string[] | null;
}

// Runbook is the metadata shape returned by the list endpoint (no body,
// no embedding vector). `has_vector` is false until the runbook has been
// embedded (requires an embedding model to be configured).
export interface Runbook {
  id: string;
  title: string;
  services?: string[];
  tags?: string[];
  source?: string;
  updated_at: string;
  has_vector: boolean;
}

// RunbookDetail adds the full markdown body for the single-runbook view.
export interface RunbookDetail extends Runbook {
  body: string;
}

export interface RunbookUploadResult {
  ingested: number;
  embeddings: boolean;
}

// uploadMultipart posts a multipart/form-data body. Unlike `request`, it
// must NOT set a JSON Content-Type — the browser sets the multipart
// boundary itself from the FormData.
async function uploadMultipart<T>(path: string, form: FormData): Promise<T> {
  const secret = getSecret() ?? "";
  const headers = new Headers();
  // Attach the gateway secret ONLY when one is held (OSS/community). The
  // enterprise console holds no secret and authenticates with the session
  // cookie via credentials: same-origin.
  if (secret) headers.set("X-Gateway-Secret", secret);

  const res = await fetch(`${API_BASE}${path}`, {
    method: "POST",
    headers,
    body: form,
    credentials: "same-origin",
  });

  if (res.status === 204) return undefined as T;

  let body: unknown = null;
  const text = await res.text();
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      body = text;
    }
  }
  if (!res.ok) {
    if (res.status === 401 && secret) notifyAuthExpired();
    const msg =
      (body && typeof body === "object" && "error" in body
        ? String((body as { error: unknown }).error)
        : null) || `HTTP ${res.status}`;
    throw new ApiError(res.status, msg, body);
  }
  return body as T;
}

// sessionRequest authenticates the privileged enterprise control plane with
// the SSO session cookie (versus_enterprise_session) instead of a static
// token. The cookie is HttpOnly, so it is sent automatically with
// credentials: "same-origin"; no secret header is attached. The org and the
// caller's RBAC role are derived server-side from the session, so the surface
// is gated by the RBAC permission (sso:manage / runtime:manage / roles:manage
// / audit:view) — fail-closed (401 no session, 403 insufficient role). Unlike
// `request` it does NOT dispatch AUTH_EXPIRED_EVENT (the gateway-secret modal
// must not hijack the SSO surface); the control renders the sign-in / role
// hint itself off the status.
async function sessionRequest<T>(
  path: string,
  init: RequestInit = {},
): Promise<T> {
  const headers = new Headers(init.headers);
  if (init.body && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }

  const res = await fetch(`${API_BASE}${path}`, {
    ...init,
    headers,
    credentials: "same-origin",
  });

  if (res.status === 204) return undefined as T;

  let body: unknown = null;
  const text = await res.text();
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      body = text;
    }
  }
  if (!res.ok) {
    const msg =
      (body && typeof body === "object" && "error" in body
        ? String((body as { error: unknown }).error)
        : null) || `HTTP ${res.status}`;
    throw new ApiError(res.status, msg, body);
  }
  return body as T;
}

// ---------- Runtime mode override (Enterprise, RBAC runtime:manage) ----------

export type AgentMode = "training" | "shadow" | "detect";

// AgentModeView mirrors the enterprise pkg/runtimemode handler shape. The
// endpoint is Enterprise-gated and authorized by the caller's RBAC role carried
// by the SSO session (runtime:manage) — the SPA gates upfront on the role, so
// these are terminal "not allowed" answers, not token prompts:
//   403 — community / unlicensed, or a viewer/responder session → upsell / role notice
//   404 — OSS binary (route absent)     → render the locked upsell
//   503 — guard not wired server-side   → treated as "not enterprise"
//   401 — no SSO session                → ask the caller to sign in
export interface AgentModeView {
  effective: AgentMode;
  yaml: AgentMode;
  override: AgentMode | ""; // "" when no override is set
  source: "override" | "yaml";
}

// ---------- Runtime AI settings (Enterprise, RBAC runtime:manage) ----------

// AISettingsView mirrors the enterprise pkg/runtimeai masked GET/PUT shape. It
// is MASKED by contract — the server NEVER returns the API key, only whether
// one is set (`key_set`) plus its last four chars (`last4`). `enabled` is the
// EFFECTIVE enable (override when set, else the YAML floor); `source` is
// "override" or "yaml"; `yaml_enabled` is the YAML `ai.enable` floor.
//
// The endpoint is Enterprise-gated and authorized by the SSO session's RBAC
// role (runtime:manage), same status surface as the mode control:
//   403 — community / unlicensed, or a viewer/responder session → upsell / role notice
//   404 — OSS binary (route absent)     → render the locked upsell
//   503 — guard not wired server-side   → treated as "not enterprise"
//   401 — no SSO session                → ask the caller to sign in
//   422 — `no_encryption_key` on a key write → server master key not set
export interface AISettingsView {
  enabled: boolean;
  provider: string;
  key_set: boolean;
  last4: string;
  yaml_enabled: boolean;
  source: "override" | "yaml";
}

// AIProvider is the closed set of model backends the runtime override accepts.
// It mirrors the OSS chat-model registry (eino.SupportedProviders); the server
// re-validates on WRITE and rejects an unknown value with 400, so this list is
// the UI's first line of defence, not the authority.
export type AIProvider =
  | "openai"
  | "deepseek"
  | "qwen"
  | "ollama"
  | "claude"
  | "gemini";

export const AI_PROVIDERS: AIProvider[] = [
  "openai",
  "deepseek",
  "qwen",
  "ollama",
  "claude",
  "gemini",
];

// AISettingsInput is the PUT body. `api_key` and `provider` are both OPTIONAL:
// omit/blank either to leave the stored value untouched while toggling
// `enabled` (the stored key + provider persist). Never persisted client-side —
// held transiently for the single PUT, then cleared.
export interface AISettingsInput {
  enabled: boolean;
  provider?: string;
  api_key?: string;
}

// ---------- Runtime notification-channel settings (Enterprise, RBAC runtime:manage) ----------

// ChannelMaskedField is one field's masked view. It is MASKED by contract — a
// secret field NEVER carries a raw value, only whether one is `set` plus a
// `hint` (last-4 for tokens, scheme+host for webhook URLs). A non-secret field
// echoes its value in `hint`.
export interface ChannelMaskedField {
  set: boolean;
  hint: string;
}

// ChannelSettingsView mirrors the enterprise pkg/runtimechannels masked GET/PUT
// shape for ONE channel. `enabled` is the EFFECTIVE enable (override when set,
// else the YAML floor); `configured` is whether a runtime override exists;
// `source` is "override" or "yaml"; `yaml_enabled` is the YAML floor. `fields`
// is the per-field masked view. NO secret value is ever present.
export interface ChannelSettingsView {
  enabled: boolean;
  configured: boolean;
  source: "override" | "yaml";
  yaml_enabled: boolean;
  fields: Record<string, ChannelMaskedField>;
}

// ChannelSettingsMap is the masked view of all six channels, keyed by channel
// name (slack | telegram | viber | email | msteams | lark).
export type ChannelSettingsMap = Record<string, ChannelSettingsView>;

// ChannelFieldSchema is an optional server-provided per-field descriptor
// (forward-compat). The UI falls back to its static schema when absent.
export type ChannelFieldSchema = Record<string, { secret: boolean }>;

// ChannelSettingsInput is the PUT body for one channel. A secret field is
// OMITTED when blank so the server preserves the stored value (write-only); a
// bool field is a real JSON boolean. Never persisted client-side — held
// transiently for the single PUT, then cleared.
export interface ChannelSettingsInput {
  enable: boolean;
  fields: Record<string, string | boolean>;
}

// ---------- Disable-Learn exclusions (Enterprise, RBAC runtime:manage) ----------

// LearnExclusionsView is the org's Disable-Learn policy as returned by GET
// /enterprise/api/agent/learn-exclusions: `services` are exact service names
// fully excluded from learning; `metrics` are signal entries that are exact
// names AND glob/prefix patterns (e.g. "up", "go_*", "prometheus_*"). Both are
// always present (possibly empty). It doubles as the PUT input (whole-list
// replace). The endpoint is Enterprise-gated and RBAC runtime:manage-guarded
// (401 no session / 403 wrong role / 403 community / 404 OSS binary). The
// canonical shape lives in lib/learnExclude (pure, where the matcher gate
// consumes it); re-exported here so it sits with the other API view/input types.
export type { LearnExclusions as LearnExclusionsView } from "@/lib/learnExclude";

// ---------- Enterprise multi-IdP connections (Keycloak-style, admin-gated) ----------

export type SSOConnectionType = "google" | "azure" | "oidc";

// SSOConnectionView mirrors the enterprise pkg/sso MaskedConnection. MASKED by
// contract — the server NEVER returns the client secret, only whether one is
// set and its last-4 hint. `issuer` is the RESOLVED issuer (derived for
// google/azure, explicit for oidc) so the UI shows where logins go.
export interface SSOConnectionView {
  id: string;
  type: SSOConnectionType;
  display_name: string;
  enabled: boolean;
  client_id: string;
  client_secret_set: boolean;
  client_secret_last4?: string;
  redirect_url: string;
  scopes?: string[];
  allowed_domains: string[];
  azure_tenant?: string;
  issuer: string;
}

export interface SSOConnectionsEnvelope {
  org: string;
  connections: SSOConnectionView[];
}

export interface SSOConnectionEnvelope {
  org: string;
  connection: SSOConnectionView;
}

// SSOConnectionInput is the PUT body for one connection. `client_secret` is
// OPTIONAL: omit/blank to update the non-secret fields without re-sealing the
// stored secret. For google/azure the issuer is derived server-side; for oidc
// supply `issuer`. For azure, `azure_tenant` selects the directory (blank ⇒
// the multi-tenant `common` authority).
export interface SSOConnectionInput {
  type: SSOConnectionType;
  display_name: string;
  enabled: boolean;
  client_id: string;
  client_secret?: string;
  redirect_url: string;
  scopes: string[];
  allowed_domains: string[];
  azure_tenant?: string;
  issuer?: string;
}

// ---------- Enterprise SSO enforcement policy (X4-T4, RBAC sso:manage, per-org) ----------

// SSOPolicyView mirrors the enterprise pkg/sso PolicyView. `require_sso`
// enforces single sign-on for human access to the org: human users sign in
// through a configured IdP (the built-in default admin stays available as a
// break-glass account; the gateway secret is OSS machine/data-plane only and is
// never a human login on a licensed binary). `require_mfa` (only meaningful
// with `require_sso`) is the LIVE multi-factor gate — it additionally rejects
// any SSO login the IdP did not report as multi-factor. The policy + config
// endpoints are authorized by the caller's SSO-session RBAC role (sso:manage),
// so an admin can always lift a misconfigured policy from a live session.
export interface SSOPolicyView {
  require_sso: boolean;
  require_mfa: boolean;
  updated_at?: string;
  by?: string;
}

// SSOPolicyEnvelope is the GET / PUT response wrapper.
export interface SSOPolicyEnvelope {
  org: string;
  policy: SSOPolicyView;
}

// SSOPolicyInput is the PUT body. The server refuses `require_sso` for an org
// with no enabled IdP config (422 `sso_not_configured`) so an admin can't lock
// the org out.
export interface SSOPolicyInput {
  require_sso: boolean;
  require_mfa: boolean;
}

// SSOStatus is the PUBLIC, unauthenticated login-screen probe (no gateway
// secret). It carries NOTHING sensitive — only whether SSO is
// enabled for the org and the enabled IdP connections to render a login button
// for. Any error (OSS binary with the route absent → 404, community mode →
// 403, network) is swallowed to { enabled: false } so the login screen simply
// omits the SSO buttons.
export interface SSOStatus {
  enabled: boolean;
  // require_sso is set when the org ENFORCES SSO for human sign-in. The login
  // screen then offers the IdP button(s) as the way in (on OSS it also drops
  // the gateway-secret fallback; the gateway secret is never a human login on a
  // licensed binary).
  require_sso?: boolean;
  // connections lists the enabled multi-IdP connections: the login screen
  // renders one "Sign in with <display_name>" button per entry. SSO is
  // configured and logged in SOLELY through these — there is no single-config
  // fallback, so an empty/absent list means no SSO button is shown.
  connections?: SSOStatusConnection[];
}

// SSOStatusConnection is one enabled IdP connection on the public login probe:
// nothing sensitive, just enough to render and start one login button.
export interface SSOStatusConnection {
  id: string;
  type: "google" | "azure" | "oidc";
  display_name: string;
  login_url: string;
}

// SSODeployment is the pre-auth deployment-org probe. The single-tenant binary
// serves SSO under ONE org sourced from the LICENSE_KEY (not a hardcoded
// "default"); the UI reads it here — without a session — to
// drive SSO status/config/connections under that org. The endpoint is inside
// the enterprise license gate, so a community/unlicensed binary returns 403
// (the UI treats that as "not enterprise" and offers no SSO).
export interface SSODeployment {
  org: string;
}

// getSSODeployment reads the single-tenant deployment org (license-issued).
// Plain GET, no gateway secret — the endpoint exposes only the
// non-secret org id. THROWS ApiError on a non-2xx response (notably 403 in
// community mode), so the caller can fall back to the non-SSO path.
export async function getSSODeployment(): Promise<SSODeployment> {
  const res = await fetch(`${API_BASE}/enterprise/api/sso/deployment`);
  if (!res.ok) {
    throw new ApiError(res.status, `HTTP ${res.status}`, null);
  }
  return (await res.json()) as SSODeployment;
}

// getSsoStatus probes whether an org offers SSO, for the login screen's
// "Sign in with <provider>" button. Deliberately uses a bare fetch (no
// X-Gateway-Secret): the endpoint is public within the
// enterprise license gate. Never throws — failures collapse to disabled.
export async function getSsoStatus(org: string): Promise<SSOStatus> {
  try {
    const res = await fetch(
      `${API_BASE}/enterprise/api/sso/${encodeURIComponent(org)}/status`,
    );
    if (!res.ok) return { enabled: false };
    const body = (await res.json()) as SSOStatus;
    return body?.enabled ? body : { enabled: false };
  } catch {
    return { enabled: false };
  }
}

// ssoLogout revokes the caller's SSO session and clears the cookie. The
// session cookie is sent automatically (same-origin); no secret is involved.
// Best-effort — a failure still lets the UI fall back to clearing local state.
export async function ssoLogout(org: string): Promise<void> {
  try {
    await fetch(
      `${API_BASE}/enterprise/api/sso/${encodeURIComponent(org)}/logout`,
      { method: "POST", credentials: "same-origin" },
    );
  } catch {
    // ignore — logout is best-effort
  }
}

// LocalLoginResult is the non-secret identity the built-in default-admin login
// returns on success: the org-bound owner session it just minted.
export interface LocalLoginResult {
  org: string;
  subject: string;
  role: string;
}

// localLogin signs the built-in default admin in against the licensed local
// login route (POST /enterprise/api/auth/local/login). It carries NO gateway
// secret — the route sets the same HttpOnly enterprise session cookie an SSO
// login does (credentials: "same-origin" so the Set-Cookie is honoured). It
// THROWS ApiError so the form can branch on the status: 401 is a GENERIC
// invalid-credentials answer (the server never distinguishes wrong-password
// from disabled — no enumeration), 429 is the lockout, 403 is a
// community/unlicensed binary.
export async function localLogin(
  username: string,
  password: string,
): Promise<LocalLoginResult> {
  const res = await fetch(`${API_BASE}/enterprise/api/auth/local/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username, password }),
    credentials: "same-origin",
  });
  let body: unknown = null;
  const text = await res.text();
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      body = text;
    }
  }
  if (!res.ok) {
    const msg =
      (body && typeof body === "object" && "error" in body
        ? String((body as { error: unknown }).error)
        : null) || `HTTP ${res.status}`;
    throw new ApiError(res.status, msg, body);
  }
  return body as LocalLoginResult;
}

// localLogout revokes the caller's built-in-admin session and clears the cookie
// via the local logout route (POST /enterprise/api/auth/local/logout). Like
// ssoLogout it is best-effort and carries no secret — the session cookie is
// sent automatically (same-origin).
export async function localLogout(): Promise<void> {
  try {
    await fetch(`${API_BASE}/enterprise/api/auth/local/logout`, {
      method: "POST",
      credentials: "same-origin",
    });
  } catch {
    // ignore — logout is best-effort
  }
}

// SSOSession is the caller's whoami: the non-secret identity of the current
// established SSO session bound to the deployment org. The HttpOnly session
// cookie is sent automatically (same-origin); no secret is involved.
export interface SSOSession {
  org: string;
  email: string;
  subject: string;
  mfa: boolean;
  amr?: string[];
  // local marks a built-in default-admin (non-SSO) session. The sign-out
  // affordance uses it to revoke via the local-admin logout route instead of
  // the SSO one. Absent/false for an SSO session.
  local?: boolean;
  // role is the caller's EFFECTIVE RBAC role in the deployment org (viewer /
  // responder / admin / owner). The SPA gates privileged controls on it — only
  // admin/owner may manage; everyone else is read-only. "" / absent when the
  // server could not resolve a role (treated as least-privileged).
  role?: string;
  issued_at: string;
  expires_at: string;
}

// getSsoSession probes whether the browser holds a live SSO session for org.
// The versus_enterprise_session cookie is HttpOnly, so it is sent automatically
// with credentials: "same-origin"; the call carries no gateway secret or admin
// token. THROWS ApiError(401) when there is no active session, and any other
// non-2xx, so AuthGate can fall back to the gateway-secret screen.
export async function getSsoSession(org: string): Promise<SSOSession> {
  const res = await fetch(
    `${API_BASE}/enterprise/api/sso/${encodeURIComponent(org)}/session`,
    { credentials: "same-origin" },
  );
  if (!res.ok) {
    throw new ApiError(res.status, `HTTP ${res.status}`, null);
  }
  return (await res.json()) as SSOSession;
}

// ---------- Enterprise RBAC members + default-admin (roles:manage, per-org) ----------

// MemberRole is the set of assignable RBAC roles. viewer is the least-
// privileged default; admin/owner are the privileged "admin user" roles.
export type MemberRole = "viewer" | "responder" | "admin" | "owner";

// MemberView is one row of the RBAC members surface: a provisioned member
// joined with their EFFECTIVE role (direct assignment OR the highest team-
// derived role). `role` is "" / absent for a member with no resolvable role.
export interface MemberView {
  subject: string;
  email: string;
  name?: string;
  connection?: string;
  role?: string;
}

export interface MembersEnvelope {
  org: string;
  members: MemberView[];
}

// BootstrapAdminStatus is the deployment default-admin ("admin user") state.
// The default admin is the built-in non-SSO root account created on first
// licensed boot. `can_disable` is the no-lockout guard: it may be turned off
// only when at least one OTHER owner/admin exists, so disabling can never
// strand the deployment.
export interface BootstrapAdminStatus {
  configured: boolean;
  username?: string;
  disabled?: boolean;
  can_disable?: boolean;
}

// AuthProbe is the set of side-effecting checks resolveInitialAuth depends on,
// injected so the decision logic is unit-testable without localStorage, fetch,
// or a DOM. AuthGate wires the real implementations.
export interface AuthProbe {
  // hasSecret reports whether a gateway secret is already held.
  hasSecret: () => boolean;
  // checkSecret verifies the held secret against the data plane (api.status).
  // Resolves on success; rejects with ApiError(401) when the secret is bad.
  checkSecret: () => Promise<unknown>;
  // deploymentOrg resolves the SSO deployment org (rejects on a non-enterprise
  // / community binary).
  deploymentOrg: () => Promise<string>;
  // probeSession checks for a live SSO session for org (rejects with
  // ApiError(401) when there is none).
  probeSession: (org: string) => Promise<unknown>;
}

// resolveInitialAuth is the pure decision the AuthGate runs on mount. It
// returns "ok" when the console should open and "needs-secret" when the
// gateway-secret screen should show.
//
// Order of resolution:
//   1. A held gateway secret is verified against the data plane.
//      A 401 means the secret is stale -> needs-secret. A transient/non-401
//      error deliberately does NOT trap the user -> ok (kept behavior).
//   2. With no held secret, probe for an established SSO session: resolve the
//      deployment org, then the session whoami. A live session opens the
//      console (the cookie now authenticates the data plane). Any failure
//      (community binary, 401 no-session, network) falls back to needs-secret.
export async function resolveInitialAuth(
  p: AuthProbe,
): Promise<"ok" | "needs-secret"> {
  if (p.hasSecret()) {
    try {
      await p.checkSecret();
      return "ok";
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) return "needs-secret";
      // Transient/network error: don't trap the user behind the secret screen.
      return "ok";
    }
  }
  // No gateway secret: an established SSO session is a valid console entry.
  try {
    const org = await p.deploymentOrg();
    await p.probeSession(org);
    return "ok";
  } catch {
    return "needs-secret";
  }
}

// ReportCapabilities is the report block of the capabilities probe: whether
// the incidents-analytics Reports action is available, the configured default
// channel + window, whether charts are on, the enabled channels to offer, and
// whether a root public_host is set (so a text-only channel fallback can carry
// a link). Sourced from the runtime settings store, not YAML.
export interface ReportCapabilities {
  enable: boolean;
  default_channel: string;
  default_window: string;
  include_chart: boolean;
  channels: string[];
  public_host_set: boolean;
}

// Capabilities is the shape of GET /api/admin/capabilities. `search` gates
// server-side full-text search; `report` (optional) gates the incidents
// report action.
export interface Capabilities {
  search: boolean;
  report?: ReportCapabilities;
}

// ReportSettings is the non-secret runtime configuration for the incidents
// analytics report, exchanged with GET/PUT /api/admin/reports/settings.
export interface ReportSettings {
  enable: boolean;
  default_channel: string;
  include_chart: boolean;
  rate_per_minute: number;
  default_window: string;
}

// ReportSendResult is the per-channel outcome of POST /reports/incidents.
// `sent` = image delivered; `fallback` = text summary + note delivered
// (image-incapable channel); `failed` = channel returned an error (the PNG is
// still downloadable via GET report.png). `window` echoes the rendered window.
export interface ReportSendResult {
  window: string;
  sent: string[];
  fallback: string[];
  failed: Record<string, string>;
  bytes: number;
}

export const api = {
  status: () => request<Status>("/api/agent/status"),
  listPatterns: () =>
    request<{ patterns: Pattern[] }>("/api/agent/patterns").then(
      (r) => r.patterns ?? [],
    ),
  getPattern: (id: string) => request<Pattern>(`/api/agent/patterns/${id}`),
  updatePattern: (id: string, body: { verdict?: string; tags?: string[] }) =>
    request<Pattern>(`/api/agent/patterns/${id}`, {
      method: "POST",
      body: JSON.stringify(body),
    }),
  deletePattern: (id: string) =>
    request<void>(`/api/agent/patterns/${id}`, { method: "DELETE" }),
  // clearPatterns wipes every learned log pattern (and resets the drain miner)
  // so the agent relearns log patterns from scratch. Discovered services are
  // left intact — that is a separate clearServices action.
  clearPatterns: () =>
    request<{ ok: boolean; patterns: number }>("/api/agent/patterns", {
      method: "DELETE",
    }),
  // clearServices wipes every discovered/manual service so the agent
  // re-discovers services from scratch. Learned log patterns are left intact.
  clearServices: () =>
    request<{ ok: boolean; services: number }>("/api/agent/services", {
      method: "DELETE",
    }),

  // listBaselines reads the Enterprise learned metric/trace baselines. It
  // does NOT swallow errors: an ApiError with status 403 (unlicensed) or 404
  // (OSS binary — endpoint absent) is how the page knows to render the locked
  // upsell state instead of a table.
  listBaselines: (params?: { type?: "metric" | "trace"; confident?: boolean }) => {
    const qs = new URLSearchParams();
    if (params?.type) qs.set("type", params.type);
    if (params?.confident) qs.set("confident", "true");
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    return request<BaselinesResponse>(`/api/agent/baselines${suffix}`);
  },

  // listSLORecommendations reads the Enterprise SLI/SLO auto-define output. Like
  // listBaselines it does NOT swallow errors: a 403 (unlicensed) or 404 (OSS
  // binary — endpoint absent) tells the page to render the locked upsell state.
  // The response carries an AI-gate status so the page can show a clear OFF
  // reason when AI is disabled.
  listSLORecommendations: () =>
    request<SLORecommendationsResponse>("/api/agent/slo-recommendations"),

  // SLI/SLO auto-define cadence (Enterprise, RBAC runtime:manage). These ride
  // the SSO session cookie via sessionRequest; the org and role are derived
  // from the session server-side. A non-admin session is 403'd (fail-closed),
  // a below-floor / unparseable cadence is 400'd.
  getSLOAutodefineConfig: () =>
    sessionRequest<SLOAutodefineConfig>(
      "/enterprise/api/agent/slo-autodefine/config",
    ),
  setSLOAutodefineConfig: (cadence: string) =>
    sessionRequest<SLOAutodefineConfig>(
      "/enterprise/api/agent/slo-autodefine/config",
      { method: "PUT", body: JSON.stringify({ cadence }) },
    ),
  // setSLOAutodefineEnabled flips the per-org feature toggle. Enabling is
  // server-validated against the AI hard gate (422 ai_required when AI is off /
  // no key); disabling is always allowed.
  setSLOAutodefineEnabled: (enabled: boolean) =>
    sessionRequest<SLOAutodefineConfig>(
      "/enterprise/api/agent/slo-autodefine/config",
      { method: "PUT", body: JSON.stringify({ enabled }) },
    ),

  // Runtime mode override (Enterprise, RBAC runtime:manage). These ride the
  // SSO session cookie via sessionRequest; the org and role are derived from
  // the session server-side. A non-admin session is 403'd (fail-closed).
  getAgentMode: () =>
    sessionRequest<AgentModeView>("/enterprise/api/agent/mode"),
  setAgentMode: (mode: AgentMode, confirm?: boolean) =>
    sessionRequest<AgentModeView>("/enterprise/api/agent/mode", {
      method: "PUT",
      body: JSON.stringify(confirm ? { mode, confirm: true } : { mode }),
    }),
  clearAgentMode: () =>
    sessionRequest<AgentModeView>("/enterprise/api/agent/mode", {
      method: "DELETE",
    }),

  // Runtime AI settings (Enterprise, RBAC runtime:manage). Same sessionRequest
  // plumbing as the mode control — the SSO session cookie, never a static
  // token. getAISettings returns the MASKED view (no key, ever). setAISettings
  // omits api_key when blank so the caller can toggle `enabled` without
  // resubmitting the key; the key is passed straight through to the single PUT
  // and never persisted client-side.
  getAISettings: () =>
    sessionRequest<AISettingsView>("/enterprise/api/agent/ai-settings"),
  setAISettings: (enabled: boolean, provider?: string, apiKey?: string) => {
    const key = apiKey?.trim() ?? "";
    const prov = provider?.trim() ?? "";
    const body: AISettingsInput = { enabled };
    if (prov) body.provider = prov;
    if (key) body.api_key = key;
    return sessionRequest<AISettingsView>("/enterprise/api/agent/ai-settings", {
      method: "PUT",
      body: JSON.stringify(body),
    });
  },
  clearAISettings: () =>
    sessionRequest<AISettingsView>("/enterprise/api/agent/ai-settings", {
      method: "DELETE",
    }),

  // Runtime notification-channel settings (Enterprise, RBAC runtime:manage).
  // Same sessionRequest plumbing as the AI-settings control — the SSO session
  // cookie, never a static token. getChannelSettings returns the MASKED view of
  // all six channels (no secret, ever). setChannelSettings PUTs ONE channel;
  // blank secret fields are omitted by buildChannelPut so the server preserves
  // the stored secret. clearChannelSettings reverts ONE channel to its YAML
  // floor. testChannel triggers a rate-limited synthetic test-send. All return
  // the authoritative post-change masked map (test returns { ok }).
  getChannelSettings: () =>
    sessionRequest<{ channels: ChannelSettingsMap }>(
      "/enterprise/api/agent/channel-settings",
    ).then((r) => r.channels ?? {}),
  setChannelSettings: (channel: string, body: ChannelSettingsInput) =>
    sessionRequest<{ channels: ChannelSettingsMap }>(
      `/enterprise/api/agent/channel-settings/${encodeURIComponent(channel)}`,
      { method: "PUT", body: JSON.stringify(body) },
    ).then((r) => r.channels ?? {}),
  clearChannelSettings: (channel: string) =>
    sessionRequest<{ channels: ChannelSettingsMap }>(
      `/enterprise/api/agent/channel-settings/${encodeURIComponent(channel)}`,
      { method: "DELETE" },
    ).then((r) => r.channels ?? {}),
  testChannel: (channel: string) =>
    sessionRequest<{ ok: boolean }>(
      `/enterprise/api/agent/channel-settings/${encodeURIComponent(channel)}/test`,
      { method: "POST" },
    ),

  // Disable-Learn exclusions (Enterprise, RBAC runtime:manage). Same
  // sessionRequest plumbing as the runtime mode / AI controls — the SSO session
  // cookie, never a static token; the org and role are derived server-side. The
  // GET is the single state source the toggle + per-metric checkboxes read;
  // setServiceLearnExclusion toggles ONE service (POST add / DELETE remove);
  // setLearnExclusions PUTs the whole list (read-modify-write off the GET),
  // which is ALSO how a per-log-pattern exclusion is toggled (the server has no
  // per-pattern POST/DELETE convenience route — the whole-list PUT is the sole
  // write path for the `patterns` grain, same as metric/trace signals). All
  // return the authoritative post-change lists. Every mutation is audited
  // server-side and takes effect on the next worker tick (no restart). 403/404
  // is the terminal community / OSS / wrong-role answer, never retried.
  //
  // WIRE FIELD NOTE: the enterprise policy serializes the per-log-pattern grain
  // as `log_patterns` (the metric grain is `metrics`, service grain is
  // `services`). The UI models it as `patterns` for brevity, so both the GET
  // response and the PUT body are mapped across the `patterns` ⇄ `log_patterns`
  // seam here — reading the wrong field is exactly what left an ignored log
  // pattern stuck in the Active tab.
  getLearnExclusions: () =>
    sessionRequest<LearnExclusionsWire>(
      "/enterprise/api/agent/learn-exclusions",
    ).then((r) => ({
      services: r.services ?? [],
      metrics: r.metrics ?? [],
      patterns: r.log_patterns ?? [],
    })),
  setLearnExclusions: (input: LearnExclusions) =>
    sessionRequest<LearnExclusionsWire>("/enterprise/api/agent/learn-exclusions", {
      method: "PUT",
      body: JSON.stringify({
        services: input.services,
        metrics: input.metrics,
        log_patterns: input.patterns,
      }),
    }).then((r) => ({
      services: r.services ?? [],
      metrics: r.metrics ?? [],
      patterns: r.log_patterns ?? [],
    })),
  setServiceLearnExclusion: (name: string, excluded: boolean) =>
    sessionRequest<LearnExclusionsWire>(
      `/enterprise/api/agent/learn-exclusions/services/${encodeURIComponent(name)}`,
      { method: excluded ? "POST" : "DELETE" },
    ).then((r) => ({
      services: r.services ?? [],
      metrics: r.metrics ?? [],
      patterns: r.log_patterns ?? [],
    })),

  // getSSODeployment reads the license-issued single-tenant deployment org so
  // the admin controls drive SSO/connections/policy under it (not "default").
  // Pre-auth (no session); 403 in community mode signals "not enterprise".
  getSSODeployment: () => getSSODeployment(),

  // Enterprise multi-IdP connections (Keycloak-style, RBAC sso:manage). Same
  // sessionRequest plumbing as the SSO config control. The list/get views are
  // MASKED (never the client secret). setSSOConnection OMITS client_secret when
  // blank so the caller can toggle/edit without re-sealing the stored secret.
  listSSOConnections: (org: string) =>
    sessionRequest<SSOConnectionsEnvelope>(
      `/enterprise/api/sso/${encodeURIComponent(org)}/connections`,
    ),
  getSSOConnection: (org: string, id: string) =>
    sessionRequest<SSOConnectionEnvelope>(
      `/enterprise/api/sso/${encodeURIComponent(org)}/connections/${encodeURIComponent(id)}`,
    ),
  setSSOConnection: (org: string, id: string, input: SSOConnectionInput) => {
    const secret = input.client_secret?.trim() ?? "";
    const body: SSOConnectionInput = { ...input };
    if (secret) body.client_secret = secret;
    else delete body.client_secret; // blank/omitted preserves the stored seal
    return sessionRequest<SSOConnectionEnvelope>(
      `/enterprise/api/sso/${encodeURIComponent(org)}/connections/${encodeURIComponent(id)}`,
      { method: "PUT", body: JSON.stringify(body) },
    );
  },
  deleteSSOConnection: (org: string, id: string) =>
    sessionRequest<{ org: string; deleted: boolean; connection: string }>(
      `/enterprise/api/sso/${encodeURIComponent(org)}/connections/${encodeURIComponent(id)}`,
      { method: "DELETE" },
    ),

  // Enterprise per-org SSO enforcement policy (X4-T4, RBAC sso:manage). Same
  // sessionRequest plumbing as the SSO config control. setSSOPolicy with
  // require_sso=true ENFORCES SSO for human sign-in (and gates require_mfa); the
  // server rejects it (422 sso_not_configured) unless an enabled IdP config
  // exists, so it can't strand the org.
  getSSOPolicy: (org: string) =>
    sessionRequest<SSOPolicyEnvelope>(
      `/enterprise/api/sso/${encodeURIComponent(org)}/policy`,
    ),
  setSSOPolicy: (org: string, input: SSOPolicyInput) =>
    sessionRequest<SSOPolicyEnvelope>(
      `/enterprise/api/sso/${encodeURIComponent(org)}/policy`,
      { method: "PUT", body: JSON.stringify(input) },
    ),

  // Enterprise RBAC members + role administration (roles:manage, per-org). Same
  // sessionRequest plumbing as the SSO controls — the SSO session cookie, never
  // a static token. listRbacMembers joins the member directory with each
  // subject's effective role; setMemberRole assigns a direct role to one
  // subject. (Named distinctly from the OSS responder-roster listMembers below,
  // which is a different surface — the incident on-call directory.)
  listRbacMembers: (org: string) =>
    sessionRequest<MembersEnvelope>(
      `/enterprise/api/rbac/${encodeURIComponent(org)}/members`,
    ),
  setMemberRole: (org: string, subject: string, role: MemberRole) =>
    sessionRequest<{ org: string; subject: string; role: string }>(
      `/enterprise/api/rbac/${encodeURIComponent(org)}/roles/${encodeURIComponent(subject)}`,
      { method: "PUT", body: JSON.stringify({ role }) },
    ),

  // Deployment default-admin ("admin user") status + disable (roles:manage).
  // getBootstrapAdmin reports whether one is configured and whether it can be
  // disabled (the no-lockout guard). disableBootstrapAdmin turns off the
  // built-in default admin; the server refuses it (422 no_other_admin) unless
  // another owner/admin exists, so the deployment can never be stranded.
  getBootstrapAdmin: (org: string) =>
    sessionRequest<BootstrapAdminStatus>(
      `/enterprise/api/rbac/${encodeURIComponent(org)}/bootstrap-admin`,
    ),
  disableBootstrapAdmin: (org: string) =>
    sessionRequest<{ org: string; disabled: boolean }>(
      `/enterprise/api/rbac/${encodeURIComponent(org)}/bootstrap-admin/disable`,
      { method: "POST" },
    ),
  // enableBootstrapAdmin turns a disabled built-in default admin back on
  // (owner break-glass). It only widens access, so the server applies no
  // no-lockout check.
  enableBootstrapAdmin: (org: string) =>
    sessionRequest<{ org: string; disabled: boolean }>(
      `/enterprise/api/rbac/${encodeURIComponent(org)}/bootstrap-admin/enable`,
      { method: "POST" },
    ),

  listShadow: () =>
    request<{ events: ShadowEvent[] }>("/api/agent/shadow").then(
      (r) => r.events ?? [],
    ),
  shadowStats: () => request<ShadowStats>("/api/agent/shadow/stats"),
  clearShadow: () =>
    request<{ ok: boolean; cleared: number }>("/api/agent/shadow", {
      method: "DELETE",
    }),
  flushShadow: () =>
    request<{ ok: boolean; events: number }>("/api/agent/shadow/flush", {
      method: "POST",
    }),

  listServices: () =>
    request<{ services: Record<string, ServiceInfo> }>(
      "/api/agent/services",
    ).then((r) => r.services ?? {}),
  // getServiceDetail reads the OSS service-detail aggregate (meta + grace +
  // patterns + bounded incident summary). 404 means the service is unknown.
  getServiceDetail: (name: string) =>
    request<ServiceDetail>(`/api/agent/services/${encodeURIComponent(name)}`),
  // getServiceIntel reads the Enterprise metrics/traces half. Like
  // listBaselines it does NOT swallow errors: a 403 (unlicensed) or 404 (OSS
  // binary — endpoint absent) tells the page to render the locked upsell state
  // for the Metrics & Traces section instead of a panel.
  getServiceIntel: (name: string) =>
    request<ServiceIntel>(
      `/api/agent/services/${encodeURIComponent(name)}/intel`,
    ),
  controlGrace: (name: string, action: "end" | "restart") =>
    request<{ ok: boolean }>(
      `/api/agent/services/${encodeURIComponent(name)}/grace`,
      { method: "POST", body: JSON.stringify({ action }) },
    ),

  // createService records an operator-created (manual) service so it is
  // selectable as an override target before any signal is attributed to it.
  createService: (name: string) =>
    request<{ service: string; manual: boolean }>("/api/agent/services", {
      method: "POST",
      body: JSON.stringify({ name }),
    }),
  // renameService renames a manual service and repoints any override rules that
  // targeted the old name. Auto-discovered services cannot be renamed (400).
  renameService: (oldName: string, newName: string) =>
    request<{ service: string; manual: boolean; overrides_repointed: number }>(
      `/api/agent/services/${encodeURIComponent(oldName)}`,
      { method: "PUT", body: JSON.stringify({ name: newName }) },
    ),
  // deleteService removes a manual service. The server blocks deletion (409)
  // while any override rule still targets it, so the caller must remove those
  // overrides first.
  deleteService: (name: string) =>
    request<void>(`/api/agent/services/${encodeURIComponent(name)}`, {
      method: "DELETE",
    }),

  // listServiceOverrides reads every manual-attribution override rule.
  listServiceOverrides: () =>
    request<{ overrides: ServiceOverride[] }>(
      "/api/agent/service-overrides",
    ).then((r) => r.overrides ?? []),
  // createServiceOverride creates (or replaces the same-key) override rule. The
  // target service must already exist (create it first).
  createServiceOverride: (input: {
    source_type: ServiceOverrideSource;
    match: string;
    service: string;
  }) =>
    request<ServiceOverride>("/api/agent/service-overrides", {
      method: "POST",
      body: JSON.stringify(input),
    }),
  // deleteServiceOverride removes one override rule by id.
  deleteServiceOverride: (id: string) =>
    request<void>(
      `/api/agent/service-overrides/${encodeURIComponent(id)}`,
      { method: "DELETE" },
    ),

  listDetect: () =>
    request<{ events: DetectEvent[] }>("/api/agent/detect").then(
      (r) => r.events ?? [],
    ),
  detectStats: () => request<DetectStats>("/api/agent/detect/stats"),
  getDetect: (id: string) =>
    request<DetectEvent>(`/api/agent/detect/${encodeURIComponent(id)}`),
  clearDetect: () =>
    request<{ ok: boolean; cleared: number }>("/api/agent/detect", {
      method: "DELETE",
    }),
  flushDetect: () =>
    request<{ ok: boolean; events: number }>("/api/agent/detect/flush", {
      method: "POST",
    }),
  getSystemPrompt: () =>
    request<{ system_prompt: string }>("/api/agent/ai/system-prompt").then(
      (r) => r.system_prompt,
    ),

  listIncidents: (limit?: number) => {
    const qs = limit ? `?limit=${limit}` : "";
    return request<{ incidents: IncidentSummary[] }>(
      `/api/admin/incidents${qs}`,
    ).then((r) => r.incidents ?? []);
  },
  // listIncidentsIndex is the Incidents-page variant: it returns the rows
  // for one origin tab PLUS the whole-set per-origin counts (so the
  // top-bar shows both feeds separately) in a single request. Pass an
  // origin to scope the rows; the counts stay whole-set regardless.
  listIncidentsIndex: (opts?: {
    origin?: string;
    page?: number;
    pageSize?: number;
    limit?: number;
  }) => {
    const p = new URLSearchParams();
    if (opts?.origin) p.set("origin", opts.origin);
    if (opts?.page) p.set("page", String(opts.page));
    if (opts?.pageSize) p.set("page_size", String(opts.pageSize));
    if (opts?.limit) p.set("limit", String(opts.limit));
    const qs = p.toString();
    return request<IncidentIndex>(
      `/api/admin/incidents${qs ? `?${qs}` : ""}`,
    );
  },
  // capabilities reports which optional storage features the running
  // backend supports. `search` is true only when the backend implements
  // server-side full-text search (Postgres); memory/file return false and
  // the UI falls back to client-side filtering. `report` gates the incident
  // report → channel share action.
  capabilities: () =>
    request<Capabilities>("/api/admin/capabilities"),
  // searchIncidents runs server-side full-text search. Only call it when
  // capabilities().search is true; otherwise the endpoint returns 501.
  searchIncidents: (q: string, limit?: number) => {
    const params = new URLSearchParams({ q });
    if (limit) params.set("limit", String(limit));
    return request<{ incidents: IncidentSummary[] }>(
      `/api/admin/incidents/search?${params.toString()}`,
    ).then((r) => r.incidents ?? []);
  },
  // searchIncidentsIndex mirrors listIncidentsIndex for the server-side
  // search path: origin-scoped rows plus whole-(match)-set origin counts.
  searchIncidentsIndex: (
    q: string,
    opts?: { origin?: string; page?: number; pageSize?: number; limit?: number },
  ) => {
    const p = new URLSearchParams({ q });
    if (opts?.origin) p.set("origin", opts.origin);
    if (opts?.page) p.set("page", String(opts.page));
    if (opts?.pageSize) p.set("page_size", String(opts.pageSize));
    if (opts?.limit) p.set("limit", String(opts.limit));
    return request<IncidentIndex>(
      `/api/admin/incidents/search?${p.toString()}`,
    );
  },
  getIncident: (id: string) =>
    request<IncidentDetail>(`/api/admin/incidents/${id}`),

  runAnalysis: (incidentID: string, requestedBy?: string) =>
    request<AnalysisRecord>(`/api/admin/incidents/${incidentID}/analyze`, {
      method: "POST",
      body: JSON.stringify({ requested_by: requestedBy ?? "" }),
    }),
  listAnalyses: (incidentID: string, limit?: number) => {
    const qs = limit ? `?limit=${limit}` : "";
    return request<{ analyses: AnalysisRecord[] }>(
      `/api/admin/incidents/${incidentID}/analyses${qs}`,
    ).then((r) => r.analyses ?? []);
  },
  listAllAnalyses: (limit?: number) => {
    const qs = limit ? `?limit=${limit}` : "";
    return request<{ analyses: AnalysisRecord[] }>(
      `/api/admin/analyses${qs}`,
    ).then((r) => r.analyses ?? []);
  },
  getAnalysis: (analysisID: string) =>
    request<AnalysisRecord>(`/api/admin/analyses/${analysisID}`),
  deleteAnalysis: (analysisID: string) =>
    request<void>(`/api/admin/analyses/${analysisID}`, { method: "DELETE" }),

  getIncidentsConfig: () =>
    request<IncidentsConfig>("/api/admin/config/incidents"),
  getAgentConfig: () => request<AgentConfigView>("/api/admin/config/agent"),

  listMembers: () =>
    request<{ members: Member[] }>("/api/admin/members").then(
      (r) => r.members ?? [],
    ),
  createMember: (body: MemberInput) =>
    request<Member>("/api/admin/members", {
      method: "POST",
      body: JSON.stringify(body),
    }),
  updateMember: (id: string, body: MemberInput) =>
    request<Member>(`/api/admin/members/${id}`, {
      method: "PATCH",
      body: JSON.stringify(body),
    }),
  deleteMember: (id: string) =>
    request<void>(`/api/admin/members/${id}`, { method: "DELETE" }),

  listTeams: () =>
    request<{ teams: Team[] }>("/api/admin/teams").then((r) => r.teams ?? []),
  createTeam: (body: TeamInput) =>
    request<Team>("/api/admin/teams", {
      method: "POST",
      body: JSON.stringify(body),
    }),
  updateTeam: (id: string, body: TeamInput) =>
    request<Team>(`/api/admin/teams/${id}`, {
      method: "PATCH",
      body: JSON.stringify(body),
    }),
  deleteTeam: (id: string) =>
    request<void>(`/api/admin/teams/${id}`, { method: "DELETE" }),

  assignIncident: (
    id: string,
    body: { team_id?: string | null; member_ids?: string[] | null },
  ) =>
    request<{
      id: string;
      assigned_team_id?: string;
      assigned_member_ids?: string[];
      updated_at: string;
    }>(`/api/admin/incidents/${id}/assign`, {
      method: "POST",
      body: JSON.stringify(body),
    }),

  resolveIncident: (id: string) =>
    request<{ id: string; resolved: boolean; resolved_at?: string | null }>(
      `/api/admin/incidents/${id}/resolve`,
      { method: "POST" },
    ),

  // sendIncidentsReport renders the aggregate dashboard for a window and
  // delivers it to a channel. A 502 (partial — at least one channel failed)
  // still resolves with the outcome (the image stays downloadable), so the UI
  // can show per-channel results instead of a bare error; other statuses
  // propagate as ApiError.
  sendIncidentsReport: async (
    window: string,
    channel?: string,
    requestedBy?: string,
  ): Promise<ReportSendResult> => {
    const qs = window ? `?window=${encodeURIComponent(window)}` : "";
    try {
      return await request<ReportSendResult>(
        `/api/admin/reports/incidents${qs}`,
        {
          method: "POST",
          body: JSON.stringify({
            channel: channel ?? "",
            requested_by: requestedBy ?? "",
          }),
        },
      );
    } catch (e) {
      if (
        e instanceof ApiError &&
        e.status === 502 &&
        e.body &&
        typeof e.body === "object" &&
        "sent" in e.body
      ) {
        return e.body as ReportSendResult;
      }
      throw e;
    }
  },

  // fetchIncidentsReportImage fetches the rendered PNG for a window with the
  // gateway-secret header and returns a Blob — an <img src> cannot carry the
  // header, so the preview loads the bytes here and renders via an object URL.
  fetchIncidentsReportImage: async (window: string): Promise<Blob> => {
    const secret = getSecret() ?? "";
    const headers = new Headers();
    if (secret) headers.set("X-Gateway-Secret", secret);
    const qs = window ? `?window=${encodeURIComponent(window)}` : "";
    const res = await fetch(
      `${API_BASE}/api/admin/reports/incidents/report.png${qs}`,
      { headers, credentials: "same-origin", cache: "no-store" },
    );
    if (!res.ok) {
      if (res.status === 401 && secret) notifyAuthExpired();
      let msg = `HTTP ${res.status}`;
      try {
        const b = await res.json();
        if (b && typeof b === "object" && "error" in b) {
          msg = String((b as { error: unknown }).error);
        }
      } catch {
        /* non-JSON error body */
      }
      throw new ApiError(res.status, msg);
    }
    return res.blob();
  },

  // getReportSettings reads the runtime report settings (non-secret toggles).
  getReportSettings: () =>
    request<ReportSettings>("/api/admin/reports/settings"),

  // updateReportSettings replaces the runtime report settings and returns the
  // effective (sanitized) values.
  updateReportSettings: (s: ReportSettings) =>
    request<ReportSettings>("/api/admin/reports/settings", {
      method: "PUT",
      body: JSON.stringify(s),
    }),

  listRunbooks: () =>
    request<{ runbooks: Runbook[]; embeddings: boolean }>(
      "/api/agent/runbooks",
    ),
  getRunbook: (id: string) =>
    request<RunbookDetail>(`/api/agent/runbooks/${encodeURI(id)}`),
  deleteRunbook: (id: string) =>
    request<void>(`/api/agent/runbooks/${encodeURI(id)}`, { method: "DELETE" }),
  uploadRunbooks: (files: File[]) => {
    const form = new FormData();
    for (const f of files) form.append("files", f, f.name);
    return uploadMultipart<RunbookUploadResult>("/api/agent/runbooks", form);
  },
};

// ---------- Config view types (read-only, secret-redacted) ----------

export interface ConfigField {
  label: string;
  value: unknown;
  secret?: boolean;
}

export interface ChannelConfig {
  id: string;
  name: string;
  enable: boolean;
  fields: ConfigField[];
}

export interface QueueProviderConfig {
  id: string;
  name: string;
  enable: boolean;
  fields: ConfigField[];
}

export interface IncidentsConfig {
  name: string;
  host: string;
  port: number;
  public_host: string;
  alert: { debug_body: boolean; channels: ChannelConfig[] };
  queue: {
    enable: boolean;
    debug_body: boolean;
    providers: QueueProviderConfig[];
  };
  oncall: {
    enable: boolean;
    initialized_only: boolean;
    wait_minutes: number;
    provider: string;
    aws_incident_manager: {
      response_plan_arn: string;
      other_response_plan_keys: string[];
    };
    pagerduty: {
      routing_key: string;
      other_routing_keys: string[];
    };
    servicenow: {
      instance_url: string;
      username: string;
      table: string;
      other_instance_keys: string[];
    };
    incident_io: {
      api_key: string;
      alert_source_config_id: string;
      other_alert_source_config_keys: string[];
    };
  };
  storage: {
    type: string;
    file: { max_incidents: number };
  };
}

export interface AgentConfigView {
  enable: boolean;
  mode: string;
  poll_interval: string;
  lookback: string;
  batch_max: number;
  signal_max_bytes: number;
  new_service_grace: string;
  service_patterns: string[];
  sources_path: string;
  sources: Array<{
    name: string;
    type: string;
    enable: boolean;
    details?: Record<string, unknown>;
  }>;
  redaction: {
    enable: boolean;
    redact_ips: boolean;
    extra_pattern_count: number;
  };
  catalog: {
    persist_interval: string;
    auto_promote_after: number;
    spike_multiplier: number;
    spike_min_frequency: number;
    spike_min_baseline_count: number;
  };
  miner: {
    similarity_threshold: number;
    tree_depth: number;
    max_children: number;
  };
  regex: {
    default_pattern: string;
    rules: Array<{ name: string; pattern: string }>;
  };
  ai: {
    enable: boolean;
    model: string;
    temperature: number;
    max_tokens: number;
    max_calls_per_hour: number;
    cache_ttl: string;
    api_key: string;
    analyze?: {
      tools?: string[];
      max_tool_iterations?: number;
    };
  };
}
