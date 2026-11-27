import type {
  CollectionCostView,
  CoverageView,
  GroupCorrection,
  OverviewIssue,
  OverviewPeopleChange,
  OverviewView,
  TopologyLink,
  TopologyNode,
  TopologyView,
} from "@/lib/awsEstate";

// The fictional Northwind estate the AWS redesign screens use: four
// connected accounts in a hub and spoke, the management account of the same
// organization not connected, two known outside accounts and one unknown.
// Built in code for the Overview, Topology, Coverage and collection cost tests.

export const NETWORK = "222233334444";
export const PROD = "333344445555";
export const PLATFORM = "555566667777";
export const DATA = "666677778888";
export const MANAGEMENT = "111100001111";
export const LOG_ARCHIVE = "777700007777";
export const DATADOG = "464622532012";
export const UNKNOWN = "889900001234";

export const NAMES: Record<string, string> = {
  [NETWORK]: "network",
  [PROD]: "prod",
  [PLATFORM]: "platform",
  [DATA]: "data",
};

export function overviewView(over: Partial<OverviewView> = {}): OverviewView {
  return {
    enabled: true,
    as_of: "2026-11-26T14:10:00Z",
    organization: { id: "o-north", name: "o-north", management_account: MANAGEMENT },
    summary: {
      accounts_read: 4,
      accounts_total: 4,
      act_now: 3,
      worth_a_look: 4,
      resources: 3000,
      resources_capped: false,
      grouped_share: 62,
      people_changes_24h: 4,
      people_changes_capped: false,
    },
    warning: { not_read: 3 },
    needs_attention: [
      issue("iss-1", "critical", "act_now", "Attack sequence detected on cluster ci-eks", PLATFORM, "platform", { where: "cluster ci-eks", since: "2026-11-26T13:02:00Z", since_known: true }),
      issue("iss-2", "high", "act_now", "Key for user report-bot is 2 years old and was used today", PROD, "prod"),
      issue("iss-3", "high", "act_now", "checkout-web target group has no healthy targets", PROD, "prod", { region: "eu-west-1", since: "2026-11-26T14:02:00Z", since_known: true }),
      issue("iss-4", "medium", "worth_a_look", "501 volumes left behind by cluster data-eks, 22 TB, not encrypted", DATA, "data", { where: "$1,840 a month" }),
      issue("iss-5", "medium", "worth_a_look", "Cluster API for prod-eks is open to the internet", PROD, "prod"),
    ],
    accounts: [
      { id: NETWORK, name: "network", role: "hub", role_detail: "transit gateway", read: "read", resources: 500, resources_capped: false, open_issues: 1 },
      { id: PROD, name: "prod", role: "spoke", read: "read", resources: 1200, resources_capped: false, open_issues: 4 },
      { id: PLATFORM, name: "platform", role: "spoke", read: "read", resources: 400, resources_capped: false, open_issues: 1 },
      { id: DATA, name: "data", role: "spoke", read: "read", resources: 900, resources_capped: false, open_issues: 1 },
    ],
    other_accounts: { count: 4, unknown: 1 },
    people_changes: [
      change("ch-1", "2026-11-26T12:31:00Z", "Added ingress rule tcp 443 from sg-alb to sg-checkout", ["sg-checkout"], PROD, "prod", "exposure", "Admin (SSO)"),
      change("ch-2", "2026-11-26T11:15:00Z", "Limited trust of role github-deploy to repository northwind/checkout", ["github-deploy"], PLATFORM, "platform", "identity", "bob@example.com"),
      change("ch-3", "2026-11-26T10:02:00Z", "Deleted user old-contractor", ["old-contractor"], PROD, "prod", "identity", "Admin (SSO)"),
    ],
    ...over,
  };
}

function issue(
  id: string,
  severity: OverviewIssue["severity"],
  tier: OverviewIssue["tier"],
  title: string,
  account: string,
  account_name: string,
  more: Partial<OverviewIssue> = {},
): OverviewIssue {
  return { id, severity, tier, kind: "security", title, account, account_name, owner: `arn:aws:::${id}`, where: "", since_known: false, opened_at: "2026-11-26T09:00:00Z", ...more };
}

function change(
  id: string,
  time: string,
  summary: string,
  resources: string[],
  account: string,
  account_name: string,
  concern: OverviewPeopleChange["concern"],
  actor: string,
): OverviewPeopleChange {
  return { id, time, summary, resources, account, account_name, region: "eu-west-1", concern, event_name: "Event", actor };
}

const acct = (id: string) => `account:${id}`;

/** The Accounts level as the server answers it: account cards, far ends and summed links. */
export function accountsTopology(over: Partial<TopologyView> = {}): TopologyView {
  const connected = (id: string, name: string, resources: number, open_issues: number, more: Partial<TopologyNode> = {}): TopologyNode => ({
    id: acct(id),
    kind: "account",
    name,
    account: id,
    label: "connected",
    read: "read",
    resources,
    open_issues,
    ...more,
  });
  return {
    enabled: true,
    level: "accounts",
    lenses: ["data", "identity", "network"],
    node_count: 9,
    max_nodes: 300,
    too_many: false,
    fan_out_capped: false,
    nodes: [
      connected(PLATFORM, "platform", 400, 1, { role: "spoke", act_now: 1, worth_a_look: 0 }),
      connected(PROD, "prod", 1200, 4, { role: "spoke", act_now: 2, worth_a_look: 2 }),
      connected(NETWORK, "network", 500, 1, { role: "hub", role_detail: "transit gateway", act_now: 0, worth_a_look: 1 }),
      connected(DATA, "data", 900, 1, { role: "spoke", role_detail: "attached to the transit gateway in network", act_now: 0, worth_a_look: 1 }),
      { id: acct(MANAGEMENT), kind: "account", name: MANAGEMENT, account: MANAGEMENT, label: "same_org" },
      { id: acct(LOG_ARCHIVE), kind: "account", name: "log-archive", account: LOG_ARCHIVE, label: "known" },
      { id: acct(DATADOG), kind: "account", name: "Datadog", account: DATADOG, label: "known" },
      { id: acct(UNKNOWN), kind: "account", name: UNKNOWN, account: UNKNOWN, label: "unknown" },
      { id: "other:Load balancers we cannot read", kind: "other", name: "Load balancers we cannot read", description: "Load balancers we cannot read", dangling: true },
    ],
    links: [
      link(acct(PLATFORM), acct(PROD), "alias_of", "configured", 12, "both"),
      link(acct(PLATFORM), acct(NETWORK), "attached_to_transit", "configured", 1, "both"),
      link(acct(PLATFORM), acct(NETWORK), "alias_of", "configured", 58, "both"),
      link(acct(PROD), acct(NETWORK), "attached_to_transit", "configured", 1, "both"),
      link(acct(DATA), acct(NETWORK), "attached_to_transit", "configured", 1, "both"),
      link(acct(PROD), acct(DATA), "may_access", "may_access", 1, "both"),
      link(acct(PLATFORM), "other:Load balancers we cannot read", "alias_of", "configured", 139, "one"),
      link(acct(DATA), acct(UNKNOWN), "trusts", "may_access", 2, "one"),
    ],
    ...over,
  };
}

function link(from: string, to: string, type: string, kind: TopologyLink["kind"], count: number, seen_from: TopologyLink["seen_from"]): TopologyLink {
  return { from, to, type, kind, count, both_sides: seen_from === "both" ? count : 0, seen_from };
}

const grp = (id: string) => `group:${PROD}/${id}`;

/** The Groups level of prod. */
export function groupsTopology(over: Partial<TopologyView> = {}): TopologyView {
  const g = (id: string, name: string, rule: string, resources: number, more: Partial<TopologyNode> = {}): TopologyNode => ({
    id: grp(id),
    kind: "group",
    name,
    account: PROD,
    group: id,
    rule,
    resources,
    open_issues: 0,
    ...more,
  });
  return {
    enabled: true,
    level: "groups",
    account: PROD,
    lenses: ["data", "identity", "network"],
    node_count: 8,
    max_nodes: 300,
    too_many: false,
    fan_out_capped: false,
    nodes: [
      g("tag:app=checkout", "checkout", "tag", 96, { key: "app", open_issues: 1, act_now: 1, worth_a_look: 0 }),
      g("tag:service=payments", "payments", "tag", 74, { key: "service" }),
      g("eks:prod-eks/ns:search", "search", "namespace", 52, { key: "search" }),
      g("stack:baseline", "baseline", "stack", 130, { open_issues: 1, act_now: 1, worth_a_look: 0 }),
      g("eks:prod-eks", "prod-eks", "cluster", 380, { open_issues: 1, act_now: 0, worth_a_look: 1 }),
      g("ungrouped", "Ungrouped", "ungrouped", 468, {
        by_type: { volumes: 212, "Lambda functions": 88, "security groups": 64, "IAM roles": 51, "log groups": 30, queues: 23 },
        open_issues: 1,
        act_now: 0,
        worth_a_look: 1,
      }),
      { id: acct(PLATFORM), kind: "account", name: "platform", account: PLATFORM, label: "connected", read: "read" },
      { id: acct(DATA), kind: "account", name: "data", account: DATA, label: "connected", read: "read" },
    ],
    links: [
      link(acct(PLATFORM), grp("tag:app=checkout"), "alias_of", "configured", 12, "both"),
      link(grp("tag:service=payments"), acct(DATA), "may_access", "may_access", 1, "both"),
      { from: grp("tag:app=checkout"), to: grp("stack:baseline"), type: "may_access", kind: "may_access", count: 2, both_sides: 0 },
      { from: grp("tag:service=payments"), to: grp("stack:baseline"), type: "may_access", kind: "may_access", count: 3, both_sides: 0 },
      { from: grp("eks:prod-eks/ns:search"), to: grp("eks:prod-eks"), type: "member_of", kind: "configured", count: 1, both_sides: 0 },
    ],
    ...over,
  };
}

/** The Ungrouped group of prod opened: its members, for the Move list. */
export function ungroupedTopology(): TopologyView {
  return {
    enabled: true,
    level: "groups",
    account: PROD,
    group: "ungrouped",
    nodes: [
      { id: "arn:aws:s3:::checkout-receipts", kind: "resource", name: "checkout-receipts", account: PROD, type: "s3:bucket", group: "ungrouped" },
      { id: grp("tag:app=checkout"), kind: "group", name: "checkout", account: PROD, group: "tag:app=checkout", rule: "tag", key: "app" },
    ],
    links: [],
  };
}

export const TG = "arn:aws:elasticloadbalancing:eu-west-1:333344445555:targetgroup/checkout-web/1";

/** A Focus view of the checkout-web target group at two hops. */
export function focusTopology(over: Partial<TopologyView> = {}): TopologyView {
  return {
    enabled: true,
    level: "focus",
    account: PROD,
    ref: TG,
    hops: 2,
    lenses: [],
    node_count: 8,
    max_nodes: 300,
    too_many: false,
    fan_out_capped: false,
    nodes: [
      { id: "far-lb", kind: "other", name: "load balancer", type: "host", dangling: true, description: "a load balancer not connected" },
      { id: "dns", kind: "resource", name: "checkout.northwind.example", account: PLATFORM, type: "route53:record" },
      { id: "cdn", kind: "resource", name: "checkout-cdn", account: PROD, type: "cloudfront:distribution" },
      { id: "lb", kind: "resource", name: "checkout-web", account: PROD, type: "elasticloadbalancing:loadbalancer" },
      {
        id: TG,
        kind: "resource",
        name: "checkout-web",
        account: PROD,
        region: "eu-west-1",
        type: "elasticloadbalancing:targetgroup",
        open_issues: 1,
        description: "IP targets, port 8080",
        health: { state: "critical", summary: "0 of 4 targets healthy", since: "2026-11-26T14:02:00Z" },
        top_issue: {
          id: "iss-3",
          title: "checkout-web target group has no healthy targets",
          severity: "high",
          tier: "act_now",
          cause_text: "Service checkout updated to task definition :42 by github-actions-deploy, 13:58",
          cause_ref: "td",
        },
        last_change: { time: "2026-11-26T13:58:00Z", actor: "github-actions-deploy", event_name: "Service checkout updated to task definition :42" },
      },
      { id: "svc", kind: "resource", name: "checkout", account: PROD, type: "ecs:service", open_issues: 0 },
      { id: "td", kind: "resource", name: "checkout:42", account: PROD, type: "ecs:task-definition", hidden: 2 },
      { id: "ecr", kind: "resource", name: "checkout", account: PLATFORM, type: "ecr:repository", not_read: ["policy"], health: { state: "not_read", summary: "access denied" } },
    ],
    links: [
      link("far-lb", "dns", "alias_of", "configured", 1, "one"),
      link("dns", "lb", "alias_of", "configured", 1, "both"),
      { from: "cdn", to: "lb", type: "origin", kind: "inferred", count: 1, both_sides: 0 },
      { from: "lb", to: TG, type: "routes_to", kind: "configured", count: 1, both_sides: 0 },
      { from: "dns", to: TG, type: "alias_of", kind: "configured", source: "route53:ListResourceRecordSets", count: 1, both_sides: 1, seen_from: "both" },
      { from: TG, to: "svc", type: "targets", kind: "configured", count: 1, both_sides: 0 },
      { from: "svc", to: "td", type: "associated", kind: "configured", count: 1, both_sides: 0 },
      link("svc", "ecr", "may_access", "may_access", 1, "both"),
    ],
    ...over,
  };
}

export function coverageView(over: Partial<CoverageView> = {}): CoverageView {
  const all = [NETWORK, PROD, PLATFORM, DATA];
  return {
    enabled: true,
    summary: { accounts_read: 4, accounts_total: 4, not_read: 3, capped: 1, off_in_aws: 4 },
    not_read: [
      {
        source: "lambda",
        operation: "ListFunctionUrlConfigs",
        outcome: "not_read",
        reason: "denied",
        accounts: all,
        seen: 28,
        total: 35,
        fix: ["lambda:ListFunctionUrlConfigs"],
        hides: "whether those functions are public",
      },
      { source: "organizations", outcome: "not_read", reason: "denied", accounts: all, hides: "which of 9 outside accounts are yours" },
      { source: "health", outcome: "not_read", reason: "not_on_plan", accounts: all, hides: "AWS events; 14 notices still arrive through Security Hub" },
    ],
    capped: [{ source: "inspector2", outcome: "capped", reason: "capped", accounts: [PLATFORM], seen: 3000, detail: "known exploited ones first" }],
    off_in_aws: [
      { source: "inspector2", outcome: "off_in_aws", reason: "not_enabled", accounts: [NETWORK, PROD], cant_run: "image and package findings" },
      { source: "accessanalyzer", outcome: "off_in_aws", reason: "not_enabled", accounts: all, cant_run: "unused access and external access" },
      { source: "eks", operation: "logging", outcome: "off_in_aws", reason: "not_enabled", accounts: [PROD], seen: 0, total: 4, cant_run: "who called the cluster API", title: "EKS control plane logs" },
      { source: "ec2", operation: "flowlogs", outcome: "off_in_aws", reason: "not_enabled", accounts: all, seen: 10, total: 19, cant_run: "traffic evidence for exposure" },
    ],
    not_gaps: { not_set: null, services_not_offered: 7, regions_not_enabled: 16 },
    ...over,
  };
}

export function costView(over: Partial<CollectionCostView> = {}): CollectionCostView {
  const rows: Array<[string, Record<string, number>]> = [
    [NETWORK, { inventory: 4360, health: 3040, changes: 2200, identity: 290, policies: 300, findings: 70 }],
    [PROD, { inventory: 9800, health: 6200, changes: 4300, identity: 610, policies: 1900, findings: 290 }],
    [PLATFORM, { inventory: 6100, health: 4000, changes: 3900, identity: 420, policies: 900, findings: 1240 }],
    [DATA, { inventory: 5200, health: 3600, changes: 2600, identity: 380, policies: 1100, findings: 40 }],
  ];
  return {
    enabled: true,
    calls_24h: 62840,
    events_24h: 62840,
    metered_24h: 1200,
    estimate_month: {
      direct_usd: 0.2,
      guardduty_usd: 7.5,
      guardduty_known: true,
      extra_trail_copies_usd: 38,
      extra_trail_copies: 1,
      unknown_accounts: [],
      lake_gb: 2.8,
      siem_gb: 2.8,
    },
    sources_capped: 1,
    stretched: [
      { account: PROD, source: "inventory", factor: 2, calls_24h: 23100, target: 20000 },
      { account: PROD, source: "policies", factor: 2, calls_24h: 23100, target: 20000 },
    ],
    stretched_accounts: 1,
    by_account_source: rows.flatMap(([account, m]) => Object.entries(m).map(([source, calls]) => ({ account, source, calls, metered: 0 }))),
    budgets: [
      { source: "calls_per_day", limit: 80000, used: 62840, unit: "calls", reached: false, coverage_sources: [] },
      { source: "health", limit: 40, used: 22, unit: "calls", reached: false, coverage_sources: ["health", "elbv2"] },
      { source: "inventory", limit: 400, used: 260, unit: "calls", reached: false, coverage_sources: ["ec2"] },
      { source: "findings", limit: 50, used: 50, unit: "pages", reached: true, coverage_sources: ["securityhub", "guardduty", "inspector2"] },
    ],
    optional_off: [
      { id: "access-analyzer-unused", name: "Access Analyzer unused access", price: "About $0.20 per identity a month", what: "Finds unused roles and keys", known: true, accounts_off: [DATA] },
    ],
    ledger_incomplete: false,
    ...over,
  };
}

export function correction(over: Partial<GroupCorrection> = {}): GroupCorrection {
  return { account_id: PROD, kind: "rename", group_id: "tag:app=checkout", name: "Checkout", created_at: "2026-11-25T10:00:00Z", status: "applied", ...over };
}
