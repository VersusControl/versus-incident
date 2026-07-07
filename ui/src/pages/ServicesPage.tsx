import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useSearchParams } from "react-router-dom";
import { Plus, Search, Trash2, Eye } from "lucide-react";
import { api } from "@/lib/api";
import { fmtAbs, fmtRel } from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { Pill } from "@/components/Pill";
import { InfoHint } from "@/components/InfoHint";
import { SegmentedControl } from "@/components/SegmentedControl";
import { AutoRefreshControl } from "@/components/AutoRefreshControl";
import { useAutoRefresh } from "@/lib/useAutoRefresh";
import { EmptyState } from "@/components/feedback";
import { RetryableError } from "@/components/RetryableError";
import { SkRows } from "@/components/Skeleton";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { TextInputModal } from "@/components/TextInputModal";
import { Pagination } from "@/components/Pagination";
import { PeekPanel, PeekField } from "@/components/PeekPanel";
import { usePagination } from "@/lib/pagination";
import { useToast } from "@/components/toastContext";
import {
  BulkActionBar,
  RowSelectCheckbox,
  SelectAllCheckbox,
  type ActionBarItem,
} from "@/components/BulkActionBar";
import { useBulkSelection } from "@/lib/useBulkSelection";
import {
  useIntelLicensed,
  useLearnExclusions,
} from "@/lib/useLearnExclusions";
import {
  countExcluded,
  filterByScope,
  isExclusionScope,
  SCOPE_PARAM,
} from "@/lib/rowActions";
import {
  GRACE_ACTION_LABEL,
  graceActionsForSelection,
  graceRemainingLabel,
  type GraceAction,
} from "@/lib/serviceGrace";

export function ServicesPage() {
  const qc = useQueryClient();
  const toast = useToast();
  const refresh = useAutoRefresh();
  const { data, isLoading, isError, error, refetch, isRefetching } = useQuery({
    queryKey: ["services"],
    queryFn: api.listServices,
    refetchInterval: refresh.refetchInterval,
  });

  const [q, setQ] = useState("");
  const [params] = useSearchParams();
  const scope = isExclusionScope(params.get(SCOPE_PARAM));

  // Eye → peek slide-out. Rows never navigate; the eye opens a read-only peek
  // of the key service facts and the peek footer links to the full detail
  // page. The list row carries origin/grace/first-seen; the pattern & incident
  // counts come from the service-detail read (fetched on open, same shape the
  // full page uses), so the peek shows a loading state until they arrive.
  const [peekName, setPeekName] = useState<string | null>(null);
  const peekInfo = peekName ? data?.[peekName] : undefined;
  const peekDetail = useQuery({
    queryKey: ["service-detail", peekName],
    queryFn: () => api.getServiceDetail(peekName as string),
    enabled: !!peekName,
  });

  // Enterprise Disable-Learn ("ignore") controls — gated to a licensed admin,
  // hidden on community / viewer. Drives the Ignore/Resume bulk actions and the
  // Active | Ignored scope toggle.
  const licensed = useIntelLicensed();
  const ignore = useLearnExclusions(licensed);

  const control = useMutation({
    mutationFn: ({ name, action }: { name: string; action: GraceAction }) =>
      api.controlGrace(name, action),
    onSuccess: (_data, { name, action }) => {
      qc.invalidateQueries({ queryKey: ["services"] });
      toast.push({
        tone: "ok",
        title:
          action === "end"
            ? `Grace ended for ${name}`
            : `Grace restarted for ${name}`,
      });
    },
    onError: (err, vars) => {
      toast.push({
        tone: "error",
        title:
          vars.action === "end"
            ? `Couldn't end grace for ${vars.name}`
            : `Couldn't restart grace for ${vars.name}`,
        description: err.message,
        action: { label: "Retry", onClick: () => control.mutate(vars) },
      });
    },
  });

  // Manual service create / rename / delete.
  const [showAdd, setShowAdd] = useState(false);
  const [confirmClear, setConfirmClear] = useState(false);
  const [renameTarget, setRenameTarget] = useState<string | null>(null);
  const [confirmBulkDelete, setConfirmBulkDelete] = useState<string[] | null>(
    null,
  );

  const createService = useMutation({
    mutationFn: (name: string) => api.createService(name),
    onSuccess: (_d, name) => {
      setShowAdd(false);
      qc.invalidateQueries({ queryKey: ["services"] });
      toast.push({ tone: "ok", title: `Service "${name}" created` });
    },
    onError: (err) =>
      toast.push({
        tone: "error",
        title: "Couldn't create service",
        description: err.message,
      }),
  });

  const renameService = useMutation({
    mutationFn: (v: { from: string; to: string }) =>
      api.renameService(v.from, v.to),
    onSuccess: (res, v) => {
      setRenameTarget(null);
      qc.invalidateQueries({ queryKey: ["services"] });
      qc.invalidateQueries({ queryKey: ["service-overrides"] });
      toast.push({
        tone: "ok",
        title: `Renamed to "${v.to}"`,
        description:
          res.overrides_repointed > 0
            ? `${res.overrides_repointed} override rule(s) repointed`
            : undefined,
      });
    },
    onError: (err) =>
      toast.push({
        tone: "error",
        title: "Couldn't rename service",
        description: err.message,
      }),
  });

  const deleteService = useMutation({
    mutationFn: (name: string) => api.deleteService(name),
    onSuccess: (_d, name) => {
      qc.invalidateQueries({ queryKey: ["services"] });
      toast.push({ tone: "ok", title: `Service "${name}" deleted` });
    },
    onError: (err) =>
      toast.push({
        tone: "error",
        title: "Couldn't delete service",
        // The 409 "override rules target it" message is surfaced verbatim so the
        // operator knows to clear the overrides first.
        description: err.message,
      }),
  });

  // Clear all services — destructive reset of every discovered/manual service.
  // Learned log patterns are left intact (that is a separate action on the Logs
  // page).
  const clearServices = useMutation({
    mutationFn: api.clearServices,
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ["services"] });
      setConfirmClear(false);
      toast.push({
        tone: "ok",
        title: "Discovered services cleared",
        description: `${res.services} services removed — the agent re-discovers services from scratch`,
      });
    },
    onError: (err) =>
      toast.push({
        tone: "error",
        title: "Clear all services failed",
        description: err.message,
        action: { label: "Retry", onClick: () => clearServices.mutate() },
      }),
  });

  const entries = data ? Object.entries(data) : [];
  // Sort A→Z, then paginate at 100/page so a multi-thousand-service estate
  // never renders every row at once (the freeze the founder hit).
  const sorted = entries.sort(([a], [b]) => a.localeCompare(b));
  const needle = q.trim().toLowerCase();
  const filtered = needle
    ? sorted.filter(([name]) => name.toLowerCase().includes(needle))
    : sorted;

  // ----- Active | Ignored scope --------------------------------------
  // A service is "ignored" when it is held out of learning (the whole-service
  // exclude). The scope control is gated on ignore.visible — absent for
  // community / viewers, so scope stays "active" and nothing is partitioned
  // out. The server stays authority: this only re-partitions what the loaded
  // policy already reports.
  const isRowExcluded = ([name]: [string, unknown]) =>
    ignore.isServiceExcluded(name);
  const scopeCounts = useMemo(
    () => ({
      active: filtered.length - countExcluded(filtered, isRowExcluded),
      ignored: countExcluded(filtered, isRowExcluded),
    }),
    // isRowExcluded closes over ignore; recompute when the policy or list
    // changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [filtered, ignore],
  );
  const scoped = useMemo(
    () =>
      ignore.visible ? filterByScope(filtered, scope, isRowExcluded) : filtered,
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [filtered, scope, ignore],
  );

  const pg = usePagination(scoped, { resetKey: `${scope}|${q}` });

  // ----- selection + grace action bar -------------------------------------
  // The SAME checkbox action model the learned-signal pages use: a select-all
  // checkbox in the header, a checkbox per row, and an action bar that APPEARS
  // when services are selected. Grace control lives ONLY in the bar now (no
  // inline End/Restart per row). The offered action is contextual per the
  // selection's grace state — "End grace" for services in grace, "Restart grace"
  // for those not — never both at once for one service. Selection resets on
  // search / PAGE change.
  const pageNames = useMemo(
    () => pg.pageItems.map(([name]) => name),
    [pg.pageItems],
  );
  const bulk = useBulkSelection(pageNames, `${scope}|${q}|${pg.page}`);

  const selectedInGrace = bulk.selectedKeys.map(
    (name) => data?.[name]?.in_grace ?? false,
  );
  const selectedManual = bulk.selectedKeys.filter(
    (name) => data?.[name]?.manual,
  );
  const anyNotIgnored =
    ignore.visible &&
    bulk.selectedKeys.some((name) => !ignore.isServiceExcluded(name));
  const anyIgnored =
    ignore.visible &&
    bulk.selectedKeys.some((name) => ignore.isServiceExcluded(name));

  // The action bar offers every action applicable to the selection: grace
  // (end/restart, contextual per each service's grace state), Ignore/Resume
  // learning (Enterprise — hidden on community / viewer), and manual-service
  // CRUD (Rename for a single manual service, Delete for any manual ones).
  // Auto-discovered services carry no manual CRUD.
  const bulkActions: ActionBarItem[] = [
    ...graceActionsForSelection(selectedInGrace).map((a) => ({
      id: a,
      label: GRACE_ACTION_LABEL[a],
    })),
    ...(anyNotIgnored ? [{ id: "ignore", label: "Ignore learning" }] : []),
    ...(anyIgnored ? [{ id: "resume", label: "Resume learning" }] : []),
    ...(selectedManual.length === 1
      ? [{ id: "rename", label: "Rename" }]
      : []),
    ...(selectedManual.length > 0
      ? [{ id: "delete", label: "Delete", danger: true }]
      : []),
  ];

  // Route each bar action to its handler. Grace splits a mixed selection so
  // "end" only touches services IN grace and "restart" only those NOT in grace;
  // Ignore/Resume touch only the applicable subset; Delete confirms first.
  const onBulkAction = (spec: ActionBarItem) => {
    switch (spec.id) {
      case "end":
      case "restart": {
        const action = spec.id as GraceAction;
        bulk.selectedKeys
          .filter((name) => {
            const inGrace = data?.[name]?.in_grace ?? false;
            return action === "end" ? inGrace : !inGrace;
          })
          .forEach((name) => control.mutate({ name, action }));
        bulk.clear();
        break;
      }
      case "ignore":
        bulk.selectedKeys
          .filter((name) => !ignore.isServiceExcluded(name))
          .forEach((name) => ignore.toggleService(name, true));
        bulk.clear();
        break;
      case "resume":
        bulk.selectedKeys
          .filter((name) => ignore.isServiceExcluded(name))
          .forEach((name) => ignore.toggleService(name, false));
        bulk.clear();
        break;
      case "rename":
        if (selectedManual.length === 1) setRenameTarget(selectedManual[0]);
        break;
      case "delete":
        if (selectedManual.length > 0) setConfirmBulkDelete(selectedManual);
        break;
    }
  };

  const bulkBusy =
    control.isPending ||
    ignore.busy ||
    renameService.isPending ||
    deleteService.isPending;

  // Columns: checkbox + Service + First seen + Origin + Status + Grace + Action.
  const cols = 7;

  return (
    <>
      <TopBar
        title="Services"
        subtitle={data ? `${entries.length} discovered` : undefined}
        actions={
          <div className="flex items-center gap-2">
            <button
              className="btn btn-primary"
              onClick={() => setShowAdd(true)}
            >
              <Plus size={12} />
              Add service
            </button>
            <button
              className="btn btn-danger"
              disabled={clearServices.isPending || entries.length === 0}
              onClick={() => setConfirmClear(true)}
            >
              <Trash2 size={12} />
              Clear all services
            </button>
          </div>
        }
      />

      <main className="flex-1 overflow-auto p-6">
        <div className="mb-3 flex flex-wrap items-center gap-2">
          {ignore.visible && (
            <SegmentedControl
              param={SCOPE_PARAM}
              defaultValue="active"
              aria-label="Learning scope"
              options={[
                { value: "active", label: "Active", badge: scopeCounts.active },
                {
                  value: "ignored",
                  label: "Ignored",
                  badge: scopeCounts.ignored,
                },
              ]}
            />
          )}
          <div className="relative w-full max-w-md sm:w-auto sm:flex-1">
            <Search
              size={12}
              className="absolute left-2.5 top-1/2 -translate-y-1/2 text-ink-400"
            />
            <input
              data-page-search
              className="input pl-7"
              placeholder="Search service…  ( / )"
              value={q}
              onChange={(e) => setQ(e.target.value)}
            />
          </div>

          <AutoRefreshControl state={refresh} />
        </div>

        {isError && (
          <div className="mb-3">
            <RetryableError
              error={error}
              onRetry={() => refetch()}
              retrying={isRefetching}
              context="Couldn't load services"
            />
          </div>
        )}

        {(!isError || data) && (
          <div className="card overflow-hidden">
            {bulk.count > 0 && (
              <BulkActionBar
                count={bulk.count}
                actions={bulkActions}
                onAction={onBulkAction}
                onClear={bulk.clear}
                busy={bulkBusy}
              />
            )}
            <div className="max-h-[calc(100vh-190px)] overflow-auto">
              <table className="ddt">
                <thead>
                  <tr>
                    <th className="w-8">
                      <SelectAllCheckbox
                        state={bulk.headerState}
                        onChange={bulk.toggleAll}
                      />
                    </th>
                    <th className="w-48">Service</th>
                    <th className="w-48">First seen</th>
                    <th className="w-24">Origin</th>
                    <th className="w-32">
                      Status
                      <InfoHint
                        label="About grace and service status"
                        text="When the agent first sees a brand-new service it opens a grace window (set by agent.new_service_grace, default 30m). During grace the service's signals are learned but not alerted on — so a new service can't page you before the agent knows what's normal for it. 'In grace' means it is still inside that window; 'tracked' means it has passed grace and is being detected on. This is the SAME status the service detail page shows. To change grace, select one or more services and use the action bar."
                        example="A service first seen at 10:00 with a 30m grace shows 'in grace' with ~20m remaining at 10:10, then 'tracked' after 10:30."
                      />
                    </th>
                    <th className="w-28 whitespace-nowrap">
                      Grace
                      <InfoHint
                        label="About the Grace column"
                        text="How much of the new-service grace window is left. It shows the remaining time while the service is in grace, or '—' once it is tracked (grace has ended or was never open)."
                        example="'12m30s' means detection starts in about 12 and a half minutes; '—' means the service is already being detected on."
                      />
                    </th>
                    <th className="w-12 text-right">
                      <span className="sr-only">Action</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {isLoading && <SkRows rows={6} cols={cols} />}
                  {!isLoading && !isError && scoped.length === 0 && (
                    <tr>
                      <td colSpan={cols}>
                        {entries.length === 0 ? (
                          <EmptyState
                            title="No services discovered yet."
                            hint="Service detection runs on every signal that matches `agent.service_patterns`."
                          />
                        ) : scope === "ignored" ? (
                          <EmptyState
                            title="No services are ignored"
                            hint="Select one or more services and choose ‘Ignore learning’ and it moves here, held out of learning until you resume it."
                          />
                        ) : (
                          <EmptyState
                            title="No services match your search"
                            hint="Try a different name or clear the search."
                          />
                        )}
                      </td>
                    </tr>
                  )}
                  {pg.pageItems.map(([name, info]) => {
                      const isUnknown = name === "_unknown";
                      return (
                        <tr key={name}>
                          <td className="w-8">
                            <RowSelectCheckbox
                              checked={bulk.isSelected(name)}
                              onChange={() => bulk.toggle(name)}
                              label={`Select service ${name}`}
                            />
                          </td>
                          <td className="font-mono">
                            {name}
                            {isUnknown && (
                              <Pill tone="warn" className="ml-2">
                                fallback
                              </Pill>
                            )}
                          </td>
                          <td
                            className="text-2xs text-ink-300"
                            title={fmtAbs(info.first_seen)}
                          >
                            {fmtAbs(info.first_seen)}{" "}
                            <span className="text-ink-400">
                              ({fmtRel(info.first_seen)})
                            </span>
                          </td>
                          <td>
                            {/* Origin: whether this service was auto-discovered
                                from a signal or created by hand (X-service-crud).
                                info.manual carries this for every row. */}
                            {info.manual ? (
                              <Pill tone="accent">Manual</Pill>
                            ) : (
                              <Pill>Auto</Pill>
                            )}
                          </td>
                          <td>
                            {/* Status: the SAME server-computed grace status the
                                service detail page shows (in_grace), so the list
                                and detail never disagree. */}
                            {info.in_grace ? (
                              <Pill tone="warn">in grace</Pill>
                            ) : (
                              <Pill tone="good">tracked</Pill>
                            )}
                          </td>
                          <td className="tabular-nums text-2xs text-ink-300">
                            {graceRemainingLabel(
                              info.in_grace,
                              info.grace_seconds_remaining,
                            )}
                          </td>
                          <td>
                            <div className="flex items-center justify-end gap-1">
                              {isUnknown ? (
                                <span className="text-ink-600">—</span>
                              ) : (
                                <button
                                  type="button"
                                  className="btn p-1"
                                  aria-label={`View service ${name}`}
                                  title="View details"
                                  onClick={() => setPeekName(name)}
                                >
                                  <Eye size={14} aria-hidden />
                                </button>
                              )}
                            </div>
                          </td>
                        </tr>
                      );
                    })}
                </tbody>
              </table>
            </div>
            <Pagination state={pg} />
          </div>
        )}
      </main>

      {renameTarget && (
        <TextInputModal
          title="Rename service"
          label="New name"
          initialValue={renameTarget}
          placeholder="New service name"
          confirmLabel="Rename"
          maxLength={256}
          busy={renameService.isPending}
          error={renameService.error}
          onSubmit={(to) => {
            if (to !== renameTarget)
              renameService.mutate({ from: renameTarget, to });
            else setRenameTarget(null);
          }}
          onClose={() => setRenameTarget(null)}
        />
      )}

      {confirmBulkDelete && (
        <ConfirmDialog
          title={`Delete ${confirmBulkDelete.length} manual service${confirmBulkDelete.length > 1 ? "s" : ""}`}
          message="This deletes the selected manually-created service(s). Auto-discovered services and learned patterns are untouched. A service still targeted by an override rule can't be deleted until that override is cleared."
          confirmLabel="Delete"
          tone="danger"
          busy={deleteService.isPending}
          error={deleteService.error}
          onConfirm={() => {
            confirmBulkDelete.forEach((name) => deleteService.mutate(name));
            setConfirmBulkDelete(null);
            bulk.clear();
          }}
          onClose={() => setConfirmBulkDelete(null)}
        />
      )}

      {showAdd && (
        <TextInputModal
          title="Add service"
          label="Service name"
          placeholder="New service name (e.g. checkout-api)"
          confirmLabel="Add service"
          maxLength={256}
          busy={createService.isPending}
          error={createService.error}
          onSubmit={(name) => createService.mutate(name)}
          onClose={() => setShowAdd(false)}
        />
      )}

      {confirmClear && (
        <ConfirmDialog
          title="Clear all discovered services"
          message="This removes ALL discovered and manually-created services, so the agent re-discovers services from scratch on the next tick. Learned log patterns are left untouched. This cannot be undone."
          confirmLabel="Clear all services"
          tone="danger"
          busy={clearServices.isPending}
          error={clearServices.error}
          onConfirm={() => clearServices.mutate()}
          onClose={() => setConfirmClear(false)}
        />
      )}

      {peekName && peekInfo && (
        <PeekPanel
          open
          onClose={() => setPeekName(null)}
          title={<span className="font-mono">{peekName}</span>}
          footer={
            <Link
              to={`/agent/services/${encodeURIComponent(peekName)}`}
              className="btn"
              onClick={() => setPeekName(null)}
            >
              Open full page ↗
            </Link>
          }
        >
          <div className="space-y-4">
            <div className="flex flex-wrap items-center gap-2">
              {peekInfo.manual ? (
                <Pill tone="accent">Manual</Pill>
              ) : (
                <Pill>Auto</Pill>
              )}
              {peekInfo.in_grace ? (
                <Pill tone="warn">in grace</Pill>
              ) : (
                <Pill tone="good">tracked</Pill>
              )}
            </div>

            <dl className="grid grid-cols-2 gap-x-4 gap-y-2 text-xs">
              <PeekField label="First seen">
                <span title={fmtAbs(peekInfo.first_seen)}>
                  {fmtRel(peekInfo.first_seen)}
                </span>
              </PeekField>
              <PeekField label="Grace remaining">
                {graceRemainingLabel(
                  peekInfo.in_grace,
                  peekInfo.grace_seconds_remaining,
                )}
              </PeekField>
              <PeekField label="Log patterns">
                {peekDetail.isLoading ? (
                  <span className="text-ink-400">…</span>
                ) : (
                  <span className="tabular-nums">
                    {peekDetail.data?.counts.patterns ?? "—"}
                  </span>
                )}
              </PeekField>
              <PeekField label="Incidents">
                {peekDetail.isLoading ? (
                  <span className="text-ink-400">…</span>
                ) : (
                  <span className="tabular-nums">
                    {peekDetail.data?.counts.incidents ?? "—"}
                  </span>
                )}
              </PeekField>
            </dl>

            {peekDetail.isError && (
              <p className="text-2xs text-ink-400">
                Couldn't load pattern and incident counts — open the full page
                for details.
              </p>
            )}
          </div>
        </PeekPanel>
      )}
    </>
  );
}
