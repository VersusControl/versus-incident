import { X } from "lucide-react";
import { ErrorBox } from "@/components/feedback";

// ConfirmDialog is a small modal used for destructive / one-way
// actions. Matches the AssignDialog chrome so the operator gets a
// consistent confirmation experience across the app.
export function ConfirmDialog({
  title,
  message,
  confirmLabel = "Confirm",
  cancelLabel = "Cancel",
  tone = "primary",
  busy,
  error,
  onConfirm,
  onClose,
}: {
  title: string;
  message: React.ReactNode;
  confirmLabel?: string;
  cancelLabel?: string;
  tone?: "primary" | "danger";
  busy?: boolean;
  error?: Error | null;
  onConfirm: () => void;
  onClose: () => void;
}) {
  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-ink-900/40 p-4"
      onClick={onClose}
    >
      <div
        className="w-full max-w-md rounded-lg bg-white shadow-xl"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between border-b border-ink-100 px-4 py-3">
          <h2 className="text-sm font-semibold text-ink-900">{title}</h2>
          <button
            className="rounded p-1 text-ink-500 hover:bg-ink-50"
            onClick={onClose}
          >
            <X size={14} />
          </button>
        </div>
        <div className="space-y-3 p-4 text-xs text-ink-700">
          <div>{message}</div>
          {error && <ErrorBox error={error} />}
        </div>
        <div className="flex justify-end gap-2 border-t border-ink-100 px-4 py-3">
          <button className="btn" onClick={onClose} disabled={busy}>
            {cancelLabel}
          </button>
          <button
            className={tone === "danger" ? "btn btn-danger" : "btn btn-primary"}
            disabled={busy}
            onClick={onConfirm}
          >
            {busy ? "Working…" : confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}
