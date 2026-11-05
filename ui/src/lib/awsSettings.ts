// Pure view logic for the AWS settings screen. Everything here is derived from
// the settings/draft payloads and a caller-supplied clock, so the answer line,
// the row wording and the wizard counts can be unit-tested without a DOM.
//
// The rule the whole screen follows: a value is never invented. A probe that
// did not answer is "not checked" — never "denied", never zero.

import { format } from "date-fns";
import type {
  AwsAccount,
  AwsAccountStatus,
  AwsApiUse,
  AwsConnectMethod,
  AwsDraftCounts,
  AwsDraftService,
  AwsOptionalData,
  AwsOptionalDataKey,
  AwsServiceCounts,
  AwsServiceStatus,
  AwsSettings,
} from "@/lib/api";

// ---------- Service catalog ----------

export interface AwsServiceMeta {
  /** Key used by the API and the draft results. */
  key: string;
  /** Human name shown in the grid and in prose. */
  label: string;
  /** IAM actions that close a denial for this service. */
  fixActions: string[];
  /** "global" services are probed through us-east-1. */
  scope: "region" | "global";
}

// The v1 catalog: one probe per service. The order is the grid's order, so the
// 24 cells fill top-to-bottom in a stable way while probes land.
export const AWS_SERVICE_CATALOG: AwsServiceMeta[] = [
  { key: "api-gateway", label: "API Gateway", fixActions: ["apigateway:GET"], scope: "region" },
  { key: "auto-scaling", label: "Auto Scaling", fixActions: ["autoscaling:Describe*"], scope: "region" },
  { key: "aws-health", label: "AWS Health", fixActions: ["health:Describe*"], scope: "global" },
  { key: "cloudformation", label: "CloudFormation", fixActions: ["cloudformation:Describe*"], scope: "region" },
  { key: "cloudfront", label: "CloudFront", fixActions: ["cloudfront:List*"], scope: "global" },
  { key: "cloudtrail", label: "CloudTrail", fixActions: ["cloudtrail:LookupEvents"], scope: "region" },
  { key: "cloudwatch", label: "CloudWatch", fixActions: ["cloudwatch:DescribeAlarms*"], scope: "region" },
  { key: "config", label: "Config", fixActions: ["config:Describe*"], scope: "region" },
  { key: "dynamodb", label: "DynamoDB", fixActions: ["dynamodb:ListTables", "dynamodb:DescribeTable"], scope: "region" },
  { key: "ec2", label: "EC2", fixActions: ["ec2:Describe*"], scope: "region" },
  { key: "ecs", label: "ECS", fixActions: ["ecs:List*", "ecs:Describe*"], scope: "region" },
  { key: "eks", label: "EKS", fixActions: ["eks:List*", "eks:Describe*"], scope: "region" },
  { key: "elasticache", label: "ElastiCache", fixActions: ["elasticache:Describe*"], scope: "region" },
  { key: "elbv2", label: "ELBv2", fixActions: ["elasticloadbalancing:Describe*"], scope: "region" },
  { key: "eventbridge", label: "EventBridge", fixActions: ["events:List*", "events:Describe*"], scope: "region" },
  { key: "iam", label: "IAM", fixActions: ["iam:List*", "iam:GetRole"], scope: "global" },
  { key: "kinesis", label: "Kinesis", fixActions: ["kinesis:List*", "kinesis:Describe*"], scope: "region" },
  { key: "lambda", label: "Lambda", fixActions: ["lambda:List*", "lambda:GetFunctionConfiguration"], scope: "region" },
  { key: "rds", label: "RDS", fixActions: ["rds:Describe*"], scope: "region" },
  { key: "route53", label: "Route 53", fixActions: ["route53:List*", "route53:GetHealthCheckStatus"], scope: "global" },
  { key: "s3", label: "S3", fixActions: ["s3:ListAllMyBuckets", "s3:GetBucket*"], scope: "global" },
  { key: "sns", label: "SNS", fixActions: ["sns:List*", "sns:GetTopicAttributes"], scope: "region" },
  { key: "sqs", label: "SQS", fixActions: ["sqs:List*", "sqs:GetQueueAttributes"], scope: "region" },
  { key: "step-functions", label: "Step Functions", fixActions: ["states:List*", "states:Describe*"], scope: "region" },
];

export const AWS_SERVICE_TOTAL = AWS_SERVICE_CATALOG.length;

const SERVICE_BY_KEY = new Map(AWS_SERVICE_CATALOG.map((s) => [s.key, s]));

/** Human name for a service key; an unknown key is shown as-is, never guessed. */
export function serviceLabel(key: string): string {
  return SERVICE_BY_KEY.get(key)?.label ?? key;
}

// ---------- Status vocabulary ----------

export type AwsTone = "ok" | "warn" | "critical" | "muted";

export const SERVICE_STATUS_LABELS: Record<AwsServiceStatus, string> = {
  readable: "Readable",
  not_used: "Not used",
  denied: "Denied",
  not_on_plan: "Not on plan",
  not_checked: "Not checked",
};

export const SERVICE_STATUS_TONES: Record<AwsServiceStatus, AwsTone> = {
  readable: "ok",
  not_used: "muted",
  denied: "critical",
  not_on_plan: "muted",
  not_checked: "warn",
};

export const ACCOUNT_STATUS_LABELS: Record<AwsAccountStatus, string> = {
  connected: "Connected",
  disconnected: "Disconnected",
};

// Why a probe is not checked. "Throttled" is a retry, not a permission
// problem, so the words never borrow the denied vocabulary.
export function notCheckedReason(errorCode: string | null | undefined): string {
  switch ((errorCode ?? "").toLowerCase()) {
    case "throttled":
    case "throttling":
    case "requestlimitexceeded":
      return "AWS throttled, retrying";
    case "timeout":
      return "AWS timed out, retrying";
    case "network":
      return "Network error, retrying";
    default:
      return "AWS did not answer, retrying";
  }
}

/** Accessible one-liner for a service result: "ElastiCache: Denied". */
export function serviceStatusText(
  service: string,
  status: AwsServiceStatus,
  errorCode?: string | null,
): string {
  const label = serviceLabel(service);
  if (status === "not_checked") {
    return `${label}: Not checked: ${notCheckedReason(errorCode)}`;
  }
  return `${label}: ${SERVICE_STATUS_LABELS[status]}`;
}

// ---------- Regions ----------

// Human names for the regions the harness is expected to meet. Anything else
// keeps its code rather than pretending to know the geography.
export const REGION_LABELS: Record<string, string> = {
  "us-east-1": "US East (N. Virginia)",
  "us-east-2": "US East (Ohio)",
  "us-west-1": "US West (N. California)",
  "us-west-2": "US West (Oregon)",
  "ca-central-1": "Canada (Central)",
  "sa-east-1": "South America (São Paulo)",
  "eu-west-1": "Europe (Ireland)",
  "eu-west-2": "Europe (London)",
  "eu-west-3": "Europe (Paris)",
  "eu-central-1": "Europe (Frankfurt)",
  "eu-north-1": "Europe (Stockholm)",
  "ap-south-1": "Asia Pacific (Mumbai)",
  "ap-southeast-1": "Asia Pacific (Singapore)",
  "ap-southeast-2": "Asia Pacific (Sydney)",
  "ap-northeast-1": "Asia Pacific (Tokyo)",
  "ap-northeast-2": "Asia Pacific (Seoul)",
};

export function regionLabel(code: string): string {
  return REGION_LABELS[code] ?? code;
}

// ---------- Optional data ----------

export const OPTIONAL_DATA_LABELS: Record<AwsOptionalDataKey, string> = {
  cost_explorer: "Cost",
  config: "Config",
  security_hub: "Security Hub",
  trusted_advisor: "Trusted Advisor",
};

export const OPTIONAL_DATA_KEYS: AwsOptionalDataKey[] = [
  "cost_explorer",
  "config",
  "security_hub",
  "trusted_advisor",
];

/** "Cost on in all accounts, Config in 2, Security Hub in 1" (no prefix). */
export function optionalDataSummary(
  optionalData: AwsOptionalData,
  accountCount: number,
): string {
  const parts: string[] = [];
  for (const key of OPTIONAL_DATA_KEYS) {
    const on = optionalData[key]?.on ?? 0;
    if (on <= 0) continue;
    const label = OPTIONAL_DATA_LABELS[key];
    if (accountCount > 0 && on >= accountCount) {
      parts.push(`${label} on in all accounts`);
    } else {
      parts.push(`${label} in ${on}`);
    }
  }
  return parts.length > 0 ? parts.join(", ") : "none on";
}

// ---------- Privacy ----------

// Words for the payload categories the privacy card lists. An unknown code
// keeps its code so a server addition is visible instead of dropped.
export const PRIVACY_LABELS: Record<string, string> = {
  resource_names_types_counts: "Resource names, types and counts",
  alarm_names_states: "Alarm names and states",
  allowlisted_tag_values: "Shared tag values",
  secrets_env_user_data: "Secrets, env vars and user data",
  log_lines_request_bodies: "Log lines and request bodies",
  other_tag_values: "Other tag values",
};

export function privacyLabel(code: string): string {
  return PRIVACY_LABELS[code] ?? code;
}

/** "AI model: claude · sonnet-4" or the off state, never a blank line. */
export function aiModelLine(privacy: AwsSettings["privacy"]): string {
  if (!privacy.ai.enabled) return "AI is off — nothing is sent";
  const provider = privacy.ai.provider.trim();
  const model = privacy.ai.model.trim();
  if (!provider && !model) return "AI is on";
  return `AI model: ${[provider, model].filter(Boolean).join(" · ")}`;
}

/** "Leak check: passing, last run 14:15, 0 leaks in 24 h". */
export function selfTestLine(
  selfTest: AwsSettings["privacy"]["self_test"],
  now: Date = new Date(),
): string {
  const state = selfTest.passing ? "passing" : "failing";
  const leaks = `${selfTest.leaks_24h} ${selfTest.leaks_24h === 1 ? "leak" : "leaks"} in 24 h`;
  const when = clockTime(selfTest.last_run_at, now);
  return when
    ? `Leak check: ${state}, last run ${when}, ${leaks}`
    : `Leak check: ${state}, ${leaks}`;
}

// ---------- Time words ----------

function clockTime(iso: string | null | undefined, now: Date): string | undefined {
  if (!iso) return undefined;
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return undefined;
  // A future timestamp is a clock skew, not a fact worth printing.
  if (t > now.getTime() + 60_000) return undefined;
  return format(new Date(t), "HH:mm");
}

function ageWords(iso: string | null | undefined, now: Date): string | undefined {
  if (!iso) return undefined;
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return undefined;
  const seconds = Math.max(0, Math.round((now.getTime() - t) / 1000));
  if (seconds < 60) return `${seconds} s ago`;
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes} min ago`;
  const hours = Math.round(minutes / 60);
  return `${hours} h ago`;
}

// ---------- Answer line ----------

export type AwsAnswerKind =
  | "off"
  | "no-accounts"
  | "disconnected"
  | "denied"
  | "regions"
  | "ok";

export type AwsAnswerTone = "plain" | AwsTone;

export interface AwsAnswerSegment {
  text: string;
  tone: AwsAnswerTone;
}

export interface AwsAnswer {
  kind: AwsAnswerKind;
  segments: AwsAnswerSegment[];
  /** True while the connector is not usable at all. */
  blocked: boolean;
}

export interface AwsAnswerInput {
  enabled: boolean;
  accounts: AwsAccount[];
  optional_data?: AwsOptionalData;
}

function countedAccounts(n: number): string {
  return `${n} ${n === 1 ? "account" : "accounts"}`;
}

function deniedServiceName(account: AwsAccount): string | undefined {
  const key = account.denied_services?.find((s) => s.length > 0);
  return key ? serviceLabel(key) : undefined;
}

// awsSettingsAnswer builds the one sentence at the top of the page. Precedence
// is fixed — off, no accounts, disconnected, denied, regions not enabled, all
// good — and only the worst problem is coloured, so the line has exactly one
// thing to look at.
export function awsSettingsAnswer(
  settings: AwsAnswerInput,
  now: Date = new Date(),
): AwsAnswer {
  if (!settings.enabled) {
    return {
      kind: "off",
      blocked: true,
      segments: [{ text: "AWS connector is off", tone: "critical" }],
    };
  }

  const accounts = settings.accounts;
  if (accounts.length === 0) {
    return {
      kind: "no-accounts",
      blocked: false,
      segments: [{ text: "No AWS accounts connected", tone: "warn" }],
    };
  }

  const disconnected = accounts.filter((a) => a.status === "disconnected");
  if (disconnected.length > 0) {
    const first = disconnected[0];
    const others = accounts.length - disconnected.length;
    const since = clockTime(first.disconnected_since, now);
    const alsoDown = disconnected.length > 1 ? ` and ${disconnected.length - 1} more` : "";
    return {
      kind: "disconnected",
      blocked: false,
      segments: [
        {
          text: `${first.display_name} disconnected${since ? ` since ${since}` : ""}${alsoDown}`,
          tone: "critical",
        },
        {
          text:
            others > 0
              ? `; ${countedAccounts(others)} still connected`
              : "; no other accounts connected",
          tone: "plain",
        },
      ],
    };
  }

  const denied = accounts.filter((a) => a.denied_services.length > 0);
  if (denied.length > 0) {
    const first = denied[0];
    const named = deniedServiceName(first);
    const worst = named
      ? `; ${first.display_name} is missing ${named} access`
      : `; ${first.display_name} is missing access to a service`;
    const segments: AwsAnswerSegment[] = [
      { text: `${countedAccounts(accounts.length)} connected`, tone: "plain" },
      { text: worst, tone: "critical" },
    ];
    if (denied.length > 1) {
      const more = denied.length - 1;
      segments.push({
        text: `; ${more} more ${more === 1 ? "account needs" : "accounts need"} access fixed`,
        tone: "plain",
      });
    }
    if ((settings.optional_data?.cost_explorer?.on ?? 0) > 0) {
      segments.push({ text: "; cost data on", tone: "plain" });
    }
    return { kind: "denied", blocked: false, segments };
  }

  const regionGap = accounts.find((a) => a.regions.not_enabled.length > 0);
  if (regionGap) {
    const count = regionGap.regions.not_enabled.length;
    return {
      kind: "regions",
      blocked: false,
      segments: [
        { text: `${countedAccounts(accounts.length)} connected`, tone: "plain" },
        {
          text: `; ${regionGap.display_name} has ${count} ${count === 1 ? "region" : "regions"} not enabled`,
          tone: "warn",
        },
      ],
    };
  }

  return {
    kind: "ok",
    blocked: false,
    segments: [
      { text: `All good across ${countedAccounts(accounts.length)}`, tone: "ok" },
    ],
  };
}

/** The only segment the answer line colours (plain text is never coloured). */
export function answerWorstTone(answer: AwsAnswer): AwsTone | undefined {
  return answer.segments.find((s) => s.tone !== "plain")?.tone as AwsTone | undefined;
}

// ---------- Account rows ----------

export interface AwsAccountRow {
  accountId: string;
  name: string;
  roleArn: string;
  fromConfig: boolean;
  connected: boolean;
  statusLabel: string;
  statusDetail: string;
  servicesLabel: string;
  /** Named only when a service is actually denied. */
  deniedLabel?: string;
  regionsLabel: string;
  regionsDetail: string;
  apiLabel: string;
  apiFraction: number;
  apiUse: AwsApiUse;
  services: AwsServiceCounts;
}

export interface AwsAccountRowInput {
  account: AwsAccount;
  now?: Date;
}

export function accountStatusWords(
  account: AwsAccount,
  now: Date = new Date(),
): { label: string; detail: string; connected: boolean } {
  if (account.status === "disconnected") {
    const since = clockTime(account.disconnected_since, now);
    return {
      label: ACCOUNT_STATUS_LABELS.disconnected,
      detail: since ? `since ${since}` : "reconnect to check again",
      connected: false,
    };
  }
  const age = ageWords(account.checked_at, now);
  return {
    label: ACCOUNT_STATUS_LABELS.connected,
    detail: age ? `checked ${age}` : "checked",
    connected: true,
  };
}

/** Row view model: every number the table prints, derived in one place. */
export function accountRow(account: AwsAccount, now: Date = new Date()): AwsAccountRow {
  const status = accountStatusWords(account, now);
  const services = account.services;
  const denied = account.denied_services.length;
  const deniedName = deniedServiceName(account);
  const notEnabled = account.regions.not_enabled.length;
  const apiBudget = account.api_use.budget_rps;
  const apiUsed = account.api_use.used_rps;

  return {
    accountId: account.account_id,
    name: account.display_name,
    roleArn: account.role_arn,
    fromConfig: account.source === "config",
    connected: status.connected,
    statusLabel: status.label,
    statusDetail: status.detail,
    servicesLabel: `${services.readable} of ${services.total} readable`,
    deniedLabel:
      denied > 0
        ? deniedName
          ? `${deniedName}: Denied`
          : "Permission denied"
        : undefined,
    regionsLabel: `${account.regions.enabled} of ${account.regions.selected.length}`,
    regionsDetail:
      notEnabled > 0
        ? `${notEnabled} not enabled`
        : account.regions.selected.length === 0
          ? "none selected"
          : "all enabled",
    apiLabel: `${apiUsed} of ${apiBudget} req/s`,
    apiFraction: apiBudget > 0 ? Math.min(1, Math.max(0, apiUsed / apiBudget)) : 0,
    apiUse: account.api_use,
    services,
  };
}

// ---------- Connect wizard ----------

export const CONNECT_STEPS = [
  { label: "Method" },
  { label: "Check access" },
  { label: "Regions" },
];

export interface AwsConnectMethodOption {
  id: AwsConnectMethod;
  label: string;
  detail: string;
}

export const CONNECT_METHODS: AwsConnectMethodOption[] = [
  {
    id: "cloudformation",
    label: "CloudFormation quick-create",
    detail: "Recommended. Creates the read-only role from the Versus template.",
  },
  {
    id: "terraform",
    label: "Terraform",
    detail: "Copy the module and apply it in your own pipeline.",
  },
  {
    id: "existing_role",
    label: "Existing role ARN",
    detail: "Use a role you already created and trust Versus in.",
  },
  {
    id: "local_profile",
    label: "Local profile",
    detail: "Development on this machine only.",
  },
  {
    id: "organizations",
    label: "AWS Organizations",
    detail: "Connect every account in the organization at once.",
  },
];

export function methodLabel(method: AwsConnectMethod): string {
  return CONNECT_METHODS.find((m) => m.id === method)?.label ?? method;
}

/** Role name out of an ARN ("…:role/path/versus-reader" → "versus-reader"). */
export function roleNameFromArn(roleArn: string): string {
  const trimmed = roleArn.trim();
  const last = trimmed.split("/").pop();
  return last && last.length > 0 ? last : "versus-reader";
}

/** Account id out of a role ARN, or undefined when the ARN is malformed. */
export function accountIdFromRoleArn(roleArn: string): string | undefined {
  const match = /^arn:aws[a-z-]*:iam::(\d{12}):role\/.+$/.exec(roleArn.trim());
  return match?.[1];
}

/** The copyable IAM fix: "Add elasticache:Describe* to versus-reader". */
export function serviceFixText(service: string, roleArn: string): string {
  const meta = SERVICE_BY_KEY.get(service);
  const role = roleNameFromArn(roleArn);
  const actions = meta?.fixActions ?? [];
  if (actions.length === 0) {
    return `Add the missing permission to ${role}`;
  }
  return `Add ${actions.join(", ")} to ${role}`;
}

/** "Role assumed in 212 ms" / the mapped failure. */
export function assumeWords(assume: {
  ok: boolean;
  latency_ms?: number | null;
  error_code?: string | null;
}): string {
  if (assume.ok) {
    const latency =
      typeof assume.latency_ms === "number" && Number.isFinite(assume.latency_ms)
        ? Math.max(0, Math.round(assume.latency_ms))
        : undefined;
    return latency !== undefined
      ? `Role assumed in ${latency} ms`
      : "Role assumed";
  }
  return "Role could not be assumed";
}

export type AwsGridCounts = AwsDraftCounts;

const EMPTY_COUNTS: AwsGridCounts = {
  total: AWS_SERVICE_TOTAL,
  readable: 0,
  not_used: 0,
  denied: 0,
  not_on_plan: 0,
  not_checked: 0,
  pending: AWS_SERVICE_TOTAL,
};

/** Counts derived from the results that have landed; the rest are pending. */
export function deriveDraftCounts(
  services: AwsDraftService[] = [],
  total: number = AWS_SERVICE_TOTAL,
): AwsGridCounts {
  const counts: AwsGridCounts = { ...EMPTY_COUNTS, total };
  const seen = new Set<string>();
  for (const result of services) {
    if (seen.has(result.service)) continue;
    seen.add(result.service);
    switch (result.status) {
      case "readable":
        counts.readable += 1;
        break;
      case "not_used":
        counts.not_used += 1;
        break;
      case "denied":
        counts.denied += 1;
        break;
      case "not_on_plan":
        counts.not_on_plan += 1;
        break;
      case "not_checked":
        counts.not_checked += 1;
        break;
    }
  }
  const settled =
    counts.readable +
    counts.not_used +
    counts.denied +
    counts.not_on_plan +
    counts.not_checked;
  counts.pending = Math.max(0, total - settled);
  return counts;
}

/** True while any probe has not landed — the draft poll runs at 1 s. */
export function draftIsRunning(services: AwsDraftService[] = []): boolean {
  return deriveDraftCounts(services).pending > 0;
}

/** "19 readable · 3 not used · 1 denied · 1 not on plan" (pending omitted). */
export function legendWords(counts: AwsGridCounts): string {
  const parts = [
    `${counts.readable} readable`,
    `${counts.not_used} not used`,
    `${counts.denied} denied`,
  ];
  if (counts.not_on_plan > 0) parts.push(`${counts.not_on_plan} not on plan`);
  if (counts.not_checked > 0) parts.push(`${counts.not_checked} not checked`);
  if (counts.pending > 0) parts.push(`${counts.pending} checking`);
  return parts.join(" · ");
}

/** Denied services in catalog order, each with its fix sentence. */
export function deniedFixes(
  services: AwsDraftService[] = [],
  roleArn: string,
): Array<{ service: string; label: string; fix: string; actions: string[] }> {
  const fixes: Array<{ service: string; label: string; fix: string; actions: string[] }> = [];
  for (const meta of AWS_SERVICE_CATALOG) {
    const result = services.find((s) => s.service === meta.key);
    if (result?.status !== "denied") continue;
    const actions =
      result.fix_actions && result.fix_actions.length > 0
        ? result.fix_actions
        : meta.fixActions;
    fixes.push({
      service: meta.key,
      label: meta.label,
      fix: serviceFixText(meta.key, roleArn),
      actions,
    });
  }
  return fixes;
}

/** Default display name when an account is committed. */
export function defaultAccountName(roleArn: string): string {
  const account = accountIdFromRoleArn(roleArn);
  return account ? `AWS account ${account}` : "AWS account";
}
