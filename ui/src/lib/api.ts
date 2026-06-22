// Centralized API client for the agent admin endpoints.
//
// All requests are authenticated with the X-Gateway-Secret header. The secret
// is read from localStorage; AuthGate prompts for it on first visit.

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
  headers.set("X-Gateway-Secret", secret);
  if (init.body && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }

  const res = await fetch(`${API_BASE}${path}`, { ...init, headers });

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
}

export interface BaselinesResponse {
  org: string;
  count: number;
  baselines: BaselineRow[];
}export interface ShadowEvent {
  pattern_id: string;
  template: string;
  source: string;
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
  headers.set("X-Gateway-Secret", secret);

  const res = await fetch(`${API_BASE}${path}`, {
    method: "POST",
    headers,
    body: form,
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

// ---------- Endpoints ----------

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
  flushPatterns: () =>
    request<{ ok: boolean; patterns: number }>("/api/agent/flush", {
      method: "POST",
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
  controlGrace: (name: string, action: "end" | "restart") =>
    request<{ ok: boolean }>(
      `/api/agent/services/${encodeURIComponent(name)}/grace`,
      { method: "POST", body: JSON.stringify({ action }) },
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
  // capabilities reports which optional storage features the running
  // backend supports. `search` is true only when the backend implements
  // server-side full-text search (Postgres); memory/file return false and
  // the UI falls back to client-side filtering.
  capabilities: () =>
    request<{ search: boolean }>("/api/admin/capabilities"),
  // searchIncidents runs server-side full-text search. Only call it when
  // capabilities().search is true; otherwise the endpoint returns 501.
  searchIncidents: (q: string, limit?: number) => {
    const params = new URLSearchParams({ q });
    if (limit) params.set("limit", String(limit));
    return request<{ incidents: IncidentSummary[] }>(
      `/api/admin/incidents/search?${params.toString()}`,
    ).then((r) => r.incidents ?? []);
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
