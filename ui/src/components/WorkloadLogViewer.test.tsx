// @vitest-environment jsdom
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  ApiError,
  api,
  type KubernetesResource,
  type KubernetesWorkloadLogEvent,
  type KubernetesWorkloadLogPod,
} from "@/lib/api";
import { PodLogViewer } from "./PodLogViewer";
import { podColour } from "@/lib/kubernetesResourceDetail";
import { POD_LOG_WORKLOAD_EARLIER_TAIL } from "@/lib/usePodLogStream";

const resource: KubernetesResource = {
  resource_id: "apps~v1~deployments",
  kind: "Deployment",
  namespace: "shop",
  name: "checkout",
  uid: "workload-uid",
};
const firstPod = {
  uid: "pod-a",
  name: "checkout-a",
  containers: ["app", "proxy"],
  init_containers: ["setup"],
  admitted: true,
};
const secondPod = { ...firstPod, uid: "pod-b", name: "checkout-b" };
function connection() {
  let emit: (event: KubernetesWorkloadLogEvent) => void = () => undefined;
  let finish: () => void = () => undefined;
  const stream = vi
    .spyOn(api, "kubernetesWorkloadLogStream")
    .mockImplementation(
      (_kind, _namespace, _name, _uid, _options, callback, signal) => {
        emit = callback;
        return new Promise<void>((resolve) => {
          finish = resolve;
          signal.addEventListener("abort", resolve, { once: true });
        });
      },
    );
  return {
    stream,
    emit: (event: KubernetesWorkloadLogEvent) => act(() => emit(event)),
    finish: () => act(() => finish()),
  };
}
const membership = (
  pods: KubernetesWorkloadLogPod[],
  truncated = false,
): KubernetesWorkloadLogEvent => ({
  event: "sources",
  text: "",
  container: "",
  sources: [],
  pods,
  truncated,
});
const line = (
  pod = firstPod,
  text = "served request",
): KubernetesWorkloadLogEvent => ({
  event: "line",
  pod: pod.name,
  pod_uid: pod.uid,
  workload_uid: resource.uid,
  namespace: "shop",
  source_id: `${pod.uid}/app`,
  incarnation: "running",
  text,
  container: "app",
  timestamp: "2026-10-21T08:00:00Z",
  ordinal: 1,
  cursor: `${pod.uid}-cursor`,
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe("Basic workload log viewer", () => {
  it("keeps four admitted Pod prefixes distinct and source status separate from reconnect dividers", async () => {
    const current = connection();
    const pods = [firstPod, secondPod, { ...firstPod, uid: "pod-c", name: "checkout-c" }, { ...firstPod, uid: "pod-d", name: "checkout-d" }];
    render(<PodLogViewer resource={resource} />);
    current.emit(membership(pods));
    pods.forEach((pod) => current.emit(line(pod)));
    current.emit({ event: "source_status", text: "", pod: secondPod.name, pod_uid: secondPod.uid, source_id: "denied", container: "app", code: "forbidden", message: "forbidden" });
    await waitFor(() => expect(screen.getByLabelText("Workload log output").textContent).toContain("logs not available: forbidden"));
    const output = screen.getByLabelText("Workload log output");
    expect(new Set(pods.map((pod) => within(output).getAllByRole("button", { name: pod.name })[0].className)).size).toBe(4);
    const status = output.querySelector(".log-source-status")!;
    expect(status.textContent).toContain(secondPod.name);
    expect(status.querySelector(".log-note")).toBeNull();
    expect(status.classList.contains("log-neutral")).toBe(false);
    expect(current.stream).toHaveBeenCalledTimes(1);
  });
  it("opens one aggregate stream, defaults to all Pods, prefixes lines and narrows from the prefix", async () => {
    const live = connection();
    render(<PodLogViewer resource={resource} />);
    expect(live.stream).toHaveBeenCalledTimes(1);
    expect(live.stream.mock.calls[0].slice(0, 5)).toEqual([
      "Deployment",
      "shop",
      "checkout",
      "workload-uid",
      {
        container: "",
        previous: false,
        timestamps: false,
        tail_lines: 200,
      },
    ]);
    expect((screen.getByLabelText("Log pod") as HTMLSelectElement).value).toBe(
      "",
    );
    live.emit(membership([firstPod, secondPod]));
    live.emit(line());
    live.emit(line(secondPod));
    await waitFor(() =>
      expect(
        screen.getByLabelText("Workload log output").textContent,
      ).toContain("checkout-b"),
    );
    const prefix = screen.getByRole("button", { name: "checkout-a" });
    expect(prefix.className).toContain(podColour(0));
    expect(
      screen.queryByText(
        /Partial coverage|Streaming .* of|Sources|Snapshot read/,
      ),
    ).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
    fireEvent.click(prefix);
    expect(live.stream.mock.calls[0][6].aborted).toBe(true);
    expect(live.stream.mock.calls[1][4]).toMatchObject({ pod_uid: ["pod-a"] });
    expect((screen.getByLabelText("Log pod") as HTMLSelectElement).value).toBe(
      "pod-a",
    );
  });
  it("updates membership on join/departure without losing retained output and lists overflow only in the picker", async () => {
    const live = connection();
    render(<PodLogViewer resource={{ ...resource, kind: "StatefulSet" }} />);
    live.emit(membership([firstPod]));
    live.emit(line());
    await waitFor(() =>
      expect(screen.getByText("served request")).toBeTruthy(),
    );
    const overflow = { ...secondPod, admitted: false, reason: "source_limit" };
    live.emit(membership([firstPod, overflow], true));
    expect(
      screen.getByRole("option", { name: "All pods (first 20)" }),
    ).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Log pod"), {
      target: { value: "pod-b" },
    });
    expect(live.stream.mock.calls.at(-1)![4]).toMatchObject({
      pod_uid: ["pod-b"],
    });
    fireEvent.change(screen.getByLabelText("Log pod"), {
      target: { value: "" },
    });
    live.emit(line());
    await waitFor(() =>
      expect(screen.getByText("served request")).toBeTruthy(),
    );
    live.emit(membership([secondPod]));
    expect(screen.queryByRole("option", { name: "checkout-a" })).toBeNull();
    expect(screen.getByText("served request")).toBeTruthy();
  });
  it("reserves capacity labels for explicit source-limit omissions", () => {
    const live = connection();
    render(<PodLogViewer resource={resource} />);
    live.emit(
      membership(
        [
          { ...firstPod, admitted: false, reason: "container_unavailable" },
          { ...secondPod, admitted: false },
        ],
        true,
      ),
    );
    expect(
      screen.getByRole("option", { name: "All pods", exact: true }),
    ).toBeTruthy();
    expect(
      screen.getByRole("option", { name: firstPod.name, exact: true }),
    ).toBeTruthy();
    expect(
      screen.getByRole("option", { name: secondPod.name, exact: true }),
    ).toBeTruthy();
    expect(
      screen.queryByRole("option", { name: /first 20|not in all pods/ }),
    ).toBeNull();
    live.emit(
      membership([
        { ...firstPod, admitted: false, reason: "source_limit" },
        secondPod,
      ]),
    );
    expect(
      screen.getByRole("option", { name: "All pods (first 20)" }),
    ).toBeTruthy();
    expect(
      screen.getByRole("option", {
        name: `${firstPod.name} (not in all pods)`,
      }),
    ).toBeTruthy();
    expect(
      screen.getByRole("option", { name: secondPod.name, exact: true }),
    ).toBeTruthy();
  });
  it("retains the unfiltered capacity labels when focusing the twenty-fifth Pod", () => {
    const live = connection();
    render(<PodLogViewer resource={resource} />);
    const pods = Array.from(
      { length: 25 },
      (_, index): KubernetesWorkloadLogPod => ({
        uid: `pod-${index}`,
        name: `checkout-${index}`,
        containers: ["app"],
        init_containers: [],
        admitted: index < 20,
        reason: index >= 20 ? "source_limit" : undefined,
        all_pods_admitted: index < 20,
        all_pods_reason: index >= 20 ? "source_limit" : undefined,
      }),
    );
    live.emit(membership(pods));
    expect(
      screen.getByRole("option", { name: "All pods (first 20)" }),
    ).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Log pod"), {
      target: { value: "pod-24" },
    });
    expect(live.stream.mock.calls.at(-1)![4]).toMatchObject({
      pod_uid: ["pod-24"],
    });
    live.emit(
      membership(
        pods.map((pod) => ({
          ...pod,
          admitted: pod.uid === "pod-24",
          reason: pod.uid === "pod-24" ? undefined : "not_selected",
        })),
      ),
    );
    expect(
      screen.getByRole("option", { name: "All pods (first 20)" }),
    ).toBeTruthy();
    expect(
      screen.getByRole("option", { name: "checkout-24 (not in all pods)" }),
    ).toBeTruthy();
    expect(
      screen.getAllByRole("option", { name: /not in all pods/ }),
    ).toHaveLength(5);
    expect(
      screen.getByRole("option", { name: "checkout-0", exact: true }),
    ).toBeTruthy();
    expect((screen.getByLabelText("Log pod") as HTMLSelectElement).value).toBe(
      "pod-24",
    );
    live.emit(
      membership(
        pods
          .slice(0, 20)
          .map((pod) => ({ ...pod, admitted: false, reason: "not_selected" })),
      ),
    );
    expect(
      screen.getByRole("option", { name: "All pods", exact: true }),
    ).toBeTruthy();
    expect(
      screen.queryByRole("option", {
        name: /checkout-24|not in all pods|first 20/,
      }),
    ).toBeNull();
  });
  it.each([false, true])("keeps one dim unreadable-Pod notice through repeated denials and keeps other sources live (already flushed: %s)", async (flushed) => {
    const live = connection();
    render(<PodLogViewer resource={resource} />);
    const status: KubernetesWorkloadLogEvent = {
      ...line(secondPod),
      event: "source_status",
      code: "forbidden",
      message: "forbidden",
      text: "",
    };
    live.emit(membership([firstPod, secondPod]));
    live.emit(line(secondPod, "previous output from denied Pod"));
    if (flushed) await screen.findByText("previous output from denied Pod");
    live.emit(status);
    if (flushed) await screen.findByText("logs not available: forbidden");
    live.emit(status);
    live.emit(line());
    await waitFor(() =>
      expect(screen.getByText("logs not available: forbidden")).toBeTruthy(),
    );
    expect(screen.getAllByText("logs not available: forbidden")).toHaveLength(
      1,
    );
    expect(await screen.findByText("served request")).toBeTruthy();
    expect(screen.queryByText("previous output from denied Pod")).toBeNull();
    expect(live.stream).toHaveBeenCalledTimes(1);
    expect(
      screen
        .getByText("logs not available: forbidden")
        .closest("[data-log-key]")?.className,
    ).toContain("text-ink-400");
    expect(screen.queryByRole("alert")).toBeNull();
  });
  it("uses all-container prefixes, init selections and a bounded earlier window", async () => {
    const live = connection();
    const earlier = vi
      .spyOn(api, "kubernetesWorkloadLogs")
      .mockResolvedValue({
        pods: ["checkout-a"],
        lines: [
          { pod: "checkout-a", container: "app", at: "2026-10-21T07:00:00Z", text: "yesterday" },
        ],
        truncated: false,
      });
    render(<PodLogViewer resource={resource} />);
    live.emit(membership([firstPod]));
    expect(screen.getByRole("option", { name: "init:setup" })).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Log container"), {
      target: { value: "*" },
    });
    expect(live.stream.mock.calls.at(-1)![4].container).toBe("*");
    live.emit(line());
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "checkout-a/app" }),
      ).toBeTruthy(),
    );
    const calls = live.stream.mock.calls.length;
    fireEvent.change(screen.getByLabelText("Since window"), {
      target: { value: "86400" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Load logs since that window" }));
    await waitFor(() =>
      expect(earlier).toHaveBeenCalledWith(
        "Deployment",
        "shop",
        "checkout",
        // One workload page covers every Pod at once, and the backend refuses
        // more than a thousand lines of it.
        expect.objectContaining({ since_seconds: 86400, tail_lines: POD_LOG_WORKLOAD_EARLIER_TAIL }),
      ),
    );
    await waitFor(() =>
      expect(
        screen.getByLabelText("Workload log output").textContent,
      ).toContain("yesterday"),
    );
    // Loading history never restarts the live follow.
    expect(live.stream.mock.calls.length).toBe(calls);
    expect(
      (screen.getByLabelText("Timestamps") as HTMLInputElement).checked,
    ).toBe(false);
  });
  it("keeps one session when switching compact Overview to full Logs and aborts it on close", async () => {
    const live = connection();
    const onOpenFull = vi.fn();
    const { rerender, unmount } = render(
      <PodLogViewer resource={resource} compact onOpenFull={onOpenFull} />,
    );
    live.emit(membership([firstPod]));
    live.emit(line());
    await waitFor(() =>
      expect(screen.getByText("served request")).toBeTruthy(),
    );
    // The compact card holds the title, the connection word and one container
    // selector; the filter belongs to the full panel.
    expect(screen.queryByLabelText("Log filter")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Open full logs" }));
    expect(onOpenFull).toHaveBeenCalledOnce();
    rerender(
      <PodLogViewer
        resource={resource}
        compact={false}
        onOpenFull={onOpenFull}
      />,
    );
    expect(live.stream).toHaveBeenCalledOnce();
    fireEvent.change(screen.getByLabelText("Log filter"), {
      target: { value: "request" },
    });
    expect(
      (screen.getByLabelText("Log filter") as HTMLInputElement).value,
    ).toBe("request");
    expect(screen.getByText("request", { selector: "mark" })).toBeTruthy();
    unmount();
    expect(live.stream.mock.calls[0][6].aborted).toBe(true);
  });
  it("never follows or reconnects a finite Previous read", async () => {
    vi.useFakeTimers();
    const stream = vi
      .spyOn(api, "kubernetesWorkloadLogStream")
      .mockImplementation(
        async (_kind, _namespace, _name, _uid, _options, emit) => {
          emit({ event: "end", text: "", container: "" });
        },
      );
    render(<PodLogViewer resource={resource} />);
    fireEvent.click(screen.getByLabelText("Previous logs"));
    expect(screen.queryByLabelText("Follow")).toBeNull();
    expect(screen.getByRole("button", { name: "Current", exact: true }).getAttribute("aria-pressed")).toBe("false");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60000);
    });
    expect(stream).toHaveBeenCalledTimes(2);
    expect(stream.mock.calls[1][4].previous).toBe(true);
  });
  it("rotates limits silently and deduplicates uncertainty pulses without hiding later gaps", async () => {
    vi.useFakeTimers();
    const live = connection();
    render(<PodLogViewer resource={resource} compact />);
    live.emit({
      event: "limit",
      text: "",
      container: "",
      reason: "bytes",
      cursor: "aggregate-checkpoint",
    });
    live.finish();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(live.stream).toHaveBeenCalledTimes(2);
    // A workload resumes on a wall clock, never on a cursor.
    expect(live.stream.mock.calls[1][4]).toEqual({
      container: "",
      previous: false,
      timestamps: false,
      tail_lines: 200,
    });
    live.emit({
      event: "heartbeat",
      text: "",
      container: "",
      replay_uncertain: true,
      sequence: 1,
    });
    live.emit({
      event: "heartbeat",
      text: "",
      container: "",
      replay_uncertain: true,
      sequence: 1,
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(50);
    });
    expect(screen.getAllByText(/reconnected at/)).toHaveLength(1);
    live.emit({ event: "heartbeat", text: "", container: "" });
    live.emit({
      event: "heartbeat",
      text: "",
      container: "",
      replay_uncertain: true,
      sequence: 2,
    });
    live.emit({ event: "heartbeat", text: "", container: "" });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(50);
    });
    expect(screen.getAllByText(/reconnected at/)).toHaveLength(2);
    expect(screen.getByLabelText("Workload live logs").textContent).not.toContain("Unavailable");
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByText(/safety limit|Partial coverage/)).toBeNull();
  });
  it("waits for a missing resource identity and does not reconnect revoked access", async () => {
    const stream = vi
      .spyOn(api, "kubernetesWorkloadLogStream")
      .mockRejectedValue(new ApiError(403, "Access denied"));
    const { rerender } = render(
      <PodLogViewer resource={{ ...resource, uid: undefined }} />,
    );
    // A row read from a link has no uid yet; the read waits for it.
    expect(stream).not.toHaveBeenCalled();
    expect(screen.getByLabelText("Workload log output").textContent).toBe(
      "Waiting for the first log line…",
    );
    rerender(<PodLogViewer key="identified" resource={resource} />);
    await waitFor(() =>
      expect(
        screen.getByLabelText("Workload log output").textContent,
      ).toContain("Access denied"),
    );
    expect(stream).toHaveBeenCalledOnce();
  });
});
