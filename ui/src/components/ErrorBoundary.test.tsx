// @vitest-environment jsdom
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import {
  QueryClient,
  QueryClientProvider,
  useQuery,
} from "@tanstack/react-query";
import { ErrorBoundary, QueryErrorBoundary } from "./ErrorBoundary";

afterEach(cleanup);

// React logs the caught error to console.error by design; silence it so the
// suite output stays readable (the boundary's own log is asserted instead).
beforeEach(() => {
  vi.spyOn(console, "error").mockImplementation(() => {});
});

afterEach(() => {
  vi.restoreAllMocks();
});

function Boom({ explode }: { explode: boolean }) {
  if (explode) throw new Error("cadence is not defined");
  return <div data-testid="child">page content</div>;
}

describe("ErrorBoundary", () => {
  it("renders a contained panel instead of a blank tree when a child throws", () => {
    render(
      <div>
        <nav data-testid="shell">sidebar</nav>
        <ErrorBoundary>
          <Boom explode />
        </ErrorBoundary>
      </div>,
    );

    expect(screen.getByTestId("error-boundary")).toBeTruthy();
    expect(screen.getByRole("alert").textContent).toContain(
      "Something went wrong on this page",
    );
    expect(screen.getByTestId("error-boundary-message").textContent).toBe(
      "cadence is not defined",
    );
    expect(screen.queryByTestId("child")).toBeNull();
    // The shell outside the boundary survives, so the user can navigate away.
    expect(screen.getByTestId("shell")).toBeTruthy();
    expect(console.error).toHaveBeenCalled();
  });

  it("renders the custom context lead-in when given one", () => {
    render(
      <ErrorBoundary context="Couldn't render this page">
        <Boom explode />
      </ErrorBoundary>,
    );
    expect(screen.getByRole("alert").textContent).toContain(
      "Couldn't render this page",
    );
  });

  it("re-renders the child when Try again is pressed", () => {
    function Harness() {
      const [explode, setExplode] = useState(true);
      return (
        <>
          <button onClick={() => setExplode(false)}>fix</button>
          <ErrorBoundary>
            <Boom explode={explode} />
          </ErrorBoundary>
        </>
      );
    }
    render(<Harness />);

    expect(screen.getByTestId("error-boundary")).toBeTruthy();
    fireEvent.click(screen.getByText("fix"));
    fireEvent.click(screen.getByTestId("error-boundary-retry"));

    expect(screen.getByTestId("child")).toBeTruthy();
    expect(screen.queryByTestId("error-boundary")).toBeNull();
  });

  it("offers a Reload fallback", () => {
    render(
      <ErrorBoundary>
        <Boom explode />
      </ErrorBoundary>,
    );
    expect(screen.getByTestId("error-boundary-reload").textContent).toBe(
      "Reload",
    );
  });

  // The shell keys the boundary by pathname: a new key remounts it, so
  // navigating away from a crashed page recovers without a manual retry.
  it("resets when the key changes, as it does on navigation", () => {
    const { rerender } = render(
      <ErrorBoundary key="/agent/slo">
        <Boom explode />
      </ErrorBoundary>,
    );
    expect(screen.getByTestId("error-boundary")).toBeTruthy();

    rerender(
      <ErrorBoundary key="/now">
        <Boom explode={false} />
      </ErrorBoundary>,
    );
    expect(screen.getByTestId("child")).toBeTruthy();
    expect(screen.queryByTestId("error-boundary")).toBeNull();
  });
});

// The commonest render throw in this app is a malformed payload held in the
// React Query cache. Clearing the boundary's own state replays it forever: the
// subtree re-reads the same cached value synchronously, and because the throw
// happens during render the query observer never mounts long enough to refetch.
describe("QueryErrorBoundary — recovery from a poisoned query cache", () => {
  let payload: unknown = "boom";

  function Page() {
    const q = useQuery({
      queryKey: ["recs"],
      queryFn: async () => ({ recommendations: payload as string[] }),
    });
    if (!q.data) return <div data-testid="loading">loading</div>;
    // Throws "list.map is not a function" while the cache holds `"boom"`.
    return (
      <div data-testid="child">{q.data.recommendations.map((s) => s)}</div>
    );
  }

  function Harness({ client, path }: { client: QueryClient; path: string }) {
    return (
      <QueryClientProvider client={client}>
        <QueryErrorBoundary key={path}>
          {path === "/agent/slo" ? (
            <Page />
          ) : (
            <div data-testid="other-page">somewhere else</div>
          )}
        </QueryErrorBoundary>
      </QueryClientProvider>
    );
  }

  function client() {
    return new QueryClient({ defaultOptions: { queries: { retry: false } } });
  }

  it("renders the child from fresh data when Try again follows a healed server", async () => {
    payload = "boom";
    render(<Harness client={client()} path="/agent/slo" />);
    expect(await screen.findByTestId("error-boundary")).toBeTruthy();

    payload = ["checkout"];
    fireEvent.click(screen.getByTestId("error-boundary-retry"));

    expect(await screen.findByTestId("child")).toBeTruthy();
    expect(screen.queryByTestId("error-boundary")).toBeNull();
  });

  it("recovers when the operator navigates away and back instead of retrying", async () => {
    payload = "boom";
    const qc = client();
    const { rerender } = render(<Harness client={qc} path="/agent/slo" />);
    expect(await screen.findByTestId("error-boundary")).toBeTruthy();

    payload = ["checkout"];
    rerender(<Harness client={qc} path="/now" />);
    expect(screen.getByTestId("other-page")).toBeTruthy();
    rerender(<Harness client={qc} path="/agent/slo" />);

    expect(await screen.findByTestId("child")).toBeTruthy();
    expect(screen.queryByTestId("error-boundary")).toBeNull();
  });
});
