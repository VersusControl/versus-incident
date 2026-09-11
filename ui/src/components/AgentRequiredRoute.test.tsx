// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { api } from "@/lib/api";
import { AgentRequiredRoute } from "./AgentRequiredRoute";

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, getAgentConfig: vi.fn(), status: vi.fn().mockResolvedValue({ patterns: 0, dirty: false }) } };
});
vi.mock("@/components/TopBar", () => ({ TopBar: ({ title }: { title: string }) => <header>{title}</header> }));

afterEach(cleanup);
beforeEach(() => vi.clearAllMocks());

function show() {
  return render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <MemoryRouter><AgentRequiredRoute title="Services"><div>protected page</div></AgentRequiredRoute></MemoryRouter>
  </QueryClientProvider>);
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
});