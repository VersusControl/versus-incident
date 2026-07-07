// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { IncidentsConfigPanel } from "./IncidentsConfigPage";
import { api, type IncidentsConfig } from "@/lib/api";

// The per-channel config card must NOT surface the template path — that is a
// system/YAML-level setting, not a channel property. Every other channel field
// stays visible.
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      getIncidentsConfig: vi.fn(),
    },
  };
});

afterEach(cleanup);

function config(): IncidentsConfig {
  return {
    name: "versus",
    host: "0.0.0.0",
    port: 3000,
    public_host: "",
    alert: {
      debug_body: false,
      channels: [
        {
          id: "slack",
          name: "Slack",
          enable: true,
          fields: [
            { label: "Channel ID", value: "C123" },
            { label: "Template", value: "config/slack_message.tmpl" },
          ],
        },
      ],
    },
    queue: { enable: false, debug_body: false, providers: [] },
    oncall: {
      enable: false,
      initialized_only: false,
      wait_minutes: 3,
      provider: "",
      aws_incident_manager: {
        response_plan_arn: "",
        other_response_plan_keys: [],
      },
      pagerduty: { routing_key: "", other_routing_keys: [] },
      servicenow: {
        instance_url: "",
        username: "",
        table: "incident",
        other_instance_keys: [],
      },
      incident_io: {
        api_key: "",
        alert_source_config_id: "",
        other_alert_source_config_keys: [],
      },
    },
    storage: { type: "file", file: { max_incidents: 100 } },
  };
}

function renderPanel() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <IncidentsConfigPanel />
    </QueryClientProvider>,
  );
}

describe("IncidentsConfigPanel channel card", () => {
  beforeEach(() => {
    vi.mocked(api.getIncidentsConfig).mockResolvedValue(config());
  });

  it("does not render the channel template path", async () => {
    renderPanel();
    // Wait for the channel to render.
    await screen.findByText("Channel ID");
    expect(screen.queryByText("Template")).toBeNull();
    expect(
      screen.queryByText("config/slack_message.tmpl"),
    ).toBeNull();
  });

  it("keeps other channel fields visible", async () => {
    renderPanel();
    expect(await screen.findByText("Channel ID")).toBeTruthy();
    expect(screen.getByText("C123")).toBeTruthy();
  });
});
