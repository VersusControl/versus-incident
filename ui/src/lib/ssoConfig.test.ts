import { describe, it, expect } from "vitest";
import { ApiError } from "@/lib/api";
import {
  classifySsoError,
  formatList,
  parseList,
  rejectsAllLogins,
  ssoErrorText,
} from "@/lib/ssoConfig";

// These tests pin the pure decision logic the SSO connections control (X4)
// hangs off, since the UI has no DOM test harness. The contracts that matter:
//   1. a 404 disambiguates between OSS route-absent (locked), a licensed but
//      unconfigured org (editable form), and an unprovisioned org.
//   2. 401/403/503 route to "forbidden" (no session / insufficient RBAC role /
//      guard not wired) — the role-gated equivalent of the retired token states.
//   3. empty allowed_domains is flagged as reject-all.

const ossRouteAbsent = () =>
  new ApiError(404, "Cannot GET /enterprise/api/sso/default/config", "Cannot GET /enterprise/api/sso/default/config");

const noConfig = () =>
  new ApiError(404, "no IdP configuration for org", {
    error: "no IdP configuration for org",
  });

const unknownOrg = () =>
  new ApiError(404, "unknown or inactive org", {
    error: "unknown or inactive org",
  });

describe("ssoErrorText", () => {
  it("reads the JSON error field", () => {
    expect(ssoErrorText(noConfig())).toBe("no IdP configuration for org");
  });

  it("falls back to a string body (Fiber's plain-text 404)", () => {
    expect(ssoErrorText(ossRouteAbsent())).toMatch(/Cannot GET/);
  });

  it("returns '' for non-ApiErrors", () => {
    expect(ssoErrorText(new Error("boom"))).toBe("");
    expect(ssoErrorText("nope")).toBe("");
  });
});

describe("classifySsoError", () => {
  it("treats a 403 as forbidden (community / insufficient role)", () => {
    expect(classifySsoError(new ApiError(403, "forbidden"))).toBe("forbidden");
  });

  it("locks on a route-absent 404 (OSS binary, plain-text body)", () => {
    expect(classifySsoError(ossRouteAbsent())).toBe("locked");
  });

  it("treats a JSON no-config 404 as unconfigured (licensed, empty org)", () => {
    expect(classifySsoError(noConfig())).toBe("unconfigured");
  });

  it("flags an unknown/inactive org 404 distinctly", () => {
    expect(classifySsoError(unknownOrg())).toBe("org-unknown");
  });

  it("routes 401 / 503 to forbidden (no session / guard not wired)", () => {
    expect(classifySsoError(new ApiError(401, "no session"))).toBe("forbidden");
    expect(classifySsoError(new ApiError(503, "no guard"))).toBe("forbidden");
  });

  it("is 'error' for anything else", () => {
    expect(classifySsoError(new ApiError(500, "boom"))).toBe("error");
    expect(classifySsoError(new Error("x"))).toBe("error");
  });
});

describe("parseList / formatList", () => {
  it("splits on commas, whitespace and newlines, trims and de-dupes", () => {
    expect(parseList("openid, email  profile\nopenid")).toEqual([
      "openid",
      "email",
      "profile",
    ]);
  });

  it("returns [] for blank input", () => {
    expect(parseList("   ")).toEqual([]);
    expect(parseList("")).toEqual([]);
  });

  it("round-trips through formatList", () => {
    expect(formatList(["acme.com", "example.org"])).toBe("acme.com, example.org");
    expect(formatList(undefined)).toBe("");
    expect(formatList(null)).toBe("");
  });
});

describe("rejectsAllLogins", () => {
  it("is true only for an empty allow-list", () => {
    expect(rejectsAllLogins([])).toBe(true);
    expect(rejectsAllLogins(["acme.com"])).toBe(false);
  });
});
