// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  waitFor,
} from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ToastProvider } from "@/components/Toast";
import { AgentChannelsSettingsControl } from "@/components/AgentChannelsSettingsControl";
import type { ChannelSettingsMap, ChannelSettingsView } from "@/lib/api";
import { CHANNELS } from "@/lib/agentChannels";

// Gate the control straight to the admin controls so the channel cards render.
vi.mock("@/lib/useEffectiveRole", () => ({
  useEffectiveRole: () => ({
    org: "acme",
    enterprise: true,
    role: "admin",
    isAdmin: true,
    hasSession: true,
    loading: false,
    session: {},
  }),
}));

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      getChannelSettings: vi.fn(),
      setChannelSettings: vi.fn(),
    },
  };
});

import { api } from "@/lib/api";

const view = (enabled: boolean): ChannelSettingsView => ({
  enabled,
  configured: false,
  source: "yaml",
  yaml_enabled: enabled,
  fields: {},
});

const channelMap = (slackEnabled: boolean): ChannelSettingsMap =>
  Object.fromEntries(
    CHANNELS.map((c) => [c, view(c === "slack" ? slackEnabled : false)]),
  );

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderControl() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  render(
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <AgentChannelsSettingsControl />
      </ToastProvider>
    </QueryClientProvider>,
  );
}

describe("AgentChannelsSettingsControl — prominent enable toggle", () => {
  it("renders the enable switch and reflects the loaded OFF state", async () => {
    vi.mocked(api.getChannelSettings).mockResolvedValue(channelMap(false));
    renderControl();

    const toggle = await screen.findByTestId("channel-enable-toggle-slack");
    expect(toggle.getAttribute("role")).toBe("switch");
    expect(toggle.getAttribute("aria-checked")).toBe("false");
    expect(
      screen.getByTestId("channel-enable-state-slack").textContent,
    ).toBe("Disabled");
  });

  it("reflects the loaded ON state", async () => {
    vi.mocked(api.getChannelSettings).mockResolvedValue(channelMap(true));
    renderControl();

    const toggle = await screen.findByTestId("channel-enable-toggle-slack");
    expect(toggle.getAttribute("aria-checked")).toBe("true");
    expect(
      screen.getByTestId("channel-enable-state-slack").textContent,
    ).toBe("Enabled");
  });

  it("toggles state on click and saves the new enable value", async () => {
    vi.mocked(api.getChannelSettings).mockResolvedValue(channelMap(false));
    vi.mocked(api.setChannelSettings).mockResolvedValue(channelMap(true));
    renderControl();

    const toggle = await screen.findByTestId("channel-enable-toggle-slack");
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-checked")).toBe("true");
    expect(
      screen.getByTestId("channel-enable-state-slack").textContent,
    ).toBe("Enabled");

    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() =>
      expect(api.setChannelSettings).toHaveBeenCalledWith(
        "slack",
        expect.objectContaining({ enable: true }),
      ),
    );
  });
});
