import { Link, Navigate } from "react-router-dom";
import { ShieldAlert } from "lucide-react";
import { landingPath, useAccess } from "@/lib/access";
import { TopBar } from "./TopBar";

/**
 * A plain, permanent refusal: the caller's role cannot open this. Quiet
 * styling, no Retry, because asking again gets the same answer.
 */
export function AccessNotice({ title, children }: { title: string; children?: React.ReactNode }) {
  return (
    <section className="card" aria-labelledby="access-denied-title" data-access-denied>
      <div className="card-body flex items-start gap-3">
        <ShieldAlert size={16} aria-hidden="true" className="mt-0.5 shrink-0 text-ink-300" />
        <div className="min-w-0 text-xs text-ink-200">
          <h2 id="access-denied-title" className="text-sm font-medium text-ink-50">
            {title}
          </h2>
          <p className="mt-1">{children ?? "Your role doesn't include this page. Ask an owner or admin for access."}</p>
        </div>
      </div>
    </section>
  );
}

/** The page shown in place of a route the caller's role cannot open; it sends no request. */
export function AccessDeniedPage() {
  const access = useAccess();
  return (
    <>
      <TopBar title="No access" />
      <main className="flex-1 overflow-auto p-4 lg:p-6">
        <div className="mx-auto max-w-2xl space-y-3">
          <AccessNotice title="You don't have access to this page">
            Your role doesn't include this page. Ask an owner or admin for access.{" "}
            <Link to={landingPath(access.permissions)} className="text-link hover:underline">
              Go to your start page
            </Link>
          </AccessNotice>
        </div>
      </main>
    </>
  );
}

/** `/` and unknown paths: Now for a role that reads incidents, else the first page it can open. */
export function LandingRedirect() {
  const access = useAccess();
  if (!access.ready) return null;
  return <Navigate to={landingPath(access.permissions)} replace />;
}
