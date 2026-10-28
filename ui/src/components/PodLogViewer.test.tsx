// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "@/lib/api";
import type { KubernetesPodLogEvent, KubernetesWorkloadLogEvent } from "@/lib/api";
import { PodLogViewer } from "./PodLogViewer";

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers(); });
const resource = { resource_id: "core~v1~pods", kind: "Pod", namespace: "shop", name: "api", summary: { log_containers: [{ name: "app", type: "regular" }, { name: "setup", type: "init" }, { name: "debug", type: "ephemeral" }] } };

describe("Pod log viewer", () => {
  it.each(["Deployment", "StatefulSet", "Pod"])("keeps the %s full Logs filter beside its actions and only expands workloads", (kind) => {
    vi.spyOn(api, "kubernetesPodLogStream").mockResolvedValue();
    vi.spyOn(api, "kubernetesWorkloadLogStream").mockResolvedValue();
    const { container } = render(<PodLogViewer resource={{ ...resource, kind }} />);
    const group = screen.getByLabelText("Log filter").closest(".log-filter-group")!;
    expect(group.classList.contains("log-filter-group-workload")).toBe(kind !== "Pod");
    expect(group.firstElementChild?.className).toBe("log-filter-wrap");
    expect(group.lastElementChild?.className).toBe("log-actions");
    expect(within(group as HTMLElement).getByLabelText("Download scrubbed logs")).toBeTruthy();
    expect(within(group as HTMLElement).getByLabelText("Pause logs")).toBeTruthy();
    expect(container.querySelector(".log-levels") !== null).toBe(kind === "Pod");
  });
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
  it("offers Retry for a stale inventory stop with the backend guidance and no permission advice", async () => {
    vi.useFakeTimers();
    const { ApiError } = await import("@/lib/api");
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockRejectedValue(new ApiError(503, "ReplicaSet inventory is 4 min old; retry once the inventory finishes.", { error: "ReplicaSet inventory is 4 min old; retry once the inventory finishes.", code: "inventory_not_ready", action: "Wait for the Kubernetes inventory to finish syncing and retry.", retryable: true }));
    render(<PodLogViewer resource={resource} />);
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    const notice = document.querySelector("[data-log-notice]")!;
    expect(notice.textContent).toContain("Wait for the Kubernetes inventory to finish syncing and retry.");
    expect(notice.textContent).not.toMatch(/RBAC|Grant/);
    const calls = stream.mock.calls.length;
    fireEvent.click(screen.getByRole("button", { name: "Retry logs" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(stream.mock.calls.length).toBeGreaterThan(calls);
  });

  it("shows a real cluster denial without a Retry button", async () => {
    vi.useFakeTimers();
    const { ApiError } = await import("@/lib/api");
    vi.spyOn(api, "kubernetesPodLogStream").mockRejectedValue(new ApiError(403, "Kubernetes denied get on pods/log in namespace vikki-mobile-dev for user arn:aws:sts::1:assumed-role/admin", { error: "Kubernetes denied get on pods/log in namespace vikki-mobile-dev for user arn:aws:sts::1:assumed-role/admin", code: "cluster_permission_denied", action: "Grant the connector identity the required read-only Kubernetes RBAC permissions.", retryable: false }));
    render(<PodLogViewer resource={resource} />);
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    const notice = document.querySelector("[data-log-notice]")!;
    expect(notice.textContent).toContain("Grant the connector identity the required read-only Kubernetes RBAC permissions.");
    expect(screen.queryByRole("button", { name: "Retry logs" })).toBeNull();
    expect(screen.getByLabelText("Pod log output").textContent).toContain("pods/log in namespace vikki-mobile-dev");
  });

  it("keeps an ended read ended on Resume and only Reload starts a new one", async () => {
    vi.useFakeTimers();
    const timestamp = "2026-10-21T08:00:00Z";
    const stream = vi.spyOn(api, "kubernetesPodLogStream")
      .mockImplementationOnce(async (_namespace, _pod, _options, emit) => {
        emit({ event: "line", text: "first", container: "app", timestamp, ordinal: 1, cursor: "first-checkpoint" });
        emit({ event: "end", text: "", container: "app", cursor: "first-checkpoint" });
      })
      .mockImplementation((_namespace, _pod, _options, emit) => {
        emit({ event: "line", text: "first", container: "app", timestamp, ordinal: 1, cursor: "first-checkpoint" });
        emit({ event: "line", text: "second", container: "app", timestamp, ordinal: 2, cursor: "second-checkpoint" });
      });
    render(<PodLogViewer resource={resource} />);
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(screen.getByLabelText("Log connection status").textContent).toBe("Ended");
    fireEvent.click(screen.getByLabelText("Pause logs"));
    fireEvent.click(screen.getByLabelText("Resume logs"));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(stream).toHaveBeenCalledTimes(1);
    expect(screen.getByLabelText("Log connection status").textContent).toBe("Ended");
    fireEvent.click(screen.getByLabelText("Reload logs"));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(stream).toHaveBeenCalledTimes(2);
    // A reload is a new read from now, never a resume of the ended one.
    expect(stream.mock.calls[1][2]).toEqual({ container: "app", previous: false, timestamps: false, tail_lines: 200 });
    const output = screen.getByLabelText("Pod log output");
    const displayedTime = new Date(timestamp).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
    expect(output.textContent).toBe(`${displayedTime} first\n${displayedTime} second\n`);
    expect(output.querySelectorAll("time")).toHaveLength(2);
    expect(output.textContent).not.toMatch(/cannot resume|reconnect|uncertain/);
  });
  it("keeps the compact header to one row and the full header to two", () => {
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => emit({ event: "end", text: "", container: "app" }));
    const { container, rerender } = render(<PodLogViewer resource={resource} compact />);
    // One row: the title, the connection word and one container selector.
    const heading = container.querySelector(".log-heading")!;
    expect(within(heading as HTMLElement).getByText("Live logs")).toBeTruthy();
    expect(within(heading as HTMLElement).getByLabelText("Log connection status")).toBeTruthy();
    expect(within(heading as HTMLElement).getAllByRole("combobox")).toHaveLength(1);
    expect(within(heading as HTMLElement).getByLabelText("Log container")).toBeTruthy();
    // Everything the chip row and the More menu used to hold is gone from the
    // compact card, and no tooltip-only menu stands in for it.
    expect(screen.queryByRole("group", { name: "Log containers" })).toBeNull();
    expect(screen.queryByLabelText("Log filter")).toBeNull();
    expect(screen.queryByRole("button", { name: "More" })).toBeNull();
    expect(screen.queryByLabelText("Follow")).toBeNull();
    expect(container.querySelector(".log-toolbar")).toBeNull();

    rerender(<PodLogViewer resource={resource} compact={false} />);
    const primary = container.querySelector(".log-primary")!;
    const secondary = container.querySelector(".log-secondary")!;
    // Row one is the read itself, in one line: connection word, container,
    // Current | Previous as one control, then Since + Load.
    expect(within(primary as HTMLElement).getByLabelText("Log connection status")).toBeTruthy();
    expect(within(primary as HTMLElement).getByLabelText("Log container")).toBeTruthy();
    const instance = within(primary as HTMLElement).getByRole("group", { name: "Container instance" });
    expect(within(instance).getAllByRole("button").map((button) => button.textContent)).toEqual(["Current", "Previous"]);
    const since = within(primary as HTMLElement).getByRole("group", { name: "Load earlier logs" });
    expect(within(since).getByText("Since")).toBeTruthy();
    expect(within(since).getByLabelText("Since window")).toBeTruthy();
    expect(within(since).getByRole("button", { name: "Load logs since that window" }).textContent).toBe("Load");
    // Row two is the reading aid: levels on the left, filter and actions right.
    const levels = within(secondary as HTMLElement).getByRole("group", { name: "Log levels" });
    expect(within(levels).getAllByRole("button").map((button) => button.textContent)).toEqual(["Errors 0", "Warnings 0", "All"]);
    expect(within(secondary as HTMLElement).getByLabelText("Log filter")).toBeTruthy();
    expect(within(secondary as HTMLElement).getByLabelText("Download scrubbed logs")).toBeTruthy();
    // No Follow toggle and no container chip row anywhere.
    expect(screen.queryByLabelText("Follow")).toBeNull();
    expect(screen.queryByRole("group", { name: "Log containers" })).toBeNull();
  });
  it("offers every container in the compact select and re-reads the chosen one", async () => {
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => emit({ event: "end", text: "", container: "app" }));
    const names = Array.from({ length: 7 }, (_, index) => ({ name: `sidecar-${index}`, type: "regular" }));
    render(<PodLogViewer resource={{ ...resource, summary: { log_containers: names, main_container: "sidecar-0" } }} compact />);
    const select = screen.getByLabelText("Log container") as HTMLSelectElement;
    expect([...select.options].map((option) => option.value)).toEqual(names.map((item) => item.name));
    fireEvent.change(select, { target: { value: "sidecar-6" } });
    await waitFor(() => expect(api.kubernetesPodLogStream).toHaveBeenLastCalledWith("shop", "api", expect.objectContaining({ container: "sidecar-6" }), expect.any(Function), expect.any(AbortSignal)));
    expect(select.value).toBe("sidecar-6");
    expect(screen.queryByRole("alert")).toBeNull();
  });
  it("distinguishes connecting, waiting, quiet, filtered, and failed empty output", async () => {
    vi.useFakeTimers();
    let emit: (event: KubernetesPodLogEvent) => void = () => undefined;
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, callback, signal) => {
      emit = callback;
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    render(<PodLogViewer resource={resource} />);
    const output = screen.getByLabelText("Pod log output");
    // A read that has not delivered a line yet is waiting, not empty.
    expect(output.textContent).toBe("Waiting for the first log line…");
    // Since offers exactly the four bounded windows.
    const windows = within(screen.getByLabelText("Since window")).getAllByRole("option").map((option) => option.textContent);
    expect(windows).toEqual(["15m", "1h", "6h", "24h"]);
    fireEvent.change(screen.getByLabelText("Log filter"), { target: { value: "missing" } });
    act(() => emit({ event: "heartbeat", text: "", container: "app" }));
    expect(output.textContent).toBe("Waiting for the first log line…");
    // Ten seconds of heartbeats with no line is a quiet container.
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(output.textContent).toBe("No new log lines yet. The container is quiet.");
    act(() => emit({ event: "line", text: "actual", container: "app", cursor: "one" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(output.textContent).toBe("No log lines match this filter.");
    fireEvent.click(screen.getByLabelText("Reload logs"));
    act(() => emit({ event: "error", text: "", container: "app", code: "forbidden", message: "Access denied", action: "Check RBAC" }));
    expect(output.textContent).toContain("forbidden Access denied Check RBAC");
    expect(screen.queryByRole("alert")).toBeNull();
    expect(document.body.textContent).not.toContain("No log lines in the selected time");
  });

  it("says an ended previous read ended instead of calling the container quiet", async () => {
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => emit({ event: "end", text: "", container: "app" }));
    render(<PodLogViewer resource={resource} />);
    fireEvent.click(screen.getByLabelText("Previous container logs"));
    await waitFor(() => expect(screen.getByLabelText("Pod log output").textContent).toBe("The previous container read ended. No further lines will arrive."));
    expect(stream).toHaveBeenCalledTimes(2);
    expect(screen.queryByLabelText("Follow")).toBeNull();
  });

  it("follows at the bottom, pauses when the reader scrolls up, and returns on Jump to latest", async () => {
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
    // Following is the default: the newest line is where the reader is.
    expect(output.scrollTop).toBe(1000);
    expect(screen.queryByText("Jump to latest")).toBeNull();
    output.scrollTop = 100;
    fireEvent.scroll(output);
    act(() => emit({ event: "line", text: "next", container: "app", cursor: "two" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    // Scrolling up paused the follow: the reading position did not move.
    expect(output.scrollTop).toBe(100);
    expect(screen.getByText("Jump to latest")).toBeTruthy();
    fireEvent.click(screen.getByText("Jump to latest"));
    expect(output.scrollTop).toBe(1000);
    expect(screen.queryByText("Jump to latest")).toBeNull();
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

  it("keeps previous-unavailable errors honest and loads a Since window without restarting the read", async () => {
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, options, emit) => {
      emit({ event: options.previous ? "error" : "end", text: "", container: "app", code: "previous_unavailable" });
    });
    const earlier = vi.spyOn(api, "kubernetesPodLogs").mockResolvedValue({ cluster_id: "production", namespace: "shop", pod: "api", container: "app", previous: false, since_seconds: 86400, tail_lines: 5000, text: "yesterday", truncated: false });
    render(<PodLogViewer resource={resource} />);
    // The opening read is a short tail from now, not an hour of history.
    expect(stream).toHaveBeenCalledWith("shop", "api", expect.objectContaining({ tail_lines: 200 }), expect.any(Function), expect.any(AbortSignal));
    expect(stream.mock.calls[0][2]).not.toHaveProperty("since_seconds");
    fireEvent.click(screen.getByLabelText("Previous container logs"));
    await waitFor(() => expect(screen.getByLabelText("Pod log output").textContent).toContain("no previous container instance"));
    expect(screen.queryByRole("alert")).toBeNull();
    const calls = stream.mock.calls.length;
    fireEvent.change(screen.getByLabelText("Since window"), { target: { value: "86400" } });
    fireEvent.click(screen.getByRole("button", { name: "Load logs since that window" }));
    await waitFor(() => expect(earlier).toHaveBeenCalledWith("shop", "api", expect.objectContaining({ since_seconds: 86400, tail_lines: 5000 })));
    await waitFor(() => expect(screen.getByLabelText("Pod log output").textContent).toContain("yesterday"));
    // The live read was never touched.
    expect(stream.mock.calls.length).toBe(calls);
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
    expect(screen.getByLabelText("Log connection status").textContent).toBe("Connecting");
  });

  it("renders only the newest thousand matching lines while search and download span the loaded window", async () => {
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      for (let ordinal = 1; ordinal <= 10_001; ordinal++) emit({ event: "line", text: `tiny ${ordinal}`, container: "app", timestamp: "2026-10-07T12:00:00Z", ordinal });
      emit({ event: "end", text: "", container: "app", cursor: "final" });
    });
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
    const { container } = render(<PodLogViewer resource={resource} />);
    const output = screen.getByLabelText("Pod log output");
    await waitFor(() => expect(output.querySelectorAll("[data-log-key]")).toHaveLength(1000));
    // The thousand rendered rows are the newest ones.
    expect(output.textContent).toContain("tiny 10001");
    expect(output.textContent).not.toContain("tiny 9000 ");
    expect(container.textContent).not.toMatch(/retained lines|evicted/);
    // Scrolling to the top of the window reveals the next thousand.
    Object.defineProperty(output, "scrollHeight", { configurable: true, value: 2000 });
    Object.defineProperty(output, "clientHeight", { configurable: true, value: 400 });
    output.scrollTop = 0;
    fireEvent.scroll(output);
    await waitFor(() => expect(output.querySelectorAll("[data-log-key]")).toHaveLength(2000));
    // A search reads the whole loaded window; copying keeps every match, not just the rendered rows.
    fireEvent.change(screen.getByLabelText("Log filter"), { target: { value: "tiny 8000" } });
    await waitFor(() => expect(output.querySelectorAll("[data-log-key]")).toHaveLength(1));
    fireEvent.click(screen.getByLabelText("Copy scrubbed logs"));
    await waitFor(() => expect(writeText).toHaveBeenCalled());
    expect(String(writeText.mock.calls.at(-1)![0])).toContain("tiny 8000");
    expect(container.querySelector('[role="alert"]')).toBeNull();
    expect(container.textContent).not.toContain("Log replay is uncertain");
    expect((screen.getByLabelText("Reload logs") as HTMLButtonElement).disabled).toBe(false);
  });

  it("loads a Since window past ten thousand lines and keeps search, copy and download over the whole loaded window", async () => {
    let emit: (event: KubernetesPodLogEvent) => void = () => undefined;
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, callback, signal) => {
      emit = callback;
      callback({ event: "line", text: "live newest", container: "app" });
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    const history = Array.from({ length: 15_000 }, (_, index) => `history ${String(index + 1).padStart(5, "0")}`);
    const earlier = vi.spyOn(api, "kubernetesPodLogs").mockResolvedValue({ cluster_id: "production", namespace: "shop", pod: "api", container: "app", previous: false, since_seconds: 86_400, tail_lines: 5000, text: history.join("\n"), truncated: false });
    const writeText = vi.fn().mockResolvedValue(undefined);
    const createObjectURL = vi.fn<(blob: Blob) => string>().mockReturnValue("blob:logs");
    const revokeObjectURL = vi.fn();
    vi.stubGlobal("URL", Object.assign(class extends URL {}, { createObjectURL, revokeObjectURL }));
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => undefined);
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
    render(<PodLogViewer resource={resource} />);
    const output = screen.getByLabelText("Pod log output");
    await waitFor(() => expect(output.textContent).toContain("live newest"));
    fireEvent.change(screen.getByLabelText("Since window"), { target: { value: "86400" } });
    fireEvent.click(screen.getByRole("button", { name: "Load logs since that window" }));
    await waitFor(() => expect(earlier).toHaveBeenCalledWith("shop", "api", expect.objectContaining({ since_seconds: 86_400, tail_lines: 5000 })));
    await waitFor(() => expect(output.textContent).toContain("history 15000"));
    expect(output.querySelectorAll("[data-log-key]")).toHaveLength(1000);
    // A live line after the earlier read must keep the raised ceiling: the ring
    // may not fall back to the 10,000-line live cap.
    act(() => emit({ event: "line", text: "live after", container: "app" }));
    await waitFor(() => expect(output.textContent).toContain("live after"));
    expect(output.querySelectorAll("[data-log-key]")).toHaveLength(1000);
    // This line is far outside both the 1,000-row render window and the
    // 10,000-line live cap, so only a raised loaded ceiling can still hold it.
    fireEvent.change(screen.getByLabelText("Log filter"), { target: { value: "history 00002" } });
    await waitFor(() => expect(output.querySelectorAll("[data-log-key]")).toHaveLength(1));
    expect(output.textContent).toContain("history 00002");
    fireEvent.change(screen.getByLabelText("Log filter"), { target: { value: "" } });
    fireEvent.click(screen.getByLabelText("Copy scrubbed logs"));
    await waitFor(() => expect(writeText).toHaveBeenCalled());
    const copied = String(writeText.mock.calls.at(-1)![0]).split("\n");
    expect(copied).toHaveLength(15_001);
    expect(copied).toContain("history 00002");
    expect(copied).toContain("history 15000");
    expect(copied).toEqual(expect.arrayContaining(["live newest", "live after"]));
    fireEvent.click(screen.getByLabelText("Download scrubbed logs"));
    const blob = createObjectURL.mock.calls.at(-1)![0];
    const downloaded = await new Promise<string>((resolve, reject) => {
      const reader = new FileReader();
      reader.onload = () => resolve(String(reader.result));
      reader.onerror = reject;
      reader.readAsText(blob);
    });
    expect(downloaded.split("\n")).toHaveLength(15_001);
    expect(downloaded).toContain("history 00002");
    expect(stream).toHaveBeenCalledTimes(1);
    expect(click).toHaveBeenCalledTimes(1);
  });

  it("never offers Follow when the read ends on a Pod with no terminal phase", async () => {
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      emit({ event: "line", text: "last line", container: "app", timestamp: "2026-10-07T12:00:00Z", ordinal: 1 });
      emit({ event: "end", text: "", container: "app", cursor: "final" });
    });
    const running = { ...resource, summary: { ...resource.summary, phase: "Running" } };
    render(<PodLogViewer resource={running} />);
    await waitFor(() => expect(screen.getByLabelText("Log connection status").textContent).toBe("Ended"));
    expect(screen.queryByLabelText("Follow")).toBeNull();
    expect(screen.getByLabelText("Pod log output").textContent).toContain("last line");
    expect((screen.getByLabelText("Reload logs") as HTMLButtonElement).disabled).toBe(false);
  });

  it("offers no Follow control on any read, running, ended or finished", async () => {
    let emit: (event: KubernetesPodLogEvent) => void = () => undefined;
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, callback, signal) => {
      emit = callback;
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    const finished = { ...resource, summary: { ...resource.summary, phase: "Succeeded" } };
    const { rerender } = render(<PodLogViewer resource={finished} />);
    await waitFor(() => expect(stream).toHaveBeenCalled());
    expect(screen.queryByLabelText("Follow")).toBeNull();
    expect(screen.getByLabelText("Log connection status").textContent).toBe("Ended");
    act(() => emit({ event: "heartbeat", text: "", container: "app" }));
    await waitFor(() => expect(screen.getByLabelText("Log connection status").textContent).toBe("Ended"));
    // A running Pod follows at the bottom exactly the same way: the toggle was
    // never the thing that made following work.
    rerender(<PodLogViewer resource={resource} key="running" />);
    await waitFor(() => expect(stream.mock.calls.length).toBeGreaterThan(1));
    expect(screen.queryByLabelText("Follow")).toBeNull();
    expect(screen.getByLabelText("Log connection status").textContent).toBe("Connecting");
  });

  it("names a clamped cursor resume as a gap in coverage, never as a failed read", async () => {
    vi.useFakeTimers();
    // A cursor that names a position far behind the newest line cannot be a
    // resume point, so the read restarts from the newest line's own time: the
    // lines in between are a gap, not a failure.
    const cursor = btoa(JSON.stringify({ timestamp: "2026-10-26T07:00:00Z" }));
    let emit: (event: KubernetesPodLogEvent) => void = () => undefined;
    const stream = vi.spyOn(api, "kubernetesPodLogStream")
      .mockImplementationOnce(async (_namespace, _pod, _options, emit) => {
        emit({ event: "line", text: "oldest kept", container: "app", timestamp: "2026-10-26T07:00:00Z", ordinal: 1, cursor });
        emit({ event: "line", text: "newest kept", container: "app", timestamp: "2026-10-26T07:05:00Z", ordinal: 2 });
        emit({ event: "limit", text: "", container: "app", cursor, reason: "duration" });
      })
      .mockImplementation(async (_namespace, _pod, _options, callback) => {
        emit = callback;
        callback({ event: "heartbeat", text: "", container: "app", replay_uncertain: true });
        callback({ event: "error", text: "", container: "app", replay_uncertain: true });
      });
    render(<PodLogViewer resource={resource} />);
    await act(async () => { await vi.advanceTimersByTimeAsync(1100); });
    // The read restarted from a wall-clock time, so it says so.
    expect(stream.mock.calls[1][2]).not.toHaveProperty("cursor");
    expect(stream.mock.calls[1][2]).toHaveProperty("since_time");
    const gap = document.querySelector("[data-log-gap]");
    expect(gap?.textContent).toContain("Gap in the log read:");
    expect(gap?.textContent).toContain("may be missing");
    // The lines the reader already had are still there...
    const output = screen.getByLabelText("Pod log output");
    expect(output.textContent).toContain("oldest kept");
    expect(output.textContent).toContain("newest kept");
    // ...and the uncertainty is not a stop: no failure copy, no Unavailable.
    expect(screen.getByLabelText("Log connection status").textContent).not.toBe("Unavailable");
    expect(output.textContent).not.toContain("Pod log stream stopped");
    expect(document.querySelector("[data-log-notice]")).toBeNull();
    expect(screen.queryByRole("button", { name: "Retry logs" })).toBeNull();
    // A genuine failure after that still stops the read.
    act(() => emit({ event: "error", text: "", container: "app", code: "forbidden", message: "Access denied" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(screen.getByLabelText("Log connection status").textContent).toBe("Unavailable");
    expect(screen.getByLabelText("Pod logs").textContent).toContain("forbidden Access denied");
  });

  it("names a sixty-three character container in full in the selector", async () => {
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, emit, signal) => {
      emit({ event: "heartbeat", text: "", container: "app" });
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    const long = "checkout-api-placement-queue-metrics-exporter-sidecar-warmed-xy";
    expect(long).toHaveLength(63);
    render(<PodLogViewer resource={{ ...resource, summary: { log_containers: [{ name: long, type: "regular" }], main_container: long } }} />);
    // The selector shows the whole name in its option and truncates in the
    // closed control rather than painting over its neighbours.
    const select = (await screen.findByLabelText("Log container")) as HTMLSelectElement;
    expect(select.value).toBe(long);
    expect([...select.options].map((option) => option.textContent)).toContain(long);
  });

  it("offers Jump to latest only while the reader is scrolled away from the newest line", async () => {
    vi.useFakeTimers();
    let emit: (event: KubernetesPodLogEvent) => void = () => undefined;
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, callback, signal) => {
      emit = callback;
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    const { container } = render(<PodLogViewer resource={resource} />);
    const output = screen.getByLabelText("Pod log output");
    Object.defineProperty(output, "scrollHeight", { configurable: true, value: 1200 });
    Object.defineProperty(output, "clientHeight", { configurable: true, value: 200 });
    act(() => emit({ event: "line", text: "first", container: "app", cursor: "one" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    const footer = container.querySelector(".log-footer")!;
    expect(within(footer).queryByText("Jump to latest")).toBeNull();
    output.scrollTop = 200;
    fireEvent.scroll(output);
    expect(within(footer).getByText("Jump to latest")).toBeTruthy();
    fireEvent.click(within(footer).getByText("Jump to latest"));
    expect(within(footer).queryByText("Jump to latest")).toBeNull();
  });

  it("keeps the row the reader is on mounted while new lines push the tail forward", async () => {
    vi.useFakeTimers();
    let emit: (event: KubernetesPodLogEvent) => void = () => undefined;
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, callback, signal) => {
      emit = callback;
      for (let ordinal = 1; ordinal <= 1_005; ordinal++) callback({ event: "line", text: `line ${ordinal}`, container: "app", timestamp: "2026-10-26T12:00:00Z", ordinal });
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    render(<PodLogViewer resource={resource} />);
    const output = screen.getByLabelText("Pod log output");
    Object.defineProperty(output, "scrollHeight", { configurable: true, value: 2_000 });
    Object.defineProperty(output, "clientHeight", { configurable: true, value: 200 });
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    const rows = Array.from(output.querySelectorAll<HTMLElement>("[data-log-key]"));
    expect(rows).toHaveLength(1_000);
    rows.forEach((row, index) => Object.defineProperty(row, "offsetTop", { configurable: true, value: index * 20 }));
    output.scrollTop = 40;
    fireEvent.scroll(output);
    const anchor = rows[2];
    const anchorKey = anchor.dataset.logKey!;
    // New lines arrive while the reader is still on that row; a trailing window
    // would have trimmed it out of the document.
    act(() => {
      for (let ordinal = 1_006; ordinal <= 1_030; ordinal++) emit({ event: "line", text: `line ${ordinal}`, container: "app", timestamp: "2026-10-26T12:00:00Z", ordinal });
    });
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(output.querySelector(`[data-log-key="${anchorKey}"]`)).toBeTruthy();
    expect(output.scrollTop).toBe(40);
  });

  it("offers Scroll to latest in the actions menu only while the reader is away from the newest line", async () => {
    vi.useFakeTimers();
    let emit: (event: KubernetesPodLogEvent) => void = () => undefined;
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, callback, signal) => {
      emit = callback;
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    render(<PodLogViewer resource={resource} />);
    const output = screen.getByLabelText("Pod log output");
    Object.defineProperty(output, "scrollHeight", { configurable: true, value: 1_200 });
    Object.defineProperty(output, "clientHeight", { configurable: true, value: 200 });
    act(() => emit({ event: "line", text: "first", container: "app", cursor: "one" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(screen.queryByLabelText("Scroll to latest")).toBeNull();
    output.scrollTop = 200;
    fireEvent.scroll(output);
    expect(screen.getByLabelText("Scroll to latest")).toBeTruthy();
    fireEvent.click(screen.getByLabelText("Scroll to latest"));
    expect(screen.queryByLabelText("Scroll to latest")).toBeNull();
  });

  it("waits for the first line again after Reload instead of claiming the container is quiet", async () => {
    vi.useFakeTimers();
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, emit, signal) => {
      emit({ event: "heartbeat", text: "", container: "app" });
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    render(<PodLogViewer resource={resource} />);
    await act(async () => { await vi.advanceTimersByTimeAsync(11_000); });
    const output = screen.getByLabelText("Pod log output");
    expect(output.textContent).toBe("No new log lines yet. The container is quiet.");
    fireEvent.click(screen.getByLabelText("Reload logs"));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(output.textContent).toBe("Waiting for the first log line…");
  });

  it("names a limit-stopped read Stopped with the limit reason instead of Ended", async () => {
    vi.useFakeTimers();
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      emit({ event: "line", text: "retained", container: "app" });
      emit({ event: "limit", text: "", container: "app", reason: "byte_limit" });
    });
    const running = { ...resource, summary: { ...resource.summary, phase: "Running" } };
    render(<PodLogViewer resource={running} />);
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    const status = screen.getByLabelText("Log connection status");
    expect(status.textContent).toBe("Stopped");
    expect(status.getAttribute("title")).toContain("byte safety limit");
    expect(screen.getByLabelText("Pod logs").querySelector("p[role='status']")?.textContent).toContain("byte safety limit");
    // The container is still running, but only a reload starts a new read.
    expect(screen.queryByLabelText("Follow")).toBeNull();
  });

  it("renders large reads cheaply: one shared clock formatter and one batch insert for all rows", async () => {
    const perCall = vi.spyOn(Date.prototype, "toLocaleTimeString");
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      for (let ordinal = 1; ordinal <= 300; ordinal++) emit({ event: "line", text: ordinal === 2 ? "ERROR boom" : "tiny", container: "app", timestamp: "2026-10-07T12:00:0" + (ordinal % 10) + "Z", ordinal });
      emit({ event: "end", text: "", container: "app", cursor: "final" });
    });
    const { container } = render(<PodLogViewer resource={resource} />);
    const output = screen.getByLabelText("Pod log output");
    await waitFor(() => expect(output.querySelectorAll("[data-log-key]")).toHaveLength(300));
    // Rows share one formatter; a per-row toLocaleTimeString builds a formatter each time.
    expect(perCall.mock.calls.length).toBeLessThan(10);
    perCall.mockRestore();
    const first = output.querySelector("[data-log-key] time")!;
    expect(first.textContent).toBe(new Date(first.getAttribute("dateTime")!).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false }));
    const batches = output.querySelectorAll("[data-log-lines]");
    expect(batches).toHaveLength(1);
    expect(batches[0].children).toHaveLength(300);
    expect(batches[0].className).toContain("contents");
    expect(container.querySelectorAll("[data-log-key] .text-sev-critical")).toHaveLength(1);
  });
  it("offers the containers a row-opened Pod's read reports when the list summary has none", async () => {
    const longContainer = "payments-checkout-orchestrator-primary-runtime-container-v2026x";
    // The row projection carries no container summary at all.
    const rowPod = { resource_id: "core~v1~pods", kind: "Pod", namespace: "payments", name: "long-container-pod", summary: { phase: "Running" } };
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, emit, signal) => {
      emit({ event: "line", text: "ready", container: longContainer, ordinal: 1, timestamp: "2026-10-26T07:00:00Z" });
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    render(<PodLogViewer resource={rowPod} />);
    const select = (await screen.findByLabelText("Log container")) as HTMLSelectElement;
    await waitFor(() => expect([...select.options].map((option) => option.value)).toEqual([longContainer]));
    expect(select.value).toBe(longContainer);
    expect(stream).toHaveBeenCalledTimes(1);
  });

  it("keeps the described container list instead of the one container the read reported", async () => {
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, emit, signal) => {
      emit({ event: "line", text: "ready", container: "app", ordinal: 1, timestamp: "2026-10-26T07:00:00Z" });
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    render(<PodLogViewer resource={resource} />);
    const select = (await screen.findByLabelText("Log container")) as HTMLSelectElement;
    // An init container is named as one, so it is never mistaken for a running
    // container of the same Pod.
    expect([...select.options].map((option) => option.textContent)).toEqual(["app", "init:setup", "debug (ephemeral)"]);
    expect(stream).toHaveBeenCalledTimes(1);
  });

  it("says an ended Pod has ended instead of claiming a quiet container", async () => {
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      emit({ event: "end", text: "", container: "app" });
    });
    render(<PodLogViewer resource={{ ...resource, summary: { phase: "Succeeded", log_containers: [{ name: "app", type: "regular" }] } }} />);
    await waitFor(() => expect(screen.getByLabelText("Pod log output").textContent).toBe("The log read ended. No further lines will arrive."));
    expect(screen.getByLabelText("Pod log output").textContent).not.toContain("quiet");
  });

  it("still calls a running container quiet when the read only has heartbeats", async () => {
    vi.useFakeTimers();
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, emit, signal) => {
      emit({ event: "heartbeat", text: "", container: "app" });
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    render(<PodLogViewer resource={resource} />);
    await act(async () => { await vi.advanceTimersByTimeAsync(11_000); });
    expect(screen.getByLabelText("Pod log output").textContent).toBe("No new log lines yet. The container is quiet.");
  });

  // F3: a reader who scrolls up and then back down is following again. Nothing
  // has to be clicked to resume: the newest line is where they are.
  it("resumes following when the reader scrolls back down to the newest line", async () => {
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

    // Scrolling up stops the follow and offers the jump control.
    output.scrollTop = 100;
    fireEvent.scroll(output);
    act(() => emit({ event: "line", text: "second", container: "app", cursor: "two" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(output.scrollTop).toBe(100);
    expect(screen.getByText("Jump to latest")).toBeTruthy();

    // Returning to the bottom resumes following on its own, at the newest line,
    // and the next line keeps the reader there.
    output.scrollTop = 800;
    fireEvent.scroll(output);
    expect(output.scrollTop).toBe(1000);
    expect(screen.queryByText("Jump to latest")).toBeNull();
    act(() => emit({ event: "line", text: "third", container: "app", cursor: "three" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(output.scrollTop).toBe(1000);
  });

  // F4: a rollout can remove or rename the container being read. The selector
  // must not hold a name no option offers, and the read must not keep asking
  // for a container that is gone.
  it("moves off a container a rollout removed and reads one that is still there", async () => {
    const deployment = { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "shop", name: "checkout", uid: "workload-uid" };
    let emit: (event: KubernetesWorkloadLogEvent) => void = () => undefined;
    const stream = vi.spyOn(api, "kubernetesWorkloadLogStream").mockImplementation((_kind, _namespace, _name, _uid, _options, callback, signal) => {
      emit = callback;
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    const membership = (containers: string[]): KubernetesWorkloadLogEvent => ({ event: "sources", text: "", container: "", sources: [], pods: [{ uid: "pod-a", name: "checkout-a", containers, init_containers: [], admitted: true }], truncated: false });
    render(<PodLogViewer resource={deployment} />);
    act(() => emit(membership(["app", "proxy"])));
    const select = (await screen.findByLabelText("Log container")) as HTMLSelectElement;
    await waitFor(() => expect([...select.options].map((option) => option.value)).toEqual(["*", "", "app", "proxy"]));
    fireEvent.change(select, { target: { value: "proxy" } });
    await waitFor(() => expect(stream).toHaveBeenLastCalledWith("Deployment", "shop", "checkout", "workload-uid", expect.objectContaining({ container: "proxy" }), expect.any(Function), expect.any(AbortSignal)));

    // The rollout replaced the pod; proxy no longer exists anywhere.
    act(() => emit(membership(["app"])));
    await waitFor(() => expect(select.value).toBe("app"));
    expect([...select.options].map((option) => option.value)).toEqual(["*", "", "app"]);
    await waitFor(() => expect(stream).toHaveBeenLastCalledWith("Deployment", "shop", "checkout", "workload-uid", expect.objectContaining({ container: "app" }), expect.any(Function), expect.any(AbortSignal)));
  });

  // F5: the compact card has no toolbar, so a limit-stopped read needs its own
  // way back instead of a message naming a control that is not on the card.
  it("offers Reload in the compact card for a read the safety limit stopped", async () => {
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      emit({ event: "line", text: "retained", container: "app" });
      emit({ event: "limit", text: "", container: "app", reason: "byte_limit" });
    });
    render(<PodLogViewer resource={{ ...resource, summary: { ...resource.summary, phase: "Running" } }} compact />);
    await waitFor(() => expect(screen.getByLabelText("Log connection status").textContent).toBe("Stopped"));
    const footer = document.querySelector(".log-footer") as HTMLElement;
    expect(within(footer).getByRole("button", { name: "Open full logs" })).toBeTruthy();
    fireEvent.click(within(footer).getByRole("button", { name: "Reload logs" }));
    await waitFor(() => expect(stream).toHaveBeenCalledTimes(2));
  });

  // QA-210: all containers is the widest read, so it is the first option.
  it("lists All containers first in a workload's container selector", async () => {
    vi.spyOn(api, "kubernetesWorkloadLogStream").mockImplementation((_kind, _namespace, _name, _uid, _options, emit, signal) => {
      emit({ event: "sources", text: "", container: "", sources: [], pods: [{ uid: "pod-a", name: "checkout-a", containers: ["app", "proxy"], init_containers: [], admitted: true }], truncated: false });
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    render(<PodLogViewer resource={{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "shop", name: "checkout", uid: "workload-uid" }} compact />);
    const select = (await screen.findByLabelText("Log container")) as HTMLSelectElement;
    await waitFor(() => expect([...select.options].map((option) => option.textContent)).toEqual(["All containers", "Main container", "app", "proxy"]));
  });
});

describe("Compact overview logs placement", () => {
 it.each(["quiet", "populated", "ended", "error"] as const)("keeps the same layout placement for %s output", async (state) => {
  vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit, signal) => {
   if (state === "populated") emit({ event: "line", text: "INFO served", container: "app", ordinal: 1 });
   if (state === "error") throw new Error("read unavailable");
   if (state === "ended") return;
   return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
  });
  const { container } = render(<div className="resource-tabpanel" data-view="overview"><div className="resource-log-slot" data-full="false"><PodLogViewer resource={resource} compact /></div></div>);
  await waitFor(() => expect(api.kubernetesPodLogStream).toHaveBeenCalled());
  const card = container.querySelector(".resource-log-viewer")!;
  expect(card.parentElement?.className).toBe("resource-log-slot");
  expect(card.getAttribute("data-compact")).toBe("true");
  expect(screen.getByText("Live logs")).toBeTruthy();
 });
});
