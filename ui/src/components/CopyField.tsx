import { useId, useState } from "react";
import { Check, Copy } from "lucide-react";
import clsx from "clsx";
import { copyText } from "@/lib/clipboard";
import { useToast } from "@/components/toastContext";

/**
 * Read-only mono value with a copy button. Used for ARNs, role names and
 * template snippets, so the value keeps its exact characters (no wrapping
 * mid-token) while staying reachable by keyboard and screen readers.
 */
export function CopyField({
  value,
  label,
  id,
  className,
}: {
  value: string;
  /** Visible label and the accessible name of the copy button. */
  label?: string;
  id?: string;
  className?: string;
}) {
  const generatedId = useId();
  const valueId = id ?? generatedId;
  const toast = useToast();
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    const ok = await copyText(value);
    if (!ok) {
      toast.push({
        tone: "error",
        title: "Copy failed",
        description: "Select the value and copy it manually.",
      });
      return;
    }
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1600);
    toast.push({ tone: "ok", title: "Copied" });
  };

  return (
    <div className={clsx("min-w-0", className)}>
      {label && (
        <label htmlFor={valueId} className="mb-1 block text-2xs text-ink-300">
          {label}
        </label>
      )}
      <div className="flex min-w-0 items-center gap-1.5 rounded-control border border-ink-500 bg-surface-sunken px-2 py-1">
        <input
          id={valueId}
          readOnly
          value={value}
          title={value}
          aria-label={label ? undefined : "Value"}
          spellCheck={false}
          onFocus={(e) => e.currentTarget.select()}
          className="min-w-0 flex-1 overflow-hidden text-ellipsis whitespace-nowrap border-0 bg-transparent p-0 font-mono text-xs text-ink-100 outline-none focus-visible:outline-none"
        />
        <button
          type="button"
          onClick={copy}
          aria-label={label ? `Copy ${label}` : "Copy value"}
          title={copied ? "Copied" : "Copy"}
          className="btn-icon"
        >
          {copied ? (
            <Check size={14} aria-hidden="true" />
          ) : (
            <Copy size={14} aria-hidden="true" />
          )}
        </button>
      </div>
    </div>
  );
}
