import { awsErrorText } from "@/lib/api";
import type {
  FinOpsBreakdownOption,
  FinOpsBridgeBody,
  FinOpsBridgeRow,
  FinOpsComparison,
  FinOpsDateRange,
  FinOpsDepth,
  FinOpsForecast,
  FinOpsHeader,
  FinOpsMissingAccount,
  FinOpsWhy,
} from "@/lib/api";

// FinOps presentation rules: money and percent formats, the words the page
// uses, and the layout rules that decide which columns, filters and cards
// appear. Every number comes from the server; this module only formats and
// chooses where it goes.

export const MINUS = "−";
const MICROS = 1_000_000;

export const DEPTH_LABELS: Record<FinOpsDepth, string> = {
  summary: "Summary",
  details: "Details",
};

/** Whole currency units; cents only for amounts under 10. */
export function fmtMoney(
  micros: number,
  opts: { signed?: boolean; currency?: string } = {},
): string {
  const value = micros / MICROS;
  const abs = Math.abs(value);
  const cents = abs > 0 && abs < 10 && Math.round(abs) !== abs;
  const text = new Intl.NumberFormat("en-US", {
    style: "currency",
    currency: opts.currency || "USD",
    minimumFractionDigits: cents ? 2 : 0,
    maximumFractionDigits: cents ? 2 : 0,
  }).format(abs);
  if (value < 0 && Math.round(abs * (cents ? 100 : 1)) !== 0) return `${MINUS}${text}`;
  if (opts.signed && value > 0) return `+${text}`;
  return text;
}

/** Axis labels: "$36k", "$1,200", "$185k". */
export function fmtMoneyAxis(micros: number, currency = "USD"): string {
  const value = micros / MICROS;
  if (Math.abs(value) < 10_000) return fmtMoney(micros, { currency });
  const symbol = fmtMoney(0, { currency }).replace(/[\d.,\s]/g, "");
  const k = value / 1000;
  const digits = Number.isInteger(k) ? 0 : Math.abs(k) < 100 ? 1 : 0;
  return `${value < 0 ? MINUS : ""}${symbol}${Math.abs(k).toFixed(digits)}k`;
}

/** Signed change in percent with one decimal: "+9.1%", "−5.8%". */
export function fmtChangePct(bp: number): string {
  const pct = Math.abs(bp) / 100;
  const sign = bp > 0 ? "+" : bp < 0 ? MINUS : "";
  return `${sign}${pct.toFixed(1)}%`;
}

/** Share of a total, rounded: "35%"; negatives (credits) keep the sign. */
export function fmtShare(bp: number): string {
  const pct = Math.round(bp / 100);
  return pct < 0 ? `${MINUS}${Math.abs(pct)}%` : `${pct}%`;
}

const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
const MONTH_NAMES = [
  "January", "February", "March", "April", "May", "June",
  "July", "August", "September", "October", "November", "December",
];

/** "2026-10-09" → "Oct 9". */
export function fmtDay(day: string | undefined): string {
  if (!day) return "";
  const [, m, d] = day.split("-").map(Number);
  if (!m || !d) return day;
  return `${MONTHS[m - 1]} ${d}`;
}

/** The year a period's numbers sit in, for naming a base from another year. */
export function periodYear(period: { range?: FinOpsDateRange } | undefined): number | undefined {
  const y = Number(period?.range?.start?.split("-")[0]);
  return y || undefined;
}

function parseDay(day: string): [number, number, number] | null {
  const [y, m, d] = day.split("-").map(Number);
  return y && m && d ? [y, m, d] : null;
}

/**
 * "Oct 1–9" when a range covers only part of one calendar month (the
 * base of a month to date), else "". When the range sits in another year
 * than the period ("same days last year") the year is named: "Nov 1–9,
 * 2025". A full month or a span across months returns "".
 */
export function partialMonthLabel(range: FinOpsDateRange | undefined, year?: number): string {
  if (!range?.start || !range.end) return "";
  const s = parseDay(range.start);
  const e = parseDay(range.end);
  if (!s || !e) return "";
  const [ys, ms, ds] = s;
  const [ye, me, de] = e;
  if (ys !== ye || ms !== me) return "";
  const last = new Date(Date.UTC(ys, ms, 0)).getUTCDate();
  if (ds === 1 && de === last) return "";
  const days = ds === de ? `${MONTHS[ms - 1]} ${ds}` : `${MONTHS[ms - 1]} ${ds}–${de}`;
  return year && year !== ys ? `${days}, ${ys}` : days;
}

/** "November 2025" for a whole calendar month in another year than the period, else "". */
function otherYearMonth(range: FinOpsDateRange | undefined, year?: number): string {
  if (!year || !range?.start || !range.end) return "";
  const s = parseDay(range.start);
  const e = parseDay(range.end);
  if (!s || !e || s[0] === year || s[0] !== e[0] || s[1] !== e[1]) return "";
  return `${MONTH_NAMES[s[1] - 1]} ${s[0]}`;
}

/**
 * The name of the comparison base: "October total" for a whole month, the
 * real days ("Oct 1–9") when the month to date is compared with the same
 * days of an earlier month, so a slice never reads as a full bill, and the
 * year when the base sits in another year ("November 2025 total").
 */
export function compareBaseName(compare: Pick<FinOpsComparison, "short" | "range"> | undefined, year?: number): string {
  if (!compare) return "";
  const month = otherYearMonth(compare.range, year);
  return partialMonthLabel(compare.range, year) || `${month || compare.short} total`;
}

/** The base row of a bridge: the day range for a partial month, else the month. */
export function compareRowName(compare: Pick<FinOpsComparison, "short" | "range"> | undefined, year?: number): string {
  if (!compare) return "";
  return partialMonthLabel(compare.range, year) || otherYearMonth(compare.range, year) || compare.short;
}

/** "2026-11" → "November". */
export function monthName(key: string | undefined): string {
  if (!key) return "";
  const m = Number(key.split("-")[1]);
  return MONTH_NAMES[m - 1] ?? key;
}

/** "Oct 9 14:02" in UTC so every viewer reads the same time as the source. */
export function fmtStamp(iso: string | undefined): string {
  if (!iso) return "";
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return "";
  const hh = String(t.getUTCHours()).padStart(2, "0");
  const mm = String(t.getUTCMinutes()).padStart(2, "0");
  return `${MONTHS[t.getUTCMonth()]} ${t.getUTCDate()} ${hh}:${mm}`;
}

/** "08:00" (UTC) for the freshness label. */
export function fmtClock(iso: string | undefined): string {
  if (!iso) return "";
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return "";
  return `${String(t.getUTCHours()).padStart(2, "0")}:${String(t.getUTCMinutes()).padStart(2, "0")}`;
}

// ---------------------------------------------------------------------------
// Words
// ---------------------------------------------------------------------------

export type ChangeTone = "up" | "down" | "same";

export function changeTone(direction: string | undefined): ChangeTone {
  return direction === "up" ? "up" : direction === "down" ? "down" : "same";
}

export const STATUS_WORDS: Record<string, string> = {
  open: "Open",
  expected: "Expected",
  resolved: "Resolved",
};

export const EFFORT_WORDS: Record<string, string> = {
  minutes: "Minutes",
  hours: "Hours",
  days: "Days",
};

export const RISK_WORDS: Record<string, string> = {
  none: "No risk",
  low: "Low risk",
  needs_testing: "Needs testing",
};

export const CAUSE_WORDS: Record<string, string> = {
  new_resource: "New",
  removed_resource: "Removed",
  usage_up: "Used more",
  usage_down: "Used less",
  price_change: "Price changed",
  discount_change: "Discount changed",
  one_off: "One-time charge",
  new_account: "New account",
  other: "Everything else",
};

export const CONFIDENCE_WORDS: Record<string, string> = {
  likely: "Likely",
  possible: "Possible",
};

/** "4×" from a usage signal; empty when the ratio is not meaningful. */
export function signalRatio(before: number, after: number): string {
  if (before <= 0 || after <= before) return "";
  const r = after / before;
  return r >= 3 ? `${Math.round(r)}×` : `${Math.round(r * 10) / 10}×`;
}

/** Usage figures are micro-units of the signal's unit: 600000 → "0.6". */
export function fmtSignal(micros: number): string {
  const v = micros / MICROS;
  return Number.isInteger(v) ? String(v) : v.toFixed(v < 10 ? 1 : 0);
}

const SIGNAL_UNITS: Record<string, string> = {
  hrs: "hours",
  hours: "hours",
  "gb-mo": "GB-months",
  gb: "GB",
  requests: "requests",
  lcu: "LCUs",
};

/**
 * The usage signal's label with its unit in one place, so the label and the
 * figures cannot disagree: "instances billed per day" with unit "Hrs/day"
 * reads "instances, hours per day" and the figures carry no unit.
 */
export function usageSignalLabel(label: string, unit: string): string {
  const group = label.replace(/\s+billed per day$/i, "").trim();
  const raw = unit.replace(/\/day$/i, "").trim();
  const word = SIGNAL_UNITS[raw.toLowerCase()] ?? raw;
  if (!word) return label;
  return group ? `${group}, ${word} per day` : `${word} per day`;
}

/** Amounts that print as $0.00 (under half a cent) carry nothing to read. */
export function isZeroAmount(micros: number): boolean {
  return Math.abs(micros) < 5_000;
}

const SAVINGS_CHECKS: Record<string, string> = {
  unattached_volume: "unattached volumes",
  empty_load_balancer: "idle load balancers",
  stopped_instance_volumes: "disks of stopped machines",
  old_snapshot: "old snapshots",
  unused_ip: "unused IP addresses",
  network_path: "private endpoints",
  storage_class: "storage classes",
  commitment: "discount coverage",
  rightsize: "rightsizing",
};

const SAVINGS_GAP_REASONS: Record<string, string> = {
  not_collected: "not collected",
  provider_unavailable: "AWS recommendations off",
  no_usage_metrics: "no usage metrics",
  needs_usage_detail: "needs usage detail (Organization)",
};

/** "Not checked: old snapshots — not collected". */
export function savingsGapText(gap: { check: string; reason: string }): string {
  const check = SAVINGS_CHECKS[gap.check] ?? gap.check.replace(/_/g, " ");
  const reason = SAVINGS_GAP_REASONS[gap.reason] ?? gap.reason.replace(/_/g, " ");
  return `Not checked: ${check} — ${reason}`;
}

export const BASIS_WORDS: Record<string, string> = {
  billed: "",
  provider: "AWS estimate",
  list_price: "list price",
};

/** "$1,520", "up to $610", or "up to $610 a month, before other savings". */
export function fmtSaving(
  s: { monthly_micros: number; up_to?: boolean; before_other_savings?: boolean },
  currency?: string,
): string {
  const amount = fmtMoney(s.monthly_micros, { currency });
  if (s.before_other_savings) return `up to ${amount} a month, before other savings`;
  return s.up_to ? `up to ${amount}` : amount;
}

/**
 * The savings card when only commitments were found: the commitment line on
 * its own ("Up to $441 a month, before other savings"), never "None found"
 * or a $0 total. "" when there is a counted saving or no commitment.
 */
export function commitmentOnlyLine(
  total: { count: number; commitment_monthly_micros?: number; commitment_count?: number },
  currency?: string,
): string {
  if (total.count > 0 || !total.commitment_count || !total.commitment_monthly_micros) return "";
  return `Up to ${fmtMoney(total.commitment_monthly_micros, { currency })} a month, before other savings`;
}

/** "or: Stop reporting-db outside working hours $90" — never summed. */
export function alternativeLines(
  s: { alternatives?: Array<{ title: string; monthly_micros: number }> },
  currency?: string,
): string[] {
  return (s.alternatives ?? []).map((a) => `or: ${a.title} ${fmtMoney(a.monthly_micros, { currency })}`);
}

export const GROUP_LABELS: Record<string, string> = {
  account_group: "Group",
  account: "Account",
  region: "Region",
  team: "Team",
  service: "Service",
};

const MISSING_REASONS: Record<string, string> = {
  denied: "access denied",
  ce_denied: "access denied",
  ce_unavailable: "Cost Explorer not available",
  throttled: "AWS throttled",
  timeout: "timed out",
};

export function missingAccountText(m: FinOpsMissingAccount): string {
  return `${m.name || m.id}: ${MISSING_REASONS[m.reason] ?? m.reason.replace(/_/g, " ")}`;
}

const STALE_REASONS: Record<string, string> = {
  throttled: "AWS throttled the refresh",
  timeout: "The refresh timed out",
  denied: "Access denied during the refresh",
  account_disconnected: "The account is disconnected",
  disconnected: "The account is disconnected",
  credentials_invalid: "The server's AWS credentials are not valid",
};

// Causes a scheduled retry cannot clear: someone has to fix access in AWS
// settings, so the gap line carries that action.
const ACCESS_LOSS = new Set([
  "denied",
  "account_disconnected",
  "disconnected",
  "credentials_invalid",
  "no_server_identity",
  "role_not_found",
  "trust_denied",
  "external_id_mismatch",
  "session_duration_invalid",
]);

export function staleReasonText(code: string | undefined): string {
  if (!code) return "The last refresh failed";
  if (STALE_REASONS[code]) return STALE_REASONS[code];
  const aws = awsErrorText(code);
  return aws.code === code ? aws.title : `The last refresh failed (${code})`;
}

/** The stale cause needs a fix in AWS settings rather than a retry. */
export function staleNeedsAccessFix(code: string | undefined): boolean {
  return !!code && ACCESS_LOSS.has(code);
}

/** "Nov 11 05:18 UTC" for a timestamp, "Nov 11" for a day. */
export function fmtSince(since: string | undefined): string {
  if (!since) return "";
  return since.length > 10 ? `${fmtStamp(since)} UTC` : fmtDay(since);
}

/**
 * Where the stated amount sits in a sentence, written exactly as the
 * sentence writes it ("$69.33", not the rounded "$69"): the first money
 * token within one unit of the amount. null when the sentence states none.
 */
export function statedAmountSpan(sentence: string, micros: number, currency?: string): [number, number] | null {
  if (!micros) return null;
  const symbol = fmtMoney(0, { currency }).replace(/[\d.,\s]/g, "");
  if (!symbol) return null;
  const escaped = symbol.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const re = new RegExp(`[+${MINUS}-]?${escaped}\\d[\\d,]*(?:\\.\\d+)?`, "g");
  const want = Math.abs(micros) / MICROS;
  for (const m of sentence.matchAll(re)) {
    const value = Number(m[0].replace(/[^\d.]/g, ""));
    if (Math.abs(value - want) < 1) return [m.index ?? 0, (m.index ?? 0) + m[0].length];
  }
  return null;
}

const SKIPPED_CALLS: Record<string, string> = {
  forecast: "forecast",
  coverage: "coverage",
  rightsizing: "rightsizing",
  resources: "resources",
};

/** "Request cap reached: forecast and coverage skipped". */
export function skippedCallsText(calls: string[] | undefined): string {
  if (!calls?.length) return "";
  const words = calls.map((c) => SKIPPED_CALLS[c] ?? c.replace(/_/g, " "));
  const list =
    words.length === 1
      ? words[0]
      : `${words.slice(0, -1).join(", ")} and ${words[words.length - 1]}`;
  return `Request cap reached: ${list} skipped`;
}

export type WhyKind = "found" | "none" | "incomplete" | "pending" | "not_connected";

export function whyKind(why: FinOpsWhy | undefined): WhyKind | null {
  if (!why) return null;
  switch (why.state) {
    case "found":
      return "found";
    case "none_found":
      return "none";
    case "history_incomplete":
      return "incomplete";
    case "collectors_pending":
      return "pending";
    case "account_not_connected":
      return "not_connected";
    default:
      return null;
  }
}

/** Short Why text for a bridge row; the gap names itself, never a guess. */
export function whyText(why: FinOpsWhy | undefined): string {
  switch (whyKind(why)) {
    case "found":
      return why?.text ?? "";
    case "none":
      return "No change found";
    case "incomplete":
      return "Change history incomplete";
    case "pending":
      return "No collector";
    case "not_connected":
      return "Not connected";
    default:
      return "";
  }
}

// ---------------------------------------------------------------------------
// State rules
// ---------------------------------------------------------------------------

/** The period's bill is not final: the previous month is still open. */
export function isNotFinal(h: FinOpsHeader | undefined): boolean {
  const sync = h?.sync;
  const period = h?.period;
  if (!sync?.previous_month_open || !period || period.open) return false;
  const last = sync.last_complete_day;
  if (!last) return false;
  const [y, m] = last.split("-").map(Number);
  const prev = m === 1 ? `${y - 1}-12` : `${y}-${String(m - 1).padStart(2, "0")}`;
  return period.calendar.end.startsWith(prev);
}

/** Partial days that fall inside the period being shown. */
export function partialDaysInPeriod(h: FinOpsHeader | undefined): string[] {
  const days = h?.sync?.partial_days ?? [];
  const cal = h?.period?.calendar;
  if (!cal) return [];
  return days.filter((d) => d >= cal.start && d <= cal.end);
}

/**
 * No cost could be read at all: Cost Explorer is unavailable for the single
 * cost source (closed code ce_unavailable), or no account is readable.
 */
export function hasNoCostData(h: FinOpsHeader | undefined): boolean {
  if (h && !h.enabled && h.reason === "ce_unavailable") return true;
  const sync = h?.sync;
  return Boolean(sync && sync.accounts_total > 0 && sync.accounts_readable === 0);
}

/** "2 of 3 accounts" when an account could not be read. */
export function accountsNote(h: FinOpsHeader | undefined): string {
  const sync = h?.sync;
  if (!sync || sync.accounts_readable >= sync.accounts_total) return "";
  return `${sync.accounts_readable} of ${sync.accounts_total} accounts`;
}

/** The organization view (Overview | Accounts tabs) starts at 10 accounts. */
export function isOrganizationView(h: FinOpsHeader | undefined): boolean {
  return (h?.sync?.accounts_total ?? 0) >= 10;
}

export type ForecastDisplay =
  | { kind: "amount"; micros: number; estimate: boolean }
  | { kind: "skipped" }
  | { kind: "no_history" };

export function forecastDisplay(f: FinOpsForecast): ForecastDisplay {
  if (f.source === "provider") return { kind: "amount", micros: f.amount_micros, estimate: false };
  if (f.source === "estimate") return { kind: "amount", micros: f.amount_micros, estimate: true };
  if (f.reason === "forecast_skipped") return { kind: "skipped" };
  return { kind: "no_history" };
}

export type SpendByLayout =
  | { kind: "none" }
  | { kind: "single"; groupBy: string }
  | {
      kind: "switch";
      defaultGroupBy: string;
      choices: Array<{ groupBy: string; label: string; disabledReason?: string }>;
    };

const SPEND_BY_ORDER = ["account_group", "account", "region", "team"];
const TAG_REASONS = new Set(["no_team_tag", "tag_not_active", "same_tag"]);

/**
 * The "Spend by" breakdown rule: an option is offered only when it splits
 * the cost (two or more values, the largest under 95%); the server judges
 * that and marks the option available. No option → the card is removed; one
 * → the card is titled with it and has no switch; two or more → a switch.
 * An unavailable Account or Region is removed, never shown disabled. Team
 * stays in the switch, disabled with its reason, when tags are missing or
 * uniform, so one qualifying option plus a locked Team still shows the
 * switch and says why there is no team split.
 */
export function spendByLayout(
  options: FinOpsBreakdownOption[] | null | undefined,
  defaultGroupBy: string | undefined,
): SpendByLayout {
  const byGroup = new Map((options ?? []).map((o) => [o.group_by, o]));
  const available = SPEND_BY_ORDER.filter((g) => byGroup.get(g)?.available);
  if (available.length === 0) return { kind: "none" };
  const teamLocked = Boolean(byGroup.get("team")?.reason && TAG_REASONS.has(byGroup.get("team")?.reason ?? ""));
  if (available.length === 1 && !teamLocked) return { kind: "single", groupBy: available[0] };
  const choices: Array<{ groupBy: string; label: string; disabledReason?: string }> = [];
  for (const g of SPEND_BY_ORDER) {
    const o = byGroup.get(g);
    if (!o) continue;
    if (o.available) choices.push({ groupBy: g, label: GROUP_LABELS[g] ?? g });
    else if (g === "team" && o.reason && TAG_REASONS.has(o.reason)) {
      choices.push({ groupBy: g, label: GROUP_LABELS[g], disabledReason: o.note || teamReasonText(o.reason) });
    }
  }
  const def = defaultGroupBy && available.includes(defaultGroupBy) ? defaultGroupBy : available[0];
  return { kind: "switch", defaultGroupBy: def, choices };
}

export function teamReasonText(reason: string | undefined): string {
  switch (reason) {
    case "tag_not_active":
      return "Team tag not active in billing";
    case "no_team_tag":
      return "No team tag on this cost";
    case "same_tag":
      return "All cost has the same team";
    default:
      return "";
  }
}

export type OwnerColumn = { show: true } | { show: false; note: string; setUpTags: boolean };

/**
 * The Owner column shows only when the server enables it and the rows name
 * at least two different owners, never a column of Unallocated. Otherwise
 * one muted line says why.
 */
export function ownerColumn(bridge: Pick<FinOpsBridgeBody, "owner_column" | "rows">, tagActive: boolean): OwnerColumn {
  const owners = new Set(
    (bridge.rows ?? [])
      .map((r) => r.owner?.name)
      .filter((n): n is string => Boolean(n)),
  );
  const named = [...owners].filter((n) => n !== "Unallocated");
  if (bridge.owner_column.show && owners.size >= 2 && named.length >= 1) return { show: true };
  let note = bridge.owner_column.note ?? "";
  if (!note) {
    if (named.length === 1 && owners.size === 1) note = `All cost has team=${named[0]}`;
    else note = "Owner not shown: no team tag on this cost";
  }
  return { show: false, note, setUpTags: !tagActive || named.length === 0 };
}

/** "Cost only" when the row's account has no Versus role. */
export function isCostOnly(row: FinOpsBridgeRow): boolean {
  if (row.account_kind) return row.account_kind === "cost_only";
  return whyKind(row.why) === "not_connected";
}

/** The Account column shows when the driver rows span two or more accounts. */
export function accountColumn(bridge: Pick<FinOpsBridgeBody, "rows">): boolean {
  const accounts = new Set((bridge.rows ?? []).map((r) => r.account).filter((a): a is string => Boolean(a)));
  return accounts.size >= 2;
}

/** The row that matters: the largest open increase. Only it is orange. */
export function focusRowId(rows: FinOpsBridgeRow[] | null | undefined): string | null {
  let best: FinOpsBridgeRow | null = null;
  for (const r of rows ?? []) {
    if (r.status !== "open" || !r.increase_id || r.amount_micros <= 0) continue;
    if (!best || r.amount_micros > best.amount_micros) best = r;
  }
  return best?.id ?? null;
}

export function oneOffTotal(rows: FinOpsBridgeRow[] | null | undefined): number {
  return (rows ?? [])
    .filter((r) => r.cause === "one_off" && r.amount_micros > 0)
    .reduce((s, r) => s + r.amount_micros, 0);
}

// ---------------------------------------------------------------------------
// Bridge geometry
// ---------------------------------------------------------------------------

export interface BridgeBar {
  key: string;
  kind: "base" | "driver" | "other" | "total";
  row?: FinOpsBridgeRow;
  from: number;
  to: number;
  amount: number;
}

export interface BridgeGeometry {
  bars: BridgeBar[];
  lo: number;
  hi: number;
  ticks: number[];
  /** True when the axis does not start at zero (drawn with a break mark). */
  broken: boolean;
  /** Running end after the last driver; equals the total when sums agree. */
  end: number;
}

function niceStep(span: number): number {
  if (span <= 0) return MICROS;
  const raw = span / 3;
  const pow = 10 ** Math.floor(Math.log10(raw));
  for (const m of [1, 2, 2.5, 5, 10]) {
    if (raw <= m * pow) return m * pow;
  }
  return 10 * pow;
}

/**
 * Waterfall geometry: the base bar from zero, each driver from the running
 * total, then the period total from zero. The axis zooms to where the bars
 * move (with a break mark) so small drivers stay visible.
 */
export function bridgeGeometry(b: Pick<FinOpsBridgeBody, "base_micros" | "total_micros" | "rows" | "other">): BridgeGeometry {
  const bars: BridgeBar[] = [
    { key: "base", kind: "base", from: 0, to: b.base_micros, amount: b.base_micros },
  ];
  let run = b.base_micros;
  const drivers = [...(b.rows ?? [])];
  for (const r of drivers) {
    bars.push({ key: r.id, kind: "driver", row: r, from: run, to: run + r.amount_micros, amount: r.amount_micros });
    run += r.amount_micros;
  }
  if (b.other && b.other.amount_micros !== 0) {
    bars.push({ key: b.other.id || "other", kind: "other", row: b.other, from: run, to: run + b.other.amount_micros, amount: b.other.amount_micros });
    run += b.other.amount_micros;
  }
  bars.push({ key: "total", kind: "total", from: 0, to: b.total_micros, amount: b.total_micros });

  const points = bars.flatMap((x) => (x.kind === "driver" || x.kind === "other" ? [x.from, x.to] : [x.to]));
  const min = Math.min(...points);
  const max = Math.max(...points);
  const span = Math.max(max - min, Math.abs(max) * 0.02, MICROS);
  let lo = Math.max(0, min - span * 0.6);
  const step = niceStep(max - lo);
  lo = Math.floor(lo / step) * step;
  const hi = Math.ceil((max + span * 0.05) / step) * step;
  const ticks: number[] = [];
  for (let t = lo; t <= hi + 1; t += step) ticks.push(t);
  return { bars, lo, hi, ticks, broken: lo > 0, end: run };
}

/** Round ticks from zero to at least max: 0, 50, 100, 150 ($ in micros). */
export function niceTicks(max: number): number[] {
  const step = niceStep(Math.max(max, MICROS));
  const ticks: number[] = [];
  for (let t = 0; t < max + step; t += step) ticks.push(t);
  return ticks;
}

/** Horizontal position (0..1) of a value on the bridge axis. */
export function bridgeX(g: Pick<BridgeGeometry, "lo" | "hi">, v: number): number {
  if (g.hi <= g.lo) return 0;
  return Math.min(1, Math.max(0, (v - g.lo) / (g.hi - g.lo)));
}

// ---------------------------------------------------------------------------
// Scope bar
// ---------------------------------------------------------------------------

export function periodOptions(lastComplete: string | undefined): Array<{ value: string; label: string }> {
  const opts = [{ value: "mtd", label: "This month, to date" }];
  if (lastComplete) {
    let [y, m] = lastComplete.split("-").map(Number);
    for (let i = 0; i < 3; i++) {
      m -= 1;
      if (m === 0) {
        m = 12;
        y -= 1;
      }
      opts.push({ value: `${y}-${String(m).padStart(2, "0")}`, label: `${MONTH_NAMES[m - 1]} ${y}` });
    }
  }
  opts.push({ value: "last_30d", label: "Last 30 days" });
  return opts;
}

/** Region key as shown: "global" is a value of its own ("Global"). */
export function regionName(key: string): string {
  return key === "global" ? "Global" : key === "unknown" ? "Unknown region" : key;
}

export const COMPARE_OPTIONS = [
  { value: "prev_period", label: "Previous period" },
  { value: "same_period_last_year", label: "Same period last year" },
];
