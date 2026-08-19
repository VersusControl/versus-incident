import {
  ApiError,
  type SLOAdoptedSLO,
  type SLOAdoptResponse,
  type SLOBurnAlert,
  type SLOErrorBudget,
  type SLOEvidence,
  type SLORecommendation,
  type SLORecommendationSLI,
} from "@/lib/api";

// sloAdvisor.ts — pure presentation logic for the SLI/SLO auto-define page,
// extracted so it is unit-testable in isolation (the project's
// lib/*.ts + lib/*.test.ts convention).

// isLockedStatus reports whether an error is the terminal Enterprise-locked
// state: 403 (unlicensed) or 404 (OSS binary — endpoint absent). Both render
// the locked upsell, never a retry.
export function isLockedStatus(err: unknown): boolean {
  return err instanceof ApiError && (err.status === 403 || err.status === 404);
}

// SLI_TYPE_LATENCY is the one indicator family whose objective is a compliance
// RATIO measured against a millisecond threshold rather than a bare ratio.
export const SLI_TYPE_LATENCY = "latency";

// isRatio reports whether a number is a usable success/compliance ratio.
function isRatio(v: number | undefined): v is number {
  return typeof v === "number" && Number.isFinite(v) && v > 0 && v < 1;
}

// latencyThresholdMs is the millisecond ceiling a latency objective's
// compliance ratio is measured against. The platform sends the discovered
// bucket boundary in `threshold_ms`; an older server carried its millisecond
// target in `objective` instead, which is the only reading a value above 1 can
// have. Null when the SLI states no threshold at all.
export function latencyThresholdMs(s: SLORecommendationSLI): number | null {
  if (
    typeof s.threshold_ms === "number" &&
    Number.isFinite(s.threshold_ms) &&
    s.threshold_ms > 0
  ) {
    return s.threshold_ms;
  }
  if (
    s.type === SLI_TYPE_LATENCY &&
    typeof s.objective === "number" &&
    Number.isFinite(s.objective) &&
    s.objective > 1
  ) {
    return s.objective;
  }
  return null;
}

// latencyObjectiveRatio is the fraction of requests a latency objective
// requires under its threshold — what the error budget and burn rungs are
// actually derived from. Null when the SLI carries no ratio (an older server
// that only ever sent a millisecond target).
export function latencyObjectiveRatio(
  s: SLORecommendationSLI,
): number | null {
  if (isRatio(s.objective_ratio)) return s.objective_ratio;
  if (isRatio(s.objective)) return s.objective;
  return null;
}

// formatObjective renders an SLI target as the percentage it is: the
// compliance ratio for latency, the success ratio for every other family. A
// latency SLI from an older server, which had only a millisecond target, still
// renders as milliseconds rather than as a nonsense percentage.
export function formatObjective(s: SLORecommendationSLI): string {
  if (s.type === SLI_TYPE_LATENCY) {
    const ratio = latencyObjectiveRatio(s);
    if (ratio !== null) return formatPct(ratio);
    const ms = latencyThresholdMs(s);
    if (ms !== null) return formatMs(ms);
  }
  return formatPct(s.objective);
}

// formatPct renders a ratio in (0,1) as a percentage carrying just enough
// decimals to keep the nines apart: 0.999 -> "99.90%", 0.99 -> "99%".
function formatPct(objective: number): string {
  const pct = objective * 100;
  const digits = objective >= 0.999 ? 2 : pct % 1 === 0 ? 0 : 1;
  return `${pct.toFixed(digits)}%`;
}

// formatConfidence renders a 0–1 confidence as a whole percentage.
export function formatConfidence(c: number | undefined): string {
  return `${Math.round((c ?? 0) * 100)}%`;
}

// --- enriched-SLI presentation ---------------------------------------------
// Everything below renders the OPTIONAL, platform-computed enrichment on an
// SLI. Each helper returns null when the field it needs is absent so the page
// can drop the whole line rather than render "—" noise against an older
// server that only sends name/target/rationale/confidence.

// trimNum renders a number with at most `digits` decimals and no trailing
// zeros: 14.4 -> "14.4", 6.0 -> "6", 0.375 -> "0.38".
function trimNum(n: number, digits = 2): string {
  const s = n.toFixed(digits);
  return s.includes(".") ? s.replace(/0+$/, "").replace(/\.$/, "") : s;
}

// formatRatioPct renders a ratio in 0..1 as a trimmed percentage: 0.9987 ->
// "99.87%", 0.9 -> "90%".
export function formatRatioPct(ratio: number): string {
  return `${trimNum(ratio * 100)}%`;
}

// formatWindowDays renders the accounting window in words: "30 days".
export function formatWindowDays(days: number): string {
  return `${days} ${days === 1 ? "day" : "days"}`;
}

// formatMs renders a millisecond number: whole milliseconds from 10 up, and
// trimmed decimals below it so a 2.5 ms bucket boundary is not rounded away.
function formatMs(ms: number): string {
  return `${trimNum(ms, ms < 10 ? 2 : 0)} ms`;
}

// formatObjectiveHuman renders the objective the way an SRE says it out loud:
// "99.5% over 30 days", or "99% of requests under 250 ms over 30 days" for a
// latency objective, which is a compliance ratio against a threshold.
export function formatObjectiveHuman(s: SLORecommendationSLI): string {
  const window = `over ${formatWindowDays(s.window_days)}`;
  if (s.type === SLI_TYPE_LATENCY) {
    const ratio = latencyObjectiveRatio(s);
    const ms = latencyThresholdMs(s);
    if (ratio !== null && ms !== null) {
      return `${formatPct(ratio)} of requests under ${formatMs(ms)} ${window}`;
    }
    if (ms !== null) return `under ${formatMs(ms)} ${window}`;
  }
  return `${formatObjective(s)} ${window}`;
}

// formatObserved renders current attainment as the percentage it is — for
// latency that is the measured COMPLIANCE ratio, not a millisecond figure. A
// value above 1 on a latency SLI can only be an older server's observed
// milliseconds, so it is still rendered in its own units rather than as a
// four-digit percentage. Null when the server sent no observation.
export function formatObserved(s: SLORecommendationSLI): string | null {
  if (typeof s.observed !== "number" || !Number.isFinite(s.observed)) {
    return null;
  }
  if (s.type === SLI_TYPE_LATENCY && s.observed > 1) {
    return formatMs(s.observed);
  }
  return formatRatioPct(s.observed);
}

// formatObservedP99 renders the measured p99 as the supporting evidence it is:
// "observed p99 312 ms". It never stands in for the objective — the objective
// is the compliance ratio. Null when the server sent no p99.
export function formatObservedP99(s: SLORecommendationSLI): string | null {
  const v = s.observed_p99_ms;
  if (typeof v !== "number" || !Number.isFinite(v) || v <= 0) return null;
  return `observed p99 ${formatMs(v)}`;
}

// formatHeadroom renders the distance between observed and target in
// percentage points: "+0.37pp headroom", "0.3pp below target", "at target".
export function formatHeadroom(pp: number | undefined): string | null {
  if (typeof pp !== "number" || !Number.isFinite(pp)) return null;
  const mag = trimNum(Math.abs(pp));
  if (pp > 0) return `+${mag}pp headroom`;
  if (pp < 0) return `${mag}pp below target`;
  return "at target";
}

// formatBudgetMinutes turns an error-budget allowance in minutes into human
// time: 219 -> "3h 39m", 2880 -> "2d", 45 -> "45m".
export function formatBudgetMinutes(minutes: number | undefined): string | null {
  if (typeof minutes !== "number" || !Number.isFinite(minutes)) return null;
  const total = Math.round(minutes);
  if (total <= 0) return "0m";
  const days = Math.floor(total / 1440);
  const hours = Math.floor((total % 1440) / 60);
  const mins = total % 60;
  if (days > 0) return hours > 0 ? `${days}d ${hours}h` : `${days}d`;
  if (hours > 0) return mins > 0 ? `${hours}h ${mins}m` : `${hours}h`;
  return `${mins}m`;
}

// formatErrorBudget renders the whole budget line: "3h 39m per 30 days".
export function formatErrorBudget(
  budget: SLOErrorBudget | undefined,
  windowDays: number,
): string | null {
  const time = formatBudgetMinutes(budget?.minutes);
  if (time === null) return null;
  return `${time} per ${formatWindowDays(windowDays)}`;
}

// formatConsumed renders how much of the budget is already spent. Sub-1% but
// non-zero reads "<1% consumed" so a live-but-tiny burn is never shown as 0%.
export function formatConsumed(ratio: number | undefined): string | null {
  if (typeof ratio !== "number" || !Number.isFinite(ratio)) return null;
  const pct = ratio * 100;
  if (pct > 0 && pct < 1) return "<1% consumed";
  return `${Math.round(pct)}% consumed`;
}

// budgetTone maps budget consumption to a severity tone for the meter: fine
// below half, warn past it, critical once the budget is nearly gone.
export function budgetTone(ratio: number | undefined): "ok" | "warn" | "bad" {
  const r = typeof ratio === "number" && Number.isFinite(ratio) ? ratio : 0;
  if (r >= 0.9) return "bad";
  if (r >= 0.5) return "warn";
  return "ok";
}

// clampPercent turns a 0..1 ratio into a 0..100 bar width.
export function clampPercent(ratio: number | undefined): number {
  const r = typeof ratio === "number" && Number.isFinite(ratio) ? ratio : 0;
  return Math.max(0, Math.min(100, Math.round(r * 100)));
}

// formatGoDuration renders a Go duration string the way a human says it:
// "1h0m0s" -> "1h", "5m0s" -> "5m", "1h30m0s" -> "1h 30m". An already-clean
// value passes through, and anything the Go grammar doesn't produce is
// returned untouched rather than mangled.
export function formatGoDuration(d: string): string {
  const raw = typeof d === "string" ? d.trim() : "";
  if (raw === "") return "";
  const unit = "ns|us|µs|μs|ms|s|m|h";
  if (!new RegExp(`^(\\d+(?:\\.\\d+)?(?:${unit}))+$`).test(raw)) return d;
  const parts: string[] = [];
  for (const m of raw.matchAll(new RegExp(`(\\d+(?:\\.\\d+)?)(${unit})`, "g"))) {
    if (Number(m[1]) !== 0) parts.push(`${m[1]}${m[2]}`);
  }
  return parts.length > 0 ? parts.join(" ") : "0s";
}

// formatBurnAlert renders one multiwindow burn-rate alert as the sentence an
// on-call reads: "Fast: >14.4× burn over 1h (bad ratio > 7.2%)".
export function formatBurnAlert(a: SLOBurnAlert): string {
  const label = a.name
    ? a.name.charAt(0).toUpperCase() + a.name.slice(1)
    : "Burn";
  return `${label}: >${trimNum(a.burn_rate)}× burn over ${formatGoDuration(a.long_window)} (bad ratio > ${formatRatioPct(a.bad_ratio_threshold)})`;
}

// formatBurnAlertDetail is the hover/secondary detail for a burn alert: the
// short confirmation window and the slice of budget the window spends.
export function formatBurnAlertDetail(a: SLOBurnAlert): string {
  return `Confirmed over a ${formatGoDuration(a.short_window)} short window · burns ${trimNum(a.budget_pct_per_window)}% of the budget per window`;
}

// MANUAL_ADOPTION_NOTE is the fallback copy for a refused adoption when the
// server sent no reason (an older build with no not_adoptable_reason).
export const MANUAL_ADOPTION_NOTE =
  "the platform can't track this one for you yet. Copy the query into your own SLO tooling.";

// formatNotAdoptable says why the platform won't enforce an SLI, preferring the
// server's plain-language reason over the generic fallback.
export function formatNotAdoptable(s: SLORecommendationSLI): string {
  const reason = s.not_adoptable_reason?.trim();
  return reason ? reason : MANUAL_ADOPTION_NOTE;
}

// AdoptionType names which of a service's two INDEPENDENT objectives an SLI
// occupies. Adopting one never clears the other, so both can be live at once.
export type AdoptionType = "availability" | "latency";

// adoptionTypeOf maps an SLI to the objective slot it is adopted into.
export function adoptionTypeOf(s: SLORecommendationSLI): AdoptionType {
  return s.type === SLI_TYPE_LATENCY ? "latency" : "availability";
}

// adoptedDetailFor picks the recommendation-level adoption record belonging to
// this SLI: `latency_adopted` for a latency indicator, `adopted` for every
// other one. Undefined when that slot is empty or names a different indicator
// (the recommendation was regenerated after the adoption).
export function adoptedDetailFor(
  s: SLORecommendationSLI,
  rec: Pick<SLORecommendation, "adopted" | "latency_adopted"> | undefined,
): SLOAdoptedSLO | undefined {
  const slot =
    adoptionTypeOf(s) === "latency" ? rec?.latency_adopted : rec?.adopted;
  return slot && slot.sli === s.name ? slot : undefined;
}

// formatAdoptAdjustment reports, honestly, what the adopt boundary changed
// from what was recommended: the latency threshold is enforced at the
// histogram bucket boundary the compliance ratio can actually be measured
// against, which may differ from the millisecond figure the model proposed.
// Null when nothing moved.
export function formatAdoptAdjustment(
  res: SLOAdoptResponse | undefined,
): string | null {
  const to = res?.adjusted?.threshold_ms?.to;
  if (typeof to !== "number" || !Number.isFinite(to) || to <= 0) return null;
  return `Adopted at the discovered threshold ${formatMs(to)}.`;
}

// formatThresholdResync reports that the platform moved an adopted latency
// threshold on its own, so the operator is not left comparing the number they
// adopted against a different one being enforced. Null when the server records
// no re-sync (older build, or the threshold never moved).
export function formatThresholdResync(
  adopted: SLOAdoptedSLO | undefined,
): string | null {
  const resync = adopted?.threshold_resync;
  if (!resync) return null;
  const to = resync.to_ms;
  if (typeof to !== "number" || !Number.isFinite(to) || to <= 0) return null;
  const from = resync.from_ms;
  return typeof from === "number" && Number.isFinite(from) && from > 0
    ? `threshold re-synced ${formatMs(from)} → ${formatMs(to)}`
    : `threshold re-synced to ${formatMs(to)}`;
}

// pickConfidence prefers the platform's deterministic evidence score over the
// model's self-reported confidence, falling back when evidence is absent.
export function pickConfidence(s: SLORecommendationSLI): number {
  const score = s.evidence?.score;
  return typeof score === "number" && Number.isFinite(score)
    ? score
    : s.confidence;
}

// formatEvidence renders what the score is built on:
// "142 observations · confident · 3 incidents/14d".
export function formatEvidence(e: SLOEvidence | undefined): string | null {
  if (!e) return null;
  const incidents = `${e.incident_count} ${e.incident_count === 1 ? "incident" : "incidents"}/${e.window_days}d`;
  return `${e.observations} observations · ${e.confident ? "confident" : "still learning"} · ${incidents}`;
}

// sortByPriority orders services so "adopt these first" floats to the top.
// The server's priority is 0.45·incidents + 0.35·breaching + 0.20·traffic, so
// HIGHER wins and the list is sorted DESCENDING. A service without a priority
// keeps its server order behind every prioritised service. Returns a new array
// — never mutates the input.
export function sortByPriority(
  list: SLORecommendation[],
): SLORecommendation[] {
  const unranked = Number.NEGATIVE_INFINITY;
  return list
    .map((rec, i) => ({ rec, i }))
    .sort((a, b) => {
      const pa =
        typeof a.rec.priority === "number" && Number.isFinite(a.rec.priority)
          ? a.rec.priority
          : unranked;
      const pb =
        typeof b.rec.priority === "number" && Number.isFinite(b.rec.priority)
          ? b.rec.priority
          : unranked;
      return pa === pb ? a.i - b.i : pb - pa;
    })
    .map((x) => x.rec);
}

// PRIORITY_TITLE explains what the ranking is built on and which end of it is
// urgent, so nobody has to guess the direction of a raw score.
export const PRIORITY_TITLE =
  "Ranked on recent incidents, breaching objectives and traffic. The highest-ranked services are the ones to adopt first.";

// PriorityBand buckets the raw 0–1 priority score into the three levels an SRE
// actually acts on.
export type PriorityBand = "high" | "medium" | "low";

// priorityBand maps the server's priority score to a band. Null when the
// server sent no usable score, so the page can drop the pill entirely.
export function priorityBand(p: number | undefined): PriorityBand | null {
  if (typeof p !== "number" || !Number.isFinite(p)) return null;
  if (p >= 0.6) return "high";
  if (p >= 0.3) return "medium";
  return "low";
}

// PriorityLabel is the rendered priority pill: never a raw number, because a
// bare score tells an operator nothing about which way is urgent.
export interface PriorityLabel {
  band: PriorityBand;
  text: string;
  tone: "accent" | "bad" | "warn" | "default";
  title: string;
}

// priorityLabel renders the priority as a rank/band rather than a score. The
// top-ranked service is called out as "Adopt first"; everything else reads as
// a High/Medium/Low band.
export function priorityLabel(
  p: number | undefined,
  topRanked: boolean,
): PriorityLabel | null {
  const band = priorityBand(p);
  if (band === null) return null;
  if (topRanked) {
    return { band, text: "Adopt first", tone: "accent", title: PRIORITY_TITLE };
  }
  const tone = band === "high" ? "bad" : band === "medium" ? "warn" : "default";
  const text =
    band === "high"
      ? "High priority"
      : band === "medium"
        ? "Medium priority"
        : "Low priority";
  return { band, text, tone, title: PRIORITY_TITLE };
}

// BurnEnforcement says who acts on an SLI's burn-rate alerts:
//   enforced — the platform is tracking this objective and will page.
//   on-adopt — the platform can enforce it, but only once it is adopted.
//   manual   — the platform cannot enforce it; the thresholds are advice.
export type BurnEnforcement = "enforced" | "on-adopt" | "manual";

// burnEnforcement resolves which of the three the SLI is in. It honours an
// explicit `enforced` flag on the burn alerts when the server sends one, and
// otherwise falls back to the adoption state: an SLI the platform refuses to
// adopt is never claimed to page.
export function burnEnforcement(
  sli: SLORecommendationSLI,
  isAdopted: boolean,
): BurnEnforcement {
  const flags = (sli.burn_alerts ?? [])
    .map((a) => a.enforced)
    .filter((v): v is boolean => typeof v === "boolean");
  if (flags.length > 0) return flags.some(Boolean) ? "enforced" : "manual";
  if (isAdopted || sli.adopted) return "enforced";
  return sli.adoptable === true ? "on-adopt" : "manual";
}

// BurnFraming is the label + note wrapped around the burn-alert numbers.
export interface BurnFraming {
  mode: BurnEnforcement;
  label: string;
  note: string;
}

// burnAlertFraming words the burn-alert block for what will ACTUALLY happen —
// only an objective the platform enforces may claim it raises an incident.
export function burnAlertFraming(
  sli: SLORecommendationSLI,
  isAdopted: boolean,
): BurnFraming {
  const mode = burnEnforcement(sli, isAdopted);
  if (mode === "enforced") {
    return {
      mode,
      label: "Will page you",
      note: "Burn-rate alerts — these are what raise an incident when the budget is going too fast.",
    };
  }
  if (mode === "on-adopt") {
    return {
      mode,
      label: "Will page you",
      note: "Burn-rate alerts — these raise an incident once you adopt this objective.",
    };
  }
  return {
    mode,
    label: "Alert thresholds",
    note: "Recommended alert thresholds — the platform can't enforce this one, so configure these in your own alerting.",
  };
}

// normalizeCadence turns an edited cadence back into a valid Go duration:
// the humanized display renders a compound value as "36h 30m", and Go's parser
// rejects the space. Stripping ALL whitespace is safe because no Go duration
// literal contains any.
export function normalizeCadence(v: string): string {
  return typeof v === "string" ? v.replace(/\s+/g, "") : "";
}

// cadenceDirty reports whether the edited cadence differs from the current one,
// so the Save button can stay disabled when nothing changed. A value that only
// differs in formatting ("36h 30m" against the server's "36h30m0s") is not a
// change.
export function cadenceDirty(draft: string, current: string): boolean {
  const d = normalizeCadence(draft);
  if (d === "") return false;
  if (d === normalizeCadence(current)) return false;
  return formatGoDuration(d) !== formatGoDuration(current);
}

// DEFAULT_OFF_REASON is the fallback shown when the AI gate is closed but the
// server supplied no reason (kept in sync with the Go offReasonAI).
export const DEFAULT_OFF_REASON =
  "SLI/SLO auto-define is OFF: enable AI and configure an API key to use it.";

// EnableToggleState is the resolved state of the "Enable SLI/SLO auto-define"
// toggle. `checked` reflects the persisted per-org feature flag; `disabled` is
// true (with a `reason`) when the AI hard gate is closed, so a user cannot turn
// the feature on until AI is enabled and an API key is configured.
export interface EnableToggleState {
  checked: boolean;
  disabled: boolean;
  reason?: string;
}

// enableToggleState gates the feature toggle on the AI hard gate: when AI is OFF
// the toggle is DISABLED and shows the off-reason (the user must configure AI
// first); when AI is ON the toggle is interactive. `checked` always reflects the
// persisted feature flag (which may be true even while AI is off, e.g. AI was
// later turned off) so the UI never misrepresents stored state.
export function enableToggleState(
  aiEnabled: boolean,
  featureEnabled: boolean,
  offReason?: string,
): EnableToggleState {
  if (!aiEnabled) {
    return {
      checked: featureEnabled,
      disabled: true,
      reason: offReason || DEFAULT_OFF_REASON,
    };
  }
  return { checked: featureEnabled, disabled: false };
}
