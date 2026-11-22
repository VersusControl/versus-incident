import { useCallback, useLayoutEffect, useRef, useState } from "react";

/** Which ends of a horizontal rail have more tabs scrolled out of view. */
export type ScrollRailEdges = { start: boolean; end: boolean };

/**
 * Width of the edge fade. The active tab is kept at least this far from an
 * edge, so the fade never covers it.
 */
const EDGE_FADE = 32;

/**
 * Keeps a horizontally scrolling tab rail readable on a narrow screen: the
 * active tab is scrolled into view whenever `active` changes, and the rail
 * reports which ends still hide tabs so it can fade those edges as a hint.
 * Only the rail scrolls; the page itself never moves.
 */
export function useScrollRail<T extends HTMLElement>(active: string) {
  const ref = useRef<T | null>(null);
  const [edges, setEdges] = useState<ScrollRailEdges>({ start: false, end: false });

  const measure = useCallback(() => {
    const rail = ref.current;
    if (!rail) return;
    const max = rail.scrollWidth - rail.clientWidth;
    const next = { start: max > 1 && rail.scrollLeft > 1, end: max > 1 && rail.scrollLeft < max - 1 };
    setEdges((previous) => previous.start === next.start && previous.end === next.end ? previous : next);
  }, []);

  useLayoutEffect(() => {
    const rail = ref.current;
    const tab = rail?.querySelector<HTMLElement>('[aria-selected="true"]');
    if (!rail || !tab) return;
    const railBox = rail.getBoundingClientRect();
    const tabBox = tab.getBoundingClientRect();
    let delta = 0;
    if (tabBox.left < railBox.left + EDGE_FADE) delta = tabBox.left - railBox.left - EDGE_FADE;
    else if (tabBox.right > railBox.right - EDGE_FADE) delta = tabBox.right - railBox.right + EDGE_FADE;
    if (delta !== 0) {
      const left = Math.max(0, rail.scrollLeft + delta);
      const still = typeof window.matchMedia === "function" && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
      if (typeof rail.scrollTo === "function") rail.scrollTo({ left, behavior: still ? "auto" : "smooth" });
      else rail.scrollLeft = left;
    }
    measure();
  }, [active, measure]);

  useLayoutEffect(() => {
    const rail = ref.current;
    if (!rail) return;
    rail.addEventListener("scroll", measure, { passive: true });
    window.addEventListener("resize", measure);
    const observer = typeof ResizeObserver === "function" ? new ResizeObserver(measure) : undefined;
    observer?.observe(rail);
    return () => {
      rail.removeEventListener("scroll", measure);
      window.removeEventListener("resize", measure);
      observer?.disconnect();
    };
  }, [measure]);

  return { ref, edges };
}

/**
 * The mask for a rail with hidden tabs at either end. It fades the content
 * itself, not a painted overlay, so it reads the same on light and dark themes.
 */
export function scrollRailMask({ start, end }: ScrollRailEdges): string | undefined {
  if (!start && !end) return undefined;
  return `linear-gradient(to right, ${start ? `transparent 0, #000 ${EDGE_FADE}px` : "#000 0"}, ${end ? `#000 calc(100% - ${EDGE_FADE}px), transparent 100%` : "#000 100%"})`;
}
