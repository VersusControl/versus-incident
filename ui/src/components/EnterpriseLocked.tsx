import { Lock } from "lucide-react";

// EnterpriseLockedBody — the shared Enterprise-only locked state (Lock glyph,
// upsell copy, Learn-more CTA), sized for an embedded card rather than a full
// page. Used by the admin-gated controls (runtime mode X26, AI settings X27)
// when the endpoint reports 403 (community / unlicensed) or 404 (OSS binary —
// route absent). Callers wrap this in their own card shell and supply the
// feature-specific heading and blurb.
export function EnterpriseLockedBody({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}) {
  return (
    <div
      data-testid="enterprise-locked"
      className="mx-auto flex max-w-md flex-col items-center gap-3 py-4 text-center"
    >
      <div className="rounded-full bg-accent-subtle p-3 text-link">
        <Lock size={20} />
      </div>
      <h3 className="text-sm font-semibold text-ink-50">{title}</h3>
      <p className="text-xs text-ink-300">{children}</p>
      <a
        className="btn btn-primary mt-1"
        href="https://versusincident.com/enterprise"
        target="_blank"
        rel="noreferrer"
      >
        Learn about Enterprise
      </a>
    </div>
  );
}
