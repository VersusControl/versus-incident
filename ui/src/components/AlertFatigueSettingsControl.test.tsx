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
import { AlertFatigueSettingsControl } from "./AlertFatigueSettingsControl";
import {
  api,
  getSsoSession,
  type AlertFatigueConfig,
  type AlertFatigueCustomChannel,
} from "@/lib/api";

// AlertFatigueSettingsControl is the Admin-page home for all alert-fatigue
// CONFIG. It gates on the caller's effective RBAC role (SSO deployment probe +
// session whoami), then reads/writes the config, custom channel, correlation,
// and dependency endpoints. Defaults fail closed (no session, community).
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    getSsoSession: vi
      .fn()
      .mockRejectedValue(new actual.ApiError(401, "no session")),
    api: {
      ...actual.api,
      getSSODeployment: vi
        .fn()
        .mockRejectedValue(new actual.ApiError(403, "community")),
      getAlertFatigueConfig: vi.fn(),
      setAlertFatigueConfig: vi.fn(),
      getAlertFatigueCustomChannel: vi
        .fn()
        .mockResolvedValue({ configured: false }),
      setAlertFatigueCustomChannel: vi.fn(),
      deleteAlertFatigueCustomChannel: vi.fn(),
      getAlertFatigueCorrelation: vi.fn().mockResolvedValue({
        correlation_enabled: false,
        correlation_window_seconds: 0,
        effective_window_seconds: 300,
      }),
      setAlertFatigueCorrelation: vi.fn(),
      listAlertFatigueCorrelationGroups: vi
        .fn()
        .mockResolvedValue({ groups: [], total: 0, page: 1, page_size: 50 }),
      listAlertFatigueCorrelationMembers: vi
        .fn()
        .mockResolvedValue({ group_id: 0, members: [] }),
      getAlertFatigueDependency: vi.fn().mockResolvedValue({
        dependency_suppress_enabled: false,
        dependency_lookback_seconds: 0,
        effective_lookback_seconds: 3600,
      }),
      setAlertFatigueDependency: vi.fn(),
      listAlertFatigueDependencyEdges: vi
        .fn()
        .mockResolvedValue({ edges: [], total: 0, page: 1, page_size: 50 }),
      addAlertFatigueDependencyEdge: vi.fn(),
      removeAlertFatigueDependencyEdge: vi.fn(),
    },
  };
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const NOW = new Date().toISOString();

function cfg(over: Partial<AlertFatigueConfig> = {}): AlertFatigueConfig {
  return { enabled: false, pending_review: false, ...over };
}

function signInAs(role: string) {
  vi.mocked(api.getSSODeployment).mockResolvedValue({ org: "acme" });
  vi.mocked(getSsoSession).mockResolvedValue({
    org: "acme",
    email: "a@acme.test",
    subject: "sub-1",
    mfa: false,
    role,
    issued_at: NOW,
    expires_at: NOW,
  });
}

function renderControl() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <AlertFatigueSettingsControl />
      </ToastProvider>
    </QueryClientProvider>,
  );
}

describe("AlertFatigueSettingsControl — role gating (fail closed)", () => {
  it("shows the Enterprise upsell and reads no config on a community binary", async () => {
    renderControl();
    expect(await screen.findByTestId("enterprise-locked")).toBeTruthy();
    expect(api.getAlertFatigueConfig).not.toHaveBeenCalled();
  });

  it("shows the read-only admin notice for a signed-in viewer and reads no config", async () => {
    signInAs("viewer");
    renderControl();
    const notice = await screen.findByTestId("admin-access-notice");
    expect(notice.getAttribute("data-reason")).toBe("role");
    expect(api.getAlertFatigueConfig).not.toHaveBeenCalled();
  });
});

describe("AlertFatigueSettingsControl — moved config renders + saves (manager)", () => {
  beforeEach(() => {
    signInAs("admin");
    vi.mocked(api.getAlertFatigueConfig).mockResolvedValue(cfg());
    vi.mocked(api.setAlertFatigueConfig).mockImplementation((c) =>
      Promise.resolve(c),
    );
    vi.mocked(api.setAlertFatigueCorrelation).mockImplementation((b) =>
      Promise.resolve({ ...b, effective_window_seconds: 300 }),
    );
    vi.mocked(api.setAlertFatigueDependency).mockImplementation((b) =>
      Promise.resolve({ ...b, effective_lookback_seconds: 3600 }),
    );
  });

  it("renders the fatigue-channel form, correlation, and dependency config", async () => {
    renderControl();
    expect(await screen.findByTestId("alert-fatigue-custom-channel")).toBeTruthy();
    expect(await screen.findByTestId("alert-fatigue-send-toggle")).toBeTruthy();
    expect(await screen.findByTestId("alert-fatigue-correlation")).toBeTruthy();
    expect(await screen.findByTestId("alert-fatigue-dependency")).toBeTruthy();
    // The two config sections use the plain-language headings.
    expect(await screen.findByText("Alert grouping")).toBeTruthy();
    expect(await screen.findByText("Downstream suppression")).toBeTruthy();
    // The old named-default picker is gone.
    expect(screen.queryByTestId("alert-fatigue-channel-select")).toBeNull();
  });

  it("PUTs correlation_enabled=true when the correlation toggle is turned on", async () => {
    renderControl();
    const toggle = await screen.findByTestId("alert-fatigue-correlation-toggle");
    fireEvent.click(toggle);
    await waitFor(() =>
      expect(api.setAlertFatigueCorrelation).toHaveBeenCalledWith(
        expect.objectContaining({ correlation_enabled: true }),
      ),
    );
  });

  it("PUTs dependency_suppress_enabled=true when the dependency toggle is turned on", async () => {
    renderControl();
    const toggle = await screen.findByTestId("alert-fatigue-dependency-toggle");
    fireEvent.click(toggle);
    await waitFor(() =>
      expect(api.setAlertFatigueDependency).toHaveBeenCalledWith(
        expect.objectContaining({ dependency_suppress_enabled: true }),
      ),
    );
  });
});

describe("AlertFatigueSettingsControl — custom fatigue channel form", () => {
  beforeEach(() => {
    signInAs("admin");
    vi.mocked(api.getAlertFatigueConfig).mockResolvedValue(cfg());
    // Default to an UNCONFIGURED custom channel; the configured cases override
    // this explicitly (mock implementations persist across clearAllMocks).
    vi.mocked(api.getAlertFatigueCustomChannel).mockResolvedValue({
      configured: false,
    });
    vi.mocked(api.setAlertFatigueCustomChannel).mockResolvedValue({
      configured: true,
      channel_type: "slack",
      enabled: true,
      fields: {
        token: { set: true, hint: "••ab" },
        channel_id: { set: true, hint: "C123" },
      },
    });
    vi.mocked(api.deleteAlertFatigueCustomChannel).mockResolvedValue({
      cleared: true,
    });
  });

  it("suppresses by default: shows the toggle OFF with the suppression note and no form", async () => {
    // Unconfigured channel → the toggle is OFF, the form is hidden, and the
    // suppression note explains that fatigued alerts are dropped.
    renderControl();
    const toggle = await screen.findByTestId("alert-fatigue-send-toggle");
    expect(toggle.getAttribute("aria-checked")).toBe("false");
    expect(screen.getByTestId("alert-fatigue-suppress-note").textContent).toContain(
      "silently suppressed",
    );
    expect(screen.queryByTestId("alert-fatigue-custom-save")).toBeNull();
    expect(screen.queryByTestId("alert-fatigue-custom-type")).toBeNull();
  });

  it("reveals the channel form when the send toggle is turned on", async () => {
    renderControl();
    const toggle = await screen.findByTestId("alert-fatigue-send-toggle");
    expect(screen.queryByTestId("alert-fatigue-custom-type")).toBeNull();
    fireEvent.click(toggle);
    expect(await screen.findByTestId("alert-fatigue-custom-type")).toBeTruthy();
    expect(screen.getByTestId("alert-fatigue-custom-save")).toBeTruthy();
  });

  it("selects the channel type from tabs, switching the visible fields", async () => {
    renderControl();
    fireEvent.click(await screen.findByTestId("alert-fatigue-send-toggle"));

    // The type selector is a tablist of role=tab buttons (was a <select>).
    const tablist = await screen.findByTestId("alert-fatigue-custom-type");
    const tabs = tablist.querySelectorAll('[role="tab"]');
    expect(tabs.length).toBeGreaterThan(1);

    // Slack is the default active tab; its fields are shown.
    expect(screen.getByTestId("alert-fatigue-custom-field-token")).toBeTruthy();

    // Switching to Telegram swaps the fields to that type's schema.
    fireEvent.click(screen.getByRole("tab", { name: /Telegram/i }));
    expect(
      await screen.findByTestId("alert-fatigue-custom-field-bot_token"),
    ).toBeTruthy();
    expect(screen.getByTestId("alert-fatigue-custom-field-chat_id")).toBeTruthy();
    expect(
      screen.queryByTestId("alert-fatigue-custom-field-channel_id"),
    ).toBeNull();
  });

  it("renders 'Use proxy' on its own line (not in the grid) and reveals the proxy reference when checked", async () => {
    renderControl();
    fireEvent.click(await screen.findByTestId("alert-fatigue-send-toggle"));

    // Slack has no use_proxy field → no proxy toggle at all.
    await screen.findByTestId("alert-fatigue-custom-field-token");
    expect(
      screen.queryByTestId("alert-fatigue-custom-field-use_proxy"),
    ).toBeNull();

    // Telegram is proxy-capable → the toggle renders, pulled OUT of the grid.
    fireEvent.click(screen.getByRole("tab", { name: /Telegram/i }));
    const proxy = (await screen.findByTestId(
      "alert-fatigue-custom-field-use_proxy",
    )) as HTMLInputElement;
    expect(proxy.type).toBe("checkbox");

    // It lives on its own line below the field grid, not inside sm:grid-cols-2.
    const grid = document.querySelector(".grid.sm\\:grid-cols-2");
    expect(grid).toBeTruthy();
    expect(grid?.contains(proxy)).toBe(false);

    // Off by default → no proxy reference. Turning it on reveals the read-only
    // shared-proxy reference.
    expect(document.body.textContent).not.toContain(
      "sends through the server's shared proxy",
    );
    fireEvent.click(proxy);
    expect(proxy.checked).toBe(true);
    await waitFor(() =>
      expect(document.body.textContent).toContain(
        "sends through the server's shared proxy",
      ),
    );
  });

  it("sets a fresh custom channel with the typed fields (secret included on write)", async () => {
    renderControl();

    // Suppressed by default → open the form first.
    fireEvent.click(await screen.findByTestId("alert-fatigue-send-toggle"));

    // Slack is the default type; fill its fields.
    const token = (await screen.findByTestId(
      "alert-fatigue-custom-field-token",
    )) as HTMLInputElement;
    const channelId = screen.getByTestId(
      "alert-fatigue-custom-field-channel_id",
    ) as HTMLInputElement;
    // The secret field is masked (password), never a text input.
    expect(token.type).toBe("password");

    fireEvent.change(token, { target: { value: "xoxb-secret" } });
    fireEvent.change(channelId, { target: { value: "C999" } });
    fireEvent.click(screen.getByTestId("alert-fatigue-custom-save"));

    await waitFor(() =>
      expect(api.setAlertFatigueCustomChannel).toHaveBeenCalledWith(
        expect.objectContaining({
          channel_type: "slack",
          enabled: true,
          config: expect.objectContaining({
            token: "xoxb-secret",
            channel_id: "C999",
          }),
        }),
      ),
    );
  });

  it("masks a stored secret and does NOT wipe it when saved without a new value", async () => {
    const stored: AlertFatigueCustomChannel = {
      configured: true,
      channel_type: "slack",
      enabled: true,
      fields: {
        token: { set: true, hint: "••ab" },
        channel_id: { set: true, hint: "C123" },
      },
    };
    vi.mocked(api.getAlertFatigueCustomChannel).mockResolvedValue(stored);
    renderControl();

    const token = (await screen.findByTestId(
      "alert-fatigue-custom-field-token",
    )) as HTMLInputElement;
    // The masked secret is NEVER rendered as a value — the input starts blank
    // and only the server hint is shown.
    expect(token.value).toBe("");
    expect(screen.getByText(/Set \(••ab\)/)).toBeTruthy();
    // The raw secret is never in the DOM.
    expect(document.body.textContent).not.toContain("xoxb");

    // Save without touching the token: the write OMITS token so the server
    // preserves the stored secret, but still sends the non-secret channel_id.
    fireEvent.click(screen.getByTestId("alert-fatigue-custom-save"));
    await waitFor(() =>
      expect(api.setAlertFatigueCustomChannel).toHaveBeenCalled(),
    );
    const body = vi.mocked(api.setAlertFatigueCustomChannel).mock.calls[0][0];
    expect(body.config.token).toBeUndefined();
    expect(body.config.channel_id).toBe("C123");
  });

  it("clears the fatigue channel when Remove is clicked", async () => {
    vi.mocked(api.getAlertFatigueCustomChannel).mockResolvedValue({
      configured: true,
      channel_type: "slack",
      enabled: true,
      fields: {
        token: { set: true, hint: "••ab" },
        channel_id: { set: true, hint: "C123" },
      },
    });
    renderControl();

    fireEvent.click(await screen.findByTestId("alert-fatigue-custom-clear"));
    await waitFor(() =>
      expect(api.deleteAlertFatigueCustomChannel).toHaveBeenCalled(),
    );
  });

  it("stops sending (PUT enabled:false) when the send toggle is turned off on a configured channel", async () => {
    vi.mocked(api.getAlertFatigueCustomChannel).mockResolvedValue({
      configured: true,
      channel_type: "slack",
      enabled: true,
      fields: {
        token: { set: true, hint: "••ab" },
        channel_id: { set: true, hint: "C123" },
      },
    });
    renderControl();

    const toggle = await screen.findByTestId("alert-fatigue-send-toggle");
    expect(toggle.getAttribute("aria-checked")).toBe("true");
    fireEvent.click(toggle);
    await waitFor(() =>
      expect(api.setAlertFatigueCustomChannel).toHaveBeenCalledWith(
        expect.objectContaining({ channel_type: "slack", enabled: false }),
      ),
    );
    // Turning off preserves secrets — the disable PUT sends an empty config.
    const body = vi.mocked(api.setAlertFatigueCustomChannel).mock.calls[0][0];
    expect(body.config).toEqual({});
  });

  it("disables Save until required fields are satisfiable on a fresh channel", async () => {
    renderControl();
    // Open the form first (suppressed by default).
    fireEvent.click(await screen.findByTestId("alert-fatigue-send-toggle"));
    const save = (await screen.findByTestId(
      "alert-fatigue-custom-save",
    )) as HTMLButtonElement;
    // Fresh slack: token + channel_id required, both blank → Save disabled.
    expect(save.disabled).toBe(true);

    fireEvent.change(screen.getByTestId("alert-fatigue-custom-field-token"), {
      target: { value: "xoxb" },
    });
    fireEvent.change(
      screen.getByTestId("alert-fatigue-custom-field-channel_id"),
      { target: { value: "C1" } },
    );
    expect(save.disabled).toBe(false);
  });

  it("preserves in-progress edits across a background refetch (no re-seed)", async () => {
    const view: AlertFatigueCustomChannel = {
      configured: true,
      channel_type: "slack",
      enabled: true,
      fields: {
        token: { set: true, hint: "••ab" },
        channel_id: { set: true, hint: "C123" },
      },
    };
    vi.mocked(api.getAlertFatigueCustomChannel).mockResolvedValue(view);

    // Own QueryClient so the test can drive a background refetch directly.
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={qc}>
        <ToastProvider>
          <AlertFatigueSettingsControl />
        </ToastProvider>
      </QueryClientProvider>,
    );

    const channelId = (await screen.findByTestId(
      "alert-fatigue-custom-field-channel_id",
    )) as HTMLInputElement;
    // The form seeds channel_id from the stored hint on first load.
    expect(channelId.value).toBe("C123");

    // The operator starts typing a new channel id.
    fireEvent.change(channelId, { target: { value: "C-EDITED" } });
    expect(channelId.value).toBe("C-EDITED");

    // A background refetch (window refocus / invalidation) returns an equivalent
    // view with the SAME stored type but a fresh object identity.
    vi.mocked(api.getAlertFatigueCustomChannel).mockResolvedValue({
      ...view,
      fields: { ...view.fields },
    });
    await qc.refetchQueries({ queryKey: ["alert-fatigue-custom-channel"] });

    // The in-progress edit survives — the form no longer re-seeds on every view.
    await waitFor(() =>
      expect(
        (
          screen.getByTestId(
            "alert-fatigue-custom-field-channel_id",
          ) as HTMLInputElement
        ).value,
      ).toBe("C-EDITED"),
    );
    // The masked-secret "Set" hint still tracks the stored view (render-driven).
    expect(screen.getByText(/Set \(••ab\)/)).toBeTruthy();
  });
});
