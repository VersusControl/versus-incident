// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { api, AUTH_EXPIRED_EVENT, clearSecret, setSecret } from "./api";

class MemoryStorage {
  private values = new Map<string, string>();
  get length() { return this.values.size; }
  clear() { this.values.clear(); }
  getItem(key: string) { return this.values.get(key) ?? null; }
  setItem(key: string, value: string) { this.values.set(key, String(value)); }
  removeItem(key: string) { this.values.delete(key); }
  key(index: number) { return Array.from(this.values.keys())[index] ?? null; }
}

const local = new MemoryStorage();
const session = new MemoryStorage();
Object.defineProperty(window, "localStorage", { value: local, configurable: true });
Object.defineProperty(globalThis, "localStorage", { value: local, configurable: true });
Object.defineProperty(window, "sessionStorage", { value: session, configurable: true });
Object.defineProperty(globalThis, "sessionStorage", { value: session, configurable: true });

afterEach(() => {
  vi.restoreAllMocks();
  clearSecret();
  local.clear();
  session.clear();
});

function streamResponse(frames: string[], status = 200) {
  const encoder = new TextEncoder();
  return new Response(
    new ReadableStream({
      start(controller) {
        frames.forEach((frame) => controller.enqueue(encoder.encode(frame)));
        controller.close();
      },
    }),
    { status, headers: { "Content-Type": "text/event-stream" } },
  );
}

describe("chat API", () => {
  it("removes legacy gateway secrets without loading or persisting them", async () => {
    local.setItem("versus.gatewaySecret", "legacy");
    session.setItem("versus.gatewaySecret", "current");
    vi.resetModules();
    const freshApi = await import("./api");

    expect(freshApi.getSecret()).toBeNull();
    expect(session.getItem("versus.gatewaySecret")).toBeNull();
    expect(local.getItem("versus.gatewaySecret")).toBeNull();

    freshApi.setSecret("memory-only");
    expect(freshApi.getSecret()).toBe("memory-only");
    expect(session.getItem("versus.gatewaySecret")).toBeNull();
    expect(local.getItem("versus.gatewaySecret")).toBeNull();
  });

  it("cleans local storage when session storage cleanup throws", async () => {
    local.setItem("versus.gatewaySecret", "legacy");
    session.setItem("versus.gatewaySecret", "legacy");
    vi.spyOn(session, "removeItem").mockImplementation(() => {
      throw new Error("session storage unavailable");
    });
    vi.resetModules();
    const freshApi = await import("./api");

    expect(freshApi.getSecret()).toBeNull();
    expect(local.getItem("versus.gatewaySecret")).toBeNull();
    expect(session.getItem("versus.gatewaySecret")).toBe("legacy");
  });

  it("cleans session storage when local storage cleanup throws", async () => {
    local.setItem("versus.gatewaySecret", "legacy");
    session.setItem("versus.gatewaySecret", "legacy");
    vi.spyOn(local, "removeItem").mockImplementation(() => {
      throw new Error("local storage unavailable");
    });
    vi.resetModules();
    const freshApi = await import("./api");

    expect(freshApi.getSecret()).toBeNull();
    expect(session.getItem("versus.gatewaySecret")).toBeNull();
    expect(local.getItem("versus.gatewaySecret")).toBe("legacy");
  });

  it("sends auth/cookie/no-store policy and parses named multiline SSE", async () => {
    setSecret("gateway-secret");
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      streamResponse([
        "event: model_delta\r\ndata: {\"seq\":1,\"at\":\"\",\"kind\":\"model_delta\",\r\n",
        "data: \"delta\":\"hello\"}\r\n\r\n",
        "event: run_finished\ndata: {\"seq\":2,\"at\":\"\",\"kind\":\"run_finished\"}\n\n",
      ]),
    );
    const events: unknown[] = [];
    await api.streamChatMessage("s/1", "hello", { service: "api" }, (event) => events.push(event));

    expect(fetchMock).toHaveBeenCalledWith(
      expect.stringContaining("/api/admin/chat/sessions/s%2F1/messages"),
      expect.objectContaining({ credentials: "same-origin", cache: "no-store" }),
    );
    const init = fetchMock.mock.calls[0][1] as RequestInit;
    expect(new Headers(init.headers).get("X-Gateway-Secret")).toBe("gateway-secret");
    expect(JSON.parse(String(init.body))).toEqual({ message: "hello", attachment: { service: "api" } });
    expect(events).toEqual([
      { seq: 1, at: "", kind: "model_delta", delta: "hello" },
      { seq: 2, at: "", kind: "run_finished" },
    ]);
  });

  it("dispatches auth-expired and throws a safe ApiError on 401", async () => {
    setSecret("rotated");
    const expired = vi.fn();
    window.addEventListener(AUTH_EXPIRED_EVENT, expired);
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response('{"error":"unauthorized"}', { status: 401 }),
    );
    await expect(api.streamChatMessage("s1", "hello", undefined, () => {})).rejects.toMatchObject({
      status: 401,
      message: "unauthorized",
    });
    expect(expired).toHaveBeenCalledTimes(1);
    window.removeEventListener(AUTH_EXPIRED_EVENT, expired);
  });

  it("dispatches auth-expired for enterprise cookie expiry without a gateway secret", async () => {
    clearSecret();
    const expired = vi.fn();
    window.addEventListener(AUTH_EXPIRED_EVENT, expired);
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response('{"error":"session expired"}', { status: 401 }));
    await expect(api.streamChatMessage("s1", "hello", undefined, () => {})).rejects.toMatchObject({ status: 401 });
    expect(expired).toHaveBeenCalledTimes(1);
    window.removeEventListener(AUTH_EXPIRED_EVENT, expired);
  });

  it("dispatches auth-expired for cookie-only multipart and report image requests", async () => {
    clearSecret();
    const expired = vi.fn();
    window.addEventListener(AUTH_EXPIRED_EVENT, expired);
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response('{"error":"session expired"}', { status: 401 }),
    );

    await expect(api.uploadRunbooks([new File(["body"], "runbook.md")])).rejects.toMatchObject({ status: 401 });
    await expect(api.fetchIncidentsReportImage("24h")).rejects.toMatchObject({ status: 401 });

    expect(expired).toHaveBeenCalledTimes(2);
    for (const [, init] of fetchMock.mock.calls) {
      expect(new Headers(init?.headers).has("X-Gateway-Secret")).toBe(false);
      expect(init?.credentials).toBe("same-origin");
    }
    window.removeEventListener(AUTH_EXPIRED_EVENT, expired);
  });

  it("completes chat and analysis streams with more than 200 model deltas", async () => {
    const deltas = Array.from({ length: 220 }, (_, index) =>
      `event: model_delta\ndata: {"seq":${index + 1},"at":"","kind":"model_delta","delta":"${index} "}\n\n`
    );
    const chatFrames = [...deltas, "event: run_finished\ndata: {\"seq\":221,\"at\":\"\",\"kind\":\"run_finished\"}\n\n"];
    const analysisFrames = [
      ...deltas.map((frame) => frame.replace(/,"delta":"[^"]*"/, "")),
      "event: run_finished\ndata: {\"seq\":221,\"at\":\"\",\"kind\":\"run_finished\",\"analysis_id\":\"a1\"}\n\n",
    ];
    vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(streamResponse(chatFrames))
      .mockResolvedValueOnce(streamResponse(analysisFrames));
    const chatEvents: unknown[] = [];
    const analysisEvents: unknown[] = [];

    const chatTerminal = await api.streamChatMessage("s1", "long answer", undefined, (next) => chatEvents.push(next));
    const analysisTerminal = await api.streamAnalysis("incident-1", (next) => analysisEvents.push(next));

    expect(chatEvents).toHaveLength(221);
    expect(chatTerminal?.kind).toBe("run_finished");
    expect(analysisEvents).toHaveLength(221);
    expect(analysisTerminal).toMatchObject({ kind: "run_finished", analysis_id: "a1" });
  });

  it("rejects non-SSE and EOF without a terminal event", async () => {
    vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response("html", { status: 200, headers: { "Content-Type": "text/html" } }))
      .mockResolvedValueOnce(streamResponse(["event: model_delta\ndata: {\"seq\":1,\"kind\":\"model_delta\",\"delta\":\"partial\"}\n\n"]));
    await expect(api.streamChatMessage("s1", "hello", undefined, () => {})).rejects.toMatchObject({ status: 502 });
    await expect(api.streamChatMessage("s1", "hello", undefined, () => {})).rejects.toThrow(/interrupted/);
  });
});