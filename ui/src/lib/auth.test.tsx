// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { AuthGate } from "./auth";
import * as apiModule from "./api";
import * as ssoReturn from "@/lib/ssoReturn";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  window.history.replaceState(null, "", "/");
});

function rejectAmbientSession() {
  vi.spyOn(apiModule.api, "getAgentConfig").mockRejectedValue(
    new apiModule.ApiError(401, "unauthorized"),
  );
}

describe("AuthGate deployment login surfaces", () => {
  it("builds each SSO login URL with the current investigation path and query", async () => {
    const path = "/agent/chat?provider=kubernetes&cluster=east&resource_id=apps~v1~deployments&name=checkout&issue=pod.crash_loop";
    window.history.replaceState(null, "", path);
    rejectAmbientSession();
    vi.spyOn(apiModule.api, "getSSODeployment").mockResolvedValue({ org: "acme" });
    vi.spyOn(apiModule, "getSsoSession").mockRejectedValue(new apiModule.ApiError(401, "no active session"));
    vi.spyOn(apiModule, "getSsoStatus").mockResolvedValue({
      enabled: true, connections: [{ id: "work", type: "oidc", display_name: "Work", login_url: "/api/sso/login/oidc?connection=work" }],
    });
    const buildUrl = vi.spyOn(ssoReturn, "ssoLoginWithReturn");
    render(<AuthGate><div>console</div></AuthGate>);
    await screen.findByRole("button", { name: "Sign in with Work" });
    expect(buildUrl).toHaveBeenCalledWith("/api/sso/login/oidc?connection=work", window.location);
    const result = buildUrl.mock.results.at(-1)?.value as string;
    expect(new URL(result, window.location.origin).searchParams.get("return_to")).toBe(path);
  });
  it("shows Enterprise login without ever presenting gateway secret", async () => {
    rejectAmbientSession();
    vi.spyOn(apiModule.api, "getSSODeployment").mockResolvedValue({ org: "acme" });
    vi.spyOn(apiModule, "getSsoSession").mockRejectedValue(
      new apiModule.ApiError(401, "no active session"),
    );
    vi.spyOn(apiModule, "getSsoStatus").mockResolvedValue({ enabled: false });

    render(<AuthGate><div>console</div></AuthGate>);

    expect(await screen.findByTestId("local-login-form")).toBeTruthy();
    expect(screen.queryByLabelText("Gateway secret")).toBeNull();
  });

  it("shows gateway login only after authoritative community discovery", async () => {
    rejectAmbientSession();
    vi.spyOn(apiModule.api, "getSSODeployment").mockRejectedValue(
      new apiModule.ApiError(403, "not enterprise"),
    );

    render(<AuthGate><div>console</div></AuthGate>);

    expect(await screen.findByLabelText("Gateway secret")).toBeTruthy();
    expect(screen.queryByTestId("local-login-form")).toBeNull();
  });

  it("shows retry with no credential form after transient discovery failure", async () => {
    rejectAmbientSession();
    vi.spyOn(apiModule.api, "getSSODeployment").mockRejectedValue(
      new TypeError("network unavailable"),
    );

    render(<AuthGate><div>console</div></AuthGate>);

    expect(await screen.findByRole("button", { name: "Retry" })).toBeTruthy();
    expect(screen.queryByLabelText("Gateway secret")).toBeNull();
    expect(screen.queryByTestId("local-login-form")).toBeNull();
  });
});