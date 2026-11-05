import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import clsx from "clsx";
import {
  Check,
  CheckCircle2,
  Pencil,
  Plus,
  RefreshCw,
  Trash2,
  X,
  XCircle,
} from "lucide-react";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { Pill } from "@/components/Pill";
import { useToast } from "@/components/toastContext";
import {
  api,
  awsErrorFrom,
  type AwsAccount,
  type AwsSettings,
} from "@/lib/api";
import {
  accountRow,
  optionalDataSummary,
  type AwsAccountRow,
} from "@/lib/awsSettings";

// AwsAccountsCard — every connected account as one row: who it is, whether the
// last check worked, what it can read, which regions it covers and how much of
// its API budget is in use. The denied service is named only in the row where a
// denial actually happened.
export function AwsAccountsCard({
  settings,
  canWrite,
  onConnect,
  onReconnect,
}: {
  settings: AwsSettings;
  canWrite: boolean;
  onConnect: () => void;
  onReconnect: (account: AwsAccount) => void;
}) {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [renaming, setRenaming] = useState<{ id: string; value: string } | null>(null);
  const [removing, setRemoving] = useState<AwsAccount | null>(null);
  const now = new Date();

  const invalidate = () =>
    queryClient.invalidateQueries({ queryKey: ["aws", "settings"] });

  const recheck = useMutation({
    mutationFn: (accountId: string) => api.recheckAwsAccount(accountId),
    onSuccess: async () => {
      toast.push({ tone: "ok", title: "Recheck started" });
      await invalidate();
    },
    onError: (e) => {
      const info = awsErrorFrom(e);
      toast.push({ tone: "error", title: info.title, description: info.detail });
    },
  });

  const rename = useMutation({
    mutationFn: (input: { accountId: string; name: string }) =>
      api.updateAwsAccount(input.accountId, { display_name: input.name }),
    onSuccess: async () => {
      setRenaming(null);
      toast.push({ tone: "ok", title: "Account renamed" });
      await invalidate();
    },
    onError: (e) => {
      const info = awsErrorFrom(e);
      toast.push({ tone: "error", title: info.title, description: info.detail });
    },
  });

  const remove = useMutation({
    mutationFn: (accountId: string) => api.deleteAwsAccount(accountId),
    onSuccess: async () => {
      setRemoving(null);
      toast.push({ tone: "ok", title: "Account removed" });
      await invalidate();
    },
    onError: (e) => {
      const info = awsErrorFrom(e);
      toast.push({ tone: "error", title: info.title, description: info.detail });
    },
  });

  const rows = settings.accounts.map((account) => ({
    account,
    row: accountRow(account, now),
  }));

  return (
    <section className="card min-w-0" aria-labelledby="aws-accounts-title">
      <div className="card-header">
        <h2 id="aws-accounts-title" className="card-title">
          Accounts
        </h2>
        {canWrite && (
          <button type="button" className="btn btn-primary-strong" onClick={onConnect}>
            <Plus size={13} aria-hidden="true" />
            Add account
          </button>
        )}
      </div>

      {rows.length === 0 ? (
        <div className="card-body">
          <p className="text-xs text-ink-200">No AWS accounts connected.</p>
          <p className="mt-1 text-2xs text-ink-300">
            {canWrite
              ? "Connect a read-only role to see what Versus can read before it reads anything."
              : "An admin can connect an account with a read-only role."}
          </p>
        </div>
      ) : (
        <>
          <div
            aria-hidden="true"
            className="hidden gap-3 px-4 py-2 text-2xs uppercase tracking-wider text-ink-300 sm:grid sm:grid-cols-[minmax(0,1.5fr)_minmax(0,1fr)_minmax(0,1fr)_minmax(0,0.9fr)_minmax(0,1fr)]"
          >
            <span>Account</span>
            <span>Status</span>
            <span>Services</span>
            <span>Regions</span>
            <span>API use</span>
          </div>
          <ul aria-label="AWS accounts" className="divide-y divide-ink-500/30">
            {rows.map(({ account, row }) => (
              <AccountRow
                key={account.account_id}
                account={account}
                row={row}
                canWrite={canWrite}
                renaming={renaming}
                setRenaming={setRenaming}
                rename={rename}
                recheck={recheck}
                onRemove={() => setRemoving(account)}
                onReconnect={() => onReconnect(account)}
              />
            ))}
          </ul>
        </>
      )}

      <div className="border-t border-ink-500/40 px-4 py-2 text-2xs text-ink-300">
        Optional data: {optionalDataSummary(settings.optional_data, settings.accounts.length)}
      </div>

      {removing && (
        <ConfirmDialog
          title="Remove account?"
          message={
            <>
              {removing.display_name} ({removing.account_id}) will be removed
              from Versus, together with its health record. The IAM role in AWS
              is left untouched.
            </>
          }
          confirmLabel="Remove"
          tone="danger"
          busy={remove.isPending}
          onConfirm={() => remove.mutate(removing.account_id)}
          onClose={() => setRemoving(null)}
        />
      )}
    </section>
  );
}

function AccountRow({
  account,
  row,
  canWrite,
  renaming,
  setRenaming,
  rename,
  recheck,
  onRemove,
  onReconnect,
}: {
  account: AwsAccount;
  row: AwsAccountRow;
  canWrite: boolean;
  renaming: { id: string; value: string } | null;
  setRenaming: (value: { id: string; value: string } | null) => void;
  rename: {
    mutate: (input: { accountId: string; name: string }) => void;
    isPending: boolean;
  };
  recheck: {
    mutate: (accountId: string) => void;
    isPending: boolean;
    variables?: string;
  };
  onRemove: () => void;
  onReconnect: () => void;
}) {
  const editing = renaming?.id === account.account_id;
  const rechecking = recheck.isPending && recheck.variables === account.account_id;

  return (
    <li
      aria-label={`${row.name} ${row.accountId}`}
      className="px-4 py-3"
    >
      <div className="grid min-w-0 grid-cols-1 gap-2 sm:grid-cols-[minmax(0,1.5fr)_minmax(0,1fr)_minmax(0,1fr)_minmax(0,0.9fr)_minmax(0,1fr)] sm:items-center sm:gap-3">
        <div className="min-w-0">
          <div className="flex min-w-0 flex-wrap items-center gap-1.5">
            {editing && renaming ? (
              <span className="flex min-w-0 items-center gap-1">
                <label htmlFor={`aws-rename-${account.account_id}`} className="sr-only">
                  Account name
                </label>
                <input
                  id={`aws-rename-${account.account_id}`}
                  className="input h-7 min-w-0 flex-1"
                  value={renaming.value}
                  autoFocus
                  onChange={(e) =>
                    setRenaming({ id: account.account_id, value: e.target.value })
                  }
                  onKeyDown={(e) => {
                    if (e.key === "Enter" && renaming.value.trim()) {
                      rename.mutate({
                        accountId: account.account_id,
                        name: renaming.value.trim(),
                      });
                    }
                    if (e.key === "Escape") setRenaming(null);
                  }}
                />
                <button
                  type="button"
                  className="btn-icon"
                  aria-label="Save name"
                  disabled={!renaming.value.trim() || rename.isPending}
                  onClick={() =>
                    rename.mutate({
                      accountId: account.account_id,
                      name: renaming.value.trim(),
                    })
                  }
                >
                  <Check size={13} aria-hidden="true" />
                </button>
                <button
                  type="button"
                  className="btn-icon"
                  aria-label="Cancel rename"
                  onClick={() => setRenaming(null)}
                >
                  <X size={13} aria-hidden="true" />
                </button>
              </span>
            ) : (
              <>
                <span className="truncate text-xs font-medium text-ink-50">
                  {row.name}
                </span>
                {row.fromConfig && <Pill>From config</Pill>}
                {canWrite && account.source === "wizard" && (
                  <>
                    <button
                      type="button"
                      className="btn-icon"
                      aria-label={`Rename ${row.name}`}
                      title="Rename"
                      onClick={() =>
                        setRenaming({ id: account.account_id, value: row.name })
                      }
                    >
                      <Pencil size={12} aria-hidden="true" />
                    </button>
                    <button
                      type="button"
                      className="btn-icon"
                      aria-label={`Remove ${row.name}`}
                      title="Remove"
                      onClick={onRemove}
                    >
                      <Trash2 size={12} aria-hidden="true" />
                    </button>
                  </>
                )}
              </>
            )}
          </div>
          <p className="mt-0.5 font-mono text-2xs text-ink-300">{row.accountId}</p>
        </div>

        <div className="min-w-0 text-xs">
          <span className="flex items-center gap-1.5 whitespace-nowrap">
            {row.connected ? (
              <CheckCircle2
                size={12}
                aria-hidden="true"
                className="shrink-0 text-sev-ok"
              />
            ) : (
              <XCircle
                size={12}
                aria-hidden="true"
                className="shrink-0 text-sev-critical"
              />
            )}
            <span
              className={clsx(
                "font-medium",
                row.connected ? "text-ink-100" : "text-sev-critical",
              )}
            >
              {row.statusLabel}
            </span>
          </span>
          <p className="mt-0.5 text-2xs text-ink-300">{row.statusDetail}</p>
          {canWrite && (
            <div className="mt-1 flex flex-wrap items-center gap-1.5">
              {!row.connected && (
                <button type="button" className="btn" onClick={onReconnect}>
                  Reconnect
                </button>
              )}
              <button
                type="button"
                className="btn"
                aria-label={`Recheck ${row.name}`}
                disabled={rechecking}
                onClick={() => recheck.mutate(account.account_id)}
              >
                <RefreshCw
                  size={11}
                  aria-hidden="true"
                  className={rechecking ? "animate-spin motion-reduce:animate-none" : undefined}
                />
                {rechecking ? "Rechecking…" : "Recheck"}
              </button>
            </div>
          )}
        </div>

        <div className="min-w-0 text-xs">
          <span className="text-2xs text-ink-300 sm:hidden">Services: </span>
          <span className="text-ink-100">{row.servicesLabel}</span>
          {row.deniedLabel && (
            <p className="mt-0.5 text-2xs font-medium text-sev-critical">
              {row.deniedLabel}
            </p>
          )}
        </div>

        <div className="min-w-0 text-xs">
          <span className="text-2xs text-ink-300 sm:hidden">Regions: </span>
          <span className="whitespace-nowrap text-ink-100">{row.regionsLabel}</span>
          <p className="mt-0.5 text-2xs text-ink-300">{row.regionsDetail}</p>
        </div>

        <div className="min-w-0 text-xs">
          <div className="flex flex-col items-start gap-1">
            <span
              aria-hidden="true"
              className="h-1 w-14 shrink-0 overflow-hidden rounded-full bg-ink-600"
            >
              <span
                className="block h-full rounded-full bg-accent"
                style={{ width: `${Math.round(row.apiFraction * 100)}%` }}
              />
            </span>
            <span className="whitespace-nowrap text-2xs text-ink-200">{row.apiLabel}</span>
          </div>
        </div>
      </div>
    </li>
  );
}
