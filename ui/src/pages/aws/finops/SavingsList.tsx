import { useRef, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Modal } from "@/components/Modal";
import { useToast } from "@/components/toastContext";
import { api, finopsErrorFrom, type FinOpsSaving, type FinOpsSavingsView } from "@/lib/api";
import { BASIS_WORDS, EFFORT_WORDS, RISK_WORDS, alternativeLines, commitmentOnlyLine, fmtMoney, fmtSaving, savingsGapText } from "@/lib/finops";
import { Card } from "./parts";

// "Savings found": the only place the savings total appears. Each item is a
// guide the user follows; Versus never applies a saving.

export function SavingsList({ savings, canDismiss = false }: { savings: FinOpsSavingsView; canDismiss?: boolean }) {
  const [dismissing, setDismissing] = useState<FinOpsSaving | null>(null);
  // After a dismissal the item's own button is gone, so focus lands on the card.
  const bodyRef = useRef<HTMLDivElement>(null);
  const items = (savings.items ?? []).filter((s) => s.status !== "dismissed");
  const currency = savings.currency;
  const gaps = savings.gaps ?? [];
  // Only commitments found and none listed here: the commitment line alone.
  const commitmentOnly = commitmentOnlyLine(savings.total, currency);
  return (
    <Card
      title="Savings found"
      aside={
        // The total counts only items it can add up; with none (a commitment
        // priced before other savings is outside it) there is no total, never $0.
        items.length > 0 && savings.total.count > 0 ? (
          <span className="font-mono text-sm font-semibold tabular-nums text-sev-ok">
            {fmtMoney(savings.total.monthly_micros, { currency })}/mo
          </span>
        ) : undefined
      }
    >
      <div ref={bodyRef} tabIndex={-1} className="outline-none">
        {items.length === 0 && commitmentOnly ? (
          <p className="font-mono text-xs tabular-nums text-ink-100">{commitmentOnly}</p>
        ) : items.length === 0 && gaps.length === 0 ? (
          <p className="text-xs text-ink-300">None found</p>
        ) : items.length === 0 ? null : (
          <ul className="space-y-2.5">
            {items.map((s) => (
              <li key={s.id} className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-3">
                <span className="min-w-0 truncate text-xs text-ink-100" title={s.title}>
                  {s.title}
                </span>
                <span className="text-right font-mono text-xs tabular-nums text-ink-100">
                  {s.before_other_savings ? "" : fmtSaving(s, currency)}
                </span>
                {s.before_other_savings && (
                  <span className="col-span-2 font-mono text-2xs tabular-nums text-ink-200">{fmtSaving(s, currency)}</span>
                )}
                <span className="text-2xs text-ink-300">
                  {[
                    EFFORT_WORDS[s.effort] ?? s.effort,
                    RISK_WORDS[s.risk] ?? s.risk,
                    s.count && s.count > 1 && !s.title.includes(String(s.count)) ? `${s.count} resources` : "",
                    BASIS_WORDS[s.basis ?? ""] ?? "",
                  ]
                    .filter(Boolean)
                    .join(" · ")}
                </span>
                <span className="flex items-center justify-end gap-3 text-2xs">
                  {s.guide?.doc_url && (
                    <a
                      href={s.guide.doc_url}
                      target="_blank"
                      rel="noreferrer"
                      className="text-link hover:underline"
                      aria-label={`Guide: ${s.title}`}
                    >
                      Guide ›
                    </a>
                  )}
                  {canDismiss && (
                    <button
                      type="button"
                      className="text-ink-300 hover:text-ink-100 hover:underline"
                      aria-label={`Dismiss ${s.title}`}
                      onClick={() => setDismissing(s)}
                    >
                      Dismiss
                    </button>
                  )}
                </span>
                {alternativeLines(s, currency).map((line) => (
                  <span key={line} className="col-span-2 text-2xs text-ink-400">
                    {line}
                  </span>
                ))}
              </li>
            ))}
          </ul>
        )}
        {gaps.length > 0 && (
          <ul aria-label="Savings checks not run" className="mt-3 space-y-1 border-t border-ink-500/40 pt-2">
            {gaps.map((g) => (
              <li key={`${g.check}-${g.reason}`} className="text-2xs text-ink-300">
                {savingsGapText(g)}
              </li>
            ))}
          </ul>
        )}
      </div>
      {dismissing && (
        <DismissSavingDialog
          saving={dismissing}
          onClose={() => setDismissing(null)}
          onDismissed={() => {
            setDismissing(null);
            window.setTimeout(() => bodyRef.current?.focus(), 0);
          }}
        />
      )}
    </Card>
  );
}

// Dismissing only hides the saving in Versus; nothing is asked of AWS.
function DismissSavingDialog({
  saving,
  onClose,
  onDismissed,
}: {
  saving: FinOpsSaving;
  onClose: () => void;
  onDismissed: () => void;
}) {
  const qc = useQueryClient();
  const toast = useToast();
  const [reason, setReason] = useState("");
  const dismiss = useMutation({
    mutationFn: () => api.finopsDismissSaving(saving.id, reason.trim()),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["finops"] });
      toast.push({ tone: "ok", title: "Saving dismissed" });
      onDismissed();
    },
    onError: (e) => {
      const info = finopsErrorFrom(e, "local");
      toast.push({ tone: "error", title: info.title, description: info.detail });
    },
  });
  const valid = reason.trim() !== "";
  const formId = `dismiss-saving-${saving.id}`;
  return (
    <Modal
      title="Dismiss saving"
      onClose={onClose}
      closeDisabled={dismiss.isPending}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={dismiss.isPending}>
            Cancel
          </button>
          <button type="submit" form={formId} className="btn btn-primary-strong" disabled={!valid || dismiss.isPending}>
            Dismiss
          </button>
        </>
      }
    >
      <form
        id={formId}
        className="space-y-3"
        onSubmit={(e) => {
          e.preventDefault();
          if (valid && !dismiss.isPending) dismiss.mutate();
        }}
      >
        <p className="truncate text-xs text-ink-100" title={saving.title}>
          {saving.title}
        </p>
        <label className="block">
          <span className="field-label">Reason</span>
          <input
            className="input w-full"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            maxLength={200}
            required
            aria-required="true"
          />
        </label>
      </form>
    </Modal>
  );
}
