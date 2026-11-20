// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { api, ApiError } from "@/lib/api";
import { AgentRequiredRoute } from "./AgentRequiredRoute";

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      getAgentConfig: vi.fn(),
      // An OSS binary: no deployment org, so the caller's role is settled at once.
      getSSODeployment: vi.fn().mockRejectedValue(new actual.ApiError(404, "not found")),
      status: vi.fn().mockResolvedValue({ patterns: 0, dirty: false }),
    },
  };
});
vi.mock("@/components/TopBar", () => ({ TopBar: ({ title }: { title: string }) => <header>{title}</header> }));

afterEach(cleanup);
beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(api.getSSODeployment).mockRejectedValue(new ApiError(404, "not found"));
});

function show(page: React.ReactNode = <div>protected page</div>) {
  return render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <MemoryRouter><AgentRequiredRoute title="Services">{page}</AgentRequiredRoute></MemoryRouter>
  </QueryClientProvider>);
}

/** A page that reads the same agent probe, as the shell's top bar does. */
function ProbeReader() {
  useQuery({ queryKey: ["agent-config"], queryFn: api.getAgentConfig, retry: false });
  return <div>protected page</div>;
}

describe("AgentRequiredRoute", () => {
  it("shows actionable guidance instead of mounting a disabled page", async () => {
    vi.mocked(api.getAgentConfig).mockResolvedValue({ enable: false });
    show();
    expect(await screen.findByRole("heading", { name: "AI Agent is disabled" })).toBeTruthy();
    expect(screen.getByText(/AGENT_ENABLE=true/)).toBeTruthy();
    expect(screen.queryByText("protected page")).toBeNull();
  });

  it("checks again and mounts the page after enablement", async () => {
    vi.mocked(api.getAgentConfig).mockResolvedValueOnce({ enable: false }).mockResolvedValueOnce({ enable: true });
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Check again" }));
    expect(await screen.findByText("protected page")).toBeTruthy();
    expect(api.getAgentConfig).toHaveBeenCalledTimes(2);
  });

  it("mounts the page when enabled or when the config probe itself fails", async () => {
    vi.mocked(api.getAgentConfig).mockResolvedValueOnce({ enable: true });
    const first = show();
    expect(await screen.findByText("protected page")).toBeTruthy();
    first.unmount();

    vi.mocked(api.getAgentConfig).mockRejectedValueOnce(new Error("config unavailable"));
    show();
    await waitFor(() => expect(screen.getByText("protected page")).toBeTruthy());
    expect(screen.queryByRole("heading", { name: "AI Agent is disabled" })).toBeNull();
  });

  it("mounts the page on the reduced agent answer a finance caller gets ({enable, mode})", async () => {
    vi.mocked(api.getAgentConfig).mockResolvedValue({ enable: true, mode: "training" } as Awaited<ReturnType<typeof api.getAgentConfig>>);
    show();
    expect(await screen.findByText("protected page")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("takes a 403 on the agent probe as final: the page mounts and the probe is not polled", async () => {
    vi.mocked(api.getAgentConfig).mockRejectedValue(new ApiError(403, "insufficient permissions", { error: "insufficient permissions", code: "finops_only" }));
    show(<ProbeReader />);
    expect(await screen.findByText("protected page")).toBeTruthy();
    await waitFor(() => expect(vi.mocked(api.getAgentConfig).mock.calls.length).toBeGreaterThan(0));
    await act(async () => {
      await new Promise((r) => setTimeout(r, 200));
    });
    const settled = vi.mocked(api.getAgentConfig).mock.calls.length;
    // Without the fix the reader re-asked, the gate fell back to its spinner,
    // and the pair looped about once a second.
    await act(async () => {
      await new Promise((r) => setTimeout(r, 1500));
    });
    expect(screen.getByText("protected page")).toBeTruthy();
    expect(screen.queryByText(/Checking AI Agent status/)).toBeNull();
    expect(vi.mocked(api.getAgentConfig).mock.calls.length).toBe(settled);
  });
});
