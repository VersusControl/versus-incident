// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  waitFor,
} from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ToastProvider } from "@/components/Toast";
import { ReportSettingsControl } from "./ReportSettingsControl";
import { api, type Capabilities, type ReportSettings } from "@/lib/api";

// The scheduled-delivery group: the timezone control maps UTC<->Local to the
// correct STORED value, and the time/zone inputs disable when the schedule is
// off. The browser zone is pinned so the Local option is deterministic.
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      getReportSettings: vi.fn(),
      updateReportSettings: vi.fn(),
      capabilities: vi.fn(),
    },
  };
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function settings(over: Partial<ReportSettings> = {}): ReportSettings {
  return {
    enable: true,
    default_channel: "",
    include_chart: true,
    rate_per_minute: 0,
    default_window: "24h",
    schedule_enabled: true,
    send_time: "17:00",
    timezone: "UTC",
    ...over,
  };
}

const caps: Capabilities = { search: false };

function renderControl() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <ReportSettingsControl />
      </ToastProvider>
    </QueryClientProvider>,
  );
}

describe("ReportSettingsControl — scheduled delivery", () => {
  beforeEach(() => {
    // Pin the browser zone so "Local time" is deterministic.
    vi.spyOn(Intl, "DateTimeFormat").mockReturnValue({
      resolvedOptions: () => ({ timeZone: "Asia/Ho_Chi_Minh" }),
    } as unknown as Intl.DateTimeFormat);
    vi.mocked(api.capabilities).mockResolvedValue(caps);
    vi.mocked(api.updateReportSettings).mockImplementation((s) =>
      Promise.resolve(s),
    );
  });

  it("selects UTC and shows the detected local zone as the Local option", async () => {
    vi.mocked(api.getReportSettings).mockResolvedValue(settings());
    renderControl();

    const utc = (await screen.findByLabelText("UTC")) as HTMLInputElement;
    const local = screen.getByLabelText(/Local time/) as HTMLInputElement;
    expect(utc.checked).toBe(true);
    expect(local.checked).toBe(false);
    // The Local option surfaces the detected browser zone.
    expect(screen.getByText("(Asia/Ho_Chi_Minh)")).toBeTruthy();
  });

  it("stores the detected IANA zone when Local time is picked", async () => {
    vi.mocked(api.getReportSettings).mockResolvedValue(settings());
    renderControl();

    const local = (await screen.findByLabelText(
      /Local time/,
    )) as HTMLInputElement;
    fireEvent.click(local);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(api.updateReportSettings).toHaveBeenCalled());
    expect(vi.mocked(api.updateReportSettings).mock.calls[0][0]).toMatchObject({
      timezone: "Asia/Ho_Chi_Minh",
    });
  });

  it("stores 'UTC' when UTC is picked from a stored local zone", async () => {
    vi.mocked(api.getReportSettings).mockResolvedValue(
      settings({ timezone: "Asia/Ho_Chi_Minh" }),
    );
    renderControl();

    // Loads with Local selected, showing the stored IANA name.
    const local = (await screen.findByLabelText(
      /Local time/,
    )) as HTMLInputElement;
    expect(local.checked).toBe(true);

    fireEvent.click(screen.getByLabelText("UTC"));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(api.updateReportSettings).toHaveBeenCalled());
    expect(vi.mocked(api.updateReportSettings).mock.calls[0][0]).toMatchObject({
      timezone: "UTC",
    });
  });

  it("disables the time + zone inputs when the schedule is off", async () => {
    vi.mocked(api.getReportSettings).mockResolvedValue(
      settings({ schedule_enabled: false }),
    );
    renderControl();

    const time = (await screen.findByLabelText(
      "Send time",
    )) as HTMLInputElement;
    expect(time.disabled).toBe(true);
    // The timezone radios sit inside a disabled <fieldset>.
    const group = screen.getByRole("group", {
      name: "Time zone",
    }) as HTMLFieldSetElement;
    expect(group.disabled).toBe(true);
  });

  it("hints that the schedule is inactive while reports are disabled", async () => {
    vi.mocked(api.getReportSettings).mockResolvedValue(
      settings({ enable: false, schedule_enabled: true }),
    );
    renderControl();

    expect(
      await screen.findByText(/inactive until the incidents report is enabled/),
    ).toBeTruthy();
  });
});
