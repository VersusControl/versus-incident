// Sparkline — tiny inline SVG trend, dependency-free, 12-bucket convention
// from the token-budget rules. Renders nothing for empty/flat data rather
// than fabricating a line. Always pass an aria-label summarizing the trend
// (screen-reader-summary rule).
export function Sparkline({
  data,
  width = 80,
  height = 20,
  className,
  "aria-label": ariaLabel,
}: {
  data: number[];
  width?: number;
  height?: number;
  className?: string;
  "aria-label": string;
}) {
  if (!data || data.length < 2) return null;
  const max = Math.max(...data);
  const min = Math.min(...data);
  if (max === min) return null;

  const pad = 2;
  const w = width - pad * 2;
  const h = height - pad * 2;
  const pts = data
    .map((v, i) => {
      const x = pad + (i / (data.length - 1)) * w;
      const y = pad + h - ((v - min) / (max - min)) * h;
      return `${x.toFixed(1)},${y.toFixed(1)}`;
    })
    .join(" ");

  return (
    <svg
      role="img"
      aria-label={ariaLabel}
      viewBox={`0 0 ${width} ${height}`}
      width={width}
      height={height}
      className={className}
    >
      <polyline
        points={pts}
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinejoin="round"
        strokeLinecap="round"
      />
    </svg>
  );
}
