import { useEffect, useId, useRef, useState, type FocusEvent, type KeyboardEvent, type PointerEvent } from "react";
import { Check, Copy, ExternalLink, Lightbulb } from "lucide-react";
import clsx from "clsx";

export const DEVOPS_AGENT_DOCS_URL = "https://devops-agent.versusincident.com";

// Action proposals cannot be approved from the web chat; they run only from the
// Versus DevOps Agent CLI. Where the chat would have shown an approval card it
// shows this small icon instead. Hover, keyboard focus or a tap opens a popover
// with the CLI command for this approval and a link to the agent docs.
//
// Hover and focus open the popover transiently; a click or tap pins it open so
// it survives the pointer leaving. Touch pointers skip the hover path so a tap
// does not open and then immediately toggle the popover closed. Escape, focus
// leaving the widget, or a pointer press outside it closes the popover.
export function ActionCliHint({ approvalId }: { approvalId: string }) {
  const [open, setOpen] = useState(false);
  const [pinned, setPinned] = useState(false);
  const [copied, setCopied] = useState(false);
  const rootRef = useRef<HTMLDivElement | null>(null);
  const buttonRef = useRef<HTMLButtonElement | null>(null);
  const closeTimer = useRef<number | undefined>(undefined);
  const returningFocus = useRef(false);
  const popoverId = useId();
  const titleId = useId();
  const command = `versus-devops approvals approve ${approvalId}`;

  const cancelClose = () => window.clearTimeout(closeTimer.current);
  const close = () => {
    cancelClose();
    setOpen(false);
    setPinned(false);
    setCopied(false);
  };

  useEffect(() => () => window.clearTimeout(closeTimer.current), []);

  useEffect(() => {
    if (!open) return;
    const dismiss = () => {
      window.clearTimeout(closeTimer.current);
      setOpen(false);
      setPinned(false);
      setCopied(false);
    };
    const onPointerDown = (event: globalThis.PointerEvent) => {
      if (!rootRef.current?.contains(event.target as Node)) dismiss();
    };
    // Escape from inside the widget is handled locally so focus can return to
    // the icon; this covers a popover opened by hover while focus is elsewhere.
    const onDocumentKeyDown = (event: globalThis.KeyboardEvent) => {
      if (event.key === "Escape" && !rootRef.current?.contains(document.activeElement)) dismiss();
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onDocumentKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onDocumentKeyDown);
    };
  }, [open]);

  const onPointerEnter = (event: PointerEvent) => {
    if (event.pointerType === "touch") return;
    cancelClose();
    setOpen(true);
  };
  const onPointerLeave = (event: PointerEvent) => {
    if (event.pointerType === "touch" || pinned) return;
    cancelClose();
    closeTimer.current = window.setTimeout(close, 150);
  };
  const onBlur = (event: FocusEvent) => {
    if (!rootRef.current?.contains(event.relatedTarget as Node | null)) close();
  };
  const onKeyDown = (event: KeyboardEvent) => {
    if (event.key !== "Escape" || !open) return;
    event.preventDefault();
    event.stopPropagation();
    close();
    // Returning focus to the icon must not reopen the popover it just closed.
    returningFocus.current = true;
    buttonRef.current?.focus();
    returningFocus.current = false;
  };
  const onClick = () => {
    cancelClose();
    if (open && pinned) {
      close();
      return;
    }
    setOpen(true);
    setPinned(true);
  };
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(command);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  };

  return (
    <div
      ref={rootRef}
      className="relative my-2 w-fit"
      onPointerEnter={onPointerEnter}
      onPointerLeave={onPointerLeave}
      onBlur={onBlur}
      onKeyDown={onKeyDown}
    >
      <button
        ref={buttonRef}
        type="button"
        aria-label="Actions run from the Versus DevOps Agent CLI"
        aria-expanded={open}
        aria-controls={open ? popoverId : undefined}
        onFocus={() => {
          if (!returningFocus.current) setOpen(true);
        }}
        onClick={onClick}
        className={clsx(
          "inline-flex size-8 items-center justify-center rounded-control hover:bg-ink-700/50",
          open ? "text-accent" : "text-ink-400 hover:text-ink-100",
        )}
      >
        <Lightbulb size={16} aria-hidden />
      </button>
      {open && (
        <div
          id={popoverId}
          role="dialog"
          aria-labelledby={titleId}
          className="absolute left-0 top-full z-dropdown w-72 max-w-[calc(100vw-2rem)] pt-1"
        >
          <div className="rounded-card border border-ink-500/60 bg-surface-raised p-3 text-left shadow-overlay">
            <h3 id={titleId} className="text-xs font-semibold text-ink-50">Actions aren't available in the web UI</h3>
            <p className="mt-1 text-xs leading-5 text-ink-300">To run this action, use the Versus DevOps Agent CLI.</p>
            <div className="mt-2 flex items-start gap-1 rounded-control bg-ink-900 p-2">
              <code className="min-w-0 flex-1 break-all font-mono text-[11px] leading-5 text-ink-100">{command}</code>
              <button
                type="button"
                onClick={() => void copy()}
                aria-label={copied ? "Command copied" : "Copy command"}
                title={copied ? "Copied" : "Copy command"}
                className="inline-flex size-7 shrink-0 items-center justify-center rounded-control text-ink-300 hover:bg-ink-700 hover:text-ink-100"
              >
                {copied ? <Check size={13} className="text-sev-ok" aria-hidden /> : <Copy size={13} aria-hidden />}
              </button>
            </div>
            <span className="sr-only" aria-live="polite">{copied ? "Command copied" : ""}</span>
            <a
              href={DEVOPS_AGENT_DOCS_URL}
              target="_blank"
              rel="noopener noreferrer"
              className="mt-2 inline-flex items-center gap-1 text-xs font-medium text-link hover:underline"
            >
              Read the docs
              <ExternalLink size={12} aria-hidden />
            </a>
          </div>
        </div>
      )}
    </div>
  );
}
