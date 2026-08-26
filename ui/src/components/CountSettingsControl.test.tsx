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
import { CountSettingsControl } from "./CountSettingsControl";
import { api, type CountSettings } from "@/lib/api";

// The incident-count window control. The window bounds every count surface, so
// what matters here is that the stored value is what gets selected, that a
// change is sent verbatim, and that saving invalidates the count queries —
// otherwise the setting saves but the numbers on screen stay stale.
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      getCountSettings: vi.fn(),
      updateCountSettings: vi.fn(),
    },
  };
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function renderControl() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return {
    qc,
    ...render(
      <QueryClientProvider client={qc}>
        <ToastProvider>
          <CountSettingsControl />
        </ToastProvider>
      </QueryClientProvider>,
    ),
  };
}

async function select(): Promise<HTMLSelectElement> {
  return (await screen.findByLabelText(
    /count incidents from/i,
  )) as HTMLSelectElement;
}

describe("CountSettingsControl", () => {
  beforeEach(() => {
    vi.mocked(api.updateCountSettings).mockImplementation((s: CountSettings) =>
      Promise.resolve(s),
    );
  });

  it("selects the stored window rather than the default", async () => {
    vi.mocked(api.getCountSettings).mockResolvedValue({ window: "30d" });
    renderControl();
    expect((await select()).value).toBe("30d");
  });

  it("offers every window the server accepts", async () => {
    vi.mocked(api.getCountSettings).mockResolvedValue({ window: "7d" });
    renderControl();
    const options = Array.from((await select()).options).map((o) => o.value);
    expect(options).toEqual(["24h", "7d", "30d", "90d", "all"]);
  });

  it("sends the picked window on save", async () => {
    vi.mocked(api.getCountSettings).mockResolvedValue({ window: "7d" });
    renderControl();

    fireEvent.change(await select(), { target: { value: "24h" } });
    fireEvent.click(screen.getByRole("button", { name: /save/i }));

    await waitFor(() =>
      expect(api.updateCountSettings).toHaveBeenCalledWith({ window: "24h" }),
    );
  });

  // Saving the window changes what every count MEANS, so the cached counts are
  // stale the moment the write lands. Without this the setting saves and the
  // header badge keeps showing the old number until the next refetch.
  it("invalidates the incident count queries after saving", async () => {
    vi.mocked(api.getCountSettings).mockResolvedValue({ window: "7d" });
    const { qc } = renderControl();
    const invalidate = vi.spyOn(qc, "invalidateQueries");

    fireEvent.change(await select(), { target: { value: "90d" } });
    fireEvent.click(screen.getByRole("button", { name: /save/i }));

    await waitFor(() =>
      expect(invalidate).toHaveBeenCalledWith({ queryKey: ["incidents"] }),
    );
  });

  it("surfaces a load failure instead of rendering a wrong window", async () => {
    vi.mocked(api.getCountSettings).mockRejectedValue(new Error("boom"));
    renderControl();
    expect(await screen.findByText(/boom/i)).toBeTruthy();
  });
});
