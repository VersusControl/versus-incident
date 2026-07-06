import { useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import {
  Eye,
  LineChart,
  Lock,
  Search,
  Waypoints,
  type LucideIcon,
} from "lucide-react";
import { api, ApiError, type BaselineRow } from "@/lib/api";
import {
  displayService,
  fmtAbs,
  fmtRel,
  formatNormalValue,
  formatWiggle,
  humanSignal,
} from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { InfoHint } from "@/components/InfoHint";
import { ReadinessProgress } from "@/components/ReadinessProgress";
import { AutoRefreshControl } from "@/components/AutoRefreshControl";
import { useAutoRefresh } from "@/lib/useAutoRefresh";
import { EmptyState } from "@/components/feedback";
import { SegmentedControl } from "@/components/SegmentedControl";
import { PeekPanel } from "@/components/PeekPanel";
import { SkRows } from "@/components/Skeleton";
import { RetryableError } from "@/components/RetryableError";
import { ReassignModal, ServiceCell } from "@/components/ServiceCell";
import {
  BulkActionBar,
  RowSelectCheckbox,
  SelectAllCheckbox,
} from "@/components/BulkActionBar";
import { useLearnExclusions } from "@/lib/useLearnExclusions";
import {
  countExcluded,
  filterByScope,
  isExclusionScope,
  SCOPE_PARAM,
} from "@/lib/rowActions";
import { buildSignalBulkActions } from "@/lib/bulkSelect";
import { useBulkSelection } from "@/lib/useBulkSelection";
import { Pagination } from "@/components/Pagination";
import { usePagination } from "@/lib/pagination";

// LearnedSignalsView — the read-only "what the agent knows right now" view for
// ONE telemetry type (Metrics or Traces). It makes the Enterprise metric/trace
// learners VISIBLE in plain words: per (service, signal) [+ operation for
// traces] it shows the everyday-normal value with real units, whether the
// agent has seen enough to start catching problems ("Ready to detect") or is
// still gathering samples ("Still learning — N of 20"), how much evidence
// backs it, and whether the signal is still flowing.
//
// Enterprise-gated: the endpoint returns 403 without an `intelligence` license
// and is absent (404) on an OSS binary — either way the page renders the
// locked upsell state, never real data. No enterprise dependency lives here:
// the lock is driven purely by the HTTP status, so OSS-only builds stay green.
// The learned baselines themselves are read-only (no label / delete / reset —
// the agent learns these on its own); the only write is the in-column "Reassign
// service" attribution correction (the pencil in the Service cell), which only
// appears in the licensed render path (so it is absent on OSS / unlicensed,
// like the old override section).

const PAGE_TITLE = "What the agent knows right now";

// VALUE_TOOLTIP is the §3.2 one-line gloss for the "what's normal" concept.
// The status concept is now owned by the shared ReadinessProgress + its column
// header InfoHint, so there is no local status tooltip constant.
const VALUE_TOOLTIP =
  "The usual range for this signal. Anything inside it is normal.";

const READONLY_NOTE =
  "The agent learns these on its own.";

type Variant = {
  kind: "metric" | "trace";
  icon: LucideIcon;
  subtitle: string;
  sourceLabel: string;
  hasOperation: boolean;
  searchPlaceholder: string;
  lockedTitle: string;
  lockedBody: string;
  // sampleLabel is the per-type wording for the peek's raw-example field — the
  // metric/trace parity of the logs page's "Example log line".
  sampleLabel: string;
};

const METRIC: Variant = {
  kind: "metric",
  icon: LineChart,
  subtitle:
    "The agent is learning what's normal for each service's numbers — request rate, errors, latency — so it can catch a value that suddenly looks wrong.",
  sourceLabel: "Prometheus",
  hasOperation: false,
  searchPlaceholder: "Search service or signal…  ( / )",
  lockedTitle: "Metrics learning is an Enterprise capability",
  lockedBody:
    "Metrics learning is an Enterprise capability — the agent learns what's normal for each service's request rate, errors and latency so it can catch problems automatically.",
  sampleLabel: "Example metric",
};

const TRACE: Variant = {
  kind: "trace",
  icon: Waypoints,
  subtitle:
    "The agent is learning the normal speed and error rate of each service operation, so it can catch requests that suddenly get slow or start failing.",
  sourceLabel: "Traces",
  hasOperation: true,
  searchPlaceholder: "Search service, operation or signal…  ( / )",
  lockedTitle: "Traces learning is an Enterprise capability",
  lockedBody:
    "Traces learning is an Enterprise capability — the agent learns the normal speed and error rate of each operation so it can catch slow or failing requests automatically.",
  sampleLabel: "Example trace",
};

const STATUS_PARAM = "status";

function rowKey(r: BaselineRow): string {
  return `${r.type}:${r.service}:${r.operation ?? ""}:${r.signal}`;
}

// NormalValue renders "what's normal right now" with units. While the signal is
// still settling the value is shown but prefixed "So far …" so the operator
// reads it as provisional, never authoritative.
function NormalValue({ row, compact }: { row: BaselineRow; compact?: boolean }) {
  const value = formatNormalValue(row.display_mean, row.unit);
  const wiggle = formatWiggle(row.display_std, row.unit, !compact);
  const body = compact ? `${value} ${wiggle}` : `${value}, usually ${wiggle}`;
  if (!row.confident) {
    return (
      <span className="text-ink-300" title={VALUE_TOOLTIP}>
        So far {body}
        {!compact && <span className="text-ink-400"> (still settling)</span>}
      </span>
    );
  }
  return (
    <span className="text-ink-100" title={VALUE_TOOLTIP}>
      {body}
    </span>
  );
}

export function LearnedSignalsView({ variant }: { variant: Variant }) {
  const refresh = useAutoRefresh();
  const { data, isLoading, isError, error, refetch, isRefetching } = useQuery({
    queryKey: ["baselines", variant.kind],
    queryFn: () => api.listBaselines({ type: variant.kind }),
    refetchInterval: refresh.refetchInterval,
    retry: (count, err) => {
      // The locked state — 403 (unlicensed) / 404 (OSS) — is terminal, not
      // transient, so never retry it.
      if (err instanceof ApiError && (err.status === 403 || err.status === 404))
        return false;
      return count < 1;
    },
  });

  const [params] = useSearchParams();
  const statusFilter = params.get(STATUS_PARAM) ?? "all";
  const scope = isExclusionScope(params.get(SCOPE_PARAM));
  const [q, setQ] = useState("");
  const [peekKey, setPeekKey] = useState<string | null>(null);
  // Bulk reassign: the selected signal names captured when "Assign to service"
  // is picked in the action bar (null = modal closed). One flow for one row or
  // many — a single-row correction is just a one-row selection.
  const [reassignMatches, setReassignMatches] = useState<string[] | null>(null);

  const locked =
    isError &&
    error instanceof ApiError &&
    (error.status === 403 || error.status === 404);

  // Disable-Learn control. The page's own baselines query is the license
  // probe — a settled `data` means the surface is licensed (a 403/404 renders
  // the locked upsell below, never the table). The action lives in the checkbox
  // action bar (Ignore/Resume) and renders only for a licensed admin
  // (runtime:manage); it is absent on community / OSS and hidden from a viewer
  // (excl.visible).
  const excl = useLearnExclusions(data !== undefined);

  const rows = useMemo(() => data?.baselines ?? [], [data]);

  const filtered = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return rows.filter((r) => {
      const ready = r.readiness?.ready ?? r.confident;
      if (statusFilter === "ready" && !ready) return false;
      if (statusFilter === "learning" && ready) return false;
      if (!needle) return true;
      return (
        r.service.toLowerCase().includes(needle) ||
        r.signal.toLowerCase().includes(needle) ||
        humanSignal(r.signal).toLowerCase().includes(needle) ||
        (r.operation ?? "").toLowerCase().includes(needle)
      );
    });
  }, [rows, q, statusFilter]);

  // ----- Active | Ignored scope --------------------------------------
  // A metric/trace row is "ignored" when its SIGNAL is held out of learning
  // (matched by name across every service). The scope control is gated on
  // excl.visible — absent for community / viewers, so scope stays "active" and
  // nothing is partitioned. The server stays authority: this only re-partitions
  // what the loaded policy already reports.
  const isRowExcluded = (r: BaselineRow) => excl.isSignalExcluded(r.signal);
  const scopeCounts = useMemo(
    () => ({
      active: filtered.length - countExcluded(filtered, isRowExcluded),
      ignored: countExcluded(filtered, isRowExcluded),
    }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [filtered, excl],
  );
  const scoped = useMemo(
    () =>
      excl.visible ? filterByScope(filtered, scope, isRowExcluded) : filtered,
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [filtered, scope, excl],
  );

  // Paginate at 100/page; reset to page 1 when the status filter, scope tab, or
  // search changes so a filter never lands the operator on an empty page.
  const pg = usePagination(scoped, {
    resetKey: `${statusFilter}|${scope}|${q}`,
  });

  // ----- selection + action bar -------------------------------------------
  // The SAME checkbox action model the logs page uses. For metrics/traces every
  // action (Ignore/Resume + Assign-to-service) requires the licensed
  // runtime:manage surface, so the checkbox column + bar appear ONLY when the
  // exclude surface is visible — absent on community / OSS and hidden from a
  // viewer. Selection resets on status / scope / search / PAGE change.
  const bulkActions = buildSignalBulkActions({
    scope,
    excludeVisible: excl.visible,
  });
  const bulkEnabled = bulkActions.length > 0;
  const pageKeys = useMemo(() => pg.pageItems.map(rowKey), [pg.pageItems]);
  const bulk = useBulkSelection(
    pageKeys,
    `${statusFilter}|${scope}|${q}|${pg.page}`,
  );

  // A metric/trace action operates on SIGNAL names (deduped — a signal is
  // matched by name across every service, so two selected rows sharing a signal
  // fold into one entry). Ignore/Resume go through a SINGLE PUT
  // (excl.toggleSignals); Assign-to-service opens the picker for the selection.
  const onBulkAction = (spec: { id: string }) => {
    const signals = Array.from(
      new Set(
        bulk.selectedKeys
          .map((k) => rows.find((r) => rowKey(r) === k)?.signal)
          .filter((s): s is string => !!s),
      ),
    );
    if (spec.id === "reassign") {
      // Keep the selection until the modal finishes (onDone clears it).
      setReassignMatches(signals);
      return;
    }
    if (spec.id === "ignore") excl.toggleSignals(signals, true);
    else if (spec.id === "resume") excl.toggleSignals(signals, false);
    bulk.clear();
  };

  const peek = peekKey ? rows.find((r) => rowKey(r) === peekKey) : undefined;
  const cols = (variant.hasOperation ? 7 : 6) + 1 + (bulkEnabled ? 1 : 0);

  // ----- locked / upsell state (OSS or unlicensed) ------------------------
  if (locked) {
    return (
      <>
        <TopBar title={PAGE_TITLE} />
        <main className="flex-1 overflow-auto p-4 lg:p-6">
          <div className="card p-8">
            <div className="mx-auto flex max-w-md flex-col items-center gap-3 text-center">
              <div className="rounded-full bg-accent-subtle p-3 text-link">
                <Lock size={20} />
              </div>
              <h2 className="text-sm font-semibold text-ink-50">
                {variant.lockedTitle}
              </h2>
              <p className="text-xs text-ink-300">{variant.lockedBody}</p>
              <a
                className="btn btn-primary mt-1"
                href="https://versusincident.com/enterprise"
                target="_blank"
                rel="noreferrer"
              >
                Learn about Enterprise
              </a>
            </div>
          </div>
        </main>
      </>
    );
  }

  return (
    <>
      <TopBar
        title={PAGE_TITLE}
        subtitle={data ? `${data.count} learned` : undefined}
      />

      <main className="flex-1 overflow-auto p-4 lg:p-6">
        <p className="mb-3 max-w-3xl text-xs text-ink-300">{variant.subtitle}</p>

        <div className="mb-3 flex flex-wrap items-center gap-2">
          <SegmentedControl
            param={STATUS_PARAM}
            defaultValue="all"
            aria-label="Status filter"
            options={[
              { value: "all", label: "All" },
              { value: "ready", label: "Ready" },
              { value: "learning", label: "Still learning" },
            ]}
          />

          {excl.visible && (
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
              placeholder={variant.searchPlaceholder}
              value={q}
              onChange={(e) => setQ(e.target.value)}
            />
          </div>

          <AutoRefreshControl state={refresh} />
        </div>

        {isError && !locked ? (
          <RetryableError
            error={error}
            onRetry={() => refetch()}
            retrying={isRefetching}
            context="Couldn't load what the agent has learned"
          />
        ) : (
          <div className="card overflow-hidden">
            {bulkEnabled && bulk.count > 0 && (
              <BulkActionBar
                count={bulk.count}
                actions={bulkActions}
                onAction={onBulkAction}
                onClear={bulk.clear}
                busy={excl.busy}
              />
            )}
            <div className="max-h-[calc(100vh-260px)] overflow-auto">
              <table className="ddt">
                <thead>
                  <tr>
                    {bulkEnabled && (
                      <th className="w-8">
                        <SelectAllCheckbox
                          state={bulk.headerState}
                          onChange={bulk.toggleAll}
                        />
                      </th>
                    )}
                    <th className="w-36">Service</th>
                    {variant.hasOperation && (
                      <th className="w-40 whitespace-nowrap">
                        Operation
                        <InfoHint
                          label="About the Operation"
                          text="The exact request or step this is measured on, like a specific endpoint or database call. The agent learns normal speed and error rate per operation, not just per service."
                          example="'POST /payments' is one operation and 'GET /health' is another — each gets its own normal range."
                        />
                      </th>
                    )}
                    <th className="w-32 whitespace-nowrap">
                      Signal
                      <InfoHint
                        label="About the Signal"
                        text="What's being measured for this service or operation — traffic (request rate), errors (how often requests fail), or latency (how long they take). Each signal is learned on its own."
                        example="'Latency' tracks how long requests take; 'Errors' tracks how often they fail."
                      />
                    </th>
                    <th className="w-56 whitespace-nowrap">
                      What's normal now
                      <InfoHint
                        label="About the What's normal now"
                        text="The everyday range the agent has learned for this signal, in real units (req/s for traffic, ms for speed, % for errors). The 'usually ± X' part is how much it normally wiggles around the middle value — anything far outside that range looks wrong and can be flagged."
                        example="≈ 40 ms, usually ± 5 ms means requests normally finish in about 35–45 ms; a sudden 400 ms looks wrong."
                      />
                    </th>
                    <th className="w-44 whitespace-nowrap">
                      To known
                      <InfoHint
                        label="About the To known"
                        text="How much more evidence until the agent treats this signal as known and starts flagging anomalies on it — a progress meter, not a status. '8 / 20' means it has 8 of the ~20 samples it needs; a full bar with a check means it got there. This is distinct from Verdict, which is the current classification (still learning / known / spike)."
                        example="'8 / 20' means 12 more samples to go; once it reaches 20 the bar fills and shows a check."
                      />
                    </th>
                    <th className="w-20 whitespace-nowrap text-right">
                      Seen
                      <InfoHint
                        label="About the Seen"
                        text="How many data points (samples) the agent has collected for this signal so far. More samples mean a more trustworthy normal range."
                        example="'18' means 18 measurements have been folded in; the agent usually needs about 20 before it's ready."
                      />
                    </th>
                    <th className="w-28 whitespace-nowrap">
                      Last seen
                      <InfoHint
                        label="About the Last seen"
                        text="When the agent last received a fresh data point for this signal. If this is old, the signal may have stopped flowing."
                        example="'2m ago' means the last sample arrived two minutes ago; '3d ago' suggests the source has gone quiet."
                      />
                    </th>
                    <th className="w-12 text-right">
                      <span className="sr-only">Action</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {isLoading && <SkRows rows={8} cols={cols} />}
                  {!isLoading && scoped.length === 0 && (
                    <tr>
                      <td colSpan={cols}>
                        {rows.length === 0 ? (
                          <EmptyState
                            title="Nothing learned yet"
                            hint="The agent starts learning the moment a metric or trace source runs in training mode. Give it a few minutes, then refresh."
                          />
                        ) : scope === "ignored" ? (
                          <EmptyState
                            title="No signals ignored"
                            hint="Select a noisy signal and choose Ignore from the action bar and it moves here, held out of learning until you resume it."
                          />
                        ) : (
                          <EmptyState
                            title="Nothing matches your filters"
                            hint="Try clearing the search or switching the status filter."
                          />
                        )}
                      </td>
                    </tr>
                  )}
                  {pg.pageItems.map((r) => (
                    <tr key={rowKey(r)}>
                      {bulkEnabled && (
                        <td className="w-8">
                          <RowSelectCheckbox
                            checked={bulk.isSelected(rowKey(r))}
                            onChange={() => bulk.toggle(rowKey(r))}
                            label={`Select ${humanSignal(r.signal)}`}
                          />
                        </td>
                      )}
                      <td className="font-medium text-ink-100">
                        <ServiceCell
                          service={r.service}
                          sourceType={variant.kind}
                          match={r.signal}
                        />
                      </td>
                      {variant.hasOperation && (
                        <td className="font-mono text-2xs text-ink-200">
                          {r.operation || "—"}
                        </td>
                      )}
                      <td className="text-ink-100">{humanSignal(r.signal)}</td>
                      <td className="tabular-nums">
                        <NormalValue row={r} compact />
                      </td>
                      <td>
                        <ReadinessProgress readiness={r.readiness} />
                      </td>
                      <td className="text-right tabular-nums text-ink-200">
                        {r.observations}
                      </td>
                      <td className="text-ink-300" title={fmtAbs(r.last_updated)}>
                        {fmtRel(r.last_updated)}
                      </td>
                      <td>
                        <div className="flex items-center justify-end gap-1">
                          <button
                            type="button"
                            className="btn p-1"
                            aria-label={`View ${humanSignal(r.signal)}`}
                            title="View details"
                            onClick={() => setPeekKey(rowKey(r))}
                          >
                            <Eye size={14} aria-hidden />
                          </button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <Pagination state={pg} />
          </div>
        )}
      </main>

      <PeekPanel
        open={!!peek}
        onClose={() => setPeekKey(null)}
        title={
          peek
            ? `${displayService(peek.service)} · ${humanSignal(peek.signal)}`
            : ""
        }
      >
        {peek && (
          <dl className="space-y-3 text-xs">
            <Field label="To known">
              <ReadinessProgress readiness={peek.readiness} />
            </Field>
            <Field label="What's normal right now">
              <NormalValue row={peek} />
              <div className="mt-0.5 text-2xs text-ink-400">
                Changes with time of day.
              </div>
            </Field>
            <Field label="Seen">{peek.observations} samples</Field>
            <Field label="Last seen">
              <span title={fmtAbs(peek.last_updated)}>
                {fmtRel(peek.last_updated)}
              </span>
            </Field>
            <Field label="Service">{displayService(peek.service)}</Field>
            {variant.hasOperation && (
              <Field label="Operation">{peek.operation || "—"}</Field>
            )}
            <Field label="Signal">
              {humanSignal(peek.signal)}
              <span className="ml-1 font-mono text-2xs text-ink-400">
                {peek.signal}
              </span>
            </Field>
            <Field label="Source">{variant.sourceLabel}</Field>
            <Field label={variant.sampleLabel}>
              {peek.latest_sample ? (
                <pre className="overflow-auto whitespace-pre-wrap break-words rounded-md border border-ink-600 bg-surface-sunken p-2 font-mono text-2xs leading-relaxed text-ink-100">
                  {peek.latest_sample}
                </pre>
              ) : (
                <span className="text-ink-400">No example captured yet</span>
              )}
            </Field>
            <p className="pt-2 text-2xs text-ink-400">{READONLY_NOTE}</p>
          </dl>
        )}
      </PeekPanel>

      {/* Reassign picker — opened from the "Assign to service" action in the
          checkbox action bar; reassigns the selected signal(s) by name. */}
      {reassignMatches && (
        <ReassignModal
          sourceType={variant.kind}
          matches={reassignMatches}
          invalidateKeys={[["baselines", variant.kind]]}
          onClose={() => setReassignMatches(null)}
          onDone={() => bulk.clear()}
        />
      )}
    </>
  );
}

function Field({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div>
      <dt className="text-2xs uppercase tracking-wide text-ink-400">{label}</dt>
      <dd className="mt-0.5 text-ink-100">{children}</dd>
    </div>
  );
}

export function MetricsPage() {
  return <LearnedSignalsView variant={METRIC} />;
}

export function TracesPage() {
  return <LearnedSignalsView variant={TRACE} />;
}
