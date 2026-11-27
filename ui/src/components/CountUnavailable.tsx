import { COUNT_UNAVAILABLE_LABEL } from "@/lib/incidentCounts";

// CountUnavailable stands in for a count the server could not give: a dash
// on screen, "Count unavailable" for screen readers and on hover. It never
// renders a 0, which would read as "nothing open".
export function CountUnavailable() {
  return (
    <span title={COUNT_UNAVAILABLE_LABEL}>
      <span aria-hidden="true">—</span>
      <span className="sr-only">{COUNT_UNAVAILABLE_LABEL}</span>
    </span>
  );
}
