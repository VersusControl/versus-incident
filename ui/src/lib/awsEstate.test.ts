import { describe, expect, it } from "vitest";
import {
  countText,
  countWords,
  coverageWarningText,
  correctionMoved,
  linkLabel,
  linkSides,
  notGapsText,
  notReadLine,
  readState,
  sharePercent,
} from "./awsEstate";

describe("AWS estate wording", () => {
  it("never prints a capped or unread count as exact", () => {
    expect(countWords(3000, "read")).toEqual({ value: "3,000", state: "read" });
    expect(countText(3000, "capped")).toBe("more than 3,000, not all read");
    expect(countWords(0, "capped").value).toBe("Not all read");
    expect(countWords(5, "not_read").value).toBe("—");
    expect(countWords(null, "read").state).toBe("not_read");
    expect(countWords(Number.NaN, "read").value).toBe("—");
  });

  it("writes the warning line only when a source was not read", () => {
    expect(coverageWarningText({ not_read: 3 })).toBe("Data not complete: 3 sources not read");
    expect(coverageWarningText({ not_read: 1 })).toBe("Data not complete: 1 source not read");
    expect(coverageWarningText({ not_read: 0 })).toBeNull();
    expect(coverageWarningText(null)).toBeNull();
  });

  it("reads a null count as not read and a capped flag as a lower bound", () => {
    expect(readState(null, false)).toBe("not_read");
    expect(readState(undefined, true)).toBe("not_read");
    expect(readState(0, false)).toBe("read");
    expect(readState(10, true)).toBe("capped");
    expect(sharePercent(null)).toBe("—");
    expect(sharePercent(62)).toBe("62%");
  });

  it("draws a link from one side unless every link behind it was read from both", () => {
    expect(linkSides({ seen_from: "both" })).toBe("both");
    expect(linkSides({ seen_from: "mixed" })).toBe("one");
    expect(linkSides({ seen_from: "one" })).toBe("one");
    // Inside one account a link has no sides: it is read whole.
    expect(linkSides({ seen_from: "" })).toBe("both");
    expect(linkLabel({ type: "alias_of", count: 58, count_capped: true })).toBe("DNS, more than 58");
    expect(linkLabel({ type: "alias_of", count: 58 })).toBe("DNS 58");
    expect(linkLabel({ type: "attached_to_transit", count: 1 })).toBe("Transit gateway");
  });

  it("sees a correction changed by someone else", () => {
    const c = { account_id: "1", kind: "rename" as const, group_id: "g", name: "A", created_at: "t1" };
    expect(correctionMoved(undefined, undefined)).toBe(false);
    expect(correctionMoved(c, { ...c })).toBe(false);
    expect(correctionMoved(undefined, c)).toBe(true);
    expect(correctionMoved(c, undefined)).toBe(true);
    expect(correctionMoved(c, { ...c, created_at: "t2" })).toBe(true);
  });

  it("words coverage rows", () => {
    const row = { source: "lambda", operation: "ListFunctionUrlConfigs", outcome: "not_read" as const, reason: "denied", accounts: ["a", "b"], seen: 28, total: 35 };
    expect(notReadLine(row, ["network", "prod"], 2)).toBe("28 of 35 read, denied in all 2 accounts");
    expect(notReadLine(row, ["network", "prod"], 4)).toBe("28 of 35 read, denied in network and prod");
    expect(notGapsText({ services_not_offered: 7, regions_not_enabled: 0 })).toEqual({ text: "7 services not offered", parts: 1 });
  });

  it("names links with the mock-up words, by the call that read them, then their type", () => {
    expect(linkLabel({ type: "may_access", count: 1, source: "s3:GetBucketPolicy" })).toBe("Bucket policy");
    expect(linkLabel({ type: "may_access", count: 2, source: "kms:GetKeyPolicy" })).toBe("KMS key 2");
    expect(linkLabel({ type: "may_access", count: 1, source: "kms:GetKeyPolicy" }, "detail")).toBe("Key policy");
    expect(linkLabel({ type: "alias_of", count: 1, source: "route53:ListResourceRecordSets" }, "detail")).toBe("Alias record");
    expect(linkLabel({ type: "attached_to_transit", count: 1, source: "ec2:DescribeTransitGatewayAttachments" }, "detail")).toBe("Transit gateway attachment");
    expect(linkLabel({ type: "trusts", count: 2, source: "iam:ListRoles" })).toBe("Role trust 2");
    expect(linkLabel({ type: "origin", count: 1, kind: "inferred" }, "detail")).toBe("Origin hostname matches");
    expect(linkLabel({ type: "routes_to", count: 1 }, "detail")).toBe("Listener rule");
    expect(linkLabel({ type: "routes_to", count: 1 }, "detail", "elasticloadbalancing:targetgroup")).toBe("Registered target");
    expect(linkLabel({ type: "member_of", count: 1 })).toBe("Runs on cluster");
    expect(linkLabel({ type: "something_new", count: 1 })).toBe("something new");
  });

  it("names each call a summed link was read by, with its count", () => {
    const link = { type: "may_access", kind: "may_access" as const, count: 3, sources: { "kms:GetKeyPolicy": 2, "ecr:GetRepositoryPolicy": 1 } };
    expect(linkLabel(link)).toBe("KMS key 2, ECR repository 1");
    expect(linkLabel({ ...link, count_capped: true })).toBe("KMS key 2, ECR repository 1, not all read");
    expect(linkLabel({ type: "may_access", count: 2, sources: { "kms:GetKeyPolicy": 2 } })).toBe("KMS key 2");
    expect(linkLabel({ type: "may_access", count: 2 })).toBe("May access 2");
  });
});
