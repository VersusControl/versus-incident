// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, type KubernetesIssueAlertsSettings } from "@/lib/api";
import { KubernetesClusterContext, kubernetesKey } from "@/lib/useKubernetesCluster";
import { ToastCtx } from "@/components/toastContext";
import { KubernetesIssueAlerts } from "@/pages/KubernetesIssueAlerts";

const defaults: KubernetesIssueAlertsSettings = {
  enabled: false, level: "critical", can_write: true, available: true,
  reason: "", links_ready: true, channels_enabled: true,
};
const push = vi.fn();
let stored: KubernetesIssueAlertsSettings;

function renderControl(settings: Partial<KubernetesIssueAlertsSettings> = {}, cluster = "east") {
  stored = { ...defaults, ...settings };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ToastCtx.Provider value={{ push }}>
        <KubernetesClusterContext.Provider value={{ multiple: true, cluster, clusters: [], client: api, key: (...parts) => kubernetesKey(cluster, ...parts) }}>
          <KubernetesIssueAlerts />
        </KubernetesClusterContext.Provider>
      </ToastCtx.Provider>
    </QueryClientProvider>,
  );
  return client;
}

beforeEach(() => {
  push.mockReset();
  vi.spyOn(api, "kubernetesIssueAlerts").mockImplementation(async () => stored);
  vi.spyOn(api, "kubernetesSaveIssueAlerts").mockImplementation(async (input) => {
    stored = { ...stored, ...input };
    return stored;
  });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

describe("Kubernetes issue alerts", () => {
  it("starts off, opens the channel docs safely and saves the level immediately", async () => {
    renderControl();
    const toggle = await screen.findByRole("switch", { name: "Alert on new issues" });
    expect(toggle.getAttribute("aria-checked")).toBe("false");
    expect(screen.queryByRole("group", { name: "Issue alert level" })).toBeNull();
    expect(screen.getByText("Sends through your alert channels")).toBeTruthy();
    const link = screen.getByRole("link", { name: "Channel settings" });
    expect(link.getAttribute("href")).toBe("https://docs.versusincident.com/#/agent/channels");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    expect(screen.queryByText(/last alert/i)).toBeNull();
    fireEvent.click(toggle);
    await screen.findByRole("button", { name: "Critical" });
    await waitFor(() => expect(push).toHaveBeenCalledWith(expect.objectContaining({ title: "Alerts on. Issues that break from now on will alert." })));
    await waitFor(() => expect(screen.getByRole("button", { name: "Warning" }).hasAttribute("disabled")).toBe(false));
    fireEvent.click(screen.getByRole("button", { name: "Warning" }));
    await waitFor(() => expect(api.kubernetesSaveIssueAlerts).toHaveBeenLastCalledWith({ enabled: true, level: "warning" }));
    await screen.findByText("Warning sends warning and critical issues");
    expect(screen.getByRole("button", { name: "Warning" }).getAttribute("aria-pressed")).toBe("true");
    await waitFor(() => expect(toggle.hasAttribute("disabled")).toBe(false));
    fireEvent.click(toggle);
    await waitFor(() => expect(api.kubernetesSaveIssueAlerts).toHaveBeenLastCalledWith({ enabled: false, level: "warning" }));
    await waitFor(() => expect(screen.queryByRole("group", { name: "Issue alert level" })).toBeNull());
  });

  it.each([
    [{ can_write: false }, "Ask an admin to change alerts"],
    [{ available: false, reason: "demand_capture" }, "Alerts need continuous change capture"],
  ] as const)("disables inputs with an explanation: %s", async (settings, reason) => {
    renderControl({ ...settings, enabled: true });
    const toggle = await screen.findByRole("switch");
    expect(toggle.hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Critical" }).hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Warning" }).hasAttribute("disabled")).toBe(true);
    expect(screen.getByText(reason)).toBeTruthy();
    fireEvent.click(toggle);
    expect(api.kubernetesSaveIssueAlerts).not.toHaveBeenCalled();
  });

  it("shows channel and public-host tips without disabling valid writes", async () => {
    renderControl({ channels_enabled: false, links_ready: false });
    expect((await screen.findByRole("switch")).hasAttribute("disabled")).toBe(false);
    expect(screen.getByText("No alert channel is on")).toBeTruthy();
    expect(screen.getByText("Set public_host to add Timeline and agent links")).toBeTruthy();
    expect(screen.getAllByRole("link", { name: "Channel settings" })).toHaveLength(1);
  });

  it.each(["switch", "level"] as const)("optimistically updates %s, serializes writes and rolls back on failure", async (change) => {
    let rejectSave!: (error: Error) => void;
    vi.mocked(api.kubernetesSaveIssueAlerts).mockImplementation(() => new Promise((_resolve, reject) => { rejectSave = reject; }));
    const client = renderControl({ enabled: change === "level" });
    client.setQueryData(kubernetesKey("west", "issue-alerts"), { ...defaults, level: "warning" });
    await screen.findByRole("switch");
    fireEvent.click(change === "switch" ? screen.getByRole("switch") : screen.getByRole("button", { name: "Warning" }));
    await waitFor(() => expect(api.kubernetesSaveIssueAlerts).toHaveBeenCalledTimes(1));
    expect(screen.getByRole("switch").getAttribute("aria-checked")).toBe("true");
    expect(screen.getByRole("button", { name: change === "switch" ? "Critical" : "Warning" }).getAttribute("aria-pressed")).toBe("true");
    expect(screen.getByRole("switch").hasAttribute("disabled")).toBe(true);
    fireEvent.click(screen.getByRole("switch"));
    expect(api.kubernetesSaveIssueAlerts).toHaveBeenCalledTimes(1);
    await act(async () => rejectSave(new Error("Save refused")));
    await waitFor(() => expect(push).toHaveBeenCalledWith(expect.objectContaining({ tone: "error", description: "Save refused" })));
    expect(screen.getByRole("switch").getAttribute("aria-checked")).toBe(String(change === "level"));
    if (change === "level") expect(screen.getByRole("button", { name: "Critical" }).getAttribute("aria-pressed")).toBe("true");
    expect(client.getQueryData(kubernetesKey("west", "issue-alerts"))).toEqual({ ...defaults, level: "warning" });
  });

  it("fails closed when settings cannot be loaded and supports retry", async () => {
    vi.mocked(api.kubernetesIssueAlerts).mockRejectedValueOnce(new Error("Unavailable"));
    renderControl();
    const retry = await screen.findByRole("button", { name: "Retry alert settings" });
    expect(screen.queryByRole("switch")).toBeNull();
    fireEvent.click(retry);
    await screen.findByRole("switch");
  });
});
