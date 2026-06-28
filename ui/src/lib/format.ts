import { format, formatDistanceToNowStrict, parseISO } from "date-fns";

export function fmtAbs(ts?: string) {
  if (!ts) return "—";
  try {
    return format(parseISO(ts), "yyyy-MM-dd HH:mm:ss");
  } catch {
    return ts;
  }
}

export function fmtRel(ts?: string) {
  if (!ts) return "—";
  try {
    return `${formatDistanceToNowStrict(parseISO(ts))} ago`;
  } catch {
    return ts;
  }
}

export function truncate(s: string, n = 80) {
  if (!s) return "";
  return s.length <= n ? s : s.slice(0, n - 1) + "…";
}

// Untitled incidents show their short id ("#f9b0dadc") instead of a
// literal "(untitled)" — operators triaging several unnamed incidents
// need SOMETHING that tells them apart.
export function incidentTitle(i: { title?: string; id: string }) {
  const t = i.title?.trim();
  return t ? t : `#${i.id.slice(0, 8)}`;
}

// "_unknown" is the agent's internal sentinel for unattributed signals
// (pkg/agent service matcher) — never a user-facing service name.
export function displayService(s?: string | null) {
  return s && s !== "_unknown" ? s : "—";
}

// --- learned-normal value formatting (Metrics / Traces views) --------------
//
// The agent's metric/trace learners report a learned-normal value + spread per
// signal. The server already converts the raw wire value into a human unit
// (req/s, ms, %, or "" for a raw gauge) and hands us the converted numbers, so
// these helpers ONLY format — they never convert a unit. The whole point is to
// kill the old "0 ± 0 looks broken" render: a tiny value reads "< 0.01 req/s"
// and a genuinely-idle one "≈ 0 req/s (idle)", never a bare unit-less "0".

// SIGNAL_LABELS turns a raw golden-signal name into plain words for the
// "Signal" column/field. Unknown names fall back to a de-snaked version.
const SIGNAL_LABELS: Record<string, string> = {
  request_rate: "request rate",
  error_rate: "error rate",
  latency_p99: "p99 latency",
};

export function humanSignal(signal: string): string {
  const s = (signal || "").trim();
  if (!s) return "—";
  if (SIGNAL_LABELS[s]) return SIGNAL_LABELS[s];
  // "saturation:cpu" / "gauge:mem_used" → "cpu" / "mem used"
  const tail = s.includes(":") ? s.slice(s.indexOf(":") + 1) : s;
  return tail.replace(/[_:]+/g, " ").trim() || s;
}

// The smallest value step we are willing to print at 2 significant figures.
// Anything nonzero below it renders as "< 0.01 {unit}" rather than rounding
// down to a misleading zero.
const NEAR_ZERO_STEP = 0.01;

// sig2 renders a number to ~2 significant figures without scientific notation
// (180 → "180", 0.4 → "0.4", 0.456 → "0.46").
function sig2(n: number): string {
  const abs = Math.abs(n);
  if (abs === 0) return "0";
  const digits = Math.max(0, 1 - Math.floor(Math.log10(abs)));
  return n.toLocaleString(undefined, { maximumFractionDigits: digits });
}

// unitJoin attaches a unit to a number string: "%" hugs the value, every other
// unit gets a space, and an empty unit renders the bare number.
function unitJoin(num: string, unit: string): string {
  if (!unit) return num;
  if (unit === "%") return `${num}%`;
  return `${num} ${unit}`;
}

// formatNormalValue renders the learned-normal value with its unit:
//   genuinely idle (exactly 0)   → "≈ 0 req/s (idle)"
//   tiny but non-zero            → "< 0.01 req/s"
//   everything else              → "≈ 0.4 req/s"
// It never returns a bare unit-less number, so "0 ± 0" can't happen.
export function formatNormalValue(mean: number, unit: string): string {
  if (!isFinite(mean)) return "—";
  if (mean === 0) return `≈ ${unitJoin("0", unit)} (idle)`;
  if (Math.abs(mean) < NEAR_ZERO_STEP) return `< ${unitJoin("0.01", unit)}`;
  return `≈ ${unitJoin(sig2(mean), unit)}`;
}

// formatWiggle renders the ± spread. withUnit=false is the compact list form
// ("± 0.1"); true is the detail form ("± 0.1 req/s"). A spread that rounds
// below the smallest step reads "± <0.01 …", never "± 0".
export function formatWiggle(
  std: number,
  unit: string,
  withUnit: boolean,
): string {
  const u = withUnit ? unit : "";
  if (!isFinite(std) || std < NEAR_ZERO_STEP) return `± <${unitJoin("0.01", u)}`;
  return `± ${unitJoin(sig2(std), u)}`;
}

// hourlyBuckets — count timestamps into `hours` hourly buckets ending at
// `now`, oldest→newest. Sparkline source: real timestamps only; invalid
// or out-of-window stamps are dropped, never interpolated. Callers inside
// React must pass a ticking `now` (useNowTick) — capturing Date.now()
// inside a data-keyed memo freezes the window when data stops changing.
export function hourlyBuckets(
  timestamps: Array<string | null | undefined>,
  hours = 24,
  now = Date.now(),
): number[] {
  const hour = 3_600_000;
  const buckets = new Array<number>(hours).fill(0);
  for (const ts of timestamps) {
    if (!ts) continue;
    const t = new Date(ts).getTime();
    if (Number.isNaN(t)) continue;
    const age = now - t;
    if (age < 0 || age >= hours * hour) continue;
    buckets[hours - 1 - Math.floor(age / hour)]++;
  }
  return buckets;
}

export function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)}s`;
  const m = Math.floor(s / 60);
  const rem = Math.round(s - m * 60);
  return `${m}m${rem.toString().padStart(2, "0")}s`;
}

export function jsonString(v: unknown): string {
  try {
    return typeof v === "string" ? v : JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}
