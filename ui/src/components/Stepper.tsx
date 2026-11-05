import { Check } from "lucide-react";
import clsx from "clsx";

export interface Step {
  label: string;
  /** One line of secondary text under the label. */
  description?: string;
}

type StepState = "done" | "current" | "upcoming";

function stateOf(index: number, current: number): StepState {
  if (index < current) return "done";
  if (index === current) return "current";
  return "upcoming";
}

/**
 * Ordered progress steps. The current step carries aria-current="step" and is
 * the only one that reads as active; completed steps show a check so the state
 * is not carried by colour alone.
 *
 * Passing onSelect makes the reachable steps real buttons, so the whole trail
 * can be walked with the keyboard (upcoming steps stay disabled rather than
 * offering a click that would be refused).
 */
export function Stepper({
  steps,
  current,
  onSelect,
  label = "Progress",
  className,
}: {
  steps: Step[];
  /** Zero-based index of the current step. */
  current: number;
  onSelect?: (index: number) => void;
  label?: string;
  className?: string;
}) {
  return (
    <ol
      aria-label={label}
      className={clsx("flex flex-wrap items-center gap-x-2 gap-y-1", className)}
    >
      {steps.map((step, index) => {
        const state = stateOf(index, current);
        const body = (
          <>
            <span
              aria-hidden="true"
              className={clsx(
                "flex size-5 shrink-0 items-center justify-center rounded-full border text-2xs font-medium tabular-nums",
                state === "done" && "border-sev-ok/40 bg-sev-ok/15 text-sev-ok",
                state === "current" && "border-accent bg-accent-subtle text-ink-50",
                state === "upcoming" && "border-ink-500 bg-ink-800 text-ink-400",
              )}
            >
              {state === "done" ? <Check size={11} /> : index + 1}
            </span>
            <span className="flex min-w-0 flex-col text-left">
              <span
                className={clsx(
                  "truncate text-xs",
                  state === "current"
                    ? "font-medium text-ink-50"
                    : state === "done"
                      ? "text-ink-200"
                      : "text-ink-400",
                )}
              >
                {step.label}
              </span>
              {step.description && (
                <span className="truncate text-2xs text-ink-300">
                  {step.description}
                </span>
              )}
            </span>
          </>
        );

        return (
          <li
            key={step.label}
            className="flex min-w-0 items-center gap-2"
            aria-current={state === "current" ? "step" : undefined}
          >
            {onSelect ? (
              <button
                type="button"
                onClick={() => onSelect(index)}
                disabled={state === "upcoming"}
                className={clsx(
                  "btn border-transparent bg-transparent px-1.5 py-1",
                  state === "upcoming" && "text-ink-400",
                )}
              >
                {body}
              </button>
            ) : (
              <span className="flex min-w-0 items-center gap-2 px-1.5 py-1">
                {body}
              </span>
            )}
            {index < steps.length - 1 && (
              <span aria-hidden="true" className="h-px w-4 shrink-0 bg-ink-500" />
            )}
          </li>
        );
      })}
    </ol>
  );
}
