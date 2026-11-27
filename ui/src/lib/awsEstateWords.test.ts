import { describe, expect, it } from "vitest";
import { coverageTitle, humanise, notReadBadge, resourceTypeWords, type CoverageRow } from "./awsEstate";

// Every code Core sends for an account or source that was not read: the
// coverage read's account details, the settings outage codes and the gap
// reasons. None may reach the page as a raw key.
const CODES = [
  "not_read_yet",
  "account_disconnected",
  "credentials_invalid",
  "assume_role_failed",
  "account_mismatch",
  "no_server_identity",
  "role_not_found",
  "trust_denied",
  "external_id_mismatch",
  "session_duration_invalid",
  "region_disabled",
  "throttled",
  "timeout",
  "network",
  "refresh_in_progress",
  "denied",
  "not_on_plan",
  "not_checked",
  "capped",
  "not_enabled",
  "not_available_in_region",
];

describe("plain words for wire keys", () => {
  it.each(CODES)("names the not-read code %s in words, never the code", (code) => {
    const words = notReadBadge(code);
    expect(words).not.toBe(code);
    expect(words).toMatch(/^[A-Z]/);
    expect(words).not.toMatch(/_/);
    expect(words).not.toBe("Not read (see Settings)");
  });

  it("names the codes QA saw on the page", () => {
    expect(notReadBadge("assume_role_failed")).toBe("Read role could not be assumed");
    expect(notReadBadge("not_read_yet")).toBe("Not read yet");
  });

  it("never prints an unknown code, and keeps words the server already wrote", () => {
    expect(notReadBadge("some_future_code")).toBe("Not read (see Settings)");
    expect(notReadBadge("AccessDeniedException")).toBe("Not read (see Settings)");
    expect(notReadBadge("access denied")).toBe("Not read: access denied");
    expect(notReadBadge(undefined)).toBe("Not read");
  });

  it.each([
    ["nat-gateway", undefined, "NAT gateway"],
    ["network-interface", undefined, "Network interface"],
    ["route-table", undefined, "Route table"],
    ["security-group", undefined, "Security group"],
    ["transit-gateway-attachment", undefined, "Transit gateway attachment"],
    ["transit-gateway", undefined, "Transit gateway"],
    ["service", "ecs", "ECS service"],
    ["cluster", "eks", "EKS cluster"],
    ["target-group", undefined, "Target group"],
    ["something-brand-new", undefined, "Something brand new"],
  ])("names the resource type %s", (type, service, words) => {
    expect(resourceTypeWords(type, service)).toBe(words);
  });

  it.each([
    ["sqs", undefined, "SQS"],
    ["sns", undefined, "SNS"],
    ["eventbridge", undefined, "EventBridge"],
    ["kinesis", undefined, "Kinesis"],
    ["step-functions", undefined, "Step Functions"],
    ["tagging", undefined, "Tagging API"],
    ["appregistry", "ListAssociatedResources", "AppRegistry resources"],
    ["lambda", "ListFunctionUrlConfigs", "Function URL auth"],
    ["new-service", "DescribeNewThings", "Describe new things"],
  ])("names the coverage source %s %s", (source, operation, words) => {
    const row: CoverageRow = { source, operation, outcome: "not_read", reason: "denied", accounts: [] };
    expect(coverageTitle(row, (id) => id)).toBe(words);
  });

  it("humanises keys in sentence case", () => {
    expect(humanise("ListAssociatedResources")).toBe("List associated resources");
    expect(humanise("foo_bar-baz")).toBe("Foo bar baz");
  });
});
