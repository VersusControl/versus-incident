// FinOps UI fixture builders: one state per design page
// (local/illustration/aws-harness/09*.svg). Every view mirrors the Go view
// structs in pkg/finops/views.go and is typed with the UI's api.ts types.
// Specs import these builders; no test reads a data file.
import type {
  FinOpsAccountRow,
  FinOpsAccountsView,
  FinOpsBreakdownOption,
  FinOpsBreakdownRow,
  FinOpsBreakdownView,
  FinOpsBridgeRow,
  FinOpsBridgeView,
  FinOpsBudgetLine,
  FinOpsComparison,
  FinOpsDayAmount,
  FinOpsExpected,
  FinOpsForecast,
  FinOpsHeader,
  FinOpsIncrease,
  FinOpsIncreaseDetailView,
  FinOpsOwner,
  FinOpsPeriod,
  FinOpsReportView,
  FinOpsSaving,
  FinOpsSavingsView,
  FinOpsSummaryBody,
  FinOpsSummaryView,
  FinOpsSuspect,
  FinOpsSync,
  FinOpsWhy,
} from "../../../ui/src/lib/api";

export interface FinOpsStateFixture {
  state: string;
  design: string;
  description: string;
  route: string;
  summary: FinOpsSummaryView;
  bridge: FinOpsBridgeView;
  breakdown: Record<string, FinOpsBreakdownView>;
  savings: FinOpsSavingsView;
  increases?: Record<string, FinOpsIncreaseDetailView>;
  accounts?: FinOpsAccountsView;
}

export interface FinOpsReportFixture {
  state: string;
  design: string;
  description: string;
  route: string;
  report: FinOpsReportView;
}

// ---------------------------------------------------------------------------
// Money, days, series
// ---------------------------------------------------------------------------

const M = 1_000_000;

/** Dollars to micro-units. */
export function usd(x: number): number {
  return Math.round(x * M);
}

/** Basis points of a change, rounding an exact half to even (1562.5 → 1562). */
function changeBp(ch: number, base: number): number {
  const x = (ch / base) * 10000;
  const r = Math.round(x);
  return Math.abs(x % 1) === 0.5 && r % 2 !== 0 ? r - 1 : r;
}

function days(start: string, end: string): string[] {
  const out: string[] = [];
  const d = new Date(`${start}T00:00:00Z`);
  const e = new Date(`${end}T00:00:00Z`);
  while (d <= e) {
    out.push(d.toISOString().slice(0, 10));
    d.setUTCDate(d.getUTCDate() + 1);
  }
  return out;
}

/** A daily series summing exactly to total, with step weights by index. */
function spread(
  total: number,
  ds: string[],
  steps: Array<[number, number]> = [[0, 1]],
  partial: Array<[string, number]> = [],
): FinOpsDayAmount[] {
  const w = ds.map((_, i) => {
    let f = 1;
    for (const [start, factor] of steps) if (i >= start) f = factor;
    return f;
  });
  const s = w.reduce((a, b) => a + b, 0);
  const tm = usd(total);
  const vals = w.map((x) => Math.trunc((tm * x) / s));
  vals[vals.length - 1] += tm - vals.reduce((a, b) => a + b, 0);
  const out: FinOpsDayAmount[] = ds.map((day, i) => ({ day, amount_micros: vals[i] }));
  for (const [day, amt] of partial) out.push({ day, amount_micros: usd(amt), partial: true });
  return out;
}

const OCT_DAYS = days("2026-10-01", "2026-10-31");
const octSpark = (total: number) =>
  spread(total, OCT_DAYS, [
    [0, 1],
    [8, 1.12],
    [19, 1.16],
  ]);

// ---------------------------------------------------------------------------
// Header
// ---------------------------------------------------------------------------

const OCT: FinOpsPeriod = {
  key: "2026-10",
  label: "October 2026",
  short: "October",
  range: { start: "2026-10-01", end: "2026-10-31" },
  calendar: { start: "2026-10-01", end: "2026-10-31" },
  open: false,
  complete_days: 31,
};
const SEP: FinOpsComparison = {
  key: "prev_period",
  label: "September",
  short: "September",
  range: { start: "2026-09-01", end: "2026-09-30" },
  available: true,
};

interface HeaderOptions {
  sync?: Partial<FinOpsSync>;
  period?: FinOpsPeriod;
  compare?: FinOpsComparison;
  links?: { ui: string };
}

function header(o: HeaderOptions = {}): FinOpsHeader {
  const sync: FinOpsSync = {
    last_complete_day: "2026-11-03",
    updated_at: "2026-11-04T08:00:00Z",
    partial_days: ["2026-11-04"],
    stale: false,
    accounts_readable: 3,
    accounts_total: 3,
    partial: false,
    learning: false,
    previous_month_open: false,
    resource_data: true,
    team_tag: { key: "team", active: true, allowlisted: true },
    ...o.sync,
  };
  return {
    enabled: true,
    period: structuredClone(o.period ?? OCT),
    compare: structuredClone(o.compare ?? SEP),
    depth: "summary",
    currency: "USD",
    measure: "net_amortized",
    sync,
    links: o.links ?? { ui: "/aws/finops?period=2026-10" },
  };
}

// ---------------------------------------------------------------------------
// Bridge
// ---------------------------------------------------------------------------

function owner(name: string, rule: "tag" | "account" | "unallocated" = "tag", text?: string): FinOpsOwner {
  const ruleText =
    text ??
    { tag: `team=${name} tag`, account: "account owner from settings", unallocated: "no team tag and no account owner" }[rule];
  return { name, rule, rule_text: ruleText };
}

const unalloc = () => owner("Unallocated", "unallocated");
const found = (text: string): FinOpsWhy => ({ state: "found", text });

interface RowOptions extends Partial<FinOpsBridgeRow> {
  own?: FinOpsOwner;
  whyIs?: FinOpsWhy;
  inc?: string;
}

function row(id: string, cause: string, label: string, amt: number, service: string, serviceLabel: string, o: RowOptions = {}): FinOpsBridgeRow {
  const { own, whyIs, inc, status, ...extra } = o;
  const r: FinOpsBridgeRow = {
    id,
    cause,
    label,
    amount_micros: usd(amt),
    account: "111122223333",
    account_name: "Production",
    service,
    service_label: serviceLabel,
    record_type: "usage",
  };
  if (own) r.owner = own;
  if (inc) r.increase_id = inc;
  if (status) r.status = status;
  if (whyIs) r.why = whyIs;
  return { ...r, ...extra };
}

const expectedAsg = (): FinOpsExpected => ({
  reason: "Holiday traffic test",
  until: "2026-11-30",
  by: "minh.le",
  at: "2026-10-21T10:00:00Z",
});

function baseRows(): FinOpsBridgeRow[] {
  return [
    row("drv-nat", "usage_up", "Network traffic", 1980, "network.nat", "VPC (NAT Gateway)", {
      own: owner("checkout"), whyIs: found("checkout-api release"), status: "open", inc: "inc-nat",
      usage_type: "USE1-NatGateway-Bytes", resource_id: "nat-1a", since: "2026-10-09",
    }),
    row("drv-ec2", "usage_up", "More servers", 1240, "compute.vm", "EC2", {
      own: owner("checkout"), whyIs: found("checkout-asg 6→9"), status: "expected", inc: "inc-ec2",
      usage_type: "USE1-BoxUsage:m6i.large", expected: expectedAsg(),
    }),
    row("drv-rds", "new_resource", "New database", 610, "database.relational", "RDS", {
      own: unalloc(), whyIs: found("created by minh.le"), status: "open", inc: "inc-rds", since: "2026-10-20",
      usage_type: "USE1-InstanceUsage:db.r6g.large",
    }),
    row("drv-sp", "discount_change", "Discount covered less", 420, "billing.savings_plans", "Savings Plans", {
      own: owner("platform"), whyIs: found("coverage 78%→71%"), record_type: "discount",
    }),
    row("drv-s3", "usage_up", "Storage", 190, "storage.object", "S3", {
      own: owner("data"), whyIs: found("growth"), usage_type: "USE1-TimedStorage-ByteHrs",
    }),
    row("drv-lambda", "usage_down", "Smaller functions", -380, "compute.functions", "Lambda", {
      own: owner("payments"), whyIs: found("resized"), usage_type: "USE1-Lambda-GB-Second",
    }),
  ];
}

function other(amt: number, lines = 14): FinOpsBridgeRow {
  return { id: "drv-other", cause: "other", label: "Everything else", amount_micros: usd(amt), lines };
}

function bridge(
  base: number,
  total: number,
  rows: FinOpsBridgeRow[],
  oth: FinOpsBridgeRow | null,
  h: FinOpsHeader,
  over: Partial<FinOpsBridgeView> = {},
): FinOpsBridgeView {
  const ch = total - base;
  const all = oth ? [...rows, oth] : rows;
  const bp = base ? changeBp(ch, base) : 0;
  const direction = Math.abs(bp) < 200 ? "same" : ch > 0 ? "up" : "down";
  const b: FinOpsBridgeView = {
    ...h,
    available: true,
    base_micros: usd(base),
    total_micros: usd(total),
    change_micros: usd(ch),
    change_bp: bp,
    direction,
    rows,
    up_micros: all.filter((r) => r.amount_micros > 0).reduce((s, r) => s + r.amount_micros, 0),
    down_micros: all.filter((r) => r.amount_micros < 0).reduce((s, r) => s + r.amount_micros, 0),
    steady: false,
    min_driver_micros: usd(50),
    owner_column: { show: true },
    // The Account column shows when the driver rows span two or more accounts.
    account_column: { show: new Set(rows.map((r) => r.account).filter(Boolean)).size >= 2 },
  };
  if (oth) b.other = oth;
  return { ...b, ...over };
}

function unavailableBridge(h: FinOpsHeader, total: number, note: string): FinOpsBridgeView {
  return {
    ...h, available: false, note, base_micros: 0, total_micros: usd(total), change_micros: 0, change_bp: 0,
    direction: "", rows: [], up_micros: 0, down_micros: 0, steady: false, min_driver_micros: usd(50),
    owner_column: { show: false }, account_column: { show: false },
  };
}

// ---------------------------------------------------------------------------
// Summary
// ---------------------------------------------------------------------------

const WORD = { up: "Up", down: "Down", same: "About the same" } as const;

interface SummaryOptions {
  forecast?: FinOpsForecast;
  budget?: FinOpsBudgetLine | null | "default";
  savings?: [number, number];
  change?: "auto" | null;
  unalloc?: [number, number];
}

function summary(total: number, base: number, spark: FinOpsDayAmount[], h: FinOpsHeader, o: SummaryOptions = {}): FinOpsSummaryView {
  const forecast: FinOpsForecast = o.forecast ?? {
    month: "2026-11", amount_micros: usd(44900), low_micros: usd(43800), high_micros: usd(46100), source: "provider",
  };
  let change: FinOpsSummaryBody["change"] = null;
  if (o.change === undefined || o.change === "auto") {
    const ch = total - base;
    const bp = changeBp(ch, base);
    const d = Math.abs(bp) < 200 ? "same" : ch > 0 ? "up" : "down";
    change = { base_micros: usd(base), change_micros: usd(ch), change_bp: bp, direction: d, word: WORD[d] };
  }
  let budget: FinOpsBudgetLine | null;
  if (o.budget === undefined || o.budget === "default") {
    const b = usd(42000);
    const over = usd(total) > b;
    budget = {
      id: "bud-total", name: "Cloud total", scope: "deployment", monthly_micros: b, source: "config", read_only: true,
      actual_micros: usd(total), forecast_micros: forecast.amount_micros, forecast_source: forecast.source,
      forecast_diff_micros: forecast.amount_micros - b, status: over ? "over_budget" : "at_risk",
      status_word: over ? "Over budget" : "At risk",
    };
  } else {
    budget = o.budget as FinOpsBudgetLine | null;
  }
  const [savedAmt, savedCount] = o.savings ?? [2500, 5];
  const [ua, ub] = o.unalloc ?? [5320, 1222];
  return {
    ...h,
    answer: "",
    total_micros: usd(total),
    change,
    sparkline: spark,
    forecast,
    budget,
    savings: { monthly_micros: usd(savedAmt), count: savedCount, commitment_monthly_micros: 0, commitment_count: 0 },
    unallocated: { amount_micros: usd(ua), share_bp: ub },
    meter: { used: 7, cap: 20 },
    scope_options: { accounts: null, regions: null, teams: null },
  };
}

// ---------------------------------------------------------------------------
// Breakdowns
// ---------------------------------------------------------------------------

function brow(key: string, label: string, amt: number, shareBp: number, change?: number, extra: Partial<FinOpsBreakdownRow> = {}): FinOpsBreakdownRow {
  const r: FinOpsBreakdownRow = { key, label, amount_micros: usd(amt), share_bp: shareBp };
  if (change !== undefined) {
    r.change_micros = usd(change);
    r.base_micros = usd(amt - change);
  }
  return { ...r, ...extra };
}

const serviceRows = () => [
  brow("compute.vm", "EC2", 15100, 3470, 1240),
  brow("database.relational", "RDS", 9300, 2137, 610),
  brow("compute.containers", "Fargate", 6200, 1425, 0),
  brow("network.nat", "VPC (NAT Gateway)", 3900, 896, 1980),
  brow("storage.object", "S3", 2800, 643, 190),
  brow("other", "Other", 6220, 1429, -380, { count: 14 }),
];
const accountRows = () => [
  brow("111122223333", "Production", 27850, 6399, 2910),
  brow("777788889999", "Shared services", 10290, 2364, 590),
  brow("444455556666", "Staging", 5380, 1237, 140),
];
const teamRows = () => [
  brow("checkout", "checkout", 18100, 4159, 3220, { owner: owner("checkout") }),
  brow("platform", "platform", 9800, 2252, 420, { owner: owner("platform") }),
  brow("data", "data", 6200, 1425, 190, {
    owner: { ...owner("data"), estimated: true, rule_text: "team=data tag (estimated split)" },
    estimated: true,
  }),
  brow("payments", "payments", 4100, 942, -380, { owner: owner("payments") }),
  brow("Unallocated", "Unallocated", 5320, 1222, 190, { owner: unalloc() }),
];
const regionRows = () => [
  brow("us-east-1", "us-east-1", 41340, 9499, 3500),
  brow("eu-west-1", "eu-west-1", 1550, 356, 100),
  brow("global", "Global", 630, 145, 40),
];

function opt(groupBy: string, available = true, reason?: string, note?: string): FinOpsBreakdownOption {
  const o: FinOpsBreakdownOption = { group_by: groupBy, available };
  if (reason) o.reason = reason;
  if (note) o.note = note;
  return o;
}

const baseOptions = () => [opt("account_group", false, "no_groups"), opt("account"), opt("region", false, "dominant_value"), opt("team")];
const singleAccountOptions = () => [opt("account_group", false, "no_groups"), opt("account", false, "single_value"), opt("region"), opt("team")];

interface BreakdownOptions {
  options?: FinOpsBreakdownOption[];
  def?: string;
  service?: FinOpsBreakdownRow[];
  account?: FinOpsBreakdownRow[];
  region?: FinOpsBreakdownRow[];
  team?: FinOpsBreakdownRow[];
  extra?: Record<string, FinOpsBreakdownRow[]>;
}

function breakdowns(total: number, h: FinOpsHeader, o: BreakdownOptions = {}): Record<string, FinOpsBreakdownView> {
  const groups: Record<string, FinOpsBreakdownRow[]> = {
    service: o.service ?? serviceRows(),
    account: o.account ?? accountRows(),
    region: o.region ?? regionRows(),
    team: o.team ?? teamRows(),
    ...o.extra,
  };
  const options = o.options ?? baseOptions();
  const out: Record<string, FinOpsBreakdownView> = {};
  for (const [g, rows] of Object.entries(groups)) {
    out[g] = { ...h, group_by: g, total_micros: usd(total), rows, options, default_group_by: o.def ?? "account" };
  }
  return out;
}

// ---------------------------------------------------------------------------
// Savings
// ---------------------------------------------------------------------------

type SavingSpec = [id: string, kind: string, title: string, amt: number, effort: string, risk: string, team: string | null];

const SAVINGS_ITEMS: SavingSpec[] = [
  ["sav-vpce", "network_path", "VPC endpoints for S3, ECR in vpc-prod", 1520, "hours", "low", "checkout"],
  ["sav-sp", "commitment", "Savings Plan to 80% coverage", 610, "minutes", "none", null],
  ["sav-ebs", "idle", "4 unattached EBS volumes", 212, "minutes", "low", null],
  ["sav-rds", "rightsize", "Smaller size for reporting-db", 140, "hours", "needs_testing", "data"],
  ["sav-alb", "idle", "Idle load balancer staging-old-alb", 18, "minutes", "low", null],
];

function saving([id, kind, title, amt, effort, risk, team]: SavingSpec): FinOpsSaving {
  const s: FinOpsSaving = {
    id, kind, title, monthly_micros: usd(amt), effort, risk, account: "111122223333", region: "us-east-1",
    guide: { steps: ["Open the console page", "Apply the change"], console_path: "VPC > Endpoints", doc_url: "https://docs.aws.amazon.com/vpc/latest/privatelink/" },
    line: `${title}: saves about $${amt.toLocaleString("en-US")} a month.`,
    owner: team ? owner(team) : unalloc(),
    status: "open",
  };
  if (team) s.team = team;
  return s;
}

const SAVING_EXTRA: Record<string, Partial<FinOpsSaving>> = {
  "sav-vpce": { basis: "billed" },
  "sav-sp": { basis: "provider", up_to: true, before_other_savings: true },
  "sav-ebs": { basis: "list_price", count: 4 },
  "sav-rds": {
    basis: "provider",
    alternatives: [{ kind: "idle", title: "Stop reporting-db outside working hours", monthly_micros: usd(90), basis: "list_price" }],
  },
  "sav-alb": { basis: "list_price", count: 1 },
};

function savings(h: FinOpsHeader, items: SavingSpec[] = SAVINGS_ITEMS, gaps: FinOpsSavingsView["gaps"] = []): FinOpsSavingsView {
  const its = items.map((x) => ({ ...saving(x), ...structuredClone(SAVING_EXTRA[x[0]] ?? { basis: "billed" }) }));
  // A commitment priced before other savings is never part of the total.
  const counted = its.filter((i) => !i.before_other_savings);
  const commitments = its.filter((i) => i.before_other_savings);
  return {
    ...h,
    total: {
      monthly_micros: counted.reduce((s, i) => s + i.monthly_micros, 0),
      count: counted.length,
      commitment_monthly_micros: commitments.reduce((s, i) => s + i.monthly_micros, 0),
      commitment_count: commitments.length,
    },
    items: its,
    gaps,
  };
}

// ---------------------------------------------------------------------------
// Increase detail
// ---------------------------------------------------------------------------

function natDetail(h: FinOpsHeader, resourceData: "available" | "off" = "available"): FinOpsIncreaseDetailView {
  const series: FinOpsDayAmount[] = days("2026-09-25", "2026-11-03").map((day) => ({
    day,
    amount_micros: usd(day < "2026-10-09" ? 30 : 95),
  }));
  series.push({ day: "2026-11-04", amount_micros: usd(41), partial: true });
  const tree: FinOpsIncreaseDetailView["cause_tree"] = [
    { usage_type: "USE1-NatGateway-Bytes", label: "NAT data processing", amount_micros: usd(1960) },
    { usage_type: "USE1-DataTransfer-Out-Bytes", label: "Data transfer out", amount_micros: usd(20) },
  ];
  if (resourceData === "available") tree[0].resources = [{ id: "nat-1a", amount_micros: usd(1960) }];
  const first: FinOpsSuspect = {
    state: "found",
    summary: "ECS service checkout-api updated to task definition :42",
    actor: "github-actions-prod",
    at: "2026-10-09T14:02:00Z",
    confidence: "likely",
    confidence_word: "Likely",
    ref: "chg-checkout-api-42",
    note: "Same network path via nat-1a",
  };
  const increase: FinOpsIncrease = {
    id: "inc-nat",
    title: "Network traffic in Production",
    sentence: "Network traffic in Production went up $1,980 in October",
    account: "111122223333",
    account_name: "Production",
    account_kind: "connected",
    service: "network.nat",
    service_label: "VPC (NAT Gateway)",
    usage_type: "USE1-NatGateway-Bytes",
    started_on: "2026-10-09",
    daily_before_micros: usd(30),
    daily_now_micros: usd(95),
    monthly_impact_micros: usd(2600),
    period_change_micros: usd(1980),
    stated_change_micros: usd(1980),
    cause: "usage_up",
    owner: owner("checkout", "tag", "team=checkout tag on nat-1a"),
    suspect_change: first,
    suspects: [
      structuredClone(first),
      { state: "found", summary: "Route table rtb-prod-private-1a changed", actor: "platform-terraform", at: "2026-10-09T11:40:00Z", confidence: "possible", ref: "chg-rtb-prod-private-1a", confidence_word: "Possible" },
      { state: "found", summary: "Security group sg-checkout updated", actor: "minh.le", at: "2026-10-08T16:20:00Z", confidence: "possible", ref: "chg-sg-checkout", confidence_word: "Possible" },
    ],
    status: "open",
    alerted_at: "2026-10-12T09:00:00Z",
    alert: { channel: "alerts", sent_at: "2026-10-12T09:00:00Z" },
  };
  const d: FinOpsIncreaseDetailView = {
    ...h,
    increase,
    series,
    cause_tree: tree,
    resource_data: resourceData,
    usage_signal: { label: "NAT bytes processed", before_micros: 600000, after_micros: 2500000, unit: "TB/day" },
    related_savings: [saving(SAVINGS_ITEMS[0])],
    history: [],
  };
  if (resourceData === "off") d.resource_note = "Turn on resource-level data in Cost Explorer to see resources (AWS charges for it)";
  return d;
}

// ---------------------------------------------------------------------------
// Organization accounts
// ---------------------------------------------------------------------------

function acct(
  id: string, name: string, kind: string, status: string, group: string, cost: number, change: number | null,
  top: string | null, topAmt: number, team: string | null, extra: Partial<FinOpsAccountRow> = {},
): FinOpsAccountRow {
  const a: FinOpsAccountRow = {
    id, name, kind, status, group, cost_micros: usd(cost),
    change_micros: change !== null ? usd(change) : null,
    top_driver: top ? { label: top, amount_micros: usd(topAmt) } : null,
    sparkline_micros: Array.from({ length: 30 }, (_, i) => usd((cost / 30) * (i < 15 ? 1 : 1.1))),
  };
  if (team) a.top_team = team;
  return { ...a, ...extra };
}

const orgAccounts = (): FinOpsAccountRow[] => [
  acct("111122225555", "data-prod", "cost_only", "ok", "Production", 16400, 3900, "More servers", 3900, "data"),
  acct("111122223333", "Production", "connected", "ok", "Production", 27850, 2910, "Network traffic", 1980, "checkout"),
  acct("111122224444", "payments-prod", "cost_only", "ok", "Production", 18200, 420, "More tasks", 300, "payments"),
  acct("111122226666", "search-prod", "cost_only", "ok", "Production", 14100, -60, "Storage", 120, "search"),
  acct("111122227777", "edge-prod", "cost_only", "ok", "Production", 11200, 120, "Network traffic", 90, "edge"),
  acct("111122228888", "ml-prod", "cost_only", "ok", "Production", 9150, -1190, "Fewer servers", -1190, "data", { top_team_estimated: true }),
  acct("444455556666", "Staging", "connected", "ok", "Non-production", 5380, 140, "Storage", 140, "checkout"),
  acct("444455557777", "data-stage", "cost_only", "ok", "Non-production", 4900, -450, "Fewer servers", -450, "data"),
  acct("444400001111", "sandbox-ml", "cost_only", "new", "Sandboxes", 4800, 4800, "First cost Oct 12", 4800, "data", { first_cost: "2026-10-12" }),
  acct("444400002222", "legacy-batch", "cost_only", "closed", "Sandboxes", 620, -1100, "Less compute", -1100, null, { closed_on: "2026-11-30" }),
  acct("444400003333", "sandbox-qa", "cost_only", "not_readable", "Sandboxes", 0, null, null, 0, null, { error_code: "denied" }),
  acct("999900002222", "Log Archive", "cost_only", "ok", "Landing zone", 8900, 1240, "Log storage", 1240, "platform"),
  acct("999900001111", "management", "cost_only", "ok", "Landing zone", 1600, 360, "Unused Savings Plan", 1600, "platform"),
  acct("999900003333", "Audit", "cost_only", "ok", "Landing zone", 2600, 90, "Config rules", 90, "platform"),
  acct("777788889999", "Shared services", "connected", "ok", "Shared and platform", 10290, 900, "Transit gateway", 600, "platform"),
];

// ---------------------------------------------------------------------------
// Page assembly and the backend fields every view carries
// ---------------------------------------------------------------------------

const CONNECTED = new Set(["111122223333", "444455556666", "777788889999"]);
const LEARNING_UNTIL: Record<string, string> = {
  "09e-07-new-account": "2026-11-08",
  "09e-15-forecast-unavailable": "2026-11-03",
};

function costSource(total: number): FinOpsHeader["cost_source"] {
  if (total >= 10) return { account_id: "999900001111", kind: "management" };
  if (total === 1) return { account_id: "111122223333", kind: "account" };
  return { account_id: "111122223333", kind: "account", count: total };
}

function finishHeader(v: FinOpsHeader, state: string) {
  if (!v.enabled || !v.sync) return;
  const sync = v.sync;
  v.cost_source = costSource(sync.accounts_total);
  sync.bill_closes_on = sync.previous_month_open ? "2026-11-05" : "2026-12-05";
  sync.next_refresh_at ??= "2026-11-05T09:00:00Z";
  if (sync.learning) sync.learning_until = LEARNING_UNTIL[state] ?? "2026-11-08";
  if (sync.stale) sync.stale_detail = { since: "2026-11-01T09:00:00Z", days: ["2026-11-02", "2026-11-03"] };
}

function accountKind(r: FinOpsBridgeRow, orgKinds: Map<string, string>): string {
  if (["fee", "credit", "tax"].includes(r.record_type ?? "") || r.label === "Unused Savings Plan") return "payer";
  if (r.why?.state === "account_not_connected") return "cost_only";
  if (r.account && CONNECTED.has(r.account)) return "connected";
  return orgKinds.get(r.account ?? "") ?? "connected";
}

function finishRows(rows: FinOpsBridgeRow[] | null) {
  const kinds = new Map(orgAccounts().map((a) => [a.id, a.kind]));
  for (const r of rows ?? []) if (r.account) r.account_kind = accountKind(r, kinds);
}

function scopeOptions(doc: FinOpsStateFixture): FinOpsSummaryBody["scope_options"] {
  const bd = doc.breakdown;
  const sync = doc.summary.sync as FinOpsSync;
  let accounts: Array<{ id: string; name: string; kind: string }>;
  if (sync.accounts_total >= 10) {
    accounts = orgAccounts().map((a) => ({ id: a.id, name: a.name, kind: a.kind }));
  } else {
    accounts = (bd.account?.rows ?? []).filter((r) => r.key !== "other").map((r) => ({ id: r.key, name: r.label, kind: "connected" }));
    accounts.push(...(sync.missing_accounts ?? []).map((m) => ({ id: m.id, name: m.name, kind: "connected" })));
    if (accounts.length === 0) accounts = [{ id: "111122223333", name: "Production", kind: "connected" }];
  }
  const keys = (g: string) => (bd[g]?.rows ?? []).filter((r) => r.key !== "other").map((r) => r.key).sort();
  return { accounts, regions: keys("region"), teams: sync.team_tag.active ? keys("team") : ["Unallocated"] };
}

function page(
  state: string,
  description: string,
  summaryView: FinOpsSummaryView,
  bridgeView: FinOpsBridgeView,
  breakdown: Record<string, FinOpsBreakdownView>,
  savingsView: FinOpsSavingsView,
  more: { route?: string; increases?: Record<string, FinOpsIncreaseDetailView>; accounts?: FinOpsAccountsView } = {},
): FinOpsStateFixture {
  // Views of one page are separate JSON documents on the wire: no sharing.
  const doc: FinOpsStateFixture = structuredClone({
    state,
    design: `local/illustration/aws-harness/${state}.svg`,
    description,
    route: more.route ?? "/aws/finops",
    summary: summaryView,
    bridge: bridgeView,
    breakdown,
    savings: savingsView,
    ...(more.increases ? { increases: more.increases } : {}),
    ...(more.accounts ? { accounts: more.accounts } : {}),
  });
  const views: FinOpsHeader[] = [doc.summary, doc.bridge, doc.savings, ...Object.values(doc.breakdown), ...Object.values(doc.increases ?? {})];
  if (doc.accounts) views.push(doc.accounts);
  for (const v of views) finishHeader(v, state);
  doc.summary.scope_options = doc.summary.enabled ? scopeOptions(doc) : { accounts: null, regions: null, teams: null };
  finishRows(doc.bridge.rows);
  return doc;
}

interface StandardOptions {
  h?: FinOpsHeader;
  sum?: SummaryOptions;
  bridgeOver?: Partial<FinOpsBridgeView>;
  rows?: FinOpsBridgeRow[];
  oth?: number | null;
  base?: number;
  total?: number;
  bd?: Record<string, FinOpsBreakdownView>;
  sav?: FinOpsSavingsView;
  route?: string;
  increases?: Record<string, FinOpsIncreaseDetailView>;
  spark?: FinOpsDayAmount[];
  accounts?: FinOpsAccountsView;
}

function standard(state: string, description: string, o: StandardOptions = {}): FinOpsStateFixture {
  const h = o.h ?? header();
  const base = o.base ?? 39880;
  const total = o.total ?? 43520;
  const oth = o.oth === undefined ? -420 : o.oth;
  return page(
    state,
    description,
    summary(total, base, o.spark ?? octSpark(total), h, o.sum),
    bridge(base, total, o.rows ?? baseRows(), oth === null ? null : other(oth), h, o.bridgeOver),
    o.bd ?? breakdowns(total, h),
    o.sav ?? savings(h),
    { route: o.route, increases: o.increases, accounts: o.accounts },
  );
}

const setAmounts = (rows: FinOpsBridgeRow[], dollars: number[]) => rows.forEach((r, i) => (r.amount_micros = usd(dollars[i])));

// ---------------------------------------------------------------------------
// The states
// ---------------------------------------------------------------------------

function disabledSummary(h: Pick<FinOpsHeader, "enabled" | "reason" | "links" | "cost_source">): FinOpsSummaryView {
  return {
    ...h, answer: "", total_micros: 0, change: null, sparkline: null,
    forecast: { month: "", amount_micros: 0, source: "" }, budget: null,
    savings: { monthly_micros: 0, count: 0, commitment_monthly_micros: 0, commitment_count: 0 }, unallocated: { amount_micros: 0, share_bp: 0 }, meter: { used: 0, cap: 0 },
    scope_options: { accounts: null, regions: null, teams: null },
  };
}

function disabledPage(state: string, description: string, h: FinOpsHeader): FinOpsStateFixture {
  return page(
    state,
    description,
    disabledSummary(h),
    {
      ...h, available: false, base_micros: 0, total_micros: 0, change_micros: 0, change_bp: 0, direction: "", rows: null,
      up_micros: 0, down_micros: 0, steady: false, min_driver_micros: 0, owner_column: { show: false }, account_column: { show: false },
    },
    { service: { ...h, group_by: "service", total_micros: 0, rows: null, options: null, default_group_by: "" } },
    { ...h, total: { monthly_micros: 0, count: 0, commitment_monthly_micros: 0, commitment_count: 0 }, items: null, gaps: null },
  );
}

const NOV: FinOpsPeriod = {
  key: "mtd", label: "November 2026, to date", short: "November",
  range: { start: "2026-11-01", end: "2026-11-01" }, calendar: { start: "2026-11-01", end: "2026-11-30" }, open: true, complete_days: 1,
};
const NOV3: FinOpsPeriod = { ...NOV, range: { start: "2026-11-01", end: "2026-11-03" }, complete_days: 3 };
const CMP_OCT13: FinOpsComparison = {
  key: "prev_period", label: "Oct 1 to 3", short: "Oct 1 to 3", range: { start: "2026-10-01", end: "2026-10-03" }, available: true,
};
const novSpark3 = () => spread(4440, days("2026-11-01", "2026-11-03"), [[0, 1]], [["2026-11-04", 610]]);
const mtdLinks = { ui: "/aws/finops?period=mtd" };

function threeAccountBreakdown(total: number, h: FinOpsHeader, svc: Array<[string, string, number, number]>, otherAmt: [number, number, number], accts: Array<[number, number]>) {
  const service = svc.map(([k, l, a, s]) => brow(k, l, a, s));
  service.push(brow("other", "Other", otherAmt[0], otherAmt[1], undefined, { count: otherAmt[2] }));
  const names: Array<[string, string]> = [["111122223333", "Production"], ["777788889999", "Shared services"], ["444455556666", "Staging"]];
  const account = accts.map(([a, s], i) => brow(names[i][0], names[i][1], a, s));
  return breakdowns(total, h, { service, account });
}

function smallServices(vals: Array<[number, number]>) {
  const keys: Array<[string, string]> = [
    ["compute.vm", "EC2"], ["database.relational", "RDS"], ["compute.containers", "Fargate"],
    ["network.nat", "VPC (NAT Gateway)"], ["storage.object", "S3"],
  ];
  return keys.map(([k, l], i) => [k, l, vals[i][0], vals[i][1]] as [string, string, number, number]);
}

const SAV16: SavingSpec[] = [
  SAVINGS_ITEMS[0],
  ["sav-sp", "commitment", "Savings Plan to 80% coverage", 410, "minutes", "none", null],
  ["sav-ebs", "idle", "3 unattached EBS volumes", 160, "minutes", "low", null],
];

function rows16(): FinOpsBridgeRow[] {
  const r = baseRows();
  const out = [r[0], r[1], r[3], r[5]];
  out[2].amount_micros = usd(280);
  return out;
}

const svc16 = () => [
  brow("compute.vm", "EC2", 10900, 3914, 1240),
  brow("database.relational", "RDS", 5600, 2011),
  brow("compute.containers", "Fargate", 4200, 1508),
  brow("network.nat", "VPC (NAT Gateway)", 3000, 1077, 1980),
  brow("storage.object", "S3", 1300, 467),
  brow("other", "Other", 2850, 1023, undefined, { count: 9 }),
];
const teams16 = () => [
  brow("checkout", "checkout", 13100, 4704, undefined, { owner: owner("checkout") }),
  brow("platform", "platform", 6900, 2478, undefined, { owner: owner("platform") }),
  brow("payments", "payments", 3600, 1293, undefined, { owner: owner("payments") }),
  brow("data", "data", 1700, 610, undefined, { owner: owner("data") }),
  brow("Unallocated", "Unallocated", 2550, 915, undefined, { owner: unalloc() }),
];
const forecast28900: FinOpsForecast = { month: "2026-11", amount_micros: usd(28900), source: "provider" };

function inside(...pairs: Array<[string, number]>): FinOpsBreakdownRow[] {
  return pairs.map(([k, v]) => ({ key: k, label: k, amount_micros: usd(v), share_bp: 0 }));
}

function buildStates(): FinOpsStateFixture[] {
  const out: FinOpsStateFixture[] = [];

  // 09 — the main story
  out.push(standard("09-finops", "October up $3,640 vs September; NAT increase, 3 accounts, budget set"));
  const why = standard("09b-finops-why", "Increase detail for the NAT increase", {
    increases: { "inc-nat": natDetail(header()) },
    route: "/aws/finops/increases/inc-nat",
  });
  out.push(why);

  // 01 — cost data off
  out.push(disabledPage("09e-01-cost-off", "Cost Explorer reads are off (the default)", { enabled: false, reason: "cost_data_off", links: { ui: "/aws/finops" } }));

  // 02 — one account not readable
  const h02 = header({ sync: { accounts_readable: 2, accounts_total: 3, partial: true, missing_accounts: [{ id: "444455556666", name: "Staging", reason: "ce_denied" }], notes: ["2 of 3 accounts"] } });
  const bd02 = breakdowns(38140, h02, {
    service: [
      ...smallServices([[13200, 3461], [8200, 2150], [5300, 1390], [3500, 918], [2500, 655]]).map(([k, l, a, s]) => brow(k, l, a, s)),
      brow("other", "Other", 5440, 1426, undefined, { count: 12 }),
    ],
    account: [brow("111122223333", "Production", 27850, 7302, 2910), brow("777788889999", "Shared services", 10290, 2698, 470)],
  });
  out.push(standard("09e-02-account-not-readable", "ce:GetCostAndUsage denied for Staging", {
    h: h02, base: 34760, total: 38140, oth: -680, bd: bd02,
    sum: { forecast: { month: "2026-11", amount_micros: usd(39300), source: "provider" }, budget: null },
  }));

  // 03 — refresh failed (throttled), data stale since Nov 1
  const h03 = header({
    period: NOV,
    compare: { key: "prev_period", label: "October, same days", short: "October 1", range: { start: "2026-10-01", end: "2026-10-01" }, available: true },
    sync: { last_complete_day: "2026-11-01", updated_at: "2026-11-02T08:00:00Z", stale: true, stale_reason: "throttled", partial: true },
    links: mtdLinks,
  });
  delete h03.sync?.partial_days;
  const r03 = baseRows();
  setAmounts(r03, [86, 69, 51, 14, 6, -12]);
  out.push(standard("09e-03-refresh-failed", "AWS throttled the refresh; last good data is Nov 1", {
    h: h03, rows: r03, oth: -14, base: 1280, total: 1480, spark: [{ day: "2026-11-01", amount_micros: usd(1480) }],
    bd: threeAccountBreakdown(1480, h03, smallServices([[520, 3514], [315, 2128], [210, 1419], [135, 912], [100, 676]]), [200, 1351, 9], [[950, 6419], [350, 2365], [180, 1216]]),
  }));

  // 04 — paid-request cap reached: forecast and coverage skipped
  const h04 = header({
    period: NOV3, compare: CMP_OCT13, links: mtdLinks,
    sync: { skipped_calls: ["forecast", "coverage"], partial: true, notes: ["Skipped today to stay under 20 requests: forecast, discount coverage"] },
  });
  const all04 = baseRows();
  const r04 = [all04[0], all04[1], all04[2], all04[3], all04[5]];
  setAmounts(r04, [260, 210, 150, 40, -40]);
  delete r04[3].why;
  out.push(standard("09e-04-request-cap", "Daily paid-request cap reached; forecast and coverage skipped", {
    h: h04, rows: r04, oth: -20, base: 3840, total: 4440, spark: novSpark3(),
    sum: { forecast: { month: "2026-11", amount_micros: 0, source: "none", reason: "forecast_skipped" }, budget: null },
    bd: threeAccountBreakdown(4440, h04, smallServices([[1560, 3514], [960, 2162], [640, 1441], [420, 946], [290, 653]]), [570, 1284, 9], [[2840, 6396], [1050, 2365], [550, 1239]]),
  }));

  // 05 — team tag not activated as a cost allocation tag
  const h05 = header({ sync: { team_tag: { key: "team", active: false, allowlisted: true } } });
  const r05 = baseRows();
  r05.forEach((r) => (r.owner = unalloc()));
  out.push(standard("09e-05-no-team-tags", "team tag not active in billing; Owner hidden, Team locked", {
    h: h05, rows: r05, bridgeOver: { owner_column: { show: false, note: "Owner: no team tag" } },
    bd: breakdowns(43520, h05, {
      options: [opt("account_group", false, "no_groups"), opt("account"), opt("region", false, "dominant_value"), opt("team", false, "tag_not_active", "Activate team as a cost allocation tag; spend shows as Unallocated until then")],
      team: [brow("Unallocated", "Unallocated", 43520, 10000, undefined, { owner: unalloc() })],
    }),
  }));

  // 06 — cost went down
  const r06 = [
    row("drv-lambda", "usage_down", "Smaller functions", -1400, "compute.functions", "Lambda", { own: owner("checkout"), whyIs: found("memory lowered") }),
    row("drv-ri", "discount_change", "Reserved capacity", -1200, "database.relational", "RDS", { own: owner("platform"), whyIs: found("reserved instance"), record_type: "discount" }),
    row("drv-ec2", "usage_down", "Fewer servers", -300, "compute.vm", "EC2", { own: owner("platform"), whyIs: found("staging-asg 4→2") }),
    row("drv-fargate", "usage_down", "Fewer tasks", -150, "compute.containers", "Fargate", { own: owner("payments"), whyIs: found("payments-worker 8→6") }),
    row("drv-nat", "usage_up", "Network traffic", 290, "network.nat", "VPC (NAT Gateway)", { own: owner("checkout"), whyIs: { state: "none_found", text: "no change found" } }),
    row("drv-s3", "usage_up", "Storage", 190, "storage.object", "S3", { own: owner("data"), whyIs: found("growth") }),
  ];
  const h06 = header();
  out.push(standard("09e-06-cost-down", "October fell $2,310; decreases lead", {
    h: h06, rows: r06, oth: 260, base: 39830, total: 37520,
    sum: { forecast: { month: "2026-11", amount_micros: usd(36400), source: "provider" }, budget: null },
    bd: breakdowns(37520, h06, {
      service: [
        brow("compute.vm", "EC2", 14300, 3811), brow("database.relational", "RDS", 8100, 2159), brow("compute.containers", "Fargate", 5800, 1546),
        brow("storage.object", "S3", 2620, 698), brow("network.nat", "VPC (NAT Gateway)", 2450, 653), brow("other", "Other", 4250, 1133, undefined, { count: 12 }),
      ],
      account: [brow("111122223333", "Production", 24010, 6399), brow("777788889999", "Shared services", 8840, 2356), brow("444455556666", "Staging", 4670, 1245)],
    }),
  }));

  // 07 — brand-new account, 7 days of history, no previous month
  const h07 = header({
    period: { key: "2026-10", label: "October 2026 (Oct 25 to 31)", short: "October", range: { start: "2026-10-25", end: "2026-10-31" }, calendar: { start: "2026-10-01", end: "2026-10-31" }, open: false, complete_days: 7 },
    compare: { key: "prev_period", label: "September", short: "September", range: { start: "2026-09-01", end: "2026-09-30" }, available: false, reason: "no_full_base_period", first_full: "2026-11" },
    sync: { accounts_readable: 1, accounts_total: 1, learning: true, notes: ["Learning: increases start after 14 days", "Compare from November (first full month)"] },
  });
  out.push(page(
    "09e-07-new-account",
    "New connection, history from Oct 25; learning, no compare",
    summary(3180, 0, spread(3180, days("2026-10-25", "2026-10-31"), [[0, 0.9], [3, 1.05]]), h07, {
      change: null, budget: null, savings: [134, 2], unalloc: [640, 2013],
      forecast: { month: "2026-11", amount_micros: usd(15300), source: "estimate" },
    }),
    unavailableBridge(h07, 3180, "Needs a full previous month"),
    breakdowns(3180, h07, {
      options: singleAccountOptions(), def: "region",
      service: [
        brow("compute.vm", "EC2", 1120, 3522), brow("database.relational", "RDS", 670, 2107), brow("compute.containers", "Fargate", 450, 1415),
        brow("network.nat", "VPC (NAT Gateway)", 290, 912), brow("storage.object", "S3", 210, 660), brow("other", "Other", 440, 1384, undefined, { count: 6 }),
      ],
      account: [brow("111122223333", "Production", 3180, 10000)],
      region: [brow("us-east-1", "us-east-1", 2550, 8019), brow("eu-west-1", "eu-west-1", 500, 1572), brow("global", "Global", 130, 409)],
      team: [
        brow("checkout", "checkout", 1500, 4717, undefined, { owner: owner("checkout") }),
        brow("platform", "platform", 1040, 3270, undefined, { owner: owner("platform") }),
        brow("Unallocated", "Unallocated", 640, 2013, undefined, { owner: unalloc() }),
      ],
    }),
    savings(h07, [
      ["sav-rs", "rightsize", "Smaller size for api-worker-1", 96, "hours", "needs_testing", null],
      ["sav-gp3", "storage_class", "6 EBS volumes from gp2 to gp3", 38, "minutes", "low", null],
    ], [{ check: "old_snapshot", reason: "not_collected" }, { check: "commitment", reason: "provider_unavailable" }]),
  ));

  // 08 — no change found / history incomplete / collectors pending
  const r08 = baseRows();
  r08[2].label = "Database storage";
  r08[2].why = { state: "none_found", text: "No change found in the 48 h before" };
  r08[4].why = { state: "collectors_pending", text: "Change history arrives with collectors" };
  r08[3].why = { state: "history_incomplete", text: "Change history incomplete" };
  out.push(standard("09e-08-no-change-found", "No change in the 48 h before; S3 has no collector", { rows: r08 }));

  // 09 — resource-level data off (increase detail)
  const h09 = header({ sync: { resource_data: false } });
  out.push(standard("09e-09-resource-data-off", "Cause tree stops at usage type", {
    h: h09, route: "/aws/finops/increases/inc-nat", increases: { "inc-nat": natDetail(h09, "off") },
  }));

  // 10 — credits and a one-time fee
  const h10 = header();
  out.push(standard("09e-10-credits-fees", "Annual support fee and credits as their own drivers", {
    h: h10, total: 45420,
    rows: [
      ...baseRows(),
      row("drv-fee", "one_off", "One-time charge", 2400, "billing.support", "Support", { own: unalloc(), whyIs: found("annual support fee"), record_type: "fee" }),
      row("drv-credit", "discount_change", "Credits applied", -500, "billing.credit", "Credits", { own: unalloc(), record_type: "credit" }),
    ],
    bd: breakdowns(45420, h10, {
      service: [
        brow("compute.vm", "EC2", 15100, 3325), brow("database.relational", "RDS", 9300, 2048), brow("compute.containers", "Fargate", 6200, 1365),
        brow("network.nat", "VPC (NAT Gateway)", 3900, 859), brow("storage.object", "S3", 2800, 616), brow("billing.support", "Support, one-time", 2400, 528),
        brow("other", "Other", 6220, 1369, undefined, { count: 12 }), brow("billing.credit", "Credits applied", -500, -110),
      ],
      account: [brow("111122223333", "Production", 27850, 6132), brow("777788889999", "Shared services", 12190, 2684), brow("444455556666", "Staging", 5380, 1184)],
    }),
  }));

  // 11 — previous month's bill not closed yet
  out.push(standard("09e-11-bill-not-closed", "October bill not closed until Nov 5; report save locked", {
    h: header({ sync: { previous_month_open: true, notes: ["October may still change until AWS closes the bill on the 5th"] } }),
  }));

  // 12 — partial day (data lag)
  const h12 = header({ period: { ...NOV3 }, compare: CMP_OCT13, links: mtdLinks });
  const all12 = baseRows();
  const r12 = [all12[0], all12[1], all12[2], all12[5]];
  setAmounts(r12, [260, 210, 150, -60]);
  out.push(standard("09e-12-partial-day", "Nov 4 is partial, dashed and kept out of the total", {
    h: h12, rows: r12, oth: 40, base: 3840, total: 4440, spark: novSpark3(),
    bd: threeAccountBreakdown(4440, h12, smallServices([[1540, 3468], [950, 2140], [630, 1419], [420, 946], [290, 653]]), [610, 1374, 9], [[2840, 6396], [1050, 2365], [550, 1239]]),
  }));

  // 13 — about the same: ups and downs offset
  out.push(standard("09e-13-cost-flat", "October +0.4%: ups and downs offset", {
    rows: [
      row("drv-nat", "usage_up", "Network traffic", 1280, "network.nat", "VPC (NAT Gateway)", { own: owner("checkout"), whyIs: found("checkout-api release"), status: "expected", inc: "inc-nat", expected: expectedAsg() }),
      row("drv-ri", "discount_change", "Reserved capacity", -1200, "database.relational", "RDS", { own: owner("platform"), whyIs: found("reserved instance"), record_type: "discount" }),
      row("drv-s3", "usage_down", "Object storage", -860, "storage.object", "S3", { own: owner("checkout"), whyIs: found("lifecycle rule") }),
      row("drv-ec2", "usage_up", "Compute instances", 820, "compute.vm", "EC2", { own: owner("platform"), whyIs: found("checkout-asg 6→8"), status: "expected", inc: "inc-ec2", expected: expectedAsg() }),
    ],
    oth: 130, base: 43350,
    bridgeOver: { offset_note: "Ups and downs offset (+$2,230 / −$2,060)" },
    sum: { forecast: { month: "2026-11", amount_micros: usd(43100), source: "provider" } },
  }));

  // 14 — the same team tag on everything
  const h14 = header();
  const r14 = baseRows();
  r14.forEach((r) => (r.owner = owner("platform")));
  out.push(standard("09e-14-same-tag", "Every line carries team=platform; Owner hidden", {
    h: h14, rows: r14, bridgeOver: { owner_column: { show: false, note: "Owner hidden: all cost is team=platform" } },
    bd: breakdowns(43520, h14, {
      options: [opt("account_group", false, "no_groups"), opt("account"), opt("region", false, "dominant_value"), opt("team", false, "same_tag", "All cost has team=platform")],
      team: [brow("platform", "platform", 43520, 10000, undefined, { owner: owner("platform") })],
    }),
  }));

  // 15 — forecast is an estimate, no full previous month
  const h15 = header({
    period: { ...NOV3, label: "November 2026, to date" },
    compare: { key: "prev_period", label: "October", short: "October", range: { start: "2026-10-01", end: "2026-10-03" }, available: false, reason: "no_full_base_period", first_full: "2026-12" },
    links: mtdLinks,
    sync: { learning: true, notes: ["Compare from December (first full month)"] },
  });
  out.push(page(
    "09e-15-forecast-unavailable",
    "History from Oct 20; forecast is a 7-day estimate",
    summary(4410, 0, spread(4410, days("2026-11-01", "2026-11-03"), [[0, 1]], [["2026-11-04", 600]]), h15, {
      change: null, budget: null, forecast: { month: "2026-11", amount_micros: usd(44100), source: "estimate" },
    }),
    unavailableBridge(h15, 4410, "Compare from December (first full month)"),
    threeAccountBreakdown(4410, h15, smallServices([[1530, 3469], [930, 2109], [630, 1429], [400, 907], [280, 635]]), [640, 1451, 8], [[2820, 6395], [1060, 2404], [530, 1201]]),
    savings(h15),
  ));

  // 16 — one account
  const h16 = header({ sync: { accounts_readable: 1, accounts_total: 1 } });
  out.push(standard("09e-16-single-account", "One account; Spend by opens on Region, no Account option, no budget", {
    h: h16, rows: rows16(), oth: -210, base: 24940, total: 27850,
    sum: { forecast: forecast28900, budget: null, savings: [2090, 3] },
    sav: savings(h16, SAV16),
    bd: breakdowns(27850, h16, {
      options: singleAccountOptions(), def: "region", service: svc16(),
      account: [brow("111122223333", "Production", 27850, 10000)],
      region: [brow("us-east-1", "us-east-1", 22410, 8047), brow("eu-west-1", "eu-west-1", 4330, 1555), brow("global", "Global", 1110, 398)],
      team: teams16(),
    }),
  }));

  // 17 — Cost Explorer not available for the single cost source: closed code, no totals
  out.push(disabledPage("09e-17-cost-explorer-unavailable", "Cost Explorer unavailable for the single cost source; no totals", {
    enabled: false, reason: "ce_unavailable", links: { ui: "/aws/finops" },
    cost_source: { account_id: "111122223333", account_name: "Production", kind: "account" },
  }));

  // 18 — one account, one region, no team tag: smallest setup
  const h18 = header({ sync: { accounts_readable: 1, accounts_total: 1, team_tag: { key: "team", active: true, allowlisted: true } } });
  const r18 = rows16();
  r18.forEach((r) => delete r.owner);
  out.push(standard("09e-18-single-region", "One account, one region, no team tag; Spend by removed", {
    h: h18, rows: r18, oth: -210, base: 24940, total: 27850,
    bridgeOver: { owner_column: { show: false, note: "No team tag" } },
    sum: { forecast: forecast28900, budget: null, savings: [2090, 3], unalloc: [27850, 10000] },
    sav: savings(h18, SAV16),
    bd: breakdowns(27850, h18, {
      options: [opt("account_group", false, "no_groups"), opt("account", false, "single_value"), opt("region", false, "single_value"), opt("team", false, "no_team_tag", "No team tag on this cost")],
      def: "",
      service: [
        brow("compute.vm", "EC2", 10900, 3914, 1240, { inside: inside(["instances", 8700], ["EBS", 1700], ["IPs", 500]) }),
        brow("database.relational", "RDS", 5600, 2011, undefined, { inside: inside(["instances", 4600], ["storage", 800], ["backups", 200]) }),
        brow("compute.containers", "Fargate", 4200, 1508, undefined, { inside: inside(["vCPU", 3100], ["memory", 1100]) }),
        brow("network.nat", "VPC (NAT Gateway)", 3000, 1077, 1980, { inside: inside(["data", 2400], ["hourly", 600]) }),
        brow("storage.object", "S3", 1300, 467, undefined, { inside: inside(["storage", 1000], ["requests", 300]) }),
        brow("other", "Other", 2850, 1023, undefined, { count: 9, inside: inside(["global", 1110], ["CloudWatch", 900], ["other", 840]) }),
      ],
      account: [brow("111122223333", "Production", 27850, 10000)],
      region: [brow("us-east-1", "us-east-1", 27850, 10000)],
      team: [brow("Unallocated", "Unallocated", 27850, 10000, undefined, { owner: unalloc() })],
    }),
  }));

  // 19 — one account, one region, team tags on several teams
  const h19 = header({ sync: { accounts_readable: 1, accounts_total: 1 } });
  out.push(standard("09e-19-single-region-tagged", "One account and region; Spend by team with no switch", {
    h: h19, rows: rows16(), oth: -210, base: 24940, total: 27850,
    sum: { forecast: forecast28900, budget: null, savings: [2090, 3] },
    sav: savings(h19, SAV16),
    bd: breakdowns(27850, h19, {
      options: [opt("account_group", false, "no_groups"), opt("account", false, "single_value"), opt("region", false, "single_value"), opt("team")],
      def: "team", service: svc16(),
      account: [brow("111122223333", "Production", 27850, 10000)],
      region: [brow("us-east-1", "us-east-1", 27850, 10000)],
      team: teams16(),
    }),
  }));

  // 20 / 21 — organization: management account as cost source, 48 linked accounts
  const h20 = header({ sync: { accounts_readable: 48, accounts_total: 48 } });
  const nc: FinOpsWhy = { state: "account_not_connected", text: "Not connected" };
  const r20 = () => [
    row("drv-newacct", "new_account", "New account", 4800, "ml.sagemaker", "SageMaker", { own: owner("data"), whyIs: found("first cost Oct 12"), status: "open", inc: "inc-sbml", account: "444400001111", account_name: "sandbox-ml", since: "2026-10-12" }),
    row("drv-dataprod", "usage_up", "More servers", 3900, "compute.vm", "EC2", { own: owner("data"), whyIs: nc, status: "open", inc: "inc-dataprod", account: "111122225555", account_name: "data-prod" }),
    row("drv-nat", "usage_up", "Network traffic", 1980, "network.nat", "VPC (NAT Gateway)", { own: owner("checkout"), whyIs: found("checkout-api release"), status: "open", inc: "inc-nat" }),
    row("drv-unusedsp", "discount_change", "Unused Savings Plan", 1600, "billing.savings_plans", "Savings Plans", { own: owner("platform"), whyIs: found("commitment used 96%→82%"), status: "open", inc: "inc-sp", account: "999900001111", account_name: "management", record_type: "discount" }),
    row("drv-logs", "usage_up", "Log storage", 1240, "storage.object", "S3", { own: owner("platform"), whyIs: nc, account: "999900002222", account_name: "Log Archive" }),
    row("drv-ec2", "usage_up", "More servers", 1240, "compute.vm", "EC2", { own: owner("checkout"), whyIs: found("checkout-asg 6→9"), status: "expected", inc: "inc-ec2", expected: expectedAsg() }),
  ];
  const accounts: FinOpsAccountsView = {
    ...h20,
    total_micros: usd(186400),
    accounts: orgAccounts(),
    groups: [
      { name: "Production", cost_micros: usd(96900), change_micros: usd(4100), count: 6 },
      { name: "Non-production", cost_micros: usd(39100), change_micros: usd(-310), count: 14 },
      { name: "Sandboxes", cost_micros: usd(16800), change_micros: usd(5260), count: 20 },
      { name: "Landing zone", cost_micros: usd(13100), change_micros: usd(1690), count: 3 },
      { name: "Shared and platform", cost_micros: usd(20500), change_micros: usd(4460), count: 5 },
    ],
  };
  const sav20: SavingSpec[] = [
    ["sav-sm", "idle", "Idle SageMaker endpoints in sandbox-ml", 3100, "minutes", "low", "data"],
    ["sav-ec2", "rightsize", "Smaller EC2 in data-prod", 2400, "hours", "needs_testing", "data"],
    ["sav-s3lc", "storage_class", "S3 lifecycle in Log Archive", 1900, "hours", "low", "platform"],
    ["sav-vpce", "network_path", "VPC endpoints in Production", 1520, "hours", "low", "checkout"],
    ["sav-ebs", "idle", "Unattached EBS in 6 accounts", 880, "minutes", "low", null],
  ];
  for (const [state, route] of [["09e-20-organization", "/aws/finops"], ["09e-21-organization-accounts", "/aws/finops?tab=accounts"]]) {
    out.push(standard(state, "Management account is the cost source; 48 linked accounts, 3 connected", {
      h: h20, rows: r20(), oth: 440, base: 171200, total: 186400, route, accounts,
      sum: {
        forecast: { month: "2026-11", amount_micros: usd(192000), source: "provider" },
        budget: {
          id: "bud-org", name: "Organization", scope: "deployment", monthly_micros: usd(180000), source: "provider", read_only: true,
          actual_micros: usd(186400), forecast_micros: usd(192000), forecast_source: "provider", forecast_diff_micros: usd(12000),
          status: "over_budget", status_word: "Over budget",
        },
        savings: [9800, 5],
      },
      sav: savings(h20, sav20, [{ check: "rightsize", reason: "needs_usage_detail" }]),
      bd: breakdowns(186400, h20, {
        options: [opt("account_group"), opt("account"), opt("region"), opt("team")],
        def: "account_group",
        service: [
          brow("compute.vm", "EC2", 62400, 3348), brow("database.relational", "RDS", 31700, 1701), brow("storage.object", "S3", 18600, 998),
          brow("ml.sagemaker", "SageMaker", 14900, 799), brow("network.nat", "VPC (NAT Gateway)", 12100, 649), brow("other", "Other", 46700, 2505, undefined, { count: 31 }),
        ],
        account: [...orgAccounts().slice(0, 8).map((a) => brow(a.id, a.name, a.cost_micros / M, 0)), brow("other", "Other", 40000, 2146, undefined, { count: 40 })],
        extra: {
          account_group: [
            brow("Production", "Production", 96900, 5198), brow("Non-production", "Non-production", 39100, 2098),
            brow("Shared and platform", "Shared and platform", 20500, 1100), brow("Sandboxes", "Sandboxes", 16800, 901),
            brow("Landing zone", "Landing zone", 13100, 703),
          ],
        },
      }),
    }));
  }

  // 09b tie: resource-level data off, two changes fit equally well.
  const tie = structuredClone(why);
  tie.state = "09b-finops-why-tied";
  tie.description = "Increase detail where two changes tie because resource-level data is off";
  const d = tie.increases!["inc-nat"];
  d.resource_data = "off";
  d.resource_note = "Turn on resource-level data in Cost Explorer to see resources (AWS charges for it)";
  delete d.cause_tree![0].resources;
  d.sync!.resource_data = false;
  const tied = d.increase.suspects!.slice(0, 2);
  for (const x of tied) {
    x.confidence = "possible";
    x.confidence_word = "Possible";
    x.tied = true;
    x.note = "Several changes fit; resource-level data is off";
  }
  d.increase.suspects = tied;
  d.increase.suspect_change = { ...tied[0] };
  out.push(tie);

  return out;
}

/** Every page state, keyed by its design slug (09-finops, 09b-…, 09e-NN-…). */
export function finopsStates(): Map<string, FinOpsStateFixture> {
  return new Map(buildStates().map((f) => [f.state, f]));
}

/** One page state by its design slug. */
export function finopsState(name: string): FinOpsStateFixture {
  const f = finopsStates().get(name);
  if (!f) throw new Error(`no FinOps fixture ${name}`);
  return f;
}

/** The monthly report model for the print route (09c), frozen on Nov 5. */
export function finopsReportFixture(): FinOpsReportFixture {
  const hr = header({ sync: { last_complete_day: "2026-11-03" } });
  const s = summary(43520, 39880, octSpark(43520), hr);
  const b = bridge(39880, 43520, baseRows(), other(-420), hr);
  const sentences: Record<string, string> = {
    "drv-nat": "New release uses a paid gateway; a private route fixes it",
    "drv-ec2": "6 to 9 servers for a load test; no action needed",
    "drv-rds": "Untagged reporting copy; data team to claim or remove",
  };
  for (const r of b.rows ?? []) if (sentences[r.id]) r.sentence = sentences[r.id];
  finishRows(b.rows);
  const sav = savings(hr);
  const strip = <T extends FinOpsHeader>(v: T) => {
    const { enabled: _e, reason: _r, period: _p, compare: _c, depth: _d, currency: _cu, measure: _m, sync: _s, links: _l, ...rest } = v;
    return rest;
  };
  const report: FinOpsReportView = structuredClone({
    ...hr,
    generated_at: "2026-11-05T09:14:00Z",
    frozen: { at: "2026-11-05T09:14:00Z", by: "admin" },
    can_freeze: true,
    not_final: false,
    summary: {
      ...strip(s),
      scope_options: {
        accounts: accountRows().map((a) => ({ id: a.key, name: a.label, kind: "connected" })),
        regions: ["eu-west-1", "global", "us-east-1"],
        teams: teamRows().map((t) => t.key).sort(),
      },
    },
    bridge: strip(b),
    spend_by_service: serviceRows(),
    spend_by_account: accountRows(),
    spend_by_team: teamRows(),
    savings: { total: sav.total, top: (sav.items ?? []).slice(0, 5) },
    notes: {
      expected: [{
        id: "inc-ec2", title: "More servers for checkout", sentence: "More servers for checkout went up $1,240 in October",
        account: "111122223333", account_name: "Production", service: "compute.vm", service_label: "EC2", started_on: "2026-10-14",
        daily_before_micros: usd(450), daily_now_micros: usd(520), monthly_impact_micros: usd(2100), period_change_micros: usd(1240), stated_change_micros: usd(1240),
        cause: "usage_up", owner: owner("checkout"), suspect_change: { state: "found", summary: "checkout-asg 6 to 9" },
        suspects: [{ state: "found", summary: "checkout-asg 6 to 9" }], status: "expected",
        expected: { reason: "Planned load test", until: "2026-11-15", by: "minh.le", at: "2026-10-14T10:00:00Z" },
      }],
      coverage: { accounts: "3 accounts", last_complete_day: "2026-10-31", measure_label: "Net amortized, after discounts and credits" },
      missing: [],
    },
    copy_summary:
      "October cost $43,520, up $3,640 (+9.1%) from September. Top causes: network traffic +$1,980 (checkout), more servers +$1,240 (expected), new database +$610. Savings found: $2,500 a month.",
  });
  finishHeader(report, "09c-finops-report");
  return {
    state: "09c-finops-report",
    design: "local/illustration/aws-harness/09c-finops-report.svg",
    description: "Monthly report model for the print route, frozen Nov 5",
    route: "/aws/finops/report?period=2026-10",
    report,
  };
}
