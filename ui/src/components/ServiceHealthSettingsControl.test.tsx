// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api, ApiError } from "@/lib/api";
import { ToastProvider } from "@/components/Toast";
import { ServiceHealthSettingsControl } from "./ServiceHealthSettingsControl";

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, getServiceHealthSettings: vi.fn(), updateServiceHealthSettings: vi.fn() } };
});

const role = vi.hoisted(() => ({ enterprise: false, isAdmin: false, hasSession: false, loading: false }));
vi.mock("@/lib/useEffectiveRole", () => ({
  useEffectiveRole: () => ({ ...role, org: null, role: null, session: {} }),
}));

afterEach(cleanup);

function renderControl() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><ServiceHealthSettingsControl /></ToastProvider></QueryClientProvider>);
}

describe("ServiceHealthSettingsControl", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    Object.assign(role, { enterprise: false, isAdmin: false, hasSession: false, loading: false });
    vi.mocked(api.getServiceHealthSettings).mockResolvedValue({ interval_seconds: 60, window_seconds: 300, revision: 4 });
    vi.mocked(api.updateServiceHealthSettings).mockResolvedValue({ interval_seconds: 90, window_seconds: 600, revision: 5 });
  });

  it("saves valid interval and window values with the current revision", async () => {
    renderControl();
    const inputs = await screen.findAllByRole("spinbutton");
    fireEvent.change(inputs[0], { target: { value: "90" } });
    fireEvent.change(inputs[1], { target: { value: "600" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(api.updateServiceHealthSettings).toHaveBeenCalledTimes(1));
    expect(vi.mocked(api.updateServiceHealthSettings).mock.calls[0][0]).toEqual({ interval_seconds: 90, window_seconds: 600, revision: 4 });
    expect(await screen.findByText("Service Health Timing saved")).toBeTruthy();
  });

  it("blocks invalid bounds and a window shorter than the interval", async () => {
    renderControl();
    const inputs = await screen.findAllByRole("spinbutton");
    fireEvent.change(inputs[0], { target: { value: "901" } });
    fireEvent.change(inputs[1], { target: { value: "60" } });
    expect(screen.getByText("Use 30 to 900 seconds.")).toBeTruthy();
    expect(screen.getByText("Window must be at least as long as the interval.")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("surfaces conflict recovery and reloads effective values", async () => {
    vi.mocked(api.updateServiceHealthSettings).mockRejectedValue(new ApiError(409, "conflict"));
    renderControl();
    fireEvent.change((await screen.findAllByRole("spinbutton"))[0], { target: { value: "90" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect((await screen.findByRole("alert")).textContent).toMatch(/changed in another session/i);
    fireEvent.click(screen.getByRole("button", { name: "Reload values" }));
    await waitFor(() => expect(api.getServiceHealthSettings).toHaveBeenCalledTimes(2));
  });

  it("shows effective values and administrator guidance without edit controls for read-only users", async () => {
    Object.assign(role, { enterprise: true, hasSession: true, isAdmin: false });
    renderControl();
    expect(await screen.findByText("60 seconds")).toBeTruthy();
    expect(screen.getByText("300 seconds")).toBeTruthy();
    expect(screen.getByText("Requires the admin role")).toBeTruthy();
    expect(screen.queryByRole("spinbutton")).toBeNull();
  });

  it("renders load errors and non-conflict save errors", async () => {
    vi.mocked(api.getServiceHealthSettings).mockRejectedValueOnce(new Error("settings unavailable"));
    const { unmount } = renderControl();
    expect(await screen.findByText("settings unavailable")).toBeTruthy();
    unmount();

    vi.mocked(api.getServiceHealthSettings).mockResolvedValue({ interval_seconds: 60, window_seconds: 300, revision: 4 });
    vi.mocked(api.updateServiceHealthSettings).mockRejectedValue(new ApiError(503, "settings unavailable"));
    renderControl();
    fireEvent.change((await screen.findAllByRole("spinbutton"))[0], { target: { value: "90" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect((await screen.findByRole("alert")).textContent).toContain("settings unavailable");
  });
});