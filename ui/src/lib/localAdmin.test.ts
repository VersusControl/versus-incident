import { describe, it, expect } from "vitest";
import { ApiError } from "@/lib/api";
import {
  GENERIC_LOGIN_ERROR,
  UNREACHABLE_LOGIN_ERROR,
  classifyLocalLoginError,
  isLocalAdminSession,
  isNoOtherAdminError,
} from "@/lib/localAdmin";

// Pure-logic tests for the built-in default-admin login decisions the SPA has
// no DOM harness to assert. The contracts that matter for the auth surface:
//   1. a 429 is a DISTINCT lockout state (not the generic 401);
//   2. every credential failure (401, and any other terminal HTTP answer) is a
//      GENERIC message — it never reveals wrong-password vs disabled (no user
//      enumeration, mirroring the server);
//   3. sign-out picks the local-admin logout route iff the session is local;
//   4. the no-lockout refusal (422 no_other_admin) is detectable so the card
//      can surface it in the DOM.

describe("classifyLocalLoginError", () => {
  it("maps a 429 to the distinct lockout state", () => {
    const state = classifyLocalLoginError(new ApiError(429, "locked"));
    expect(state).toEqual({ kind: "locked" });
  });

  it("maps a 401 to a generic, non-enumerating message", () => {
    const state = classifyLocalLoginError(
      new ApiError(401, "invalid credentials"),
    );
    expect(state).toEqual({ kind: "error", message: GENERIC_LOGIN_ERROR });
    // The message must NOT leak whether the account exists or is disabled.
    if (state.kind === "error") {
      expect(state.message).not.toMatch(
        /disabled|no such user|unknown user|not found/i,
      );
    }
  });

  it("collapses other terminal HTTP answers (403/500) to the same generic message", () => {
    // A community 403 or a 500 must not be distinguishable from a bad password.
    expect(classifyLocalLoginError(new ApiError(403, "enterprise license required"))).toEqual({
      kind: "error",
      message: GENERIC_LOGIN_ERROR,
    });
    expect(classifyLocalLoginError(new ApiError(500, "could not verify credentials"))).toEqual({
      kind: "error",
      message: GENERIC_LOGIN_ERROR,
    });
  });

  it("maps a non-HTTP (transport) failure to the unreachable message", () => {
    const state = classifyLocalLoginError(new Error("network down"));
    expect(state).toEqual({ kind: "error", message: UNREACHABLE_LOGIN_ERROR });
  });
});

describe("isLocalAdminSession", () => {
  it("is true only for a session flagged local", () => {
    expect(isLocalAdminSession({ local: true })).toBe(true);
  });

  it("is false for an SSO session, a missing flag, or no session (fail closed)", () => {
    expect(isLocalAdminSession({ local: false })).toBe(false);
    expect(isLocalAdminSession({})).toBe(false);
    expect(isLocalAdminSession(null)).toBe(false);
    expect(isLocalAdminSession(undefined)).toBe(false);
  });
});

describe("isNoOtherAdminError", () => {
  it("recognises the 422 no_other_admin refusal", () => {
    expect(
      isNoOtherAdminError(
        new ApiError(422, "cannot disable", { code: "no_other_admin" }),
      ),
    ).toBe(true);
    // A 422 on this route is the no-lockout guard even without an explicit code.
    expect(isNoOtherAdminError(new ApiError(422, "cannot disable"))).toBe(true);
  });

  it("ignores other statuses / non-API errors (fail closed)", () => {
    expect(
      isNoOtherAdminError(new ApiError(403, "forbidden", { code: "no_other_admin" })),
    ).toBe(false);
    expect(isNoOtherAdminError(new ApiError(500, "boom"))).toBe(false);
    expect(isNoOtherAdminError(new Error("nope"))).toBe(false);
    expect(isNoOtherAdminError(null)).toBe(false);
  });
});
