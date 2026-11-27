import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { Sparkles } from "lucide-react";
import { TopBar } from "@/components/TopBar";
import { Breadcrumb } from "@/components/Breadcrumb";
import { Dropdown } from "@/components/Dropdown";
import { AccessNotice } from "@/components/AccessDenied";
import { RetryableError } from "@/components/RetryableError";
import { isForbidden } from "@/lib/access";
import { ApiError } from "@/lib/api";

// The frame every AWS estate page shares: the top bar with Ask AI, the
// breadcrumb, a scope bar of labelled pickers, and the page's error states.

export function AwsPage({
  title,
  crumbs,
  children,
  wide = true,
}: {
  title: string;
  crumbs: Array<{ label: string; to?: string }>;
  children: ReactNode;
  wide?: boolean;
}) {
  const headerRight = (
    <div className="flex flex-wrap items-center gap-2">
      <Link to="/agent/chat" className="btn border-accent text-ink-50">
        <Sparkles size={12} aria-hidden="true" />
        Ask AI
      </Link>
    </div>
  );
  return (
    <main className="flex min-h-0 flex-1 flex-col overflow-auto">
      <TopBar title={title} subtitle="AWS" actions={headerRight} />
      <div className={`mx-auto w-full ${wide ? "max-w-7xl" : "max-w-5xl"} space-y-4 p-4 lg:p-6`}>
        <Breadcrumb items={[{ label: "AWS", to: "/aws/overview" }, ...crumbs]} />
        {children}
      </div>
    </main>
  );
}

/** Labelled pickers ("Accounts [All 4 ▾]", "Regions [3 regions ▾]"), then the page's own controls on the right. */
export function ScopeBar({
  accounts,
  regions,
  account,
  region,
  onAccount,
  onRegion,
  showRegions = true,
  children,
}: {
  accounts: Array<{ id: string; name: string }>;
  regions: string[];
  account: string;
  region: string;
  onAccount: (v: string) => void;
  onRegion?: (v: string) => void;
  showRegions?: boolean;
  children?: ReactNode;
}) {
  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-ink-500/40 pb-3 text-xs text-ink-300" data-scope-bar>
      <span className="flex min-w-0 max-w-full items-center gap-1.5" data-accounts-filter>
        <span aria-hidden="true">Accounts</span>
        <Dropdown
          value={account}
          options={[
            { value: "", label: accounts.length > 0 ? `All ${accounts.length}` : "All" },
            ...accounts.map((a) => ({ value: a.id, label: a.name === a.id ? a.id : `${a.name} (${a.id})` })),
          ]}
          onChange={onAccount}
          aria-label="Accounts"
          wrapOptions
          className="min-w-[8rem] max-w-full sm:w-44"
          listClassName="w-max min-w-full max-w-[min(22rem,calc(100vw-2rem))]"
        />
      </span>
      {showRegions && onRegion && (
        <span className="inline-flex min-w-0 max-w-full items-center gap-1.5" data-regions-filter>
          <span aria-hidden="true">Regions</span>
          <Dropdown
            value={region}
            options={[
              { value: "", label: `${regions.length} ${regions.length === 1 ? "region" : "regions"}` },
              ...regions.map((r) => ({ value: r, label: r })),
            ]}
            onChange={onRegion}
            aria-label="Regions"
            className="min-w-[8rem] max-w-full"
          />
        </span>
      )}
      {children && <div className="flex flex-wrap items-center gap-3 sm:ml-auto">{children}</div>}
    </div>
  );
}

/** A refused read names the missing access; anything else offers a retry. */
export function PageError({ error, onRetry, retrying, what }: { error: unknown; onRetry: () => void; retrying: boolean; what: string }) {
  if (isForbidden(error)) {
    return (
      <AccessNotice title={`You don't have access to AWS ${what}`}>
        Your role can't view AWS infrastructure. Ask an owner or admin for access.
      </AccessNotice>
    );
  }
  if (error instanceof ApiError && error.status === 404) {
    return (
      <section className="card p-4 text-sm text-ink-200" data-not-available>
        This server does not serve AWS {what} yet. Update Versus to the latest release.
      </section>
    );
  }
  return <RetryableError error={error} onRetry={onRetry} retrying={retrying} context={`Couldn't load AWS ${what}`} />;
}

/** The AWS connector is off: one line and the way to turn it on. */
export function AwsOff() {
  return (
    <section className="card p-4 text-sm text-ink-200" data-aws-off>
      The AWS connector is off.{" "}
      <Link to="/aws/settings" className="text-link hover:underline">
        Settings ›
      </Link>
    </section>
  );
}

/** No account connected yet. */
export function NoAccounts() {
  return (
    <section className="card p-4 text-sm text-ink-200" data-empty>
      No AWS account is connected.{" "}
      <Link to="/aws/settings" className="text-link hover:underline">
        Connect an account ›
      </Link>
    </section>
  );
}
