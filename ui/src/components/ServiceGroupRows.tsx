import { Fragment, type ReactNode } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
import { displayService } from "@/lib/format";
import { GROUP_PREVIEW_ROWS, type ServiceGroupView } from "@/lib/serviceGroups";

// ServiceGroupedRows renders a table body's rows either flat or as service
// groups. `renderRow` receives each row's index among the rows on screen.
export function ServiceGroupedRows<T>({
  grouped,
  views,
  flatRows,
  colSpan,
  renderRow,
  onToggle,
  onShowMore,
}: {
  grouped: boolean;
  views: ServiceGroupView<T>[];
  flatRows: T[];
  colSpan: number;
  renderRow: (row: T, index: number) => ReactNode;
  onToggle: (group: ServiceGroupView<T>) => void;
  onShowMore: (group: ServiceGroupView<T>) => void;
}) {
  if (!grouped) return <>{flatRows.map(renderRow)}</>;
  let index = 0;
  return (
    <>
      {views.map((g) => (
        <Fragment key={`group:${g.service}`}>
          <ServiceGroupHeaderRow
            colSpan={colSpan}
            service={g.service}
            total={g.total}
            expanded={g.expanded}
            onToggle={() => onToggle(g)}
          />
          {g.rows.map((row) => renderRow(row, index++))}
          {g.hidden > 0 && (
            <ServiceGroupMoreRow
              colSpan={colSpan}
              hidden={g.hidden}
              service={g.service}
              onShowMore={() => onShowMore(g)}
            />
          )}
        </Fragment>
      ))}
    </>
  );
}

// ServiceGroupHeaderRow is the full-width row that opens a service group in
// the "By service" view.
export function ServiceGroupHeaderRow({
  colSpan,
  service,
  total,
  expanded,
  onToggle,
}: {
  colSpan: number;
  service: string;
  total: number;
  expanded: boolean;
  onToggle: () => void;
}) {
  const label = displayService(service);
  const expandable = total > GROUP_PREVIEW_ROWS;
  const Chevron = expanded ? ChevronDown : ChevronRight;
  return (
    <tr data-group-header="">
      <td colSpan={colSpan} className="bg-surface-sunken/60 py-1.5">
        {expandable ? (
          <button
            type="button"
            className="inline-flex items-center gap-1.5 rounded text-xs font-semibold text-ink-50 hover:text-link"
            aria-expanded={expanded}
            aria-label={`${expanded ? "Collapse" : "Expand"} ${label} (${total})`}
            onClick={onToggle}
          >
            <Chevron size={14} className="text-ink-300" aria-hidden />
            <span className="font-mono">{label}</span>
            <span className="pill">{total}</span>
          </button>
        ) : (
          <span className="inline-flex items-center gap-1.5 pl-5 text-xs font-semibold text-ink-50">
            <span className="font-mono">{label}</span>
            <span className="pill">{total}</span>
          </span>
        )}
      </td>
    </tr>
  );
}

// ServiceGroupMoreRow reveals the rest of a group that is only partly shown.
export function ServiceGroupMoreRow({
  colSpan,
  hidden,
  service,
  onShowMore,
}: {
  colSpan: number;
  hidden: number;
  service: string;
  onShowMore: () => void;
}) {
  return (
    <tr>
      <td colSpan={colSpan} className="py-1 pl-9">
        <button
          type="button"
          className="inline-flex items-center gap-1 text-2xs text-link hover:underline"
          aria-label={`Show ${hidden} more in ${displayService(service)}`}
          onClick={onShowMore}
        >
          <ChevronDown size={12} aria-hidden />
          Show {hidden.toLocaleString()} more
        </button>
      </td>
    </tr>
  );
}
