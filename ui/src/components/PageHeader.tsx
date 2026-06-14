import { Link } from "react-router-dom";
import { ArrowLeft } from "lucide-react";
import clsx from "clsx";

// PageHeader is the sticky identity strip every page leads with: back link,
// title, status meta (pills/badges) and the page's primary actions — always
// visible, fixing the audited pattern where incident state + Resolve sat
// below a viewport of content (D2/D3).
export function PageHeader({
  back,
  title,
  meta,
  subtitle,
  actions,
  className,
}: {
  back?: { to: string; label: string };
  title: React.ReactNode;
  /** Pills / badges rendered inline after the title. */
  meta?: React.ReactNode;
  subtitle?: React.ReactNode;
  actions?: React.ReactNode;
  className?: string;
}) {
  return (
    <header
      className={clsx(
        "sticky top-0 z-sticky shrink-0 border-b border-ink-600",
        "bg-surface-sunken/95 px-4 py-3 backdrop-blur lg:px-6",
        className,
      )}
    >
      {back && (
        <Link
          to={back.to}
          className="mb-1 inline-flex items-center gap-1 text-2xs text-ink-300 hover:text-ink-100"
        >
          <ArrowLeft size={12} />
          {back.label}
        </Link>
      )}
      <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <h1 className="truncate text-base font-semibold text-ink-50">
            {title}
          </h1>
          {meta}
        </div>
        {actions && <div className="flex shrink-0 items-center gap-2">{actions}</div>}
      </div>
      {subtitle && <div className="mt-1 text-2xs text-ink-300">{subtitle}</div>}
    </header>
  );
}
