import { describe, expect, it } from "vitest";
import type { AwsAccount, AwsDraftService, AwsOptionalData } from "@/lib/api";
import { ApiError, awsErrorFrom, awsErrorText } from "@/lib/api";
import {
  AWS_SERVICE_CATALOG,
  AWS_SERVICE_TOTAL,
  accountRow,
  accountStatusWords,
  accountIdFromRoleArn,
  aiModelLine,
  answerWorstTone,
  assumeWords,
  awsSettingsAnswer,
  defaultAccountName,
  deniedFixes,
  deriveDraftCounts,
  gapWords,
  draftIsRunning,
  legendWords,
  notCheckedReason,
  optionalDataSummary,
  privacyLabel,
  regionLabel,
  roleNameFromArn,
  selfTestLine,
  serviceFixText,
  serviceLabel,
  serviceStatusLabel,
  serviceStatusText,
} from "@/lib/awsSettings";

const NOW = new Date("2026-11-04T14:21:00Z");

// Every field is set on purpose: no fixture is allowed to rely on a default
// that could hide a missing value on the real payload.
function account(overrides: Partial<AwsAccount> = {}): AwsAccount {
  return {
    account_id: "444455556666",
    display_name: "Staging",
    source: "wizard",
    status: "connected",
    checked_at: "2026-11-04T14:20:48Z",
    disconnected_since: null,
    error_code: null,
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
    role_arn: "arn:aws:iam::444455556666:role/versus-reader",
    denied_services: ["elasticache"],
    not_on_plan_services: ["aws-health"],
    ...overrides,
  };
}

function healthy(name: string, id = "111122223333"): AwsAccount {
  return account({
    account_id: id,
    display_name: name,
    services: {
      total: 24,
      readable: 23,
      not_used: 1,
      not_checked: 0,
    },
    regions: { selected: ["us-east-1", "eu-west-1"], enabled: 2, not_enabled: [] },
    api_use: { used_rps: 3, budget_rps: 4 },
    denied_services: [],
    not_on_plan_services: [],
  });
}

function kinds(segments: Array<{ tone: string }>) {
  return segments.filter((s) => s.tone !== "plain").map((s) => s.tone);
}

describe("awsSettingsAnswer precedence", () => {
  it("stops at AWS off even when accounts look healthy", () => {
    const answer = awsSettingsAnswer(
      { enabled: false, accounts: [healthy("Production")] },
      NOW,
    );
    expect(answer.kind).toBe("off");
    expect(answer.blocked).toBe(true);
    expect(answer.segments).toEqual([
      { text: "AWS connector is off", tone: "critical" },
    ]);
  });

  it("reports no accounts without claiming anything is wrong", () => {
    const answer = awsSettingsAnswer({ enabled: true, accounts: [] }, NOW);
    expect(answer.kind).toBe("no-accounts");
    expect(answer.blocked).toBe(false);
    expect(answer.segments[0].text).toBe("No AWS accounts connected");
    expect(kinds(answer.segments)).toEqual(["warn"]);
  });

  it("puts disconnected ahead of denied and unenabled regions", () => {
    const answer = awsSettingsAnswer(
      {
        enabled: true,
        accounts: [
          account({
            status: "disconnected",
            disconnected_since: "2026-11-04T13:05:00Z",
            denied_services: ["elasticache"],
          }),
          healthy("Production"),
        ],
      },
      NOW,
    );
    expect(answer.kind).toBe("disconnected");
    expect(answer.segments[0].text).toMatch(/^Staging disconnected since \d{2}:\d{2}$/);
    expect(answer.segments[1].text).toBe("; 1 account still connected");
    // The denied service is still in the payload, but the worst problem wins.
    expect(answer.segments.map((s) => s.text).join("")).not.toContain("ElastiCache");
    expect(kinds(answer.segments)).toEqual(["critical"]);
  });

  it("counts only the accounts that are still connected, not all rows", () => {
    const answer = awsSettingsAnswer(
      {
        enabled: true,
        accounts: [
          account({ status: "disconnected" }),
          account({ status: "disconnected", account_id: "777788889999", display_name: "Shared" }),
          healthy("Production"),
        ],
      },
      NOW,
    );
    expect(answer.segments[0].text).toMatch(/^Staging disconnected/);
    expect(answer.segments[0].text).toContain("and 1 more");
    expect(answer.segments[1].text).toBe("; 1 account still connected");
  });

  it("names the disconnected account alone when nobody else is connected", () => {
    const answer = awsSettingsAnswer(
      { enabled: true, accounts: [account({ status: "disconnected" })] },
      NOW,
    );
    expect(answer.segments[1].text).toBe("; no other accounts connected");
  });

  it("puts denied ahead of unenabled regions and names one service", () => {
    const answer = awsSettingsAnswer(
      {
        enabled: true,
        accounts: [
          account({ denied_services: ["elasticache"] }),
          healthy("Production", "111122223333"),
          healthy("Shared", "777788889999"),
        ],
      },
      NOW,
    );
    expect(answer.kind).toBe("denied");
    expect(answer.segments.map((s) => s.text).join("")).toBe(
      "3 accounts connected; Staging is missing ElastiCache access",
    );
    expect(answer.segments[1].tone).toBe("critical");
    expect(kinds(answer.segments)).toEqual(["critical"]);
  });

  it("falls back to a generic name when the denied service is not named", () => {
    const answer = awsSettingsAnswer(
      { enabled: true, accounts: [account({ denied_services: [""] })] },
      NOW,
    );
    expect(answer.kind).toBe("denied");
    expect(answer.segments[1].text).toBe("; Staging is missing access to a service");
  });

  it("counts further accounts that need access fixed", () => {
    const answer = awsSettingsAnswer(
      {
        enabled: true,
        accounts: [
          account({ denied_services: ["elasticache"] }),
          account({ account_id: "777788889999", display_name: "Shared", denied_services: ["rds"] }),
        ],
      },
      NOW,
    );
    expect(answer.segments[2].text).toBe("; 1 more account needs access fixed");
  });

  it("mentions cost data only when a paid read is on", () => {
    const off = awsSettingsAnswer(
      { enabled: true, accounts: [account({ denied_services: ["elasticache"] })] },
      NOW,
    );
    expect(off.segments.map((s) => s.text).join("")).not.toContain("cost data on");

    const on = awsSettingsAnswer(
      {
        enabled: true,
        accounts: [account({ denied_services: ["elasticache"] })],
        optional_data: { cost_explorer: { on: 1 } },
      },
      NOW,
    );
    expect(on.segments[on.segments.length - 1].text).toBe("; cost data on");
  });

  it("reports regions not enabled after denied and before all-good", () => {
    const answer = awsSettingsAnswer(
      {
        enabled: true,
        accounts: [
          account({
            services: {
              total: 24,
              readable: 24,
              not_used: 0,
              not_checked: 0,
            },
            denied_services: [],
            not_on_plan_services: [],
          }),
        ],
      },
      NOW,
    );
    expect(answer.kind).toBe("regions");
    expect(answer.segments.map((s) => s.text).join("")).toBe(
      "1 account connected; Staging has 1 region not enabled",
    );
    expect(kinds(answer.segments)).toEqual(["warn"]);
  });

  it("says all good only when nothing above applies", () => {
    const answer = awsSettingsAnswer(
      { enabled: true, accounts: [healthy("Production"), healthy("Staging", "444455556666")] },
      NOW,
    );
    expect(answer.kind).toBe("ok");
    expect(answer.segments).toEqual([
      { text: "All good across 2 accounts", tone: "ok" },
    ]);
  });

  it("colours exactly one segment in every state", () => {
    const fixtures = [
      awsSettingsAnswer({ enabled: false, accounts: [] }, NOW),
      awsSettingsAnswer({ enabled: true, accounts: [] }, NOW),
      awsSettingsAnswer({ enabled: true, accounts: [account({ status: "disconnected" })] }, NOW),
      awsSettingsAnswer({ enabled: true, accounts: [account({ denied_services: ["rds"] })] }, NOW),
      awsSettingsAnswer({ enabled: true, accounts: [healthy("Production")] }, NOW),
    ];
    for (const answer of fixtures) {
      expect(kinds(answer.segments)).toHaveLength(1);
      expect(answerWorstTone(answer)).toBe(kinds(answer.segments)[0]);
    }
  });
});

describe("account rows", () => {
  it("writes connected freshness from the check time", () => {
    const row = accountRow(account(), NOW);
    expect(row.statusLabel).toBe("Connected");
    expect(row.statusDetail).toBe("checked 12 s ago");
    expect(row.servicesLabel).toBe("19 of 24 readable");
    expect(row.deniedLabel).toBe("ElastiCache: Denied");
    expect(row.regionsLabel).toBe("2 of 3");
    expect(row.regionsDetail).toBe("1 not enabled");
    expect(row.apiLabel).toBe("3 of 4 req/s");
    expect(row.apiFraction).toBeCloseTo(0.75);
    expect(row.fromConfig).toBe(false);
  });

  it("names the denied service only when one is denied", () => {
    const denied = accountRow(account({ denied_services: ["elasticache"] }), NOW);
    expect(denied.deniedLabel).toBe("ElastiCache: Denied");

    const clean = accountRow(
      account({
        services: {
          total: 24,
          readable: 20,
          not_used: 4,
          not_checked: 0,
        },
        denied_services: [],
        not_on_plan_services: [],
      }),
      NOW,
    );
    expect(clean.deniedLabel).toBeUndefined();
  });

  it("dates a disconnected row and never prints an age as if it were live", () => {
    const words = accountStatusWords(
      account({ status: "disconnected", disconnected_since: "2026-11-04T13:05:00Z" }),
      NOW,
    );
    expect(words.label).toBe("Disconnected");
    expect(words.detail).toMatch(/^since \d{2}:\d{2}$/);
    expect(words.connected).toBe(false);
  });

  it("does not invent a time when the server sent none", () => {
    const words = accountStatusWords(
      account({ status: "disconnected", disconnected_since: null }),
      NOW,
    );
    expect(words.detail).toBe("reconnect to check again");
  });

  it("marks config accounts read-only to the row", () => {
    expect(accountRow(account({ source: "config" }), NOW).fromConfig).toBe(true);
  });

  it("shows all-enabled regions without a false gap", () => {
    const row = accountRow(
      account({
        regions: { selected: ["us-east-1"], enabled: 1, not_enabled: [] },
      }),
      NOW,
    );
    expect(row.regionsLabel).toBe("1 of 1");
    expect(row.regionsDetail).toBe("all enabled");
  });

  it("keeps the API bar empty when the budget is unknown", () => {
    const row = accountRow(
      account({ api_use: { used_rps: 3, budget_rps: 0 } }),
      NOW,
    );
    expect(row.apiFraction).toBe(0);
    expect(row.apiLabel).toBe("3 of 0 req/s");
  });
});

describe("label maps", () => {
  it("names all 24 catalog services with unique keys", () => {
    expect(AWS_SERVICE_TOTAL).toBe(24);
    const keys = new Set(AWS_SERVICE_CATALOG.map((s) => s.key));
    expect(keys.size).toBe(24);
    expect(serviceLabel("elasticache")).toBe("ElastiCache");
    expect(serviceLabel("step-functions")).toBe("Step Functions");
    expect(serviceLabel("mystery")).toBe("mystery");
  });

  it("keeps a probe failure in the not-checked vocabulary", () => {
    expect(notCheckedReason("throttled")).toBe("AWS throttled, retrying");
    expect(notCheckedReason("Throttling")).toBe("AWS throttled, retrying");
    expect(notCheckedReason("timeout")).toBe("AWS timed out, retrying");
    expect(notCheckedReason("network")).toBe("Network error, retrying");
    expect(notCheckedReason(undefined)).toBe("AWS did not answer, retrying");
    expect(notCheckedReason("throttled")).not.toContain("Denied");
  });

  it("writes a status as words plus the service name", () => {
    expect(serviceStatusText("elasticache", "denied")).toBe("ElastiCache: Denied");
    expect(serviceStatusText("ec2", "readable")).toBe("EC2: Readable");
    expect(serviceStatusText("ecs", "not_checked", "throttled")).toBe(
      "ECS: Not checked: AWS throttled, retrying",
    );
  });

  it("words a sync gap for a service missing in the region", () => {
    expect(gapWords("not_available_in_region")).toBe("Not in this region");
  });

  it("names a not-used probe with a known regional cause instead of Not used", () => {
    expect(serviceStatusLabel("not_used", "not_available_in_region")).toBe("Not in this region");
    expect(serviceStatusText("eks", "not_used", "not_available_in_region")).toBe(
      "EKS: Not in this region",
    );
    expect(serviceStatusLabel("not_used")).toBe("Not used");
  });

  it("names known regions and falls back to the code", () => {
    expect(regionLabel("us-east-1")).toBe("US East (N. Virginia)");
    expect(regionLabel("me-south-1")).toBe("me-south-1");
  });

  it("translates privacy payload codes", () => {
    expect(privacyLabel("allowlisted_tag_values")).toBe("Shared tag values");
    expect(privacyLabel("something_new")).toBe("something_new");
  });
});

describe("privacy and optional data lines", () => {
  it("summarises paid reads per account count", () => {
    const data: AwsOptionalData = {
      cost_explorer: { on: 3 },
      config: { on: 2 },
      security_hub: { on: 1 },
    };
    expect(optionalDataSummary(data, 3)).toBe(
      "Cost on in all accounts, Config in 2, Security Hub in 1",
    );
    expect(optionalDataSummary({}, 3)).toBe("none on");
    expect(optionalDataSummary({ cost_explorer: { on: 1 } }, 1)).toBe(
      "Cost on in all accounts",
    );
  });

  it("prints the configured provider and model", () => {
    const privacy = {
      sent: [],
      never_sent: [],
      tag_value_allowlist: [],
      tag_value_allowlist_source: "wizard",
      ai: { enabled: true, provider: "claude", model: "sonnet-4" },
      self_test: { passing: true, last_run_at: null, leaks_24h: 0 },
    };
    expect(aiModelLine(privacy)).toBe("AI model: claude · sonnet-4");
    expect(
      aiModelLine({ ...privacy, ai: { enabled: false, provider: "claude", model: "sonnet-4" } }),
    ).toBe("AI is off — nothing is sent");
  });

  it("says the leak check passed with its count", () => {
    expect(
      selfTestLine({ passing: true, last_run_at: null, leaks_24h: 0 }, NOW),
    ).toBe("Leak check: passing, 0 leaks in 24 h");
    expect(
      selfTestLine({ passing: false, last_run_at: "2026-11-04T14:15:00Z", leaks_24h: 1 }, NOW),
    ).toMatch(/^Leak check: failing, last run \d{2}:\d{2}, 1 leak in 24 h$/);
  });
});

describe("connect wizard derivations", () => {
  const services: AwsDraftService[] = [
    { service: "ec2", status: "readable" },
    { service: "lambda", status: "readable" },
    { service: "kinesis", status: "not_used" },
    { service: "elasticache", status: "denied", fix_actions: ["elasticache:Describe*"] },
    { service: "aws-health", status: "not_on_plan" },
    { service: "ecs", status: "not_checked", error_code: "throttled" },
  ];

  it("counts what landed and leaves the rest pending", () => {
    const counts = deriveDraftCounts(services);
    expect(counts).toMatchObject({
      total: 24,
      readable: 2,
      not_used: 1,
      denied: 1,
      not_on_plan: 1,
      not_checked: 1,
      pending: 18,
    });
    expect(draftIsRunning(services)).toBe(true);
    expect(draftIsRunning(AWS_SERVICE_CATALOG.map((s) => ({ service: s.key, status: "readable" as const })))).toBe(false);
  });

  it("does not double count a service reported twice", () => {
    const counts = deriveDraftCounts([
      { service: "ec2", status: "not_checked", error_code: "throttled" },
      { service: "ec2", status: "readable" },
    ]);
    expect(counts.readable + counts.not_checked).toBe(1);
  });

  it("writes the legend without hiding a retry", () => {
    expect(legendWords(deriveDraftCounts(services))).toBe(
      "2 readable · 1 not used · 1 denied · 1 not on plan · 1 not checked · 18 checking",
    );
  });

  it("builds one copyable fix per denied service", () => {
    const fixes = deniedFixes(services, "arn:aws:iam::444455556666:role/versus-reader");
    expect(fixes).toHaveLength(1);
    expect(fixes[0].fix).toBe("Add elasticache:Describe* to versus-reader");
    expect(serviceFixText("dynamodb", "arn:aws:iam::1:role/read-only")).toBe(
      "Add dynamodb:ListTables, dynamodb:DescribeTable to read-only",
    );
    expect(serviceFixText("unknown", "arn:aws:iam::1:role/x")).toBe(
      "Add the missing permission to x",
    );
  });

  it("reads the role name and account id out of the ARN", () => {
    expect(roleNameFromArn("arn:aws:iam::444455556666:role/team/versus-reader")).toBe(
      "versus-reader",
    );
    expect(roleNameFromArn("")).toBe("versus-reader");
    expect(accountIdFromRoleArn("arn:aws:iam::444455556666:role/versus-reader")).toBe(
      "444455556666",
    );
    expect(accountIdFromRoleArn("not-an-arn")).toBeUndefined();
    expect(defaultAccountName("arn:aws:iam::444455556666:role/versus-reader")).toBe(
      "AWS account 444455556666",
    );
  });

  it("writes the assume result in milliseconds or as a failure", () => {
    expect(assumeWords({ ok: true, latency_ms: 212 })).toBe("Role assumed in 212 ms");
    expect(assumeWords({ ok: true })).toBe("Role assumed");
    expect(assumeWords({ ok: false, error_code: "external_id_mismatch" })).toBe(
      "Role could not be assumed",
    );
  });
});

describe("refresh in progress wording", () => {
  it("puts refresh_in_progress into short plain words", () => {
    const info = awsErrorText("refresh_in_progress");
    expect(`${info.title}. ${info.detail}`).toBe("A cost refresh is running. Try again in a few seconds.");
    expect(awsErrorFrom(new ApiError(409, "refresh_in_progress", { error: "refresh_in_progress" }))).toBe(info);
    expect(awsErrorText("busy").code).toBe("unknown");
  });
});

describe("before the first health check", () => {
  // A config account with regions "auto" right after boot: no check has run,
  // so the server still reports zero counts and no regions.
  function unchecked(): AwsAccount {
    return healthy("Harness", "123456789012");
  }
  const fresh = (): AwsAccount => ({
    ...unchecked(),
    source: "config",
    checked_at: null,
    services: { total: 24, readable: 0, not_used: 0, not_checked: 0 },
    regions: { selected: [], enabled: 0, not_enabled: [] },
  });

  it("waits instead of saying all good over zero counts", () => {
    const answer = awsSettingsAnswer({ enabled: true, accounts: [fresh()] }, NOW);
    expect(answer.kind).toBe("checking");
    expect(answer.segments.map((s) => s.text).join("")).toBe("1 account connected; waiting for the first check");
    expect(kinds(answer.segments)).toEqual([]);
  });

  it("shows waiting words in the row, never 0 of 24 or 0 of 0", () => {
    const row = accountRow(fresh(), NOW);
    expect(row.statusDetail).toBe("Waiting for first check");
    expect(row.servicesLabel).toBe("Waiting");
    expect(row.regionsLabel).toBe("Waiting");
    expect(row.regionsDetail).toBe("");
  });

  it("counts again once the check has run", () => {
    const row = accountRow({ ...fresh(), checked_at: "2026-11-04T14:20:48Z" }, NOW);
    expect(row.servicesLabel).toBe("0 of 24 readable");
    expect(awsSettingsAnswer({ enabled: true, accounts: [unchecked()] }, NOW).kind).toBe("ok");
  });
});
