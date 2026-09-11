import { afterEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "./api";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("Service Health API", () => {
  it("uses the snapshot and settings routes with the backend PATCH shape", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ snapshot_id: "snap-1" }), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ interval_seconds: 60, window_seconds: 300, revision: 2 }), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ interval_seconds: 30, window_seconds: 120, revision: 3 }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);

    await api.getServiceHealth();
    await api.getServiceHealthSettings();
    await api.updateServiceHealthSettings({ interval_seconds: 30, window_seconds: 120, revision: 2 });

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/api/agent/service-health",
      "/api/agent/service-health/settings",
      "/api/agent/service-health/settings",
    ]);
    expect(fetchMock.mock.calls[2][1]).toMatchObject({
      method: "PATCH",
      body: JSON.stringify({ interval_seconds: 30, window_seconds: 120, revision: 2 }),
    });
  });

  it("preserves a settings conflict as ApiError status 409", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(
      JSON.stringify({ error: "service health settings changed concurrently; retry" }),
      { status: 409 },
    )));

    await expect(api.updateServiceHealthSettings({
      interval_seconds: 60,
      window_seconds: 300,
      revision: 1,
    })).rejects.toMatchObject<ApiError>({ status: 409 });
  });
});