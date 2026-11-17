import { useState } from "react";
import { Check, Copy, XCircle } from "lucide-react";
import { copyText } from "@/lib/clipboard";
import { useToast } from "@/components/toastContext";

// AwsDeniedFix — the one place a permission problem is shown. It sits under
// the service grid, names the role and the exact IAM actions, and copies the
// policy fragment. The rest of the page stays normal: a denial is local, not a
// page-level failure.
export function AwsDeniedFix({
  label,
  fix,
  actions,
  policy,
}: {
  /** Human service name, e.g. "ElastiCache". */
  label: string;
  /** "Add elasticache:Describe* to versus-reader". */
  fix: string;
  actions: string[];
  /** Full statement when the server built one; actions are the fallback. */
  policy?: string | null;
}) {
  const toast = useToast();
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    const ok = await copyText(policy && policy.trim() ? policy : actions.join("\n"));
    if (!ok) {
      toast.push({
        tone: "error",
        title: "Copy failed",
        description: "Select the actions and copy them manually.",
      });
      return;
    }
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1600);
    toast.push({ tone: "ok", title: "Fix copied" });
  };

  return (
    <div className="rounded-control border border-sev-critical/40 bg-sev-critical/10 p-2.5">
      <p className="flex items-center gap-1.5 text-xs font-medium text-sev-critical">
        <XCircle size={13} aria-hidden="true" />
        {label}: Denied
      </p>
      <p className="mt-1 break-words text-xs text-ink-100">
        {fix.split(/(\s+)/).map((part, index) =>
          part.includes(":") ? (
            <code key={index} className="font-mono text-2xs text-ink-50">
              {part}
            </code>
          ) : (
            <span key={index}>{part}</span>
          ),
        )}
      </p>
      <button
        type="button"
        className="btn mt-2"
        onClick={copy}
        aria-label={`Copy fix for ${label}`}
      >
        {copied ? (
          <Check size={12} aria-hidden="true" />
        ) : (
          <Copy size={12} aria-hidden="true" />
        )}
        {copied ? "Copied" : "Copy fix"}
      </button>
    </div>
  );
}
