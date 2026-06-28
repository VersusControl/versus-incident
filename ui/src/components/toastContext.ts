import { createContext, useContext } from "react";

export interface ToastInput {
  title: string;
  description?: string;
  tone?: "ok" | "error" | "info";
  /** Optional action button (e.g. Undo / Retry). */
  action?: { label: string; onClick: () => void };
  /** ms; defaults 4s, errors 6s. */
  duration?: number;
}

export const ToastCtx = createContext<{ push: (t: ToastInput) => void } | null>(
  null,
);

export function useToast() {
  const ctx = useContext(ToastCtx);
  if (!ctx) throw new Error("useToast must be used inside <ToastProvider>");
  return ctx;
}
