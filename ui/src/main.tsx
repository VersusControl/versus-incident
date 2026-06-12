import React from "react";
import ReactDOM from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import App from "./App";
import { applyTheme, getStoredTheme } from "./lib/theme";
import "./index.css";

// Apply the persisted theme before first render to avoid a flash of the
// wrong theme.
applyTheme(getStoredTheme());

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // No global auto-poll: the old 5s default refetched EVERY query
      // (members, teams, configs…) forever. Live surfaces opt in with
      // their own refetchInterval (Now 15s, liveness 30s — paused when
      // the tab is hidden); everything else refreshes on window focus.
      refetchInterval: false,
      refetchOnWindowFocus: true,
      retry: 1,
      staleTime: 15_000,
    },
  },
});

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </QueryClientProvider>
  </React.StrictMode>,
);
