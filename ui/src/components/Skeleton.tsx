import clsx from "clsx";

// Layout-preserving loading placeholders. The audit found zero skeletons —
// every page was a lone 14px spinner followed by a layout pop-in. These
// mirror the shapes they replace so CLS stays under control; shimmer is
// disabled automatically under prefers-reduced-motion (global CSS rule).

export function SkLine({ className }: { className?: string }) {
  return <div aria-hidden className={clsx("sk h-3", className)} />;
}

export function SkCard({ lines = 3, className }: { lines?: number; className?: string }) {
  return (
    <div aria-hidden className={clsx("card p-4", className)}>
      <div className="sk mb-3 h-4 w-1/3" />
      <div className="space-y-2">
        {Array.from({ length: lines }).map((_, i) => (
          <div key={i} className="sk h-3" style={{ width: `${90 - i * 12}%` }} />
        ))}
      </div>
    </div>
  );
}

// SkRows renders table-shaped placeholder rows matching the host table's
// column count so the sticky header + widths never jump on settle.
export function SkRows({ rows = 6, cols }: { rows?: number; cols: number }) {
  return (
    <>
      {Array.from({ length: rows }).map((_, r) => (
        // .sk-row opts out of the .ddt row-entrance stagger — otherwise the
        // animation plays on the shimmer AND replays on the real rows.
        <tr key={r} aria-hidden className="sk-row">
          {Array.from({ length: cols }).map((_, c) => (
            <td key={c} className="px-3 py-2">
              <div className="sk h-3" style={{ width: c === 0 ? "60%" : "80%" }} />
            </td>
          ))}
        </tr>
      ))}
    </>
  );
}

// SkStat — the KpiTile-shaped placeholder ("—" + shimmer, never a false 0).
export function SkStat() {
  return (
    <div aria-hidden className="stat-card">
      <div className="sk h-3 w-16" />
      <div className="sk mt-1 h-7 w-12" />
    </div>
  );
}
