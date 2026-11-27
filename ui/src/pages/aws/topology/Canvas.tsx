import { useCallback, useEffect, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import clsx from "clsx";
import { ChevronDown, ChevronRight, Maximize2, Minus, Plus } from "lucide-react";
import { fitView, linkStyle, pathData, type Layout } from "./layout";
import { mapHeight } from "./useMapView";

// The drawing surface shared by the three levels: links as one SVG under
// the boxes, boxes as positioned HTML so they stay real buttons and links,
// both scaled together. On open, and whenever the map area changes size,
// the map fits its view (see fitView); −, + and Fit zoom by hand. When
// more of the map is out of view, an edge shade and a button say so.

const ZOOM_MIN = 0.3;
const ZOOM_MAX = 1.5;
const ZOOM_STEP = 0.1;

export function Canvas({
  layout,
  label,
  children,
}: {
  layout: Layout;
  label: string;
  children: ReactNode;
}) {
  const scroller = useRef<HTMLDivElement>(null);
  const [scale, setScale] = useState(1);
  const pending = useRef<{ left: number; top: number } | null>(null);
  const [more, setMore] = useState({ right: false, down: false });
  const measure = useCallback(() => {
    const el = scroller.current;
    if (!el) return;
    setMore({
      right: el.scrollLeft + el.clientWidth < el.scrollWidth - 4,
      down: el.scrollTop + el.clientHeight < el.scrollHeight - 4,
    });
  }, []);

  /** Fit the map to its view and remember where to scroll once drawn at that scale. */
  const fit = useCallback(() => {
    const el = scroller.current;
    if (!el) return;
    const view = { width: el.clientWidth, height: mapHeight() };
    if (view.width <= 0) {
      // Nothing measured (no layout engine): draw at full size, still centred on the focus.
      const box = layout.focus;
      if (box) pending.current = { left: Math.max(0, box.x + box.w / 2 - 200), top: Math.max(0, box.y + box.h / 2 - 100) };
      setScale(1);
      return;
    }
    const r = fitView(layout, view);
    pending.current = { left: r.left, top: r.top };
    el.dataset.fit = r.target.id;
    setScale(r.scale);
  }, [layout]);

  /** Zoom by hand, keeping the middle of the view where it is. */
  const zoom = (next: number) => {
    const el = scroller.current;
    const s = Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, Math.round(next * 10) / 10));
    if (el) {
      const cx = (el.scrollLeft + el.clientWidth / 2) / scale;
      const cy = (el.scrollTop + el.clientHeight / 2) / scale;
      pending.current = { left: Math.max(0, cx * s - el.clientWidth / 2), top: Math.max(0, cy * s - el.clientHeight / 2) };
    }
    setScale(s);
  };

  // Fit on open and whenever the layout changes.
  useLayoutEffect(() => {
    fit();
  }, [fit]);

  // Scroll once the map is drawn at its new scale.
  useLayoutEffect(() => {
    const el = scroller.current;
    const at = pending.current;
    if (!el || !at) return;
    el.scrollLeft = at.left;
    el.scrollTop = at.top;
    el.dataset.scale = String(scale);
    pending.current = null;
    measure();
  });

  // Fit again when the map area changes width (the side panel opens or closes, the window resizes).
  useEffect(() => {
    const el = scroller.current;
    let width = el?.clientWidth ?? 0;
    const onResize = () => {
      const w = scroller.current?.clientWidth ?? 0;
      if (w !== width) {
        width = w;
        fit();
      }
      measure();
    };
    window.addEventListener("resize", onResize);
    const watcher = el && typeof ResizeObserver !== "undefined" ? new ResizeObserver(onResize) : null;
    if (el) watcher?.observe(el);
    return () => {
      window.removeEventListener("resize", onResize);
      watcher?.disconnect();
    };
  }, [fit, measure]);

  const height = Math.min(Math.ceil(layout.height * scale) + 2, mapHeight());
  return (
    <div className="relative min-w-0">
      {/* The zoom control sits in its own row above the map, so it never covers a card. */}
      <div className="flex justify-end px-3 pb-1 pt-2" data-zoom-row>
        <div className="flex items-center gap-1 rounded-control border border-ink-500/60 bg-surface-raised p-0.5" role="group" aria-label="Zoom">
          <button type="button" className="btn-icon" aria-label="Zoom out" title="Zoom out" disabled={scale <= ZOOM_MIN} onClick={() => zoom(scale - ZOOM_STEP)}>
            <Minus size={14} aria-hidden="true" />
          </button>
          <button type="button" className="btn-icon" aria-label="Zoom in" title="Zoom in" disabled={scale >= ZOOM_MAX} onClick={() => zoom(scale + ZOOM_STEP)}>
            <Plus size={14} aria-hidden="true" />
          </button>
          <button type="button" className="btn-icon" aria-label="Fit the map to the view" title="Fit" onClick={fit}>
            <Maximize2 size={14} aria-hidden="true" />
          </button>
        </div>
      </div>
      <div ref={scroller} className="min-w-0 overflow-auto" style={{ height }} data-canvas data-scale={scale} onScroll={measure}>
        <div className="relative" style={{ width: layout.width * scale, height: layout.height * scale }}>
          <div className="absolute left-0 top-0 origin-top-left" style={{ width: layout.width, height: layout.height, transform: `scale(${scale})` }}>
            <svg
              className="absolute inset-0"
              width={layout.width}
              height={layout.height}
              viewBox={`0 0 ${layout.width} ${layout.height}`}
              role="img"
              aria-label={label}
            >
              {layout.frame && (
                <rect
                  x={layout.frame.x}
                  y={layout.frame.y}
                  width={layout.frame.w}
                  height={layout.frame.h}
                  rx="8"
                  className="fill-none stroke-ink-500"
                  strokeOpacity="0.6"
                />
              )}
              {layout.links.map((l) => {
                const s = linkStyle(l.kind, l.sides);
                return (
                  <g
                    key={l.key}
                    className={s.tone === "warn" ? "text-sev-warn" : "text-ink-300"}
                    data-link={`${l.from}>${l.to}`}
                    data-kind={l.kind}
                    data-sides={l.sides}
                    data-label-shown={l.labelAt ? "true" : "false"}
                  >
                    <title>{`${l.title}, ${s.name}`}</title>
                    <path d={pathData(l.points)} fill="none" stroke="currentColor" strokeWidth="1.5" strokeDasharray={s.dash} />
                    {l.labelAt && (
                      <>
                        <rect x={l.labelAt.x} y={l.labelAt.y} width={l.labelAt.w} height={l.labelAt.h} rx="4" className="fill-surface" />
                        <text x={l.labelAt.x + l.labelAt.w / 2} y={l.labelAt.y + 13} textAnchor="middle" fontSize="12" className="fill-ink-100">
                          {l.labelLines.map((line, i) => (
                            <tspan key={i} x={l.labelAt!.x + l.labelAt!.w / 2} dy={i === 0 ? 0 : 18}>
                              {line}
                            </tspan>
                          ))}
                        </text>
                      </>
                    )}
                  </g>
                );
              })}
            </svg>
            {children}
          </div>
        </div>
      </div>
      {more.right && (
        <div className="pointer-events-none absolute bottom-0 right-0 top-12 flex w-12 items-center justify-end bg-gradient-to-l from-surface to-transparent pr-1" data-scroll-cue="right">
          <button
            type="button"
            className="btn-icon pointer-events-auto border-ink-500 bg-surface-raised"
            aria-label="Scroll the map right"
            title="More of the map to the right"
            onClick={() => scroller.current?.scrollBy({ left: (scroller.current?.clientWidth ?? 300) * 0.8, behavior: "smooth" })}
          >
            <ChevronRight size={14} aria-hidden="true" />
          </button>
        </div>
      )}
      {more.down && (
        <div className={clsx("pointer-events-none absolute inset-x-0 bottom-0 flex h-10 items-end justify-center bg-gradient-to-t from-surface to-transparent pb-1")} data-scroll-cue="down">
          <button
            type="button"
            className="btn-icon pointer-events-auto border-ink-500 bg-surface-raised"
            aria-label="Scroll the map down"
            title="More of the map below"
            onClick={() => scroller.current?.scrollBy({ top: (scroller.current?.clientHeight ?? 300) * 0.8, behavior: "smooth" })}
          >
            <ChevronDown size={14} aria-hidden="true" />
          </button>
        </div>
      )}
    </div>
  );
}

const KEYS: Array<{ id: string; label: string; dash?: string; warn?: boolean; box?: boolean }> = [
  { id: "both", label: "Seen from both sides" },
  { id: "configured", label: "Configured" },
  { id: "may_access", label: "May access", dash: "2 4" },
  { id: "inferred", label: "Inferred", dash: "6 4" },
  { id: "one", label: "Seen from one side", dash: "6 4", warn: true },
  { id: "not_connected", label: "Not connected", box: true },
];

/** The line key under a map; each level names only the styles it draws. */
export function Legend({ keys }: { keys: string[] }) {
  return (
    <ul className="flex flex-wrap items-center gap-x-8 gap-y-2 px-6 pb-5 pt-2 text-xs text-ink-300" aria-label="Line key" data-legend>
      {KEYS.filter((k) => keys.includes(k.id)).map((k) => (
        <li key={k.id} className="inline-flex items-center gap-2" data-key={k.id}>
          {k.box ? (
            <span className="inline-block h-3.5 w-5 rounded-sm border border-dashed border-ink-300" aria-hidden="true" />
          ) : (
            <svg width="28" height="4" aria-hidden="true" className={clsx(k.warn ? "text-sev-warn" : "text-ink-300")}>
              <line x1="0" y1="2" x2="28" y2="2" stroke="currentColor" strokeWidth="1.5" strokeDasharray={k.dash} />
            </svg>
          )}
          {k.label}
        </li>
      ))}
    </ul>
  );
}
