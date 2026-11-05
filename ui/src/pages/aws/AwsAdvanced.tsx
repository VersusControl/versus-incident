import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ChevronDown, Lock } from "lucide-react";
import { Pill } from "@/components/Pill";
import { useToast } from "@/components/toastContext";
import {
  api,
  awsErrorFrom,
  type AwsOptionalDataKey,
  type AwsSettings,
} from "@/lib/api";
import { OPTIONAL_DATA_KEYS, OPTIONAL_DATA_LABELS } from "@/lib/awsSettings";

// The limiter reports fractional usage; keep one decimal only when needed so
// the line never reads "1.5000000001".
function rpsWords(value: number): string {
  if (!Number.isFinite(value)) return "0";
  return Number.isInteger(value) ? String(value) : value.toFixed(1);
}

// AwsAdvanced — the quiet settings: the API budget, the opt-in paid reads and
// the tag-value allowlist. Values that came from configuration are shown
// read-only with a "From config" pill; unset or wizard-owned values stay
// editable, and only by an admin.
export function AwsAdvanced({
  settings,
  canWrite,
}: {
  settings: AwsSettings;
  canWrite: boolean;
}) {
  const queryClient = useQueryClient();
  const toast = useToast();
  const budget = settings.budget;
  const budgetFromConfig = budget?.source === "config";
  const allowlistFromConfig =
    settings.privacy.tag_value_allowlist_source === "config";
  const [budgetDraft, setBudgetDraft] = useState(
    String(budget?.budget_rps ?? 2),
  );
  const [allowlistDraft, setAllowlistDraft] = useState(
    settings.privacy.tag_value_allowlist.join(", "),
  );

  const invalidate = () =>
    queryClient.invalidateQueries({ queryKey: ["aws", "settings"] });

  const saveOptions = useMutation({
    mutationFn: (body: {
      optional_data?: Partial<Record<AwsOptionalDataKey, boolean>>;
      max_requests_per_second?: number;
    }) => api.updateAwsOptions(body),
    onSuccess: async () => {
      toast.push({ tone: "ok", title: "Saved" });
      await invalidate();
    },
    onError: (e) => {
      const info = awsErrorFrom(e);
      toast.push({ tone: "error", title: info.title, description: info.detail });
    },
  });

  const savePrivacy = useMutation({
    mutationFn: (tags: string[]) =>
      api.updateAwsPrivacy({ tag_value_allowlist: tags }),
    onSuccess: async () => {
      toast.push({ tone: "ok", title: "Allowlist saved" });
      await invalidate();
    },
    onError: (e) => {
      const info = awsErrorFrom(e);
      toast.push({ tone: "error", title: info.title, description: info.detail });
    },
  });

  const budgetNumber = Number(budgetDraft);
  const budgetValid =
    Number.isFinite(budgetNumber) && budgetNumber >= 1 && budgetNumber <= 100;
  const budgetChanged =
    budget !== undefined && budgetNumber !== budget.budget_rps;

  const parsedAllowlist = allowlistDraft
    .split(",")
    .map((tag) => tag.trim())
    .filter((tag) => tag.length > 0);
  const allowlistChanged =
    parsedAllowlist.join(",") !== settings.privacy.tag_value_allowlist.join(",");

  return (
    <details className="card min-w-0">
      <summary className="card-header cursor-pointer list-none select-none">
        <span className="flex items-center gap-2 text-sm font-semibold text-ink-50">
          Advanced
          <ChevronDown size={14} aria-hidden="true" className="text-ink-300" />
        </span>
        <span className="text-2xs text-ink-300">
          Budget, optional data, tag allowlist
        </span>
      </summary>

      <div className="card-body space-y-5">
        <div className="min-w-0">
          <h3 className="flex items-center gap-1.5 text-xs font-medium text-ink-100">
            API budget
            {budgetFromConfig && (
              <span className="inline-flex items-center gap-1">
                <Pill>From config</Pill>
                <Lock size={11} aria-hidden="true" className="text-ink-400" />
              </span>
            )}
          </h3>
          <p className="mt-0.5 text-2xs text-ink-300">
            Requests per second per account and region. Probes and, later,
            collectors share this budget.
          </p>
          {budget && (
            <p className="mt-0.5 text-2xs text-ink-200 tabular-nums">
              In use {rpsWords(budget.used_rps)} of {budget.budget_rps} req/s
            </p>
          )}
          <div className="mt-1.5 flex flex-wrap items-end gap-2">
            <div>
              <label htmlFor="aws-budget" className="field-label">
                Requests per second
              </label>
              <input
                id="aws-budget"
                type="number"
                min={1}
                max={100}
                className="input w-24 tabular-nums"
                value={budgetDraft}
                disabled={!canWrite || budgetFromConfig || saveOptions.isPending}
                onChange={(e) => setBudgetDraft(e.target.value)}
              />
            </div>
            {canWrite && !budgetFromConfig && (
              <button
                type="button"
                className="btn mb-0.5"
                disabled={!budgetValid || !budgetChanged || saveOptions.isPending}
                onClick={() =>
                  saveOptions.mutate({ max_requests_per_second: budgetNumber })
                }
              >
                Save budget
              </button>
            )}
          </div>
        </div>

        <div className="min-w-0">
          <h3 className="text-xs font-medium text-ink-100">Optional data</h3>
          <p className="mt-0.5 text-2xs text-ink-300">
            Paid reads are off by default. Turning one on is the only way that
            data reaches Versus.
          </p>
          <ul className="mt-1.5 space-y-1.5 text-xs">
            {OPTIONAL_DATA_KEYS.map((key) => {
              const entry = settings.optional_data[key];
              const on = (entry?.on ?? 0) > 0;
              const fromConfig = entry?.source === "config";
              return (
                <li key={key} className="flex flex-wrap items-center gap-2">
                  <label className="flex items-center gap-2">
                    <input
                      type="checkbox"
                      checked={on}
                      disabled={!canWrite || fromConfig || saveOptions.isPending}
                      onChange={(e) =>
                        saveOptions.mutate({ optional_data: { [key]: e.target.checked } })
                      }
                      aria-label={`Read ${OPTIONAL_DATA_LABELS[key]} data`}
                    />
                    <span className="text-ink-100">{OPTIONAL_DATA_LABELS[key]}</span>
                  </label>
                  {fromConfig && <Pill>From config</Pill>}
                  <span className="text-2xs text-ink-300">
                    on in {entry?.on ?? 0}{" "}
                    {(entry?.on ?? 0) === 1 ? "account" : "accounts"}
                  </span>
                </li>
              );
            })}
          </ul>
        </div>

        <div className="min-w-0">
          <h3 className="flex items-center gap-1.5 text-xs font-medium text-ink-100">
            Tag value allowlist
            {allowlistFromConfig && (
              <span className="inline-flex items-center gap-1">
                <Pill>From config</Pill>
                <Lock size={11} aria-hidden="true" className="text-ink-400" />
              </span>
            )}
          </h3>
          <p className="mt-0.5 text-2xs text-ink-300">
            Comma-separated tag keys whose values may be shared with the model.
          </p>
          <div className="mt-1.5 flex flex-wrap items-end gap-2">
            <div className="min-w-0 flex-1">
              <label htmlFor="aws-allowlist" className="field-label">
                Tag keys
              </label>
              <input
                id="aws-allowlist"
                className="input font-mono"
                value={allowlistDraft}
                disabled={!canWrite || allowlistFromConfig || savePrivacy.isPending}
                onChange={(e) => setAllowlistDraft(e.target.value)}
                placeholder="app, team, env"
              />
            </div>
            {canWrite && !allowlistFromConfig && (
              <button
                type="button"
                className="btn mb-0.5"
                disabled={!allowlistChanged || savePrivacy.isPending}
                onClick={() => savePrivacy.mutate(parsedAllowlist)}
              >
                Save allowlist
              </button>
            )}
          </div>
        </div>
      </div>
    </details>
  );
}
