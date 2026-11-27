// AWS estate reads: Overview, Topology, Coverage, the collection cost, group
// corrections and account labels. Types follow the server's JSON; the
// helpers hold the page's wording rules: a count over an input that was not
// read in full is never shown as exact, and data that was not read is never
// shown as zero or as fine.

// ---------- Shared ----------

/**
 * Old AWS bookmarks and their new homes: Map is Topology, Resources is its
 * list view, Applications its Groups level, Security a filter on Issues.
 */
export const AWS_REDIRECTS: ReadonlyArray<readonly [string, string]> = [
  ["/aws", "/aws/overview"],
  ["/aws/map", "/aws/topology"],
  ["/aws/resources", "/aws/topology?view=list"],
  ["/aws/applications", "/aws/topology?level=groups"],
  ["/aws/security", "/aws/issues"],
];

/** What a number stands on: read in full, capped by a call budget, or not read. */
export type ReadState = "read" | "capped" | "not_read";

/** The summary card's warning: how many sources were not read. Absent or zero means complete. */
export interface CoverageWarning {
  not_read: number;
}

/** "Data not complete: 3 sources not read", or null when nothing is missing. */
export function coverageWarningText(warning: CoverageWarning | null | undefined): string | null {
  const n = warning?.not_read ?? 0;
  if (!Number.isFinite(n) || n <= 0) return null;
  return `Data not complete: ${fmtNumber(n)} ${n === 1 ? "source" : "sources"} not read`;
}

/** "?account=…&region=…", or "" when neither is set. */
export function scopeQueryString(q: { account?: string; region?: string }): string {
  const p = new URLSearchParams();
  if (q.account) p.set("account", q.account);
  if (q.region) p.set("region", q.region);
  const qs = p.toString();
  return qs ? `?${qs}` : "";
}

/** A count in words; a count the server left out or nulled is not read, never a crash or a zero. */
export function fmtNumber(n: number | null | undefined): string {
  if (n === null || n === undefined || !Number.isFinite(n)) return "—";
  return n.toLocaleString("en-US");
}

/**
 * A count with what it stands on. Capped reads "more than N, not all read";
 * not read is a dash, never a zero.
 */
export interface CountWords {
  /** Small words before the number ("more than"). */
  prefix?: string;
  value: string;
  /** Small words after the number ("not all read"). */
  suffix?: string;
  state: ReadState;
}

export function countWords(n: number | null | undefined, state: ReadState): CountWords {
  if (state === "not_read" || n === null || n === undefined || !Number.isFinite(n) || n < 0) {
    return { value: "—", state: "not_read" };
  }
  if (state === "capped") {
    // A capped read with nothing counted has no number to stand on.
    if (n <= 0) return { value: "Not all read", state };
    return { prefix: "more than", value: fmtNumber(n), suffix: "not all read", state };
  }
  return { value: fmtNumber(n), state };
}

/** The same words on one line, for rows and tooltips. */
export function countText(n: number | null | undefined, state: ReadState): string {
  const w = countWords(n, state);
  if (w.state === "not_read") return "not read";
  return [w.prefix, w.value, w.suffix].filter(Boolean).join(" ").replace(" not all read", ", not all read");
}

/** "network and prod", "network, prod and data", "all 4 accounts". */
export function accountListText(names: readonly string[], total?: number): string {
  if (names.length === 0) return "";
  if (total !== undefined && total > 1 && names.length === total) return `all ${total} accounts`;
  if (names.length === 1) return names[0];
  return `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
}

// ---------- Overview ----------

export type IssueSeverity = "critical" | "high" | "medium" | "low";
export type IssueTier = "act_now" | "worth_a_look";
/** How much of an account was read: in full, partly (counts are lower bounds), or not at all. */
export type AccountRead = "read" | "partial" | "not_read";

/** The Overview's numbers. Null was not read; `*_capped` is "more than N, not all read". */
export interface OverviewSummary {
  accounts_read: number;
  accounts_total: number;
  act_now: number | null;
  /** The act-now count is a lower bound: an account's issues were not all read. */
  act_now_capped?: boolean;
  worth_a_look: number | null;
  worth_a_look_capped?: boolean;
  resources: number | null;
  resources_capped: boolean;
  /** Whole percent 0 to 100 of resources in a group other than Ungrouped. */
  grouped_share: number | null;
  /** The share stands on accounts not all read. */
  grouped_share_capped?: boolean;
  people_changes_24h: number | null;
  people_changes_capped: boolean;
}

export interface OverviewIssue {
  id: string;
  severity: IssueSeverity;
  tier: IssueTier;
  kind: string;
  title: string;
  account: string;
  account_name?: string;
  region?: string;
  owner: string;
  /** A short plain place: "cluster ci-eks", "bucket logs". */
  where: string;
  /** When the problem started, when `since_known`. */
  since?: string;
  since_known: boolean;
  opened_at: string;
}

export interface OverviewAccount {
  id: string;
  name: string;
  role: "hub" | "spoke" | "";
  /** "transit gateway", "attached to the transit gateway in network". */
  role_detail?: string;
  read: AccountRead;
  not_read_reason?: string;
  resources: number | null;
  resources_capped: boolean;
  open_issues: number | null;
}

export interface OverviewOtherAccounts {
  count: number;
  unknown: number;
}

export interface OverviewPeopleChange {
  id: string;
  time: string;
  account: string;
  account_name?: string;
  region: string;
  concern: "exposure" | "identity";
  event_name: string;
  summary: string;
  actor: string;
  /** Names of the resources the change touched; the summary shows them in bold. */
  resources: string[];
}

/** The organization, when DescribeOrganization was read. `name` is the organization ID: AWS has no name. */
export interface OverviewOrganization {
  id: string;
  name: string;
  management_account?: string;
}

export interface OverviewView {
  enabled: boolean;
  as_of?: string;
  organization?: OverviewOrganization;
  /** The Regions the scope reads; the Regions picker counts these. */
  regions?: string[];
  summary: OverviewSummary;
  warning: CoverageWarning | null;
  needs_attention: OverviewIssue[];
  accounts: OverviewAccount[];
  other_accounts: OverviewOtherAccounts;
  people_changes: OverviewPeopleChange[];
}

export const SEVERITY_LABEL: Record<IssueSeverity, string> = {
  critical: "Critical",
  high: "High",
  medium: "Medium",
  low: "Low",
};

/** A count's state from the server's null and capped flag. */
export function readState(n: number | null | undefined, capped: boolean | undefined): ReadState {
  if (n === null || n === undefined) return "not_read";
  return capped ? "capped" : "read";
}

/** Grouped share as a whole percent; null is not read. */
export function sharePercent(pct: number | null | undefined): string {
  if (pct === null || pct === undefined || !Number.isFinite(pct)) return "—";
  return `${Math.round(Math.min(Math.max(pct, 0), 100))}%`;
}


export const ROLE_LABEL: Record<string, string> = { hub: "Hub", spoke: "Spoke" };

// ---------- Topology ----------

export type TopologyLevel = "accounts" | "groups" | "focus";
export type TopologyLens = "network" | "identity" | "data" | "changes";
export const TOPOLOGY_LENSES: Array<{ id: TopologyLens; label: string }> = [
  { id: "network", label: "Network" },
  { id: "identity", label: "Identity" },
  { id: "data", label: "Data and events" },
  { id: "changes", label: "Recent changes" },
];
export const DEFAULT_LENSES: TopologyLens[] = ["network", "identity", "data"];

/** How a link is known. */
export type RelationKind = "configured" | "may_access" | "inferred";
/** Who read a link between accounts; empty inside one account. */
export type SeenFrom = "both" | "one" | "mixed" | "";
/** Connected, same organization, known (labelled) or unknown. */
export type AccountLabel = "connected" | "same_org" | "known" | "unknown";

export interface TopologyQuery {
  /** Empty lets the server pick: Accounts with two or more accounts, the one account's Groups otherwise. */
  level?: TopologyLevel;
  /** Groups and Focus take exactly one account. */
  account?: string;
  region?: string;
  /** At the Groups level, opens one group. */
  group?: string;
  ref?: string;
  hops?: 1 | 2 | 3;
  /** Empty draws every link. */
  lens?: TopologyLens[];
}

export type TopologyNodeKind = "account" | "group" | "resource" | "other";

/**
 * One card. Counts that were not read are left out; a `*_capped` count is
 * a lower bound. `read` is set on connected accounts.
 */
export interface TopologyNode {
  id: string;
  kind: TopologyNodeKind;
  name: string;
  account?: string;
  type?: string;
  service?: string;
  region?: string;
  /** The raw group ID (for corrections) on group cards and resources. */
  group?: string;
  label?: AccountLabel;
  role?: "hub" | "spoke";
  role_detail?: string;
  rule?: string;
  key?: string;
  renamed?: boolean;
  read?: AccountRead;
  not_read_reason?: string;
  description?: string;
  dangling?: boolean;
  /** Detail reads a call budget skipped: those flags read "not read", never clear. */
  not_read?: string[];
  by_type?: Record<string, number>;
  last_read_at?: string;
  resources?: number;
  resources_capped?: boolean;
  open_issues?: number;
  open_issues_capped?: boolean;
  /** Open issues by tier on account and group cards; left out when not read. */
  act_now?: number;
  act_now_capped?: boolean;
  worth_a_look?: number;
  worth_a_look_capped?: boolean;
  changes_24h?: number;
  changes_capped?: boolean;
  /** Neighbours the per-hop fan-out cap left out of a Focus view. */
  hidden?: number;
  /** Focus only: health in words. `none` is clear (nothing to check); `not_read` is never shown as fine. */
  health?: NodeHealth;
  /** Focus only: the first open issue, Act now first. */
  top_issue?: TopIssue;
  /** Focus only: the newest change in the window, exact when present. */
  last_change?: LastChange;
  /**
   * Focus only, when there is no `last_change`: true is certain (no change
   * in the window); false means the feed was not read far enough to know.
   */
  last_change_known?: boolean;
}

export type HealthState = "ok" | "warning" | "critical" | "unknown" | "none" | "not_read";

export interface NodeHealth {
  state: HealthState;
  summary?: string;
  since?: string;
}

export interface TopIssue {
  id: string;
  title: string;
  severity: IssueSeverity;
  tier: IssueTier;
  /** The likely cause, when one was found. */
  cause_text?: string;
  /** The node the likely cause changed; Focus marks it "Likely cause". */
  cause_ref?: string;
}

/** The latest change to a resource. */
export interface LastChange {
  time: string;
  actor: string;
  event_name: string;
}

/** A resource found by name for Focus. */
export interface SearchItem {
  ref: string;
  name: string;
  type: string;
  service: string;
  account: string;
  region: string;
}

/** At most 20 items; `capped` says more matched. */
export interface SearchResult {
  enabled: boolean;
  items: SearchItem[];
  capped: boolean;
}

export const SEARCH_MIN = 2;
export const SEARCH_MAX = 200;

export interface TopologyLink {
  from: string;
  to: string;
  type: string;
  kind: RelationKind;
  source?: string;
  access?: string;
  pattern?: string;
  seen_from?: SeenFrom;
  /** On summed links: how many of `count` each AWS call read. Left out when no source is named. */
  sources?: Record<string, number>;
  count: number;
  count_capped?: boolean;
  both_sides: number;
  last_read_at?: string;
}

export interface ScopeHint {
  text: string;
  level?: TopologyLevel;
  /** For Focus, the largest hop count that fits. */
  hops?: number;
}

export interface TopologyView {
  enabled: boolean;
  level: TopologyLevel;
  account?: string;
  /** The scope the counts are over. */
  accounts?: string[];
  region?: string;
  /** The Regions the scope reads. */
  regions?: string[];
  group?: string;
  ref?: string;
  hops?: number;
  lenses?: string[];
  nodes: TopologyNode[];
  links: TopologyLink[];
  /** With too_many, a lower bound. */
  node_count?: number;
  max_nodes?: number;
  /** The scope holds more than max_nodes: nodes and links are empty. */
  too_many?: boolean;
  hint?: ScopeHint;
  fan_out_capped?: boolean;
}

export function topologyQueryString(q: TopologyQuery): string {
  const p = new URLSearchParams();
  if (q.level) p.set("level", q.level);
  if (q.account) p.set("account", q.account);
  if (q.region) p.set("region", q.region);
  if (q.group) p.set("group", q.group);
  if (q.ref) p.set("ref", q.ref);
  if (q.hops) p.set("hops", String(q.hops));
  if (q.lens && q.lens.length > 0) p.set("lens", q.lens.join(","));
  const qs = p.toString();
  return qs ? `?${qs}` : "";
}


/** Plain words for a relation type, used when a link carries no label of its own. */
const RELATION_WORDS: Record<string, string> = {
  in_vpc: "In VPC",
  in_subnet: "In subnet",
  uses_security_group: "Security group",
  routes_to: "Routes to",
  triggers: "Triggers",
  subscribes: "Subscribes",
  targets: "Targets",
  part_of_stack: "Stack",
  member_of: "Member of",
  attached_to: "Attached to",
  dead_letter: "Dead letter",
  origin: "Origin",
  monitors: "Monitors",
  associated: "Associated",
  contains: "Contains",
  triggered_by: "Triggered by",
  attached_to_transit: "Transit gateway",
  shared_via: "Shared",
  alias_of: "DNS",
  trusts: "Role trust",
  may_access: "May access",
};

export function relationWords(type: string): string {
  return RELATION_WORDS[type] ?? type.replace(/_/g, " ");
}

/**
 * Link words by the AWS call that read the link: [summary, detail]. The
 * summary names a summed link between cards ("DNS 12"); the detail names
 * one link in Focus ("Alias record").
 */
const SOURCE_WORDS: Record<string, [string, string]> = {
  "route53:ListResourceRecordSets": ["DNS", "Alias record"],
  "ec2:DescribeTransitGatewayAttachments": ["Transit gateway", "Transit gateway attachment"],
  "ram:GetResourceShareAssociations": ["Resource share", "Resource share"],
  "ram:ListResources": ["Resource share", "Resource share"],
  "iam:ListRoles": ["Role trust", "Role trust"],
  "s3:GetBucketPolicy": ["Bucket policy", "Bucket policy"],
  "kms:GetKeyPolicy": ["KMS key", "Key policy"],
  "ecr:GetRepositoryPolicy": ["ECR repository", "Repository policy"],
};

/** Link words by relation type, when the link names no source. */
const TYPE_WORDS: Record<string, [string, string]> = {
  alias_of: ["DNS", "Alias record"],
  attached_to_transit: ["Transit gateway", "Transit gateway attachment"],
  shared_via: ["Resource share", "Resource share"],
  trusts: ["Role trust", "Role trust"],
  may_access: ["May access", "May access"],
  routes_to: ["Routes to", "Listener rule"],
  origin: ["Origin", "Origin"],
  member_of: ["Runs on cluster", "Runs on"],
  contains: ["Contains", "Contains"],
  triggers: ["Triggers", "Trigger"],
  triggered_by: ["Triggered by", "Trigger"],
  subscribes: ["Subscription", "Subscription"],
  targets: ["Rule target", "Rule target"],
  dead_letter: ["Dead-letter queue", "Dead-letter queue"],
  uses_security_group: ["Security group", "Security group"],
  in_vpc: ["In VPC", "In VPC"],
  in_subnet: ["In subnet", "In subnet"],
  attached_to: ["Attached to", "Attached to"],
  monitors: ["Alarm", "Alarm"],
  associated: ["Associated", "Associated"],
  part_of_stack: ["Stack", "Stack"],
};

/**
 * A link's words. Matched by name or address, a DNS or origin link reads
 * "Hostname matches"; otherwise the call that read it names it, then its
 * type, then the type in words. A summed link carries its count; a capped
 * count reads "more than".
 */
export function linkLabel(
  link: { type: string; count: number; count_capped?: boolean; source?: string; sources?: Record<string, number>; kind?: RelationKind },
  style: "summary" | "detail" = "summary",
  fromType = "",
): string {
  const at = style === "summary" ? 0 : 1;
  // A summed link read by several calls names each with its count: "KMS key 2, ECR repository 1".
  const parts = Object.entries(link.sources ?? {}).filter(([, n]) => n > 0);
  if (parts.length > 1) {
    const words = parts
      .map(([src, n]) => [SOURCE_WORDS[src]?.[at] ?? src, n] as const)
      .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
      .map(([w, n]) => `${w} ${fmtNumber(n)}`)
      .join(", ");
    return link.count_capped ? `${words}, not all read` : words;
  }
  if (parts.length === 1 && !link.source) link = { ...link, source: parts[0][0] };
  let base: string;
  if (link.kind === "inferred" && (link.type === "origin" || link.type === "alias_of")) {
    base = link.type === "origin" ? "Origin hostname matches" : "Hostname matches";
  } else if (link.type === "routes_to" && /targetgroup/i.test(fromType)) {
    base = "Registered target";
  } else {
    base = (link.source && SOURCE_WORDS[link.source]?.[at]) || TYPE_WORDS[link.type]?.[at] || relationWords(link.type);
  }
  if (link.count_capped) return `${base}, more than ${fmtNumber(link.count)}`;
  return link.count > 1 ? `${base} ${fmtNumber(link.count)}` : base;
}

/** Seen from one side unless every link behind it was read from both. Inside one account: both. */
export function linkSides(link: Pick<TopologyLink, "seen_from">): "both" | "one" {
  return link.seen_from === "one" || link.seen_from === "mixed" ? "one" : "both";
}

/** The rule that made a group, as its card names it: "tag app", "stack", "cluster". */
export function groupRuleText(group: { rule?: string; key?: string }): string {
  switch (group.rule) {
    case "tag":
      return group.key ? `tag ${group.key}` : "tag";
    case "namespace":
      return group.key ? `namespace ${group.key}` : "namespace";
    case "user":
      return "yours";
    case "appregistry":
      return "AppRegistry";
    case "asg":
      return "Auto Scaling";
    case "vpc":
      return "VPC";
    default:
      return group.rule ?? "";
  }
}

/** "volumes 212, Lambda functions 88, …": the largest types first, the rest as "other". */
export function byTypeText(byType: Record<string, number>, keep = 4): string {
  const entries = Object.entries(byType).filter(([, n]) => n > 0).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
  const shown = entries.slice(0, keep).map(([t, n]) => `${t} ${fmtNumber(n)}`);
  const rest = entries.slice(keep).reduce((s, [, n]) => s + n, 0);
  if (rest > 0) shown.push(`other ${fmtNumber(rest)}`);
  return shown.join(", ");
}

export const UNGROUPED_ID = "ungrouped";

/** Account display name: the label or name the server gave, else the ID cut short. */
export function accountDisplay(card: { id: string; name?: string }): string {
  if (card.name && card.name !== card.id) return card.name;
  const id = card.id.startsWith("account:") ? card.id.slice(8) : card.id;
  // The whole ID, grouped by four, so two accounts with the same prefix stay apart.
  return /^[0-9]{12}$/.test(id) ? `${id.slice(0, 4)}-${id.slice(4, 8)}-${id.slice(8)}` : id;
}

/** A node's count with its state: left out means not read. */
export function nodeCount(
  n: TopologyNode,
  field: "resources" | "open_issues" | "changes_24h" | "act_now" | "worth_a_look",
): { n: number | null; state: ReadState } {
  const v = n[field];
  const capped = {
    resources: n.resources_capped,
    open_issues: n.open_issues_capped,
    changes_24h: n.changes_capped,
    act_now: n.act_now_capped,
    worth_a_look: n.worth_a_look_capped,
  }[field];
  return { n: v ?? null, state: readState(v, capped) };
}

// ---------- Coverage ----------

export type ReadOutcome = "not_read" | "capped" | "off_in_aws" | "not_offered";

export interface CoverageRow {
  source: string;
  operation?: string;
  outcome: ReadOutcome;
  reason: string;
  detail?: string;
  accounts: string[];
  seen?: number;
  total?: number;
  last_good_at?: string;
  never_read?: boolean;
  fix?: string[];
  hides?: string;
  cant_run?: string;
  /** A display title for the source and operation, when the server sends one. */
  title?: string;
}

export interface CoverageSummary {
  accounts_read: number;
  accounts_total: number;
  not_read: number;
  capped: number;
  off_in_aws: number;
  resources_not_read?: number;
}

export interface CoverageNotGaps {
  /** Not read yet by the server: always null. */
  not_set?: number | null;
  services_not_offered: number;
  regions_not_enabled: number;
}

export interface CoverageView {
  enabled: boolean;
  /** Each account's read state, and the Regions the scope reads. */
  accounts?: Array<{ account: string; read: AccountRead; reason?: string }>;
  regions?: string[];
  summary: CoverageSummary;
  not_read: CoverageRow[];
  capped: CoverageRow[];
  off_in_aws: CoverageRow[];
  not_gaps: CoverageNotGaps;
}

/** The row whose source is a whole account that could not be read. */
export const ACCOUNT_SOURCE = "account";


/** A gap reason inside a sentence ("denied in all 4 accounts"). */
export function reasonWords(reason: string): string {
  return GAP_REASON_WORDS[reason] ?? humanise(reason).toLowerCase();
}

/** A not-read row's muted line: "28 of 35 functions, denied in all 4 accounts". */
export function notReadLine(row: CoverageRow, names: readonly string[], total: number): string {
  if (row.source === ACCOUNT_SOURCE) return notReadBadge(row.detail || row.reason);
  const where = accountListText(names, total);
  const reason = reasonWords(row.reason);
  const counted = row.total && row.total > 0 && row.seen !== undefined ? `${fmtNumber(row.seen)} of ${fmtNumber(row.total)} read, ` : "";
  return `${counted}${reason} in ${where}`;
}

/** A capped row's muted line: "more than 3,000, not all read". */
export function cappedLine(row: CoverageRow): string {
  const seen = row.seen ?? 0;
  const words = seen > 0 ? `More than ${fmtNumber(seen)}, not all read` : "Not all read";
  return row.detail ? `${words}, ${row.detail}` : words;
}

/** The fix to copy: an IAM statement allowing the denied actions. */
export function fixStatement(actions: readonly string[]): string {
  return JSON.stringify({ Effect: "Allow", Action: actions.length === 1 ? actions[0] : [...actions], Resource: "*" }, null, 2);
}

/** "121 settings not set, 7 services not offered, 16 Regions not turned on", and how many parts. */
export function notGapsText(n: CoverageNotGaps): { text: string; parts: number } {
  const parts: string[] = [];
  const add = (v: number | undefined, one: string, many: string) => {
    if (v && v > 0) parts.push(`${fmtNumber(v)} ${v === 1 ? one : many}`);
  };
  add(n.not_set ?? undefined, "setting not set", "settings not set");
  add(n.services_not_offered, "service not offered", "services not offered");
  add(n.regions_not_enabled, "Region not turned on", "Regions not turned on");
  return { text: parts.join(", "), parts: parts.length };
}

// ---------- Collection cost ----------

export interface CostEstimate {
  direct_usd: number;
  guardduty_usd: number | null;
  guardduty_known: boolean;
  extra_trail_copies_usd: number | null;
  extra_trail_copies: number | null;
  unknown_accounts: string[];
  lake_gb: number;
  siem_gb: number;
}

export interface CostStretched {
  account: string;
  source: string;
  factor: number;
  calls_24h: number;
  target: number;
}

export interface CostAccountSource {
  account: string;
  source: string;
  calls: number;
  metered: number;
}

export interface CostBudget {
  source: string;
  limit: number;
  used: number;
  unit: string;
  /** The budget was reached in the last 24 hours. */
  reached: boolean;
  /** The coverage sources (unit services) this budget covers, to match the Capped row. */
  coverage_sources: string[];
}

export interface CostOptional {
  id: string;
  name: string;
  price: string;
  what: string;
  known: boolean;
  accounts_off: string[];
}

export interface CollectionCostView {
  enabled: boolean;
  calls_24h: number;
  events_24h: number;
  metered_24h: number;
  estimate_month: CostEstimate;
  sources_capped: number;
  stretched: CostStretched[];
  /** Accounts the daily target slowed down. */
  stretched_accounts: number;
  by_account_source: CostAccountSource[];
  budgets: CostBudget[];
  optional_off: CostOptional[];
  ledger_incomplete: boolean;
}

/** Budget sources in the server's display order. */
export const COST_SOURCES = ["inventory", "health", "changes", "identity", "policies", "credential_report", "findings"] as const;

export const COST_SOURCE_LABEL: Record<string, string> = {
  calls_per_day: "Calls a day",
  health: "Health",
  inventory: "Inventory",
  changes: "Changes",
  identity: "Identity",
  policies: "Policies",
  credential_report: "Credential report",
  findings: "Findings",
};

/** What each source reads, for its column's tooltip. */
export const COST_SOURCE_HINT: Record<string, string> = {
  health: "Target health, alarms and AWS Health",
  inventory: "Describe and list calls for every resource",
  changes: "CloudTrail event history",
  identity: "IAM roles, users and their trust",
  policies: "Bucket, key and repository policies",
  credential_report: "The IAM credential report",
  findings: "Security Hub, GuardDuty and Inspector findings",
};

export function fmtUsd(v: number): string {
  if (v >= 100) return `$${Math.round(v).toLocaleString("en-US")}`;
  if (v >= 10) return `$${Math.round(v)}`;
  return `$${v.toFixed(2)}`;
}

/** The month's estimate: direct, GuardDuty and extra trail copies; "~$46". Null parts are left out. */
/**
 * Whether the month's estimate is a lower bound: a part was not read (no
 * trail could be read), is not known (GuardDuty status), or is itself a
 * lower bound (some accounts' trails were not read).
 */
export function monthEstimateIsLowerBound(e: CostEstimate): boolean {
  return e.extra_trail_copies_usd === null || e.guardduty_usd === null || e.unknown_accounts.length > 0;
}

export function monthEstimate(e: CostEstimate): number {
  return e.direct_usd + (e.guardduty_usd ?? 0) + (e.extra_trail_copies_usd ?? 0);
}

// ---------- Group corrections ----------

export type CorrectionKind = "rename" | "merge" | "move";

export interface GroupCorrection {
  account_id: string;
  kind: CorrectionKind;
  group_id?: string;
  member_arn?: string;
  target_group_id?: string;
  name?: string;
  created_by?: string;
  created_at: string;
  inherited?: boolean;
  /** applied, or no_longer_matches when the group or member it names is gone. */
  status?: "applied" | "no_longer_matches";
}

export interface GroupCorrectionsView {
  enabled: boolean;
  corrections: GroupCorrection[];
}

export interface GroupCorrectionRequest {
  account_id: string;
  kind: CorrectionKind;
  group_id?: string;
  member_arn?: string;
  target_group_id?: string;
  name?: string;
  /**
   * The created_at of the correction this edit replaces, as read. The
   * server refuses the write with 409 correction_conflict when that
   * correction changed or is gone. Left out when there was none.
   */
  expected_created_at?: string;
}

/** The stored correction an edit would replace: one rename or merge per group, one move per member. */
export function correctionFor(
  list: readonly GroupCorrection[],
  key: { kind: CorrectionKind; group_id?: string; member_arn?: string },
): GroupCorrection | undefined {
  return list.find((c) =>
    c.kind === key.kind && (key.kind === "move" ? c.member_arn === key.member_arn : c.group_id === key.group_id),
  );
}

/** True when the stored correction for the key is not the one the edit started from. */
export function correctionMoved(before: GroupCorrection | undefined, now: GroupCorrection | undefined): boolean {
  if (!before && !now) return false;
  if (!before || !now) return true;
  return before.created_at !== now.created_at || before.name !== now.name || before.target_group_id !== now.target_group_id;
}

/** "Rename to Checkout", "Merge into payments", "Move checkout-receipts". */
export function correctionText(c: GroupCorrection): string {
  if (c.kind === "rename") return `Rename ${c.group_id ?? ""} to ${c.name ?? ""}`.trim();
  if (c.kind === "merge") return `Merge ${c.group_id ?? ""} into ${c.target_group_id ?? ""}`;
  const member = c.member_arn?.split(/[:/]/).pop() ?? c.member_arn ?? "";
  return `Move ${member} to ${c.target_group_id ?? ""}`;
}

// ---------- Account labels ----------

export interface AccountLabelEntry {
  account_id: string;
  label: string;
  name?: string;
  created_by?: string;
  created_at: string;
  inherited?: boolean;
}

export interface AccountLabelsView {
  enabled: boolean;
  labels: AccountLabelEntry[];
}

// ---------- Plain words for wire keys ----------
// One table turns every key the server sends (resource types, collector
// sources, AWS operations, not-read reasons) into words. A key not in it
// is humanised; a not-read code not in it never shows raw.

/** "nat-gateway" → "Nat gateway", "ListAssociatedResources" → "List associated resources". */
export function humanise(key: string): string {
  const words = key
    .replace(/^[a-z0-9]+:/, "")
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .replace(/[-_]+/g, " ")
    .trim()
    .toLowerCase();
  return words ? words.charAt(0).toUpperCase() + words.slice(1) : key;
}

/** Resource types, by bare type or by "service:type" where the type alone is ambiguous. */
const RESOURCE_WORDS: Record<string, string> = {
  vpc: "VPC",
  subnet: "Subnet",
  "nat-gateway": "NAT gateway",
  "internet-gateway": "Internet gateway",
  "network-interface": "Network interface",
  "route-table": "Route table",
  "security-group": "Security group",
  "vpc-endpoint": "VPC endpoint",
  "transit-gateway": "Transit gateway",
  "transit-gateway-attachment": "Transit gateway attachment",
  "resource-share": "Resource share",
  "load-balancer": "Load balancer",
  "target-group": "Target group",
  targetgroup: "Target group",
  loadbalancer: "Load balancer",
  instance: "EC2 instance",
  "auto-scaling-group": "Auto Scaling group",
  function: "Lambda function",
  "ecs:cluster": "ECS cluster",
  "eks:cluster": "EKS cluster",
  cluster: "Cluster",
  "ecs:service": "ECS service",
  service: "ECS service",
  "task-definition": "Task definition",
  "node-group": "Node group",
  "db-instance": "RDS instance",
  "db-cluster": "RDS cluster",
  table: "DynamoDB table",
  "cache-cluster": "ElastiCache cluster",
  queue: "SQS queue",
  topic: "SNS topic",
  rule: "EventBridge rule",
  "event-bus": "Event bus",
  "state-machine": "Step Functions state machine",
  stream: "Kinesis stream",
  "rest-api": "API Gateway API",
  bucket: "S3 bucket",
  distribution: "CloudFront distribution",
  "hosted-zone": "Hosted zone",
  "dns-record": "DNS record",
  "health-check": "Health check",
  role: "IAM role",
  user: "IAM user",
  alarm: "Alarm",
  "composite-alarm": "Composite alarm",
  stack: "CloudFormation stack",
  application: "AppRegistry application",
  repository: "ECR repository",
  key: "KMS key",
  trail: "CloudTrail trail",
  // Older keys qualified by service.
  "elasticloadbalancing:targetgroup": "Target group",
  "elasticloadbalancing:loadbalancer": "Load balancer",
  "route53:record": "DNS record",
  "cloudfront:distribution": "CloudFront distribution",
  "ecs:task-definition": "Task definition",
  "ecr:repository": "ECR repository",
  "s3:bucket": "S3 bucket",
  "kms:key": "KMS key",
  "iam:role": "IAM role",
  "lambda:function": "Lambda function",
  // Ends of links the reads cannot see.
  account: "Account",
  organization: "Organization",
  "unknown-principal": "Principal",
  anyone: "Anyone",
  host: "Host name",
  group: "Wide grants",
};

/** A resource type in words: "Target group", "NAT gateway"; an unknown type humanised. */
export function resourceTypeWords(type: string, service?: string): string {
  if (!type) return "";
  return (service && RESOURCE_WORDS[`${service}:${type}`]) || RESOURCE_WORDS[type] || RESOURCE_WORDS[type.toLowerCase()] || humanise(type);
}

/** Collector sources (unit services) and the AWS operations a gap names. */
const SOURCE_WORDS_TITLE: Record<string, string> = {
  cloudwatch: "CloudWatch",
  ecs: "ECS",
  elbv2: "Load balancers",
  ec2: "EC2",
  rds: "RDS",
  cloudformation: "CloudFormation",
  "aws-health": "AWS Health",
  health: "AWS Health",
  vpc: "VPC",
  "auto-scaling": "Auto Scaling",
  lambda: "Lambda",
  eks: "EKS",
  dynamodb: "DynamoDB",
  elasticache: "ElastiCache",
  sqs: "SQS",
  sns: "SNS",
  eventbridge: "EventBridge",
  "step-functions": "Step Functions",
  kinesis: "Kinesis",
  "api-gateway": "API Gateway",
  tagging: "Tagging API",
  appregistry: "AppRegistry",
  s3: "S3",
  "s3-policy": "Bucket policies",
  cloudfront: "CloudFront",
  route53: "Route 53",
  iam: "IAM",
  "transit-gateway": "Transit gateways",
  ram: "Resource shares",
  organizations: "Account list",
  trails: "CloudTrail trails",
  ecr: "ECR",
  kms: "KMS",
  inspector2: "Inspector",
  accessanalyzer: "IAM Access Analyzer",
  guardduty: "GuardDuty",
  securityhub: "Security Hub",
  config: "AWS Config",
  // Operations.
  ListAssociatedResources: "AppRegistry resources",
  ListFunctionUrlConfigs: "Function URL auth",
  GetFunctionUrlConfig: "Function URL auth",
  DescribeOrganization: "Account list",
  ListAccounts: "Account list",
  GetBucketPolicy: "Bucket policy",
  GetBucketPolicyStatus: "Bucket public status",
  GetBucketPublicAccessBlock: "Bucket public access block",
  GetPublicAccessBlock: "Account public access block",
  GetBucketEncryption: "Bucket encryption",
  GetKeyPolicy: "Key policy",
  GetRepositoryPolicy: "Repository policy",
  GetResourceShareAssociations: "Resource share members",
  DescribeTransitGatewayAttachments: "Transit gateway attachments",
  DescribeTargetHealth: "Target health",
  DescribeInstanceStatus: "Instance status",
  DescribeAlarms: "Alarms",
  DescribeTrails: "Trails",
  GetEventSelectors: "Trail event selectors",
  ListResourceRecordSets: "DNS records",
  ListHostedZones: "Hosted zones",
  ListEventSourceMappings: "Lambda triggers",
  ListTargetsByRule: "EventBridge rule targets",
  ListSubscriptions: "SNS subscriptions",
  GetQueueAttributes: "Queue settings",
  ListStackResources: "Stack resources",
  ListNodegroups: "EKS node groups",
  DescribeNodegroup: "EKS node group",
  "eks:logging": "EKS control plane logs",
  "ec2:flowlogs": "VPC flow logs",
};

/** A coverage row's title: the server's, then the operation's words, then the source's, else humanised. */
export function coverageTitle(row: CoverageRow, accountName: (id: string) => string): string {
  if (row.title) return row.title;
  if (row.source === ACCOUNT_SOURCE) return accountName(row.accounts[0] ?? "");
  const op = row.operation ?? "";
  return (
    (op && (SOURCE_WORDS_TITLE[`${row.source}:${op}`] || SOURCE_WORDS_TITLE[op])) ||
    SOURCE_WORDS_TITLE[row.source] ||
    humanise(op || row.source)
  );
}

/** A collector source in words ("SQS", "Step Functions"). */
export function sourceWords(source: string): string {
  return SOURCE_WORDS_TITLE[source] ?? humanise(source);
}

/** Gap reasons inside a sentence. */
const GAP_REASON_WORDS: Record<string, string> = {
  denied: "denied",
  throttled: "throttled past the retry budget",
  not_on_plan: "needs a higher support plan",
  not_checked: "not read yet",
  not_enabled: "off in AWS",
  capped: "over the call budget",
  not_available_in_region: "not offered",
};

/**
 * Every code Core can send for why an account or source was not read: the
 * snapshot's account details, the settings outage codes, and the coverage
 * gap reasons. Short plain words for the not-read badge.
 */
const NOT_READ_REASON: Record<string, string> = {
  // Account details from the coverage read.
  not_read_yet: "Not read yet",
  first_read_in_progress: "First read in progress",
  account_disconnected: "Disconnected",
  credentials_invalid: "Credentials not accepted",
  assume_role_failed: "Read role could not be assumed",
  account_mismatch: "Credentials belong to another account",
  // Settings outage codes.
  no_server_identity: "Versus has no AWS identity",
  role_not_found: "Read role not found",
  trust_denied: "Role trust refused Versus",
  external_id_mismatch: "ExternalId not allowed by the role trust",
  session_duration_invalid: "Session duration not allowed",
  region_disabled: "Region not enabled",
  timeout: "AWS did not answer in time",
  network: "Could not reach AWS",
  refresh_in_progress: "A refresh is running",
  // Coverage gap reasons.
  denied: "Access denied",
  throttled: "AWS throttled the read",
  not_on_plan: "Needs a higher support plan",
  not_checked: "Not checked",
  capped: "Over the call budget",
  not_enabled: "Off in AWS",
  not_available_in_region: "Not offered in this Region",
};

/**
 * The not-read badge. A known code reads in its own words; plain words
 * already sent read "Not read: <words>"; an unknown code never shows raw.
 */
export function notReadBadge(reason: string | undefined): string {
  if (!reason) return "Not read";
  const known = NOT_READ_REASON[reason];
  if (known) return known;
  if (/^[a-z0-9_.:-]+$/i.test(reason)) return "Not read (see Settings)";
  return `Not read: ${reason}`;
}
