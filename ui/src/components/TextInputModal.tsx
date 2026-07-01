import { useState } from "react";
import { Modal } from "./Modal";
import { ErrorBox } from "@/components/feedback";

// TextInputModal — a minimal single-text-field dialog on the accessible Modal
// base (companion to ConfirmDialog). Used where an action needs one short
// string before it runs (e.g. "Add service"). Enter submits; the confirm
// button is disabled while empty or busy; busy guards close.
export function TextInputModal({
  title,
  label,
  placeholder,
  confirmLabel = "Save",
  cancelLabel = "Cancel",
  maxLength,
  busy,
  error,
  onSubmit,
  onClose,
}: {
  title: string;
  label: string;
  placeholder?: string;
  confirmLabel?: string;
  cancelLabel?: string;
  maxLength?: number;
  busy?: boolean;
  error?: Error | null;
  onSubmit: (value: string) => void;
  onClose: () => void;
}) {
  const [value, setValue] = useState("");
  const submit = () => {
    const v = value.trim();
    if (v) onSubmit(v);
  };
  return (
    <Modal
      title={title}
      onClose={onClose}
      closeDisabled={busy}
      size="sm"
      footer={
        <>
          <button className="btn" onClick={onClose} disabled={busy}>
            {cancelLabel}
          </button>
          <button
            className="btn btn-primary"
            disabled={busy || !value.trim()}
            onClick={submit}
          >
            {busy ? "Working…" : confirmLabel}
          </button>
        </>
      }
    >
      <form
        className="space-y-2"
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        <label className="field-label" htmlFor="text-input-modal-field">
          {label}
        </label>
        <input
          id="text-input-modal-field"
          className="input w-full"
          autoFocus
          value={value}
          maxLength={maxLength}
          placeholder={placeholder}
          disabled={busy}
          onChange={(e) => setValue(e.target.value)}
          aria-label={label}
        />
        {error && <ErrorBox error={error} />}
      </form>
    </Modal>
  );
}
