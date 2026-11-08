// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  ApiError,
  api,
  createKubernetesApi,
  clearSecret,
  gatewaySessionLogout,
  signIn,
} from "./api";

afterEach(() => {
  vi.restoreAllMocks();
  clearSecret();
});

describe("OSS gateway session", () => {
  it.each([
    [503, "history_not_established", true],
    [403, "history_authority_denied", false],
  ] as const)("preserves the structured commit-stream authority distinction for HTTP %s", async (status, code, retryable) => {
    const body = { code, retryable };
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify(body), { status }));
    await expect(api.kubernetesCommitStream(vi.fn())).rejects.toMatchObject({ status, body });
  });
  it("keeps first-page count metadata when skipping empty history continuations", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(JSON.stringify({ items: [], next: "next", total: 1001, counts: { break: 1001 }, count_capped: true }), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ items: [{ id: "visible" }], total: 0, counts: {}, count_capped: false }), { status: 200 }));
    const page = await createKubernetesApi("east").kubernetesTimeline({ since: "2026-10-01T00:00:00Z", until: "2026-10-02T00:00:00Z" });
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(page).toMatchObject({ total: 1001, counts: { break: 1001 }, count_capped: true });
    expect(page.items).toHaveLength(1);
  });
  it("authenticates commit streams with ambient cookies, scoped URL and resume headers", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(
      'id: 7\nevent: committed\ndata: {"revision":7,"cluster_id":"east","evidence_sequence":8,"kinds":["Pod"]}\n\n',
      { headers: { "Content-Type": "text/event-stream" } },
    ));
    const controller = new AbortController();
    const received = vi.fn();
    await createKubernetesApi("east").kubernetesCommitStream(received, controller.signal, "6");
    const [url, options] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/api/admin/kubernetes/commits/stream?cluster=east");
    expect(options).toMatchObject({ credentials: "same-origin", cache: "no-store", signal: controller.signal });
    const headers = new Headers(options?.headers);
    expect(headers.get("Accept")).toBe("text/event-stream");
    expect(headers.get("Last-Event-ID")).toBe("6");
    expect(headers.has("X-Gateway-Secret")).toBe(false);
    expect(received).toHaveBeenCalledWith(expect.objectContaining({ event: "committed", id: "7" }));
  });

  it.each([401, 403, 503])("does not turn a commit stream HTTP %s into live success", async status => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(null, { status }));
    const received = vi.fn();
    await expect(api.kubernetesCommitStream(received)).rejects.toMatchObject({ status });
    expect(received).not.toHaveBeenCalled();
  });
  it("exchanges the secret once and retains no browser-readable credential", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(new Response("{}", { status: 200 }));

    await signIn("  gateway-secret  ");
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
    const fetchMock = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(null, { status: 401 }))
      .mockResolvedValueOnce(new Response("{}", { status: 200 }));
    await expect(signIn("wrong-secret")).rejects.toEqual(expect.objectContaining<ApiError>({ status: 401 }));
    await api.getAgentConfig();
    const next = fetchMock.mock.calls[1][1] as RequestInit;
    expect(new Headers(next.headers).has("X-Gateway-Secret")).toBe(false);
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