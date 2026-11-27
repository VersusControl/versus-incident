import { useCallback, useEffect, useState } from "react";
import type { View } from "./layout";

/** The tallest a map gets: three quarters of the window, at most 720 px. */
export function mapHeight(): number {
  const h = typeof window !== "undefined" ? window.innerHeight : 0;
  return Math.min(720, Math.round((h || 960) * 0.75));
}

/**
 * The map's viewport: the width of the element the ref is put on, and the
 * map height. Zero width until measured, so layouts fall back to their
 * default direction (and jsdom, which measures nothing, keeps them stable).
 */
export function useMapView(): [(el: HTMLElement | null) => void, View] {
  const [el, setEl] = useState<HTMLElement | null>(null);
  const [view, setView] = useState<View>({ width: 0, height: 0 });
  const measure = useCallback(() => {
    if (!el) return;
    const width = el.clientWidth;
    const height = mapHeight();
    setView((v) => (v.width === width && v.height === height ? v : { width, height }));
  }, [el]);
  useEffect(() => {
    measure();
    window.addEventListener("resize", measure);
    const watcher = el && typeof ResizeObserver !== "undefined" ? new ResizeObserver(measure) : null;
    if (el) watcher?.observe(el);
    return () => {
      window.removeEventListener("resize", measure);
      watcher?.disconnect();
    };
  }, [el, measure]);
  return [setEl, view];
}
