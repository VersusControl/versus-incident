import { format } from "date-fns";

// AWS Changes (/api/admin/aws/changes): the wire types, field for field, and
// the presentation rules the page relies on. The server decides what is
// complete; this module only words it. The one hard rule: the page never
// says "No changes" while any data in the window is missing. Missing data is
// named once, by the summary card's warning line, never by hedged counts.

export type ChangesWindow = "1h" | "24h" | "7d";
/** Every option the window switch lists; 7d opens only when `limits` allow it. */
export type ChangesWindowOption = ChangesWindow;
export type ActorClass = "pipeline" | "person" | "aws_service" | "unknown";
/**
 * `partial`: some account × Region pairs in the bucket were read and some
 * were not; the counts are what was read, a lower bound. `gap`: nothing read.
 */
export type BucketState = "complete" | "partial" | "gap" | "arriving";
export type ChangeItemKind = "change" | "burst" | "group" | "routine";
export type BeforeStatus = "known" | "new_only" | "hidden" | "ambiguous";
/** `resource_unknown`: the alarm's resources did not resolve to ARNs, so nothing can be matched. */
export type SuspectStatus = "found" | "none" | "start_unknown" | "incomplete" | "no_alarm" | "resource_unknown";
export type GapReason =
  | "cloudtrail_denied"
  | "throttled"
  | "too_busy"
  | "collector_down"
  | "not_checked"
  | "before_connect"
  | "beyond_retention"
  | "region_not_enabled"
  | "credentials_invalid"
  | "account_disconnected"
  | "assume_role_failed"
  | "account_mismatch"
  | "arriving"
  | "global_unknown";

export interface ChangesWindowRange {
  since: string;
  until: string;
  settled_until: string;
}

export interface ChangesSummary {
  total: number;
  total_is_lower_bound: boolean;
  by_actor_class: Record<ActorClass, number>;
  people: number;
  routine: number;
  errors: number;
  truncated: boolean;
  /** The five busiest actors in scope. */
  top_actors?: TopActor[];
  /** The newest change before `since` in scope, or null when none is on record. */
  last_change_before?: string | null;
}

export interface TopActor {
  actor: string;
  actor_class: ActorClass;
  count: number;
  /** Set only when every change behind the entry is in one account. */
  account?: string | null;
}

export interface SuspectCandidate {
  change_id: string;
  confidence: number | string | null;
  /** Equally likely as the one before it. */
  tied: boolean;
}

export type RegionState = "selected" | "not_enabled" | "global";

/** Every Region in scope, including ones not enabled in AWS. */
export interface ChangesRegion {
  region: string;
  state: RegionState;
  complete: boolean;
}

export interface ChangesBucket {
  start: string;
  pipeline: number;
  person: number;
  aws_service: number;
  unknown: number;
  errors: number;
  state: BucketState;
  /** Accounts with a gap in this bucket. */
  gap_accounts?: string[];
}

export interface ChangesAlarm {
  id: string;
  name: string;
  account: string;
  region: string;
  resource_arns: string[];
  start: string | null;
  start_known: boolean;
  end: string | null;
}

export interface ChangesSuspect {
  status: SuspectStatus;
  change_id?: string | null;
  /** The full suspect item, so it reads even when it is not on the loaded page. */
  change?: ChangeItem | null;
  /** Every candidate, best first; the first is `change_id`/`change`. */
  candidates?: SuspectCandidate[];
  alarm_id?: string | null;
  searched?: { accounts: string[]; regions: string[]; from: string; to: string } | null;
  /**
   * False when the likely cause sits in a service outside the page's service
   * filter. The cause is always found over every service; the card still
   * names it, marked.
   */
  in_filter?: boolean;
}

export interface ChangesGap {
  account: string;
  region: string;
  from: string;
  to: string;
  reason: GapReason | string;
  /** Set for `cloudtrail_denied`, e.g. `cloudtrail:LookupEvents`. */
  missing_permission?: string | null;
  /** Set on `too_busy` gaps: how many events an hour the stretch carried. */
  events_per_hour?: number | null;
}

export interface ChangesAccount {
  account: string;
  name: string;
  complete: boolean;
  reason: GapReason | string | null;
  /** End of the account's last unbroken coverage; no stored row of the account is newer. */
  as_of: string | null;
  /** When Settings recorded the account as disconnected (with reason `account_disconnected`). */
  disconnected_at?: string | null;
}

export interface ChangesSync {
  last_poll_at: string | null;
  late_arrivals: number;
  clock_skew_seconds: number;
  first_read_pending: boolean;
  /** Malformed CloudTrail rows the reader skipped. */
  dropped: number;
}

export interface ChangeField {
  field: string;
  before: string | null;
  after: string | null;
  before_status: BeforeStatus;
  missing_permission: string | null;
}

export interface ChangeItem {
  kind: ChangeItemKind;
  id: string;
  time: string;
  end_time: string | null;
  account: string;
  region: string;
  service: string;
  actor_class: ActorClass;
  actor: string;
  summary: string;
  /** Raw API name in `service:Action` form, e.g. `s3:PutBucketPolicy`. */
  event_name: string;
  resources: string[];
  fields: ChangeField[];
  error_code: string | null;
  count: number | null;
}

export interface ChangesView {
  window: ChangesWindowRange;
  complete: boolean;
  summary: ChangesSummary;
  buckets: ChangesBucket[];
  alarms: ChangesAlarm[];
  alarm_history_starts: string | null;
  suspect: ChangesSuspect;
  gaps: ChangesGap[];
  accounts: ChangesAccount[];
  regions: ChangesRegion[];
  sync: ChangesSync;
  items: ChangeItem[];
  next_cursor: string | null;
  /**
   * Every service with changes in the window and scope, counted without the
   * service filter and sorted by count, for the Services filter panel.
   */
  services?: ChangesService[];
  /** What this edition serves; absent on older servers, read as 24 h each. */
  limits?: ChangesLimits;
  /**
   * The length of one bucket in seconds: 300 up to 1 h, 3 600 up to 24 h,
   * 21 600 beyond. Absent on older servers, which send hourly buckets.
   */
  bucket_seconds?: number;
}

/**
 * The edition's limits, as Go durations ("24h", "168h"): the longest window
 * the list serves and how far back Compare can start.
 */
export interface ChangesLimits {
  max_window: string;
  max_compare_age: string;
}

export interface ChangesService {
  service: string;
  label: string;
  count: number;
}

/** `/preferences`: the signed-in user's saved service filter. Empty means all services. */
export interface ChangesPreferences {
  services: string[];
}

/** The services a user who has never chosen sees. */
export const DEFAULT_SERVICES: readonly string[] = ["rds", "iam"];

/** `rds,iam` for the `service` query parameter; undefined (all services) when empty. */
export function serviceParam(services: readonly string[]): string | undefined {
  return services.length > 0 ? services.join(",") : undefined;
}

/** The URL's `service=rds,iam`, as a list; an empty value means all services. */
export function parseServiceParam(value: string): string[] {
  return [...new Set(value.split(",").map((s) => s.trim()).filter(Boolean))];
}

/** A service's display name: the server's label, else the name in capitals for short ones. */
export function serviceLabel(services: readonly ChangesService[] | undefined, service: string): string {
  const known = services?.find((s) => s.service === service);
  if (known?.label) return known.label;
  return service.length <= 3 ? service.toUpperCase() : service.charAt(0).toUpperCase() + service.slice(1);
}

/** Up to this many services are named; more read as "N services". */
const NAMED_SERVICES = 3;

/** The card's filter label: "RDS, IAM", "5 services", or "All services" when unfiltered. */
export function serviceFilterLabel(selected: readonly string[], services?: readonly ChangesService[]): string {
  if (selected.length === 0) return "All services";
  if (selected.length > NAMED_SERVICES) return `${selected.length} services`;
  return selected.map((s) => serviceLabel(services, s)).join(", ");
}

/** "RDS or IAM", "RDS, IAM or S3": the services a "No changes" line names. */
function servicesOr(selected: readonly string[], services?: readonly ChangesService[]): string {
  const names = selected.map((s) => serviceLabel(services, s));
  if (names.length === 1) return names[0];
  return `${names.slice(0, -1).join(", ")} or ${names[names.length - 1]}`;
}

/** `/groups/{id}`: the members in the item shape, paged by cursor. */
export interface ChangesGroupPage {
  items: ChangeItem[];
  next_cursor: string | null;
}

export interface CompareResource {
  resource_arn: string;
  type: string;
  name: string;
  account: string;
  region: string;
  /** A short identifier, set when two items share a name and type. */
  id_hint?: string | null;
}

export interface CompareChanged extends CompareResource {
  fields: Array<{ field: string; before: string | null; after: string | null; hidden: boolean }>;
}

export interface ComparePartial {
  account: string;
  region: string;
  service: string;
  /** Null for a configured Region with no history yet. */
  history_starts: string | null;
  /**
   * Why the unit was left out: a gap at either compare time, or a state
   * Settings already knows (`service_denied`, `not_on_plan`).
   */
  reason?: GapReason | CompareExclusion | string | null;
  /** Known unreadable for this account; does not count against `complete`. */
  excluded?: boolean;
}

/** Compare groups left out because Settings knows they can't be read. */
export type CompareExclusion = "service_denied" | "not_on_plan" | "not_available_in_region";

export interface CompareView {
  from: string;
  to: string;
  status: "ok" | "unavailable";
  history_starts: string | null;
  complete: boolean;
  added: CompareResource[];
  removed: CompareResource[];
  changed: CompareChanged[];
  partial: ComparePartial[];
  /** The saved snapshots for the scope; `oldest` is the earliest time compare can start from. */
  snapshots: { oldest: string | null; count: number };
  /** Up to 20 change-list items between `from` and `to`, newest first. */
  changes_between: ChangeItem[];
  changes_between_total: number;
  limits?: ChangesLimits;
}

export interface ChangesQuery {
  window?: ChangesWindow;
  /** An exact start; with it the window is `[since, until]` and `window` is ignored. */
  since?: string;
  until?: string;
  account?: string;
  region?: string;
  actor_class?: ActorClass;
  service?: string;
  resource?: string;
  q?: string;
  show_routine?: boolean;
  cursor?: string;
  limit?: number;
}

export interface CompareQuery {
  from: string;
  to?: string;
  account?: string;
  region?: string;
  service?: string;
}

function qs(entries: Array<[string, string | number | boolean | undefined | null]>): string {
  const p = new URLSearchParams();
  for (const [k, v] of entries) {
    if (v === undefined || v === null || v === "" || v === false) continue;
    p.set(k, String(v));
  }
  const s = p.toString();
  return s ? `?${s}` : "";
}

export function changesQueryString(q: ChangesQuery): string {
  return qs([
    ["window", q.since ? undefined : q.window],
    ["since", q.since],
    ["until", q.until],
    ["account", q.account],
    ["region", q.region],
    ["actor_class", q.actor_class],
    ["service", q.service],
    ["resource", q.resource],
    ["q", q.q],
    ["show_routine", q.show_routine],
    ["cursor", q.cursor],
    ["limit", q.limit],
  ]);
}

export function groupQueryString(cursor?: string | null, limit?: number): string {
  return qs([
    ["cursor", cursor],
    ["limit", limit],
  ]);
}

export function compareQueryString(q: CompareQuery): string {
  return qs([
    ["from", q.from],
    ["to", q.to],
    ["account", q.account],
    ["region", q.region],
    ["service", q.service],
  ]);
}

/** 403 `window_locked`: the edition caps the window; shown as a quiet note, not an error. */
// Duck-typed on ApiError's status and body so this module stays free of a
// runtime import cycle with the client.
export function isWindowLocked(error: unknown): boolean {
  if (!error || typeof error !== "object") return false;
  const e = error as { status?: unknown; body?: unknown };
  if (e.status !== 403) return false;
  const body = e.body;
  return Boolean(body && typeof body === "object" && "error" in body && (body as { error: unknown }).error === "window_locked");
}

/** Any other 403: the caller's role is refused the page. A final answer, shown as a denial. */
export function isPermissionDenied(error: unknown): boolean {
  if (!error || typeof error !== "object") return false;
  return (error as { status?: unknown }).status === 403 && !isWindowLocked(error);
}

/** 400 `cursor_expired`: a list or group cursor's window left the 24 h tier; reload the first page. */
export function isCursorExpired(error: unknown): boolean {
  if (!error || typeof error !== "object") return false;
  const e = error as { status?: unknown; body?: unknown };
  if (e.status !== 400) return false;
  const body = e.body;
  return Boolean(body && typeof body === "object" && "error" in body && (body as { error: unknown }).error === "cursor_expired");
}

// ---------- Words ----------------------------------------------------------

export const WINDOW_LABEL: Record<ChangesWindowOption, string> = {
  "1h": "1 h",
  "24h": "24 h",
  "7d": "7 d",
};

export const ACTOR_LABEL: Record<ActorClass, string> = {
  // The API value stays `pipeline`; it covers pipelines, web-identity roles,
  // roles a service acts through, and SDK callers, so the page says Automation.
  pipeline: "Automation",
  person: "Person",
  aws_service: "AWS service",
  unknown: "Unknown",
};

const REASON_TEXT: Record<GapReason, string> = {
  cloudtrail_denied: "CloudTrail not readable",
  throttled: "CloudTrail throttled the collector",
  too_busy: "too many events to fetch in time",
  collector_down: "the collector was not running",
  not_checked: "not checked yet",
  before_connect: "before the account was connected",
  beyond_retention: "older than the kept change history",
  region_not_enabled: "Region not enabled in AWS",
  credentials_invalid: "credentials not valid",
  account_disconnected: "account disconnected",
  assume_role_failed: "could not assume the reader role",
  account_mismatch: "the credentials belong to another account",
  arriving: "newest minutes not settled yet",
  global_unknown: "global changes (IAM, Route 53, CloudFront, …) unknown",
};

// Reasons a service is left out of a sync or compare, worded as Settings
// words them so the two pages never disagree.
const SERVICE_REASON_TEXT: Record<string, string> = {
  not_on_plan: "not on this support plan",
  not_available_in_region: "not offered in this Region",
  capped: "not all read",
};

export function gapReasonText(reason: string | null | undefined): string {
  if (!reason) return "reason not given";
  return (REASON_TEXT as Record<string, string>)[reason] ?? SERVICE_REASON_TEXT[reason] ?? "reason not given";
}

/** Reasons that cover a whole account, drawn as their own row under the actor rows. */
const ACCOUNT_REASONS = new Set<string>([
  "cloudtrail_denied",
  "credentials_invalid",
  "account_disconnected",
  "assume_role_failed",
  "account_mismatch",
  "not_checked",
]);

export function isAccountGap(g: ChangesGap): boolean {
  return ACCOUNT_REASONS.has(g.reason);
}

/** Gaps drawn as hatched bands across the actor rows. */
export function isBandGap(g: ChangesGap): boolean {
  return !ACCOUNT_REASONS.has(g.reason) && g.reason !== "region_not_enabled" && g.reason !== "global_unknown" && g.reason !== "arriving";
}

export function fmtClock(iso: string | null | undefined): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return format(d, "HH:mm");
}

export function fmtDayClock(iso: string | null | undefined): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return format(d, "MMM d HH:mm");
}

/** The detail a gap carries beyond its reason, e.g. the event rate of a `too_busy` stretch. */
export function gapDetail(g: Pick<ChangesGap, "reason" | "events_per_hour">): string | null {
  if (g.reason === "too_busy" && typeof g.events_per_hour === "number" && g.events_per_hour > 0) {
    return `~${fmtCount(Math.round(g.events_per_hour))} events/hour, above the collector's budget`;
  }
  return null;
}

/** "N late events caught; …", or null when none were. */
export function lateArrivalsText(n: number | null | undefined): string | null {
  if (!n || n <= 0) return null;
  return `${fmtCount(n)} late ${n === 1 ? "event" : "events"} caught; raise connectors.aws.collect.changes.overlap if this keeps growing`;
}

/** Skew under 60 s is within CloudTrail's own jitter and not worth a note. */
export function clockSkewText(seconds: number | null | undefined): string | null {
  if (typeof seconds !== "number" || !Number.isFinite(seconds) || Math.abs(seconds) < 60) return null;
  return `This server's clock is ${fmtCount(Math.round(Math.abs(seconds)))} s off AWS time`;
}

/** "1 change", "3 changes". */
export function changesText(n: number): string {
  return `${fmtCount(n)} ${n === 1 ? "change" : "changes"}`;
}

/** "1 routine update", "24 routine updates". */
export function routineText(n: number): string {
  return `${fmtCount(n)} routine ${n === 1 ? "update" : "updates"}`;
}

/** True when the scope reads more than one Region, so a row names its Region. */
export function multiRegion(view: Pick<ChangesView, "regions">): boolean {
  return (view.regions ?? []).filter((r) => r.state === "selected").length > 1;
}

/** True when Settings records the account as disconnected. */
export function isDisconnected(a: Pick<ChangesAccount, "reason"> | undefined): boolean {
  return a?.reason === "account_disconnected";
}

/**
 * "Disconnected 13:16; as of 13:01": when the account dropped and how far it
 * is known. Each time carries its date when it falls on an earlier day than
 * `ref` (the window end), the way Settings prints the same instant.
 */
export function disconnectedText(a: Pick<ChangesAccount, "as_of" | "disconnected_at">, fallback: string, ref: string): string {
  const asOf = a.as_of ?? fallback;
  return `Disconnected ${fmtClockOrDay(a.disconnected_at ?? asOf, ref)}; as of ${fmtClockOrDay(asOf, ref)}`;
}

/**
 * True when a row lies past the end of its account's coverage, the only
 * case where the row carries an "as of" chip.
 */
export function pastCoverage(item: Pick<ChangeItem, "time">, a: Pick<ChangesAccount, "as_of"> | undefined): boolean {
  if (!a?.as_of) return false;
  return Date.parse(item.time) > Date.parse(a.as_of);
}

/**
 * Top actors as one entry per `(actor, actor_class)`: the server already
 * merges across accounts and Regions; folding again keeps an older answer
 * from showing one label twice. A label that still repeats (two classes, or
 * two masked actors that read alike) names its class, then its account.
 */
export function topActorLabels(
  view: Pick<ChangesView, "summary" | "accounts">,
  limit = 5,
): Array<{ key: string; label: string; count: number }> {
  const merged = new Map<string, TopActor>();
  for (const a of view.summary.top_actors ?? []) {
    const key = `${a.actor_class}\u0000${a.actor}`;
    const prev = merged.get(key);
    if (prev) {
      merged.set(key, { ...prev, count: prev.count + a.count, account: prev.account === a.account ? prev.account : null });
    } else {
      merged.set(key, { ...a });
    }
  }
  const top = [...merged.entries()].sort((x, y) => y[1].count - x[1].count).slice(0, limit);
  // The principal name whenever the server sends one, whatever its class;
  // "Unknown actor" only when there is none.
  const base = (a: TopActor) => (a.actor ? maskActor(a.actor) : "Unknown actor");
  const withClass = top.map(([key, a]) => {
    const name = base(a);
    const otherClass = top.some(([, t]) => base(t) === name && t.actor_class !== a.actor_class);
    return { key, a, label: otherClass ? `${name} (${ACTOR_LABEL[a.actor_class]})` : name };
  });
  const withAccount = withClass.map(({ key, a, label }) => {
    const repeats = withClass.filter((t) => t.label === label).length > 1;
    return { key, label: repeats && a.account ? `${label} · ${accountName(view, a.account)}` : label, count: a.count };
  });
  // Masking can still fold two names into one (j…@example.com): number the
  // later ones so two different actors never share a label.
  return withAccount.map((t, i) => {
    const earlier = withAccount.slice(0, i).filter((u) => u.label === t.label).length;
    return earlier > 0 ? { ...t, label: `${t.label} (${earlier + 1})` } : t;
  });
}

/**
 * Poll period for the open page. Faster while the first read runs or the
 * newest slice is still arriving, so transient states clear on their own.
 */
export function pollInterval(view: Pick<ChangesView, "sync" | "window"> | undefined): number {
  if (!view) return 30_000;
  if (view.sync.first_read_pending || hasArrivingTail(view.window)) return 10_000;
  return 30_000;
}

/**
 * The alarms for the Activity card's alarm list, one entry per alarm, each
 * with its account and Region and its start.
 */
export function alarmEntries(view: Pick<ChangesView, "alarms" | "accounts" | "window">): Array<{ alarm: ChangesAlarm; name: string; where: string; when: string }> {
  const seen = new Set<string>();
  const out: Array<{ alarm: ChangesAlarm; name: string; where: string; when: string }> = [];
  for (const a of view.alarms) {
    const where = `${accountName(view, a.account)} ${a.region}`;
    const when = a.start_known && a.start ? `from ${fmtClockOrDay(a.start, view.window.until)}` : "start unknown";
    const key = `${a.name}\u0000${where}\u0000${when}`;
    if (seen.has(key)) continue;
    seen.add(key);
    out.push({ alarm: a, name: a.name, where, when });
  }
  return out;
}

/** "checkout 5xx from 14:02": the hover name of an alarm line on the grid. */
export function alarmTitle(view: Pick<ChangesView, "window">, a: ChangesAlarm): string {
  return a.start_known && a.start ? `${a.name} from ${fmtClockOrDay(a.start, view.window.until)}` : a.name;
}

function sameLocalDay(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

/**
 * One past instant on an AWS page: HH:mm when it falls on the local day of
 * `ref` (the window end), otherwise `MMM d HH:mm`.
 */
export function fmtClockOrDay(iso: string | null | undefined, ref: string): string {
  if (!iso) return "";
  const a = new Date(iso);
  if (Number.isNaN(a.getTime())) return "";
  return sameLocalDay(a, new Date(ref)) ? fmtClock(iso) : fmtDayClock(iso);
}

/**
 * Every time span on the Changes page. The start follows `fmtClockOrDay`
 * against `ref`; the end carries its date whenever it falls on another local
 * day than the start, so a span lying wholly on an earlier day reads
 * "Nov 18 01:10–17:57" and one crossing midnight "Nov 18 23:30–Nov 19 00:00".
 * `sep` joins the two ends ("–", " to ", " and ").
 */
export function fmtSpanOrDay(from: string, to: string | null | undefined, ref: string, sep = "–"): string {
  const start = fmtClockOrDay(from, ref);
  if (!to) return start;
  return `${start}${sep}${fmtClockOrDay(to, from)}`;
}

/** "N malformed events skipped", or null when none were. */
export function droppedText(dropped: number | null | undefined): string | null {
  if (!dropped || dropped <= 0) return null;
  return `${fmtCount(dropped)} malformed ${dropped === 1 ? "event" : "events"} skipped`;
}

/** The change-list route for exactly the span `[from, to]`. */
export function changesSpanSearch(from: string, to: string, scope: { account?: string; region?: string; service?: string } = {}): string {
  return changesQueryString({ since: from, until: to, account: scope.account, region: scope.region, service: scope.service });
}

/** "3 d ago", "5 h ago", "12 min ago": how long before `now` a time was. */
export function agoText(iso: string, now: string): string {
  const mins = Math.max(0, Math.round((Date.parse(now) - Date.parse(iso)) / 60_000));
  if (mins < 60) return `${mins} min ago`;
  const hours = Math.round(mins / 60);
  if (hours < 48) return `${hours} h ago`;
  return `${Math.round(hours / 24)} d ago`;
}

/** The empty-window line from `last_change_before`, or null when the server sent none. */
export function lastChangeBeforeText(view: Pick<ChangesView, "summary" | "window">): string | null {
  const t = view.summary.last_change_before;
  if (!t) return null;
  return `Last change before this window: ${agoText(t, view.window.until)}`;
}

/** "in the last 24 h", or the exact span when the page reads a custom `since`. */
export function windowPhrase(view: Pick<ChangesView, "window">, window: ChangesWindow, custom = false): string {
  if (custom) return `between ${fmtSpanOrDay(view.window.since, view.window.until, view.window.until, " and ")}`;
  return `in the last ${WINDOW_LABEL[window]}`;
}

export function fmtCount(n: number): string {
  return n.toLocaleString("en-US");
}

/**
 * A grid cell's count: exact under 1,000, short above ("3.8k", "12k", ".4M",
 * "3M"), so it fits a 6-hour column at 1280 px. Every short form is at most
 * two digits and a unit, which is narrower than "999". The exact count stays
 * in the cell's hover and aria-label.
 */
export function fmtCellCount(n: number): string {
  if (n < 1000) return fmtCount(n);
  const k = n / 1e3;
  if (k < 9.95) return `${k.toFixed(1)}k`;
  if (k < 99.5) return `${Math.round(k)}k`;
  for (const [unit, size] of [["M", 1e6], ["B", 1e9]] as const) {
    const v = n / size;
    if (v < 0.95) return `.${Math.max(1, Math.round(v * 10))}${unit}`;
    if (v < 99.5) return `${Math.round(v)}${unit}`;
  }
  return `${Math.round(n / 1e9)}B`;
}

/** Server-side masking (H31) is the rule; this is a second guard for an email-shaped actor. */
export function maskActor(actor: string): string {
  return actor.replace(/([A-Za-z0-9._%+-])[A-Za-z0-9._%+-]*@([A-Za-z0-9.-]+\.[A-Za-z]{2,})/g, "$1…@$2");
}

export function accountName(view: Pick<ChangesView, "accounts">, id: string): string {
  return view.accounts.find((a) => a.account === id)?.name || id;
}

/** True when the data runs past `settled_until`, the tail that is still arriving (H1). */
export function hasArrivingTail(w: ChangesWindowRange): boolean {
  return Date.parse(w.settled_until) < Date.parse(w.until);
}

/** The suspect item: the server's full copy first, then the loaded page. Only when found. */
export function suspectItem(view: Pick<ChangesView, "suspect" | "items">): ChangeItem | undefined {
  if (view.suspect.status !== "found") return undefined;
  if (view.suspect.change) return view.suspect.change;
  const id = view.suspect.change_id;
  return id ? view.items.find((i) => i.id === id) : undefined;
}

/** The alarm the likely cause answers, else the first alarm in scope. */
export function causeAlarm(view: Pick<ChangesView, "suspect" | "alarms">): ChangesAlarm | undefined {
  const id = view.suspect.alarm_id;
  return (id ? view.alarms.find((a) => a.id === id) : undefined) ?? view.alarms[0];
}

/**
 * The summary card's likely-cause slot: the change, or the honest status when
 * none can be named. Null only when no alarm is in scope.
 */
export type LikelyCause =
  | { kind: "found"; item: ChangeItem | undefined }
  | { kind: "status"; alarm: string; text: string };

export function likelyCause(view: ChangesView): LikelyCause | null {
  const alarm = causeAlarm(view)?.name ?? "This alarm";
  switch (view.suspect.status) {
    case "found":
      return { kind: "found", item: suspectItem(view) };
    case "none":
      return { kind: "status", alarm, text: "no change matches this alarm" };
    case "resource_unknown":
      return { kind: "status", alarm, text: "can't tell which resource this alarm watches" };
    case "incomplete":
      return { kind: "status", alarm, text: "not enough data to tell" };
    case "start_unknown":
      return { kind: "status", alarm, text: "start time unknown, so not enough data to tell" };
    default:
      return null;
  }
}

/** Where the server looked for a likely cause, for the slot's hover text. */
export function searchedText(view: ChangesView): string | null {
  const s = view.suspect.searched;
  if (!s) return null;
  const where = s.accounts.length > 0 ? ` in ${s.accounts.map((a) => accountName(view, a)).join(", ")}` : "";
  const regions = s.regions.length > 0 ? ` (${s.regions.join(", ")})` : "";
  return `Looked at changes from ${fmtClockOrDay(s.from, view.window.until)} to ${fmtClockOrDay(s.to, view.window.until)}${where}${regions}`;
}

/** Candidates the server marks equally likely as the named one; never the named one itself. */
export function equallyLikelyIds(view: Pick<ChangesView, "suspect">): Set<string> {
  const s = view.suspect;
  if (s.status !== "found") return new Set();
  const named = s.change_id ?? s.change?.id;
  return new Set((s.candidates ?? []).filter((c) => c.tied && c.change_id !== named).map((c) => c.change_id));
}

/** "1/2": accounts in scope read in full, out of all accounts in scope. */
export function accountsFraction(view: Pick<ChangesView, "accounts">): string {
  return `${view.accounts.filter((a) => a.complete).length}/${view.accounts.length}`;
}

/**
 * True when no count in the window can be told yet: the first read is still
 * running, or every bucket is a gap, which the server sets only when no
 * account and Region pair was read for any of it. The summary numbers and the
 * grid's row totals then show a dash, not a zero.
 */
export function nothingRead(view: Pick<ChangesView, "buckets" | "sync">): boolean {
  if (view.sync.first_read_pending) return true;
  return view.buckets.length > 0 && view.buckets.every((b) => b.state === "gap");
}

/** One stretch of missing data, as the warning popover lists it. */
export interface MissingStretch {
  key: string;
  account: string;
  region: string;
  from: string;
  to: string;
  reason: string;
  missing_permission: string | null;
  detail: string | null;
}

/**
 * Every stretch of missing data in the window: the server's gaps (not the
 * unsettled tail, not Regions that are not enabled), plus an account the
 * server marks incomplete or disconnected without listing a gap for it.
 */
export function missingStretches(view: ChangesView): MissingStretch[] {
  // A disconnected account tells one story: every stretch of it reads
  // "account disconnected", whatever the collector last saw fail.
  const out: MissingStretch[] = view.gaps
    .filter((g) => g.reason !== "arriving" && g.reason !== "region_not_enabled")
    .map((g) => ({
      key: `${g.account}-${g.region}-${g.from}-${g.reason}`,
      account: g.account,
      region: g.region,
      from: g.from,
      to: g.to,
      reason: isDisconnected(view.accounts.find((a) => a.account === g.account)) ? "account_disconnected" : g.reason,
      missing_permission: g.missing_permission ?? null,
      detail: gapDetail(g),
    }));
  for (const a of view.accounts) {
    if (out.some((g) => g.account === a.account)) continue;
    if (a.as_of || isDisconnected(a)) {
      const from = a.as_of ?? view.window.since;
      out.push({ key: `acct-${a.account}`, account: a.account, region: "", from, to: view.window.until, reason: a.reason ?? "account_disconnected", missing_permission: null, detail: null });
    } else if (!a.complete) {
      out.push({ key: `acct-${a.account}`, account: a.account, region: "", from: view.window.since, to: view.window.until, reason: a.reason ?? "", missing_permission: null, detail: null });
    }
  }
  return out;
}

/** Regions in scope that are not enabled in AWS, from `regions[]` and the server's gaps. */
export function notEnabledRegions(view: Pick<ChangesView, "regions" | "gaps">): string[] {
  return [
    ...new Set([
      ...(view.regions ?? []).filter((r) => r.state === "not_enabled").map((r) => r.region),
      ...view.gaps.filter((g) => g.reason === "region_not_enabled").map((g) => g.region),
    ]),
  ];
}

/** "16:19–now", "02:10–03:40": a stretch's span, open-ended at the window end. */
export function stretchSpan(view: Pick<ChangesView, "window">, from: string, to: string): string {
  if (Date.parse(to) >= Date.parse(view.window.until)) return `${fmtClockOrDay(from, view.window.until)}–now`;
  return fmtSpanOrDay(from, to, view.window.until);
}

function whenText(view: Pick<ChangesView, "window">, from: string, to: string): string {
  const startsAtStart = Date.parse(from) <= Date.parse(view.window.since);
  const runsToEnd = Date.parse(to) >= Date.parse(view.window.until);
  if (startsAtStart && runsToEnd) return "";
  if (runsToEnd) return ` after ${fmtClockOrDay(from, view.window.until)}`;
  if (startsAtStart) return ` before ${fmtClockOrDay(to, view.window.until)}`;
  return ` from ${fmtSpanOrDay(from, to, view.window.until, " to ")}`;
}

function regionPhrase(regions: string[]): string {
  const named = regions.filter(Boolean);
  if (named.length === 0) return "";
  if (named.length <= 2) return ` in ${named.join(" and ")}`;
  return ` in ${named.length} Regions`;
}

/** One cause of missing data as a plain clause, e.g. "AWS throttled Prod in us-east-1". */
function causeClause(reason: string, name: string, regions: string[]): string {
  const where = regionPhrase(regions);
  switch (reason) {
    case "cloudtrail_denied":
      return `Versus can't read CloudTrail in ${name}`;
    case "credentials_invalid":
      return `the credentials for ${name} are not valid`;
    case "assume_role_failed":
      return `Versus could not assume the reader role in ${name}`;
    case "account_mismatch":
      return `the credentials for ${name} belong to another account`;
    case "not_checked":
      return `${name}${where} is not checked yet`;
    case "throttled":
      return `AWS throttled ${name}${where}`;
    case "too_busy":
      return `${name}${where} had too many events to fetch in time`;
    case "collector_down":
      return `the collector was not running for ${name}${where}`;
    case "before_connect":
      return `${name} was not connected yet`;
    case "beyond_retention":
      return `changes in ${name}${where} are older than the kept history`;
    case "global_unknown":
      return `global changes (IAM, Route 53, CloudFront, …) in ${name} are unknown`;
    default:
      return `changes in ${name}${where} are unknown`;
  }
}

/** Account-wide failures that only fixing the connection in AWS Settings clears. */
const RECONNECT_REASONS = new Set<string>(["account_disconnected", "credentials_invalid", "assume_role_failed", "account_mismatch"]);

/** Reasons that mean the account is still being read, not that reading failed. */
const STILL_READING_REASONS = new Set<string>(["not_checked", "arriving"]);

/**
 * True when an account cannot be read while the first read runs: a
 * connection failure, or an incomplete account with a failure reason and
 * nothing read for it yet.
 */
function failedBeforeFirstRead(a: ChangesAccount): boolean {
  if (a.reason && RECONNECT_REASONS.has(a.reason)) return true;
  return !a.complete && !!a.reason && !STILL_READING_REASONS.has(a.reason) && !a.as_of;
}

/** The summary card's warning line. `reconnect` when its first cause is fixed by reconnecting the account in Settings. */
export interface WarningLine {
  text: string;
  reconnect: boolean;
  reading: boolean;
}

/**
 * The one honesty signal for the summary numbers: shown only while the first
 * read runs or some data in the window is missing. The unsettled newest
 * minutes alone never raise it. With several causes it names the first and
 * counts the rest; the popover lists them all.
 */
export function warningLine(view: ChangesView, window: ChangesWindow, custom = false): WarningLine | null {
  // During the first read an account that cannot be read at all would keep
  // the card on "Reading…" forever, so its failure is named instead; the
  // causes are then limited to those failed accounts.
  let inScope: (account: string) => boolean = () => true;
  if (view.sync.first_read_pending) {
    const failed = new Set(view.accounts.filter(failedBeforeFirstRead).map((a) => a.account));
    if (failed.size === 0) {
      const text = custom
        ? `Reading changes between ${fmtSpanOrDay(view.window.since, view.window.until, view.window.until, " and ")}…`
        : `Reading the last ${WINDOW_LABEL[window]}…`;
      return { text, reconnect: false, reading: true };
    }
    inScope = (account) => failed.has(account);
  }
  const causes: Array<{ when: string; clause: string; reconnect: boolean }> = [];
  const stretches = missingStretches(view).filter((g) => inScope(g.account));
  const disconnected = new Set<string>();
  for (const a of view.accounts) {
    if (!inScope(a.account)) continue;
    const own = stretches.filter((g) => g.account === a.account);
    if (!isDisconnected(a) && !own.some((g) => g.reason === "account_disconnected")) continue;
    disconnected.add(a.account);
    const asOf = a.as_of ?? own.reduce((m, g) => (Date.parse(g.from) < Date.parse(m) ? g.from : m), own[0]?.from ?? view.window.since);
    causes.push({
      when: whenText(view, asOf, view.window.until),
      clause: `${accountName(view, a.account)} was disconnected at ${fmtClockOrDay(a.disconnected_at ?? asOf, view.window.until)}`,
      reconnect: true,
    });
  }
  // Global changes are read through one Region's CloudTrail, so when that
  // read fails the global stretch of the same account and time is a result
  // of the same cause, not a second one.
  const overlaps = (a: MissingStretch, b: MissingStretch) => Date.parse(a.from) < Date.parse(b.to) && Date.parse(b.from) < Date.parse(a.to);
  const consequence = (g: MissingStretch) =>
    g.reason === "global_unknown" && stretches.some((o) => o.account === g.account && o.reason !== "global_unknown" && overlaps(o, g));
  const grouped = new Map<string, MissingStretch[]>();
  for (const g of stretches) {
    if (disconnected.has(g.account) || consequence(g)) continue;
    const key = `${g.account}\u0000${g.reason}`;
    grouped.set(key, [...(grouped.get(key) ?? []), g]);
  }
  for (const gs of grouped.values()) {
    const from = gs.reduce((m, g) => (Date.parse(g.from) < Date.parse(m) ? g.from : m), gs[0].from);
    const to = gs.reduce((m, g) => (Date.parse(g.to) > Date.parse(m) ? g.to : m), gs[0].to);
    causes.push({
      when: whenText(view, from, to),
      clause: causeClause(gs[0].reason, accountName(view, gs[0].account), [...new Set(gs.map((g) => g.region))]),
      reconnect: RECONNECT_REASONS.has(gs[0].reason),
    });
  }
  if (view.summary.truncated) causes.push({ when: "", clause: "the collector hit its cap, so more changes exist", reconnect: false });
  if (view.sync.dropped > 0) causes.push({ when: "", clause: droppedText(view.sync.dropped) ?? "", reconnect: false });
  if (causes.length === 0 && (!view.complete || view.summary.total_is_lower_bound)) {
    causes.push({ when: "", clause: "part of this window is missing", reconnect: false });
  }
  if (causes.length === 0) return null;
  const [first] = causes;
  const more = causes.length - 1;
  return {
    text: `Data not complete${first.when}: ${first.clause}${more > 0 ? `, and ${fmtCount(more)} more` : ""}.`,
    reconnect: first.reconnect,
    reading: false,
  };
}

/**
 * "No changes in the last 24 h", or the exact span; said only when no warning
 * line shows (H1). With a service filter it names the services: "No RDS or
 * IAM changes in the last 24 h", or "No changes in 5 services …".
 */
export function noChangesText(
  view: Pick<ChangesView, "window"> & Partial<Pick<ChangesView, "services">>,
  window: ChangesWindow,
  custom = false,
  selected: readonly string[] = [],
): string {
  const when = windowPhrase(view, window, custom);
  if (selected.length === 0) return `No changes ${when}`;
  if (selected.length > NAMED_SERVICES) return `No changes in ${selected.length} services ${when}`;
  return `No ${servicesOr(selected, view.services)} changes ${when}`;
}

/** Where a time sits along the window, 0..100. */
export function pct(view: Pick<ChangesView, "window">, iso: string): number {
  const a = Date.parse(view.window.since);
  const b = Date.parse(view.window.until);
  const t = Date.parse(iso);
  if (!(b > a) || Number.isNaN(t)) return 0;
  return Math.min(100, Math.max(0, ((t - a) / (b - a)) * 100));
}

/** The bucket a time falls into, by UTC start. */
export function bucketIndexFor(buckets: ChangesBucket[], iso: string): number {
  const t = Date.parse(iso);
  let idx = -1;
  for (let i = 0; i < buckets.length; i++) {
    if (Date.parse(buckets[i].start) <= t) idx = i;
  }
  return idx;
}

const DAY_MS = 86_400_000;
/** The column width the grid uses for windows longer than a day. */
export const LONG_STEP_SECONDS = 21_600;

/**
 * The step the server bucketed by, in seconds: `bucket_seconds` when sent,
 * otherwise read from the first two starts (older servers), or the window
 * spread over the buckets when there is only one.
 */
export function bucketStepSeconds(view: Pick<ChangesView, "buckets" | "window" | "bucket_seconds">): number {
  if (typeof view.bucket_seconds === "number" && view.bucket_seconds > 0) return view.bucket_seconds;
  const b = view.buckets;
  if (b.length >= 2) {
    const d = (Date.parse(b[1].start) - Date.parse(b[0].start)) / 1000;
    if (d > 0) return d;
  }
  const span = (Date.parse(view.window.until) - Date.parse(view.window.since)) / 1000;
  return b.length > 0 && span > 0 ? Math.round(span / b.length) : 3600;
}

/**
 * The combined state of several buckets read as one: `gap` only when none
 * of them was read, `partial` when any part is missing, `arriving` when any
 * part is not settled yet, else `complete`.
 */
export function mergedBucketState(states: BucketState[]): BucketState {
  if (states.length > 0 && states.every((s) => s === "gap")) return "gap";
  if (states.some((s) => s === "gap" || s === "partial")) return "partial";
  if (states.some((s) => s === "arriving")) return "arriving";
  return "complete";
}

/**
 * The columns the activity grid draws, and their step in seconds. A window
 * longer than a day reads in 6 hour columns; an older server that still
 * sends hourly buckets for it has them grouped here, counts summed and the
 * state merged as the server would.
 */
export function gridBuckets(view: Pick<ChangesView, "buckets" | "window" | "bucket_seconds">): { buckets: ChangesBucket[]; stepSeconds: number } {
  const step = bucketStepSeconds(view);
  const span = Date.parse(view.window.until) - Date.parse(view.window.since);
  if (!(span > DAY_MS) || step >= LONG_STEP_SECONDS) return { buckets: view.buckets, stepSeconds: step };
  const per = Math.max(1, Math.round(LONG_STEP_SECONDS / step));
  if (per === 1) return { buckets: view.buckets, stepSeconds: step };
  const out: ChangesBucket[] = [];
  for (let i = 0; i < view.buckets.length; i += per) {
    const group = view.buckets.slice(i, i + per);
    const accounts = new Set<string>();
    for (const b of group) for (const a of b.gap_accounts ?? []) accounts.add(a);
    const merged: ChangesBucket = {
      start: group[0].start,
      pipeline: group.reduce((n, b) => n + b.pipeline, 0),
      person: group.reduce((n, b) => n + b.person, 0),
      aws_service: group.reduce((n, b) => n + b.aws_service, 0),
      unknown: group.reduce((n, b) => n + b.unknown, 0),
      errors: group.reduce((n, b) => n + b.errors, 0),
      state: mergedBucketState(group.map((b) => b.state)),
    };
    if (accounts.size > 0) merged.gap_accounts = [...accounts];
    out.push(merged);
  }
  return { buckets: out, stepSeconds: step * per };
}

/** Row label width in px at `sm` and up (`sm:w-36`). */
const ROW_HEADER_PX = 144;
/** The widest the hour columns may need together, so the grid fits the card at 1280 px. */
const COLUMNS_BUDGET_PX = 720;
/** The usual column floor, enough for a three digit count. */
const CELL_FLOOR_PX = 24;

/**
 * The grid's horizontal sizing: each column's minimum width and the table's,
 * so the columns share the card's width and never push past it. The table's
 * floor grows with the columns, so the overlays (percent of the row box)
 * stay over the hours they describe.
 */
export function gridLayout(columns: number): { cellMinPx: number; tableMinPx: number } {
  const cellMinPx = columns > 0 ? Math.min(CELL_FLOOR_PX, Math.floor(COLUMNS_BUDGET_PX / columns)) : CELL_FLOOR_PX;
  return { cellMinPx, tableMinPx: Math.max(576, ROW_HEADER_PX + columns * cellMinPx) };
}

/** Three shading steps: 1–99, 100–999, 1 000+. */
export function cellStep(n: number): 0 | 1 | 2 | 3 {
  if (n <= 0) return 0;
  if (n < 100) return 1;
  if (n < 1000) return 2;
  return 3;
}

/** Merge items from a later page without repeating a row (keyset paging, H13). */
export function appendUnique(pages: ChangeItem[][]): ChangeItem[] {
  const seen = new Set<string>();
  const out: ChangeItem[] = [];
  for (const page of pages) {
    for (const item of page) {
      if (seen.has(item.id)) continue;
      seen.add(item.id);
      out.push(item);
    }
  }
  return out;
}

/** The copyable IAM statement for a missing CloudTrail permission. */
export function permissionFix(permission: string): string {
  return JSON.stringify({ Effect: "Allow", Action: [permission], Resource: "*" }, null, 2);
}

// ---------- Limits ----------------------------------------------------------

const HOUR_MS = 3_600_000;

/** What an older server without `limits` serves: 24 h for both. */
export const DEFAULT_LIMIT_MS = 24 * HOUR_MS;

const DURATION_UNIT_MS: Record<string, number> = { h: HOUR_MS, m: 60_000, s: 1_000, ms: 1 };

/** A Go duration ("168h", "24h0m0s", "90m") in milliseconds, or null when it does not parse. */
export function parseDuration(text: string | null | undefined): number | null {
  const s = text?.trim() ?? "";
  if (!/^(?:\d+(?:\.\d+)?(?:ms|h|m|s))+$/.test(s)) return null;
  let sum = 0;
  for (const [, n, unit] of s.matchAll(/(\d+(?:\.\d+)?)(ms|h|m|s)/g)) sum += Number(n) * DURATION_UNIT_MS[unit];
  return sum;
}

/** The longest window the list serves; 24 h when the server does not say. */
export function maxWindowMs(limits: ChangesLimits | null | undefined): number {
  return parseDuration(limits?.max_window) ?? DEFAULT_LIMIT_MS;
}

/** How far back Compare can start; 24 h when the server does not say. */
export function maxCompareAgeMs(limits: ChangesLimits | null | undefined): number {
  return parseDuration(limits?.max_compare_age) ?? DEFAULT_LIMIT_MS;
}

const WINDOW_MS: Record<ChangesWindow, number> = { "1h": HOUR_MS, "24h": 24 * HOUR_MS, "7d": 168 * HOUR_MS };

/** Whether the edition serves `w`, from its `limits`. */
export function windowAllowed(w: ChangesWindow, limits: ChangesLimits | null | undefined): boolean {
  return WINDOW_MS[w] <= maxWindowMs(limits);
}

// ---------- Compare ---------------------------------------------------------

/** The Compare "from" presets inside 24 h, offered by every edition. */
export const COMPARE_PRESETS = [
  { key: "1h", hours: 1 },
  { key: "3h", hours: 3 },
  { key: "6h", hours: 6 },
  { key: "12h", hours: 12 },
  { key: "24h", hours: 24 },
] as const;

/** Presets past 24 h, offered as far back as `max_compare_age` reaches. */
const LONG_COMPARE_HOURS = [48, 72, 168, 336, 720] as const;

export const DEFAULT_COMPARE = "6h";

/**
 * The Compare "from" presets the edition allows: the 24 h set, then 2, 3,
 * 7, 14 and 30 days back while inside `maxAgeMs`, and `maxAgeMs` itself
 * (whole hours) when it falls between them.
 */
export function comparePresets(maxAgeMs: number): Array<{ key: string; hours: number }> {
  const out: Array<{ key: string; hours: number }> = COMPARE_PRESETS.map((p) => ({ key: p.key, hours: p.hours }));
  const maxHours = Math.floor(maxAgeMs / HOUR_MS);
  for (const h of LONG_COMPARE_HOURS) if (h <= maxHours) out.push({ key: `${h}h`, hours: h });
  if (maxHours > 24 && !out.some((p) => p.hours === maxHours)) out.push({ key: `${maxHours}h`, hours: maxHours });
  return out;
}

/** "(6 h ago)", "(3 d ago)": how far back a preset starts. */
export function presetAgo(hours: number): string {
  return hours > 24 && hours % 24 === 0 ? `${hours / 24} d ago` : `${hours} h ago`;
}

/** A preset key ("6h", "168h") or an RFC 3339 time, resolved against the server's `until`. */
export function resolveCompareFrom(value: string, until: string): string {
  const preset = /^(\d+)h$/.exec(value);
  if (preset) return new Date(Date.parse(until) - Number(preset[1]) * HOUR_MS).toISOString();
  return Number.isNaN(Date.parse(value)) ? resolveCompareFrom(DEFAULT_COMPARE, until) : value;
}

/**
 * The compare end as the API takes it: "" for now, or the picked time when it
 * lies after `from` and before `until`. Anything else falls back to now.
 */
export function resolveCompareTo(to: string, from: string, until: string): string {
  if (!to) return "";
  const t = Date.parse(to);
  if (Number.isNaN(t) || t <= Date.parse(from) || t >= Date.parse(until)) return "";
  return new Date(t).toISOString();
}

/**
 * The compare-to picker: Now, then each whole hour after `from` and before
 * `until`, newest first. A picked time off the hour keeps its own option.
 */
export function compareToOptions(from: string, current: string, until: string): Array<{ value: string; label: string }> {
  const f = Date.parse(from);
  const u = Date.parse(until);
  const times: number[] = [];
  for (let t = Math.ceil(u / HOUR_MS) * HOUR_MS - HOUR_MS; t > f && times.length < 24; t -= HOUR_MS) {
    if (t < u) times.push(t);
  }
  if (current && !times.includes(Date.parse(current))) times.push(Date.parse(current));
  times.sort((a, b) => b - a);
  return [
    { value: "", label: "Now" },
    ...times.map((t) => {
      const iso = new Date(t).toISOString();
      return { value: iso, label: fmtClockOrDay(iso, until) };
    }),
  ];
}
