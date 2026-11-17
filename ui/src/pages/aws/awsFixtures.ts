import type {
  AwsAccount,
  AwsConnectDraft,
  AwsDraftService,
  AwsRegionOption,
  AwsSettings,
} from "@/lib/api";

// Shared fixtures for the AWS settings tests. They mirror the product's own
// scenarios: three accounts, Staging denying ElastiCache, Health not on plan and
// one probe throttled — the states the screen must never round away.

export const PRINCIPAL_ARN = "arn:aws:iam::999900001111:role/versus-server";
export const STAGING_ROLE_ARN = "arn:aws:iam::444455556666:role/versus-reader";

export function isoSecondsAgo(seconds: number): string {
  return new Date(Date.now() - seconds * 1000).toISOString();
}

export function awsAccount(overrides: Partial<AwsAccount> = {}): AwsAccount {
  return {
    account_id: "111122223333",
    display_name: "Production",
    source: "wizard",
    status: "connected",
    checked_at: isoSecondsAgo(12),
    disconnected_since: null,
    error_code: null,
    services: {
      total: 24,
      readable: 23,
      not_used: 1,
      not_checked: 0,
    },
    regions: { selected: ["us-east-1", "eu-west-1"], enabled: 2, not_enabled: [] },
    api_use: { used_rps: 3, budget_rps: 4 },
    role_arn: "arn:aws:iam::111122223333:role/versus-reader",
    denied_services: [],
    not_on_plan_services: [],
    ...overrides,
  };
}

export function stagingAccount(overrides: Partial<AwsAccount> = {}): AwsAccount {
  return awsAccount({
    account_id: "444455556666",
    display_name: "Staging",
    checked_at: isoSecondsAgo(15),
    services: {
      total: 24,
      readable: 19,
      not_used: 3,
      not_checked: 0,
    },
    regions: {
      selected: ["us-east-1", "eu-west-1", "ap-southeast-1"],
      enabled: 2,
      not_enabled: ["ap-southeast-1"],
    },
    api_use: { used_rps: 3, budget_rps: 4 },
    role_arn: STAGING_ROLE_ARN,
    denied_services: ["elasticache"],
    not_on_plan_services: ["aws-health"],
    ...overrides,
  });
}

export function awsSettings(overrides: Partial<AwsSettings> = {}): AwsSettings {
  const accounts = overrides.accounts ?? [
    awsAccount(),
    stagingAccount(),
    awsAccount({
      account_id: "777788889999",
      display_name: "Shared services",
      checked_at: isoSecondsAgo(9),
      regions: {
        selected: ["us-east-1", "eu-west-1", "ap-southeast-1"],
        enabled: 3,
        not_enabled: [],
      },
    }),
  ];
  return {
    enabled: true,
    principal_arn: PRINCIPAL_ARN,
    accounts,
    optional_data: {
      cost_explorer: { on: accounts.length, source: "config" },
      config: { on: 2, source: "config" },
      security_hub: { on: 1, source: "config" },
      trusted_advisor: { on: 0, source: "wizard" },
    },
    privacy: {
      sent: [
        "resource_names_types_counts",
        "alarm_names_states",
        "allowlisted_tag_values",
      ],
      never_sent: [
        "secrets_env_user_data",
        "log_lines_request_bodies",
        "other_tag_values",
      ],
      tag_value_allowlist: ["app", "team", "env", "owner", "cost-center"],
      tag_value_allowlist_source: "config",
      ai: { enabled: true, provider: "claude", model: "sonnet-4" },
      self_test: { passing: true, last_run_at: isoSecondsAgo(3600), leaks_24h: 0 },
    },
    capabilities: { organizations: false, can_write: true },
    budget: { used_rps: 1.5, budget_rps: 2, source: "config" },
    ...overrides,
  };
}

export function draft(overrides: Partial<AwsConnectDraft> = {}): AwsConnectDraft {
  return {
    draft_id: "draft-1",
    method: "cloudformation",
    external_id_masked: "vrs-ext-••••••••••••7f3a",
    principal_arn: PRINCIPAL_ARN,
    template_url: "https://downloads.example.com/versus-reader.yaml",
    terraform_snippet: 'module "versus_reader" {\n  source = "./versus-reader"\n}',
    role_arn: "",
    assume: null,
    services: [],
    fix_policy: null,
    probe_id: null,
    ...overrides,
  };
}

export function partialServices(): AwsDraftService[] {
  return [
    { service: "ec2", status: "readable" },
    { service: "lambda", status: "readable" },
    { service: "kinesis", status: "not_used" },
    { service: "elasticache", status: "denied", fix_actions: ["elasticache:Describe*"] },
    { service: "aws-health", status: "not_on_plan" },
    { service: "ecs", status: "not_checked", error_code: "throttled" },
  ];
}

export function fullServices(): AwsDraftService[] {
  return [
    ...partialServices(),
    { service: "api-gateway", status: "readable" },
    { service: "auto-scaling", status: "readable" },
    { service: "cloudformation", status: "readable" },
    { service: "cloudfront", status: "readable" },
    { service: "cloudtrail", status: "readable" },
    { service: "cloudwatch", status: "readable" },
    { service: "config", status: "readable" },
    { service: "dynamodb", status: "readable" },
    { service: "eks", status: "readable" },
    { service: "elbv2", status: "readable" },
    { service: "eventbridge", status: "readable" },
    { service: "iam", status: "readable" },
    { service: "rds", status: "readable" },
    { service: "route53", status: "readable" },
    { service: "s3", status: "readable" },
    { service: "sns", status: "readable" },
    { service: "sqs", status: "readable" },
    { service: "step-functions", status: "readable" },
  ];
}

export function regionOptions(): AwsRegionOption[] {
  return [
    { region: "us-east-1", enabled: true, opt_in_status: "opt-in-not-required", resources_found: 241 },
    { region: "eu-west-1", enabled: true, opt_in_status: "opt-in-not-required", resources_found: 52 },
    { region: "ap-southeast-1", enabled: false, opt_in_status: "not-opted-in", resources_found: 0 },
  ];
}
