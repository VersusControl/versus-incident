import { describe, expect, it } from "vitest";
import { ApiError } from "@/lib/api";
import { isNotConfiguredAnswer, kubernetesErrorCode, NOT_CONFIGURED_MESSAGE, reportProvesConfigured } from "./kubernetesConnector";

const notConfigured = () => new ApiError(503, NOT_CONFIGURED_MESSAGE, { error: NOT_CONFIGURED_MESSAGE });

describe("the not-configured answer", () => {
  it("recognises the plain answer a deployment with no connector gets", () => {
    expect(isNotConfiguredAnswer(notConfigured())).toBe(true);
  });

  it("recognises a coded setup answer whatever the code is prefixed with", () => {
    for (const code of ["not_configured", "connector_not_configured", "kubernetes_connector_not_configured"]) {
      expect(isNotConfiguredAnswer(new ApiError(503, NOT_CONFIGURED_MESSAGE, { error: NOT_CONFIGURED_MESSAGE, code }))).toBe(true);
    }
  });

  // The words alone are not the answer: a dead credential used to produce them.
  it("refuses any other code, even when the message is identical", () => {
    for (const code of ["credential_unavailable", "cluster_authentication_failed", "connector_unreachable"]) {
      expect(isNotConfiguredAnswer(new ApiError(503, NOT_CONFIGURED_MESSAGE, { error: NOT_CONFIGURED_MESSAGE, code }))).toBe(false);
    }
  });

  it("refuses a status the setup answer is never sent with", () => {
    for (const status of [500, 502, 401, 403, 404]) {
      expect(isNotConfiguredAnswer(new ApiError(status, NOT_CONFIGURED_MESSAGE, { error: NOT_CONFIGURED_MESSAGE }))).toBe(false);
    }
  });

  it("refuses anything that is not an API answer", () => {
    expect(isNotConfiguredAnswer(new Error(NOT_CONFIGURED_MESSAGE))).toBe(false);
    expect(isNotConfiguredAnswer(undefined)).toBe(false);
    expect(isNotConfiguredAnswer(null)).toBe(false);
  });

  it("reads a code only from a body that has one", () => {
    expect(kubernetesErrorCode({ code: "credential_unavailable" })).toBe("credential_unavailable");
    expect(kubernetesErrorCode({ code: "" })).toBeUndefined();
    expect(kubernetesErrorCode({ code: 3 })).toBeUndefined();
    expect(kubernetesErrorCode("credential_unavailable")).toBeUndefined();
    expect(kubernetesErrorCode(undefined)).toBeUndefined();
  });
});

describe("report evidence", () => {
  // The report route answers for a configured cluster whatever its credential is
  // doing, and never answers for a deployment with no connector.
  it("counts a report as proof that a cluster is configured", () => {
    expect(reportProvesConfigured({ isSuccess: true, isError: false, error: undefined })).toBe(true);
    expect(reportProvesConfigured({ isSuccess: false, isError: true, error: new ApiError(502, "credential revoked", { error: "credential revoked", code: "credential_unavailable" }) })).toBe(true);
  });

  it("does not count a not-configured report as proof", () => {
    expect(reportProvesConfigured({ isSuccess: false, isError: true, error: notConfigured() })).toBe(false);
  });

  it("does not count a report that has not answered yet", () => {
    expect(reportProvesConfigured({ isSuccess: false, isError: false, error: undefined })).toBe(false);
  });
});
