import { useCallback, useEffect, useMemo, useRef } from "react";
import { useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useToast } from "@/components/toastContext";
import { api } from "@/lib/api";
import { DEFAULT_SERVICES, parseServiceParam } from "@/lib/awsChanges";

// The Changes page's service filter. The saved choice lives on the server per
// signed-in user (RDS and IAM until one is saved). A `service=` in the URL
// wins for that view, so a shared link shows what its sender saw, but only an
// explicit change in the panel or its chips saves a new choice.

const PREFS_KEY = ["aws-changes-preferences"] as const;
/** Clicks in quick succession save once. */
export const SAVE_DEBOUNCE_MS = 400;

export interface ServiceFilterState {
  /** The services the page asks for; empty means all. */
  selected: string[];
  /** False until the saved choice (or the URL's) is known; the page waits so it never flashes every service. */
  ready: boolean;
  /** An explicit change: applied, dropped from the URL, and saved. */
  choose: (next: string[]) => void;
  /** Adds a service for this view only: written to the URL, not saved. */
  addForView: (service: string) => void;
}

export function useServiceFilter(): ServiceFilterState {
  const [params, setParams] = useSearchParams();
  const qc = useQueryClient();
  const toast = useToast();
  const prefs = useQuery({
    queryKey: PREFS_KEY,
    queryFn: () => api.awsChangesPreferences(),
    // The cache holds the user's latest choice; a refetch must not undo a
    // change whose save is still waiting.
    staleTime: Infinity,
    retry: false,
  });
  const urlValue = params.get("service");
  const loaded = prefs.data?.services;
  const failed = prefs.isError;
  const saved = useMemo(
    () => (Array.isArray(loaded) ? loaded : failed ? [...DEFAULT_SERVICES] : undefined),
    [loaded, failed],
  );
  const selected = useMemo(() => (urlValue !== null ? parseServiceParam(urlValue) : (saved ?? [])), [urlValue, saved]);
  const ready = urlValue !== null || saved !== undefined;

  const pending = useRef<string[] | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Takes the waiting change, if any, and sends it once. `unload` uses a
  // request that survives the page going away.
  const flush = useCallback(
    (how: "now" | "unload" = "now") => {
      if (timer.current) clearTimeout(timer.current);
      timer.current = null;
      const next = pending.current;
      pending.current = null;
      if (!next) return;
      const save =
        how === "unload"
          ? api.saveAwsChangesPreferencesOnUnload({ services: next })
          : api.saveAwsChangesPreferences({ services: next });
      save.catch(() => {
        toast.push({ tone: "error", title: "Couldn't save the service filter" });
      });
    },
    [toast],
  );
  // A reload, a tab close or another site never unmounts React, so a change
  // made just before goes out on pagehide or when the tab is hidden. Leaving
  // the page inside the app unmounts it and saves the same way.
  useEffect(() => {
    const onPageHide = () => flush("unload");
    const onVisibility = () => {
      if (document.visibilityState === "hidden") flush("unload");
    };
    window.addEventListener("pagehide", onPageHide);
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      window.removeEventListener("pagehide", onPageHide);
      document.removeEventListener("visibilitychange", onVisibility);
      flush();
    };
  }, [flush]);

  const choose = useCallback(
    (next: string[]) => {
      qc.setQueryData(PREFS_KEY, { services: next });
      setParams(
        (prev) => {
          if (!prev.has("service")) return prev;
          const n = new URLSearchParams(prev);
          n.delete("service");
          return n;
        },
        { replace: true },
      );
      pending.current = next;
      if (timer.current) clearTimeout(timer.current);
      timer.current = setTimeout(() => flush(), SAVE_DEBOUNCE_MS);
    },
    [qc, setParams, flush],
  );

  const addForView = useCallback(
    (service: string) => {
      if (selected.length === 0 || selected.includes(service)) return;
      setParams(
        (prev) => {
          const n = new URLSearchParams(prev);
          n.set("service", [...selected, service].join(","));
          return n;
        },
        { replace: true },
      );
    },
    [selected, setParams],
  );

  return { selected, ready, choose, addForView };
}
