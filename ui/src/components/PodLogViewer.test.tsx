// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "@/lib/api";
import type { KubernetesPodLogEvent } from "@/lib/api";
import { PodLogViewer } from "./PodLogViewer";

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers(); });
const resource = { resource_id: "core~v1~pods", kind: "Pod", namespace: "shop", name: "api", summary: { log_containers: [{ name: "app", type: "regular" }, { name: "setup", type: "init" }, { name: "debug", type: "ephemeral" }] } };

describe("Pod log viewer", () => {
  it("distinguishes connecting, quiet, filtered, and failed empty output", async () => {
    vi.useFakeTimers();
    let emit: (event: KubernetesPodLogEvent) => void = () => undefined;
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, callback, signal) => {
      emit = callback;
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    render(<PodLogViewer resource={resource} />);
    const output = screen.getByLabelText("Pod log output");
    expect(output.textContent).toContain("Connecting");
    expect(screen.getByRole("option", { name: "1 hour", exact: true })).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Log filter"), { target: { value: "missing" } });
    act(() => emit({ event: "heartbeat", text: "", container: "app" }));
    expect(output.textContent).toBe("Connected; no log lines received yet.");
    act(() => emit({ event: "line", text: "actual", container: "app", cursor: "one" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(output.textContent).toBe("No log lines match this filter.");
    fireEvent.click(screen.getByLabelText("Reload logs"));
    act(() => emit({ event: "error", text: "", container: "app", code: "forbidden", message: "Access denied", action: "Check RBAC" }));
    expect(output.textContent).toContain("See the stream error");
    expect(screen.getByRole("alert").textContent).toContain("forbidden Access denied Check RBAC");
  });

  it("shows finite previous completion without a filter miss or reconnect", async () => {
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => emit({ event: "end", text: "", container: "app" }));
    render(<PodLogViewer resource={resource} />);
    fireEvent.click(screen.getByLabelText("Previous container logs"));
    await waitFor(() => expect(screen.getByLabelText("Pod log output").textContent).toBe("Previous container log read completed with no lines."));
    expect(stream).toHaveBeenCalledTimes(2);
  });

  it("follows near the bottom but preserves older reading until explicitly returning", async () => {
    vi.useFakeTimers();
    let emit: (event: KubernetesPodLogEvent) => void = () => undefined;
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, callback, signal) => {
      emit = callback;
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    render(<PodLogViewer resource={resource} />);
    const output = screen.getByLabelText("Pod log output");
    Object.defineProperty(output, "scrollHeight", { configurable: true, value: 1000 });
    Object.defineProperty(output, "clientHeight", { configurable: true, value: 200 });
    act(() => emit({ event: "line", text: "first", container: "app", cursor: "one" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(output.scrollTop).toBe(1000);
    output.scrollTop = 100;
    fireEvent.scroll(output);
    act(() => emit({ event: "line", text: "next", container: "app", cursor: "two" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(output.scrollTop).toBe(100);
    fireEvent.click(screen.getByLabelText("Scroll logs to bottom"));
    expect(output.scrollTop).toBe(1000);
    fireEvent.click(screen.getByLabelText("Follow"));
    output.scrollTop = 800;
    fireEvent.scroll(output);
    act(() => emit({ event: "line", text: "last", container: "app", cursor: "three" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(output.scrollTop).toBe(800);
  });

  it("requests raw scrubbed text and applies metadata timestamps exactly once to display, copy, and download", async () => {
    const timestamp = "2026-10-07T12:00:00Z";
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      const request = new URL(String(input), "http://localhost");
      const prefix = request.searchParams.get("timestamps") === "false" ? "" : `${timestamp} `;
      return new Response(`event: line\ndata: ${JSON.stringify({ text: `${prefix}token=[REDACTED] a.b`, container: "app", timestamp, ordinal: 1, sequence: 1 })}\n\nevent: line\ndata: ${JSON.stringify({ text: `${prefix}axb`, container: "app", timestamp, ordinal: 2, sequence: 2 })}\n\nevent: heartbeat\ndata: {"cursor":"private-checkpoint","replay_uncertain":true}\n\nevent: end\ndata: {"cursor":"private-checkpoint"}\n\n`, { headers: { "Content-Type": "text/event-stream" } });
    });
    const writeText = vi.fn().mockResolvedValue(undefined);
    const createObjectURL = vi.fn<(blob: Blob) => string>().mockReturnValue("blob:logs");
    const revokeObjectURL = vi.fn();
    vi.stubGlobal("URL", Object.assign(class extends URL {}, { createObjectURL, revokeObjectURL }));
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => undefined);
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
    render(<PodLogViewer resource={resource} />);
    await waitFor(() => expect(screen.getByLabelText("Pod log output").textContent).toContain("axb"));
    expect(screen.getByText(/Log replay is uncertain/)).toBeTruthy();
    expect(new URL(String(fetchMock.mock.calls[0][0]), "http://localhost").searchParams.get("timestamps")).toBe("false");
    expect(screen.getAllByRole("option").map((option) => option.textContent)).toContain("debug (ephemeral)");
    fireEvent.change(screen.getByLabelText("Log filter"), { target: { value: "a.b" } });
    expect(screen.getByLabelText("Pod log output").textContent).not.toContain("axb");
    for (const enabled of [true, false, true]) {
      const toggle = screen.getByLabelText("Timestamps") as HTMLInputElement;
      if (toggle.checked !== enabled) fireEvent.click(toggle);
      const expected = `${enabled ? `${timestamp} ` : ""}token=[REDACTED] a.b`;
      expect(screen.getByLabelText("Pod log output").textContent?.trim()).toBe(expected);
      fireEvent.click(screen.getByLabelText("Copy scrubbed logs"));
      await waitFor(() => expect(writeText).toHaveBeenLastCalledWith(expected));
      fireEvent.click(screen.getByLabelText("Download scrubbed logs"));
      const blob = createObjectURL.mock.calls.at(-1)![0];
      const downloaded = await new Promise<string>((resolve, reject) => {
        const reader = new FileReader();
        reader.onload = () => resolve(String(reader.result));
        reader.onerror = reject;
        reader.readAsText(blob);
      });
      expect(downloaded).toBe(expected);
    }
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(click).toHaveBeenCalledTimes(3);
    expect(revokeObjectURL).toHaveBeenCalledTimes(3);
  });

  it("keeps previous-unavailable errors honest and applies bounded since/tail controls", async () => {
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, options, emit) => {
      emit({ event: options.previous ? "error" : "end", text: "", container: "app", code: "previous_unavailable" });
    });
    render(<PodLogViewer resource={resource} />);
    fireEvent.click(screen.getByLabelText("Previous container logs"));
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByRole("alert").textContent).toContain("no previous container instance");
    fireEvent.change(screen.getByLabelText("Log since seconds"), { target: { value: "86400" } });
    fireEvent.change(screen.getByLabelText("Log tail lines"), { target: { value: "5000" } });
    await waitFor(() => expect(stream).toHaveBeenLastCalledWith("shop", "api", expect.objectContaining({ previous: true, since_seconds: 86400, tail_lines: 5000 }), expect.any(Function), expect.any(AbortSignal)));
  });

  it.each([
    ["duration", "duration safety limit"],
    ["bytes", "byte safety limit"],
    ["byte_limit", "byte safety limit"],
    ["private-unknown-reason", "safety limit"],
  ])("shows an honest empty %s limit and keeps uncertainty until an explicit reload", async (reason, limitDescription) => {
    vi.useFakeTimers();
    let emit: (event: KubernetesPodLogEvent) => void = () => undefined;
    let finish: () => void = () => undefined;
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, callback, signal) => {
      emit = callback;
      return new Promise<void>((resolve) => { finish = resolve; signal.addEventListener("abort", () => resolve(), { once: true }); });
    });
    render(<PodLogViewer resource={resource} />);
    fireEvent.change(screen.getByLabelText("Log filter"), { target: { value: "missing" } });
    act(() => {
      emit({ event: "heartbeat", text: "", container: "app", cursor: "quiet-checkpoint", replay_uncertain: true });
      emit({ event: "heartbeat", text: "", container: "app", replay_uncertain: false });
    });
    expect(screen.getByLabelText("Pod log output").textContent).toBe("Connected; no log lines received yet.");
    act(() => {
      emit({ event: "limit", text: "private-log-payload", container: "app", cursor: "limit-checkpoint", reason, code: "private-code", message: "private-message", action: "private-action" });
      emit({ event: "end", text: "", container: "app", cursor: "end-checkpoint", replay_uncertain: false });
      finish();
    });
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(screen.getByLabelText("Log connection status").textContent).toBe("limited");
    expect(screen.getByLabelText("Pod log output").textContent).toBe("Log read reached its safety limit before any lines were received.");
    const limitMessage = `Log read reached its ${limitDescription}. Reload logs to start a new read.`;
    expect(screen.getByText(limitMessage).getAttribute("role")).toBe("status");
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByLabelText("Pod logs").textContent).not.toContain("private-");
    expect(screen.getByText(/Log replay is uncertain/).getAttribute("role")).toBe("status");
    expect(stream).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByLabelText("Pause logs"));
    fireEvent.click(screen.getByLabelText("Resume logs"));
    expect(screen.getByLabelText("Log connection status").textContent).toBe("limited");
    expect(screen.getByText(limitMessage)).toBeTruthy();
    expect(screen.getByText(/Log replay is uncertain/)).toBeTruthy();
    expect(stream).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByLabelText("Reload logs"));
    expect(stream).toHaveBeenCalledTimes(2);
    expect(stream.mock.calls[1][2]).not.toHaveProperty("cursor");
    expect(screen.queryByText(/Log replay is uncertain/)).toBeNull();
    expect(screen.getByLabelText("Log connection status").textContent).toBe("connecting");
  });

  it("shows buffer and replay-history cap warnings without hiding the restart action", async () => {
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      for (let ordinal = 1; ordinal <= 10001; ordinal++) emit({ event: "line", text: "tiny", container: "app", timestamp: "2026-10-07T12:00:00Z", ordinal });
      emit({ event: "end", text: "", container: "app", cursor: "final" });
    });
    render(<PodLogViewer resource={resource} />);
    await waitFor(() => expect(screen.getByText(/Browser buffer capped/).textContent).toContain("1 oldest lines dropped"));
    expect(screen.getByText(/Log replay is uncertain/)).toBeTruthy();
    expect(screen.getByLabelText("Pod log output").querySelectorAll("[data-log-key]")).toHaveLength(10000);
    expect((screen.getByLabelText("Reload logs") as HTMLButtonElement).disabled).toBe(false);
  });
});