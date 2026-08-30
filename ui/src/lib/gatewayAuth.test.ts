// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  ApiError,
  api,
  clearSecret,
  gatewaySessionLogout,
  getSecret,
  setSecret,
  signIn,
} from "./api";

afterEach(() => {
  vi.restoreAllMocks();
  clearSecret();
});

describe("OSS gateway session", () => {
  it("exchanges the secret once and retains no browser-readable credential", async () => {
    setSecret("stale-memory-value");
    const fetchMock = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(new Response("{}", { status: 200 }));

    await signIn("  gateway-secret  ");
    expect(getSecret()).toBeNull();
    expect(fetchMock).toHaveBeenNthCalledWith(
      1,
      expect.stringContaining("/api/auth/gateway-session"),
      expect.objectContaining({
        method: "POST",
        credentials: "same-origin",
        headers: { "X-Gateway-Secret": "gateway-secret" },
      }),
    );

    await api.getAgentConfig();
    const protectedInit = fetchMock.mock.calls[1][1] as RequestInit;
    expect(protectedInit.credentials).toBe("same-origin");
    expect(new Headers(protectedInit.headers).has("X-Gateway-Secret")).toBe(false);
  });

  it("rejects a failed exchange without retaining the secret", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(null, { status: 401 }));
    await expect(signIn("wrong-secret")).rejects.toEqual(expect.objectContaining<ApiError>({ status: 401 }));
    expect(getSecret()).toBeNull();
  });

  it("deletes the OSS session cookie and tolerates revoke failure", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockRejectedValue(new Error("offline"));
    await expect(gatewaySessionLogout()).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledWith(
      expect.stringContaining("/api/auth/gateway-session"),
      { method: "DELETE", credentials: "same-origin" },
    );
  });
});