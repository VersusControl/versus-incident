import { afterEach, describe, expect, it, vi } from "vitest";

// Each test loads a fresh copy of the API module so the page-load cache of the
// deployment probe starts empty.
async function loadApi() {
  vi.resetModules();
  return import("./api");
}

function respond(status: number, body: unknown = {}) {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("deployment probe", () => {
  it("asks the server once per page load for a community answer and skips the baselines route", async () => {
    const fetchMock = vi.fn().mockResolvedValue(respond(404));
    vi.stubGlobal("fetch", fetchMock);
    const mod = await loadApi();

    await expect(mod.getSSODeployment()).rejects.toMatchObject({ status: 404 });
    await expect(mod.getSSODeployment()).rejects.toMatchObject({ status: 404 });
    expect(mod.knownCommunityDeploymentStatus()).toBe(404);
    await expect(mod.api.listBaselines({ type: "metric" })).rejects.toMatchObject({ status: 404 });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("keeps a licensed org and still calls the baselines route", async () => {
    const fetchMock = vi.fn((url: string) =>
      Promise.resolve(url.includes("/sso/deployment") ? respond(200, { org: "acme" }) : respond(200, { baselines: [] })),
    );
    vi.stubGlobal("fetch", fetchMock);
    const mod = await loadApi();

    await expect(mod.getSSODeployment()).resolves.toEqual({ org: "acme" });
    await expect(mod.getSSODeployment()).resolves.toEqual({ org: "acme" });
    expect(mod.knownCommunityDeploymentStatus()).toBeNull();
    await mod.api.listBaselines();
    const urls = fetchMock.mock.calls.map(([url]) => url);
    expect(urls.filter((url) => url.includes("/sso/deployment"))).toHaveLength(1);
    expect(urls.some((url) => url.includes("/api/agent/baselines"))).toBe(true);
  });

  it("asks again after a transient failure", async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(respond(503)).mockResolvedValueOnce(respond(200, { org: "acme" }));
    vi.stubGlobal("fetch", fetchMock);
    const mod = await loadApi();

    await expect(mod.getSSODeployment()).rejects.toMatchObject({ status: 503 });
    expect(mod.knownCommunityDeploymentStatus()).toBeNull();
    await expect(mod.getSSODeployment()).resolves.toEqual({ org: "acme" });
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });
});
