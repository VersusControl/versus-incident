import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { api } from "./api";

// ---------------------------------------------------------------------------
// useTableKeys — j/k row navigation + Enter to open, for dense tables.
// The container receives tabIndex/onKeyDown; each row gets data-active for
// the .ddt[data-active] styling. Extra single-key actions (e.g. Patterns'
// K=known) hook in via `extra`.
// ---------------------------------------------------------------------------
export function useTableKeys({
  size,
  onOpen,
  extra,
}: {
  size: number;
  onOpen?: (index: number) => void;
  extra?: (key: string, index: number) => boolean | void;
}) {
  const [active, setActive] = useState(-1);

  useEffect(() => {
    // Clamp when the list shrinks under the cursor (filtering).
    if (active >= size) setActive(size - 1);
  }, [size, active]);

  const onKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (isEditableTarget(e.target)) return;
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      switch (e.key) {
        case "j":
        case "ArrowDown":
          e.preventDefault();
          setActive((a) => Math.min(size - 1, a + 1));
          break;
        case "k":
        case "ArrowUp":
          e.preventDefault();
          setActive((a) => Math.max(0, a - 1));
          break;
        case "Enter":
          if (active >= 0 && onOpen) {
            e.preventDefault();
            onOpen(active);
          }
          break;
        case "Escape":
          setActive(-1);
          break;
        default:
          if (active >= 0 && extra?.(e.key, active)) e.preventDefault();
      }
    },
    [size, active, onOpen, extra],
  );

  return {
    activeIndex: active,
    setActiveIndex: setActive,
    containerProps: { tabIndex: 0, onKeyDown },
    rowProps: (i: number) => ({
      "data-active": i === active ? "true" : undefined,
      onMouseEnter: () => setActive(i),
    }),
  };
}

function isEditableTarget(t: EventTarget | null): boolean {
  const el = t as HTMLElement | null;
  if (!el) return false;
  const tag = el.tagName;
  return (
    tag === "INPUT" ||
    tag === "TEXTAREA" ||
    tag === "SELECT" ||
    el.isContentEditable
  );
}

// ---------------------------------------------------------------------------
// useShortcuts — global keymap, installed once in AppShell.
//   /      → focus the current page's search ([data-page-search]); falls back
//            to the TopBar search affordance. One binding, one precedence rule.
//   g n/i/p → navigate Now / Incidents / Patterns
//   ?      → toggle the shortcut overlay
// Ignores keystrokes while typing in inputs and while a modal is open
// (modals own Escape; sequences would surprise mid-dialog).
// ---------------------------------------------------------------------------
export function useShortcuts({ onHelp }: { onHelp: () => void }) {
  const navigate = useNavigate();
  const pending = useRef<string | null>(null);
  const timer = useRef<number | undefined>(undefined);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (isEditableTarget(e.target)) return;
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      if (document.querySelector("[role='dialog']")) return;

      if (pending.current === "g") {
        pending.current = null;
        window.clearTimeout(timer.current);
        const dest =
          e.key === "n" ? "/now" : e.key === "i" ? "/incidents" : e.key === "p" ? "/agent/logs" : null;
        if (dest) {
          e.preventDefault();
          navigate(dest);
        }
        return;
      }

      if (e.key === "g") {
        pending.current = "g";
        window.clearTimeout(timer.current);
        timer.current = window.setTimeout(() => (pending.current = null), 800);
        return;
      }
      if (e.key === "/") {
        e.preventDefault();
        const el = document.querySelector<HTMLElement>("[data-page-search]");
        if (el) {
          el.focus();
        } else {
          // Documented fallback (§2.4): pages without a search land on the
          // Incidents search instead of a silent no-op.
          navigate("/incidents");
          window.setTimeout(() => {
            document.querySelector<HTMLElement>("[data-page-search]")?.focus();
          }, 80);
        }
        return;
      }
      if (e.key === "?") {
        e.preventDefault();
        onHelp();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("keydown", onKey);
      window.clearTimeout(timer.current);
    };
  }, [navigate, onHelp]);
}

// ---------------------------------------------------------------------------
// useOpenIncidentCount — shared by the TopBar count + the Now page. Polls the
// cheap server counts endpoint every 30s (never loads incident rows), pausing
// while the tab is hidden. It exposes the OPEN grand total plus the per-ORIGIN
// OPEN tally (AI-detect vs webhook) straight from the server's authoritative
// per-origin × per-status breakdown, so the top bar shows the two feeds
// separately ("AI: N · Webhook: M") without ever counting a bounded, loaded
// page. The Sidebar deliberately shows NO count.
// ---------------------------------------------------------------------------
export function useOpenIncidentCount() {
  const q = useQuery({
    queryKey: ["incidents", "counts"],
    queryFn: () => api.incidentCounts(),
    refetchInterval: () => (document.hidden ? false : 30_000),
    staleTime: 15_000,
  });
  const open = q.data?.by_status?.open;
  return { open: open?.total ?? 0, originCounts: open, query: q };
}

// ---------------------------------------------------------------------------
// useCountUp — eases a numeric display toward `target` (~350ms, ease-out
// cubic). First render shows the real value immediately (numbers must be
// readable without waiting on motion); only CHANGES animate, so a live
// refetch ticking 4→5 counts up. Snaps instantly under reduced motion.
// ---------------------------------------------------------------------------
export function useCountUp(target: number, duration = 350): number {
  const [display, setDisplay] = useState(target);
  const prev = useRef(target);

  useEffect(() => {
    const from = prev.current;
    prev.current = target;
    if (from === target) return;
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
      setDisplay(target);
      return;
    }
    let raf = 0;
    const t0 = performance.now();
    const tick = (t: number) => {
      const p = Math.min(1, (t - t0) / duration);
      const eased = 1 - Math.pow(1 - p, 3);
      setDisplay(Math.round(from + (target - from) * eased));
      if (p < 1) raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf);
  }, [target, duration]);

  return display;
}

// ---------------------------------------------------------------------------
// useNowTick — a clock that re-renders the consumer every `intervalMs`.
// Windowed computations (hourlyBuckets) must anchor on this, not on a
// Date.now() captured inside a data-keyed useMemo: TanStack structural
// sharing returns the SAME data reference for unchanged payloads, so a
// data-keyed memo never re-runs on quiet dashboards and the 24h window
// freezes at the last data change.
// ---------------------------------------------------------------------------
export function useNowTick(intervalMs = 60_000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), intervalMs);
    return () => window.clearInterval(id);
  }, [intervalMs]);
  return now;
}
