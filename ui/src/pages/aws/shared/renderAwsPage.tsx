import type { ReactElement } from "react";
import { render } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { ToastProvider } from "@/components/Toast";
import { LocationProbe } from "./LocationProbe";

// Renders an AWS page for a test: a fresh query client with no retries, the
// toast provider, a router at `path`, and a probe that shows where the page
// navigated to.

export function renderAwsPage(page: ReactElement, path: string, route = path.split("?")[0]) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return {
    client,
    ...render(
      <QueryClientProvider client={client}>
        <ToastProvider>
          <MemoryRouter initialEntries={[path]}>
            <Routes>
              <Route path={route} element={page} />
              <Route path="*" element={null} />
            </Routes>
            <LocationProbe />
          </MemoryRouter>
        </ToastProvider>
      </QueryClientProvider>,
    ),
  };
}
