import { useEffect, useRef, useState } from "react";
import { Check, Copy, Eye, EyeOff, Loader2 } from "lucide-react";
import clsx from "clsx";
import { copyText } from "@/lib/clipboard";
import { useToast } from "@/components/toastContext";

/** How long a revealed secret stays on screen before it masks itself again. */
export const SECRET_REVEAL_MS = 30_000;

const MASK = "••••••••";

/**
 * A secret shown as prefix…suffix until the operator asks for it. Revealing
 * calls the caller's audited fetcher (no endpoint lives here), and the value
 * re-masks itself after 30 seconds even if the tab is left open.
 *
 * The secret is only held in component state while it is visible; hiding or
 * timing out drops it, so a later render can't resurrect it.
 */
export function SecretReveal({
  label,
  prefix = "",
  suffix = "",
  reveal,
  className,
}: {
  /** Visible label and the accessible name stem for both buttons. */
  label: string;
  /** Characters kept visible on the left of the mask. */
  prefix?: string;
  /** Characters kept visible on the right of the mask. */
  suffix?: string;
  reveal: () => Promise<string>;
  className?: string;
}) {
  const toast = useToast();
  const [secret, setSecret] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [copied, setCopied] = useState(false);
  const timer = useRef<number | null>(null);

  const clearTimer = () => {
    if (timer.current !== null) {
      window.clearTimeout(timer.current);
      timer.current = null;
    }
  };

  const hide = () => {
    clearTimer();
    setSecret(null);
  };

  useEffect(() => clearTimer, []);

  const show = async () => {
    if (busy) return;
    setBusy(true);
    try {
      const value = await reveal();
      if (!value) {
        toast.push({ tone: "error", title: "Nothing to reveal" });
        return;
      }
      setSecret(value);
      clearTimer();
      timer.current = window.setTimeout(() => {
        timer.current = null;
        setSecret(null);
      }, SECRET_REVEAL_MS);
    } catch {
      // The caller maps its own error vocabulary; the screen only says the
      // reveal did not happen, never why in AWS's words.
      toast.push({
        tone: "error",
        title: "Could not reveal",
        description: "Try again or recreate the value.",
      });
    } finally {
      setBusy(false);
    }
  };

  const copy = async () => {
    if (secret === null) return;
    const ok = await copyText(secret);
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

  const revealed = secret !== null;

  return (
    <div className={clsx("min-w-0", className)}>
      <div className="mb-1 flex items-baseline justify-between gap-2">
        <span className="text-2xs text-ink-300">{label}</span>
        <span className="text-2xs text-ink-400">
          {revealed ? `Hides in ${SECRET_REVEAL_MS / 1000}s` : "Hidden"}
        </span>
      </div>
      <div className="flex min-w-0 items-center gap-1.5 rounded-control border border-ink-500 bg-surface-sunken px-2 py-1">
        <span
          className="min-w-0 flex-1 truncate font-mono text-xs text-ink-100"
        >
          <span aria-hidden={revealed ? undefined : "true"} data-testid="secret-value">
            {revealed ? secret : `${prefix}${MASK}${suffix}`}
          </span>
        </span>
        <button
          type="button"
          onClick={revealed ? hide : show}
          disabled={busy}
          aria-label={revealed ? `Hide ${label}` : `Reveal ${label}`}
          title={revealed ? "Hide" : "Reveal"}
          className="btn-icon"
        >
          {busy ? (
            <Loader2 size={14} aria-hidden="true" className="animate-spin motion-reduce:animate-none" />
          ) : revealed ? (
            <EyeOff size={14} aria-hidden="true" />
          ) : (
            <Eye size={14} aria-hidden="true" />
          )}
        </button>
        <button
          type="button"
          onClick={copy}
          disabled={!revealed}
          aria-label={`Copy ${label}`}
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
