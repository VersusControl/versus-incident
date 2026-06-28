import {
  useCallback,
  useMemo,
  useRef,
  useState,
} from "react";
import { AlertTriangle, CheckCircle2, Info, X } from "lucide-react";
import clsx from "clsx";
import { ToastCtx, type ToastInput } from "./toastContext";

// Toast system — the answer to the audit's "mutations fail silently" class
// (S3): every mutation outcome gets a visible, screen-reader-announced
// confirmation. aria-live="polite" so toasts never steal focus; optional
// action slot powers Undo for optimistic updates.

interface ToastItem extends ToastInput {
  id: number;
}

export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([]);
  const nextId = useRef(1);

  const dismiss = useCallback((id: number) => {
    setItems((cur) => cur.filter((t) => t.id !== id));
  }, []);

  const push = useCallback(
    (t: ToastInput) => {
      const id = nextId.current++;
      setItems((cur) => [...cur.slice(-3), { ...t, id }]);
      const ms = t.duration ?? (t.tone === "error" ? 6000 : 4000);
      window.setTimeout(() => dismiss(id), ms);
    },
    [dismiss],
  );

  const value = useMemo(() => ({ push }), [push]);

  return (
    <ToastCtx.Provider value={value}>
      {children}
      <div
        aria-live="polite"
        aria-atomic="false"
        className="pointer-events-none fixed bottom-4 right-4 z-toast flex w-80 flex-col gap-2"
      >
        {items.map((t) => (
          <div
            key={t.id}
            className={clsx(
              "pointer-events-auto flex items-start gap-2 rounded-card border p-3 shadow-overlay",
              "motion-safe:animate-[toast-in_180ms_ease-out] bg-surface-raised",
              t.tone === "error"
                ? "border-sev-critical/40"
                : t.tone === "ok"
                  ? "border-sev-ok/40"
                  : "border-ink-500",
            )}
          >
            {t.tone === "error" ? (
              <AlertTriangle size={14} className="mt-0.5 shrink-0 text-sev-critical" />
            ) : t.tone === "ok" ? (
              <CheckCircle2 size={14} className="mt-0.5 shrink-0 text-sev-ok" />
            ) : (
              <Info size={14} className="mt-0.5 shrink-0 text-sev-info" />
            )}
            <div className="min-w-0 flex-1">
              <div className="text-xs font-medium text-ink-50">{t.title}</div>
              {t.description && (
                <div className="mt-0.5 break-words text-2xs text-ink-300">
                  {t.description}
                </div>
              )}
              {t.action && (
                <button
                  className="mt-1.5 text-2xs font-medium text-link hover:underline"
                  onClick={() => {
                    t.action!.onClick();
                    dismiss(t.id);
                  }}
                >
                  {t.action.label}
                </button>
              )}
            </div>
            <button
              aria-label="Dismiss notification"
              className="shrink-0 rounded p-0.5 text-ink-400 hover:text-ink-100"
              onClick={() => dismiss(t.id)}
            >
              <X size={12} />
            </button>
          </div>
        ))}
      </div>
    </ToastCtx.Provider>
  );
}
