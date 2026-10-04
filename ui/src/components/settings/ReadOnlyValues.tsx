import { ExternalLink, Lock } from "lucide-react";

// Shared building blocks for read-only configuration sections. Secret-bearing
// fields only ever render as configured / not set; values never reach the UI.

export function ReadOnlyNotice({ docsUrl }: { docsUrl?: string }) {
  return (
    <p className="mb-4 flex flex-wrap items-center gap-x-2 gap-y-1 text-2xs text-ink-400">
      <Lock size={11} aria-hidden className="shrink-0" />
      Edit{" "}
      <code className="rounded bg-ink-700 px-1 py-0.5 font-mono text-ink-200">config/config.yaml</code>
      or the matching environment variable to change these values.
      {docsUrl && (
        <a
          href={docsUrl}
          target="_blank"
          rel="noreferrer"
          className="inline-flex items-center gap-1 text-link hover:underline"
        >
          Configuration reference <ExternalLink size={11} aria-hidden />
        </a>
      )}
    </p>
  );
}

export function ReadOnlyCard({
  title,
  aside,
  children,
}: {
  title?: React.ReactNode;
  aside?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <div className="card">
      {(title || aside) && (
        <div className="card-header">
          {title ? <h3 className="card-title">{title}</h3> : <span />}
          {aside}
        </div>
      )}
      <div className="card-body space-y-3">{children}</div>
    </div>
  );
}

export function KV({ k, v }: { k: string; v: string }) {
  return (
    <div>
      <div className="text-2xs uppercase tracking-wider text-ink-400">{k}</div>
      <div className="mt-0.5 break-words font-mono text-xs text-ink-100">{v}</div>
    </div>
  );
}

export function SecretField({ k, configured }: { k: string; configured: boolean }) {
  return (
    <div>
      <div className="text-2xs uppercase tracking-wider text-ink-400">{k}</div>
      <div className="mt-0.5 flex items-center gap-1.5 text-xs">
        <Lock size={11} className="text-ink-400" aria-hidden />
        {configured ? <span className="pill pill-good">Configured</span> : <span className="pill">Not set</span>}
      </div>
    </div>
  );
}

export function KVGrid({ children }: { children: React.ReactNode }) {
  return (
    <div className="grid grid-cols-1 gap-x-6 gap-y-3 sm:grid-cols-2 lg:grid-cols-3">{children}</div>
  );
}

export function SubCard({ title, children }: { title: React.ReactNode; children: React.ReactNode }) {
  return (
    <div className="rounded-md border border-ink-600 bg-surface-sunken px-3 py-2">
      <div className="mb-2 text-xs font-medium text-ink-200">{title}</div>
      {children}
    </div>
  );
}

export function EnablePill({ enabled }: { enabled: boolean }) {
  return <span className={`pill ${enabled ? "pill-good" : ""}`}>{enabled ? "Enabled" : "Disabled"}</span>;
}
