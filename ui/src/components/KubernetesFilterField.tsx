import type { ReactNode } from "react";

/**
 * One filter control in a filter row. Every control a row pairs up uses
 * `filterFieldLabelClass` for its label and `filterFieldControlClass` for the
 * control itself, so label baseline, control top, control height and border box
 * are identical in every column at every width.
 */
export const filterFieldClass = "min-w-0 text-2xs text-ink-400";
export const filterFieldControlClass = "input mt-1 w-full";

/**
 * The one layout contract for a filter row. Every row that pairs two controls
 * uses it, so both columns keep one label baseline, control top, height and
 * border box, and the pair never stacks at a narrow width: two controls fit in
 * one row at every viewport the explorer supports.
 */
export const filterRowClass = "grid min-w-0 grid-cols-2 gap-2";

export function KubernetesFilterField({ label, children, className = "" }: {
  label: string;
  children: ReactNode;
  className?: string;
}) {
  return <label className={`${filterFieldClass} block ${className}`}>
    <span className="block">{label}</span>
    {children}
  </label>;
}
