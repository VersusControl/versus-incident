import { Modal } from "./Modal";
import { ErrorBox } from "@/components/feedback";

// ConfirmDialog — destructive / one-way confirmations, now on the
// accessible Modal base (role=dialog, focus trap, Escape). Behavior kept:
// busy state disables both actions and guards close while pending.
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
    <Modal
      title={title}
      onClose={onClose}
      closeDisabled={busy}
      size="md"
      footer={
        <>
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
        </>
      }
    >
      <div className="space-y-3 text-xs text-ink-200">
        <div>{message}</div>
        {error && <ErrorBox error={error} />}
      </div>
    </Modal>
  );
}
