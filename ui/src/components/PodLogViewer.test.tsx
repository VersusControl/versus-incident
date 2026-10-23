// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "@/lib/api";
import type { KubernetesPodLogEvent } from "@/lib/api";
import { PodLogViewer } from "./PodLogViewer";

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers(); });
const resource = { resource_id: "core~v1~pods", kind: "Pod", namespace: "shop", name: "api", summary: { log_containers: [{ name: "app", type: "regular" }, { name: "setup", type: "init" }, { name: "debug", type: "ephemeral" }] } };

describe("Pod log viewer", () => {
  it("counts retained error and warning lines and changes presentation without restarting the stream", async () => {
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, emit, signal) => {
      ["INFO served", "ERROR refused", "WARN slow", "ERROR timeout"].forEach((text, index) => emit({ event: "line", text, container: "app", ordinal: index + 1, timestamp: "2026-10-21T08:00:00Z" }));
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    render(<PodLogViewer resource={resource} />);
    await screen.findByRole("button", { name: "Errors 2" });
    expect(screen.getByRole("button", { name: "Warnings 1" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Current" }).getAttribute("aria-pressed")).toBe("true");
    fireEvent.click(screen.getByLabelText("Timestamps"));
    fireEvent.click(screen.getByLabelText("Wrap lines"));
    fireEvent.change(screen.getByLabelText("Log filter"), { target: { value: "timeout" } });
    expect(stream).toHaveBeenCalledTimes(1);
    expect(screen.getByLabelText("Pod log output").textContent).toContain("ERROR timeout");
    fireEvent.click(screen.getByRole("button", { name: "Previous container logs" }));
    await waitFor(() => expect(stream).toHaveBeenCalledTimes(2));
    expect(stream.mock.calls[1][2].previous).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Current", exact: true }));
    await waitFor(() => expect(stream).toHaveBeenCalledTimes(3));
    expect(stream.mock.calls[2][2].previous).toBe(false);
    expect(stream.mock.calls.every((call) => call[2].timestamps === false)).toBe(true);
  });
  it("shows the blocking unavailable reason when retained output contains only a neutral reconnect row", async () => {
    vi.useFakeTimers();
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, options, emit) => {
      if (options.previous) {
        emit({ event: "heartbeat", text: "", container: "app", replay_uncertain: true });
        emit({ event: "error", text: "", container: "app", code: "previous_unavailable", message: "Previous container logs are unavailable.", action: "Select current container logs." });
      }
      emit({ event: "end", text: "", container: "app" });
    });
    render(<PodLogViewer resource={resource} />);
    fireEvent.click(screen.getByLabelText("Previous container logs"));
    fireEvent.change(screen.getByLabelText("Log filter"), { target: { value: "missing" } });
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    const output = screen.getByLabelText("Pod log output");
    expect(output.textContent).toContain("previous_unavailable Previous container logs are unavailable. Select current container logs.");
    expect(output.textContent).not.toMatch(/resumed after reconnect|No log lines match|Connecting/);
    expect(output.querySelectorAll("[data-log-key]")).toHaveLength(0);
    expect((screen.getByLabelText("Download scrubbed logs") as HTMLButtonElement).disabled).toBe(true);
  });
  it("opens a resumed read from the Pause and Resume buttons after a normal end", async () => {
    vi.useFakeTimers();
    const timestamp = "2026-10-21T08:00:00Z";
    const stream = vi.spyOn(api, "kubernetesPodLogStream")
      .mockImplementationOnce(async (_namespace, _pod, _options, emit) => {
        emit({ event: "line", text: "first", container: "app", timestamp, ordinal: 1, cursor: "first-checkpoint" });
        emit({ event: "end", text: "", container: "app", cursor: "first-checkpoint" });
      })
      .mockImplementation((_namespace, _pod, _options, emit, signal) => {
        emit({ event: "line", text: "first", container: "app", timestamp, ordinal: 1, cursor: "first-checkpoint" });
        emit({ event: "line", text: "second", container: "app", timestamp, ordinal: 2, cursor: "second-checkpoint" });
        return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
      });
    render(<PodLogViewer resource={resource} />);
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    fireEvent.click(screen.getByLabelText("Pause logs"));
    fireEvent.click(screen.getByLabelText("Resume logs"));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(stream).toHaveBeenCalledTimes(2);
    expect(stream.mock.calls[1][2]).toEqual({ container: "app", previous: false, timestamps: false, cursor: "first-checkpoint" });
    const output = screen.getByLabelText("Pod log output");
    const displayedTime = new Date(timestamp).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
    expect(output.textContent).toBe(`${displayedTime} first\n${displayedTime} second\n`);
    expect(output.querySelectorAll("time")).toHaveLength(2);
    expect(output.textContent).not.toMatch(/cannot resume|reconnect|uncertain/);
  });
  it("keeps compact container chips outside More and puts mobile filters and levels inside the controlled menu", () => {
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => emit({ event: "end", text: "", container: "app" }));
    const { rerender } = render(<PodLogViewer resource={resource} compact />);
    const more = screen.getByRole("button", { name: "More" });
    const controls = document.getElementById(more.getAttribute("aria-controls")!)!;
    const chips = screen.getByRole("group", { name: "Log containers" });
    expect(controls.contains(chips)).toBe(false);
    expect(chips.classList.contains("hidden")).toBe(false);
    expect(controls.contains(screen.getByLabelText("Log filter"))).toBe(true);
    expect(controls.classList.contains("hidden")).toBe(true);
    fireEvent.click(more);
    expect(more.getAttribute("aria-expanded")).toBe("true");
    expect(controls.classList.contains("hidden")).toBe(false);
    fireEvent.click(more);
    rerender(<PodLogViewer resource={resource} compact={false} />);
    expect(controls.contains(screen.getByRole("button", { name: "Errors 0", exact: true }))).toBe(true);
    expect(controls.classList.contains("hidden")).toBe(true);
    expect(screen.getByRole("group", { name: "Log containers" })).toBeTruthy();
  });
  it("opens the complete returned container catalog from the compact overflow chip", async () => {
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => emit({ event: "end", text: "", container: "app" }));
    const names = Array.from({ length: 7 }, (_, index) => ({ name: `sidecar-${index}`, type: "regular" }));
    render(<PodLogViewer resource={{ ...resource, summary: { log_containers: names, main_container: "sidecar-0" } }} compact />);
    expect(screen.getByRole("button", { name: "+3" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "sidecar-6" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "+3" }));
    fireEvent.click(screen.getByRole("button", { name: "sidecar-6" }));
    await waitFor(() => expect(api.kubernetesPodLogStream).toHaveBeenLastCalledWith("shop", "api", expect.objectContaining({ container: "sidecar-6" }), expect.any(Function), expect.any(AbortSignal)));
    expect(screen.queryByRole("alert")).toBeNull();
  });
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
    expect(screen.getByRole("option", { name: "1h", exact: true })).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Log filter"), { target: { value: "missing" } });
    act(() => emit({ event: "heartbeat", text: "", container: "app" }));
    expect(output.textContent).toBe("No log lines in the selected time");
    act(() => emit({ event: "line", text: "actual", container: "app", cursor: "one" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(output.textContent).toBe("No log lines match this filter.");
    fireEvent.click(screen.getByLabelText("Reload logs"));
    act(() => emit({ event: "error", text: "", container: "app", code: "forbidden", message: "Access denied", action: "Check RBAC" }));
    expect(output.textContent).toContain("forbidden Access denied Check RBAC");
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("shows finite previous completion without a filter miss or reconnect", async () => {
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => emit({ event: "end", text: "", container: "app" }));
    render(<PodLogViewer resource={resource} />);
    fireEvent.click(screen.getByLabelText("Previous container logs"));
    await waitFor(() => expect(screen.getByLabelText("Pod log output").textContent).toBe("No log lines in the selected time"));
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
    expect((screen.getByLabelText("Follow") as HTMLInputElement).checked).toBe(false);
    fireEvent.click(screen.getByLabelText("Scroll to latest"));
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
    expect(screen.getByText(/resumed after reconnect at/)).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(new URL(String(fetchMock.mock.calls[0][0]), "http://localhost").searchParams.get("timestamps")).toBe("false");
    expect(screen.getAllByRole("option").map((option) => option.textContent)).toContain("debug (ephemeral)");
    fireEvent.change(screen.getByLabelText("Log filter"), { target: { value: "a.b" } });
    expect(screen.getByLabelText("Pod log output").textContent).not.toContain("axb");
    for (const enabled of [true, false, true]) {
      const toggle = screen.getByLabelText("Timestamps") as HTMLInputElement;
      if (toggle.checked !== enabled) fireEvent.click(toggle);
      const expected = `${enabled ? `${timestamp} ` : ""}token=[REDACTED] a.b`;
      const displayed = `${enabled ? `${new Date(timestamp).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false })} ` : ""}token=[REDACTED] a.b`;
      expect(screen.getByLabelText("Pod log output").textContent?.trim()).toBe(displayed);
      expect(screen.getByLabelText("Pod log output").querySelectorAll("time")).toHaveLength(enabled ? 1 : 0);
      if (enabled) expect(screen.getByLabelText("Pod log output").querySelector("time")?.title).toBe(timestamp);
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
    await waitFor(() => expect(screen.getByLabelText("Pod log output").textContent).toContain("no previous container instance"));
    expect(screen.queryByRole("alert")).toBeNull();
    fireEvent.change(screen.getByLabelText("Log since seconds"), { target: { value: "86400" } });
    await waitFor(() => expect(stream).toHaveBeenLastCalledWith("shop", "api", expect.objectContaining({ previous: true, since_seconds: 86400, tail_lines: 5000 }), expect.any(Function), expect.any(AbortSignal)));
  });

  it.each([
    "duration", "bytes", "byte_limit", "private-unknown-reason",
  ])("silently rotates a %s limit using its checkpoint and keeps the Live session", async (reason) => {
    vi.useFakeTimers();
    let emit: (event: KubernetesPodLogEvent) => void = () => undefined;
    let finish: () => void = () => undefined;
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, callback, signal) => {
      emit = callback;
      return new Promise<void>((resolve) => { finish = resolve; signal.addEventListener("abort", () => resolve(), { once: true }); });
    });
    render(<PodLogViewer resource={resource} />);
    act(() => {
      emit({ event: "heartbeat", text: "", container: "app", cursor: "quiet-checkpoint", replay_uncertain: true });
      emit({ event: "heartbeat", text: "", container: "app", replay_uncertain: false });
    });
    act(() => {
      emit({ event: "limit", text: "private-log-payload", container: "app", cursor: "limit-checkpoint", reason, code: "private-code", message: "private-message", action: "private-action" });
      emit({ event: "end", text: "", container: "app", cursor: "end-checkpoint", replay_uncertain: false });
      finish();
    });
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    expect(stream).toHaveBeenCalledTimes(2);
    expect(stream.mock.calls[1][2].cursor).toBe("end-checkpoint");
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByLabelText("Pod logs").textContent).not.toContain("private-");
    expect(screen.getByText(/resumed after reconnect at/)).toBeTruthy();
    expect(screen.queryByText(/safety limit/)).toBeNull();
    fireEvent.click(screen.getByLabelText("Reload logs"));
    expect(stream).toHaveBeenCalledTimes(3);
    expect(stream.mock.calls[2][2]).not.toHaveProperty("cursor");
    expect(screen.queryByText(/resumed after reconnect at/)).toBeNull();
    expect(screen.getByLabelText("Log connection status").textContent).toBe("connecting");
  });

  it("quietly evicts oldest lines and reports retained download scope in the footer", async () => {
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      for (let ordinal = 1; ordinal <= 10001; ordinal++) emit({ event: "line", text: "tiny", container: "app", timestamp: "2026-10-07T12:00:00Z", ordinal });
      emit({ event: "end", text: "", container: "app", cursor: "final" });
    });
    render(<PodLogViewer resource={resource} />);
    await waitFor(() => expect(screen.getByText(/evicted/).textContent).toContain("1 evicted"));
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByText(/Log replay is uncertain/)).toBeNull();
    expect(screen.getByLabelText("Pod log output").querySelectorAll("[data-log-key]")).toHaveLength(10000);
    expect((screen.getByLabelText("Reload logs") as HTMLButtonElement).disabled).toBe(false);
  });
});