// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { AuthGate } from "./auth";
import * as apiModule from "./api";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function rejectAmbientSession() {
  vi.spyOn(apiModule.api, "getAgentConfig").mockRejectedValue(
    new apiModule.ApiError(401, "unauthorized"),
  );
}

describe("AuthGate deployment login surfaces", () => {
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