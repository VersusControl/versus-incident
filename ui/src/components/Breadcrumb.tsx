import { Link } from "react-router-dom";
import { ChevronRight } from "lucide-react";
import clsx from "clsx";

export interface Crumb {
  label: string;
  /** Omitted on the current page, which is rendered as plain text. */
  to?: string;
}

/**
 * Trail of links ending in the current page ("AWS / Settings"). The crumbs sit
 * on the page header line, so they use the same 11px secondary tone as the
 * header's back link and never wrap into a second visual row unless the labels
 * themselves overflow.
 */
export function Breadcrumb({
  items,
  label = "Breadcrumb",
  className,
}: {
  items: Crumb[];
  label?: string;
  className?: string;
}) {
  if (items.length === 0) return null;

  return (
    <nav aria-label={label} className={clsx("min-w-0", className)}>
      <ol className="flex min-w-0 flex-wrap items-center gap-1.5 text-2xs text-ink-300">
        {items.map((item, index) => {
          const current = index === items.length - 1;
          return (
            <li
              key={`${item.label}-${index}`}
              className="flex min-w-0 items-center gap-1.5"
            >
              {index > 0 && (
                <ChevronRight
                  size={12}
                  aria-hidden="true"
                  className="shrink-0 text-ink-400"
                />
              )}
              {current || !item.to ? (
                <span
                  aria-current={current ? "page" : undefined}
                  className="truncate text-ink-200"
                >
                  {item.label}
                </span>
              ) : (
                <Link
                  to={item.to}
                  className="truncate rounded transition-colors hover:text-ink-100 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent"
                >
                  {item.label}
                </Link>
              )}
            </li>
          );
        })}
      </ol>
    </nav>
  );
}
