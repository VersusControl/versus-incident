import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "@/lib/api";
import {
  learnExcludeGate,
  listExcludeControlVisible,
  metricExcluded,
  patternExcluded,
  serviceExcluded,
  toggleLogPatternExclusion,
  toggleLogPatternExclusions,
  toggleMetricExclusion,
  toggleMetricExclusions,
  type LearnExcludeGate,
  type LearnExclusions,
} from "@/lib/learnExclude";
import { useEffectiveRole } from "@/lib/useEffectiveRole";
import { useToast } from "@/components/toastContext";

// useLearnExclusions — the shared control state behind the Disable-Learn
// (learn-exclusion) action on the three learned-signal LIST pages: logs
// (PatternsPage, per-pattern grain), metrics and traces (LearnedSignalsView,
// signal-level). It mirrors the per-control gate + enterprise API the
// ServiceDetailPage toggle/checkbox already use, so a licensed admin gets ONE
// coherent "Ignore from learning" action everywhere the agent shows what it has
// learned. The action is surfaced from the checkbox selection + BulkActionBar
// (single or multi row), whose scope-named Ignore/Resume actions call the
// toggles below; this hook stays free of components so it keeps the
// react-refresh boundary clean.
//
// Gating is identical to the ServiceDetailPage control and fails closed:
//   • licensed — driven purely by an enterprise HTTP-status probe (the shared
//     /intel baselines endpoint: 200 ⇒ licensed for any session, 403/404 ⇒
//     community / OSS). No enterprise dependency lives here.
//   • runtime:manage — the RBAC admin/owner role off the SSO session.
// On a dense list row the control renders ONLY for a licensed admin ("editable")
// — it is absent on community / OSS and hidden from a licensed viewer, so the
// feature never leaks a header or an inert widget to a non-admin.
//
// Disable-Learn semantics are preserved end to end: toggling calls the SAME enterprise
// endpoints (POST/DELETE .../services/:name for a service; the whole-list PUT
// for a metric/trace signal AND for a log pattern — the log-pattern grain has
// no per-pattern POST/DELETE route, so it rides the same read-modify-write PUT
// the metric grain does), and an excluded (service, signal) or (service,
// pattern) is dropped BEFORE learn in EVERY mode (training | shadow | detect)
// on the next worker tick.

const LEARN_EXCLUSIONS_KEY = ["learn-exclusions"] as const;

// INTEL_LICENSE_PROBE_KEY is a dedicated key for the logs-page license probe so
// it never entangles with the metrics page's own ["baselines","metric"] query
// options (that one carries a refetch interval; this is a one-shot license
// sniff). react-query caches it, so navigating between the agent pages does not
// refetch it within the stale window.
const INTEL_LICENSE_PROBE_KEY = ["intel-license-probe"] as const;

// isTerminalAuthStatus reports the HTTP statuses that are a DEFINITE community /
// OSS / wrong-role / no-session answer — terminal, never retried, so the gate
// degrades immediately instead of flapping while a doomed request retries.
function isTerminalAuthStatus(err: unknown): boolean {
  return err instanceof ApiError && [401, 403, 404, 503].includes(err.status);
}

// useIntelLicensed probes whether the binary is licensed for the intelligence
// surface, for pages that do NOT already load an enterprise resource (the logs
// page — log learning is OSS, so listPatterns succeeds regardless of license).
// It sniffs the read-only baselines endpoint: 200 ⇒ licensed (any session);
// 401/403/404 ⇒ community / OSS / logged-out ⇒ NOT licensed. Pages that already
// hold a baselines query (metrics / traces) pass their own success instead.
export function useIntelLicensed(): boolean {
  const probe = useQuery({
    queryKey: INTEL_LICENSE_PROBE_KEY,
    queryFn: () => api.listBaselines({ type: "metric" }),
    retry: (count, err) => {
      if (isTerminalAuthStatus(err)) return false;
      return count < 1;
    },
    staleTime: 30_000,
  });
  return probe.isSuccess;
}

// LearnExclusionControls is the bundle a list page threads to its per-row cells:
// the render gate, membership tests, and the two toggle actions. `visible` is
// the single boolean the page gates BOTH the column header AND the cell on.
export interface LearnExclusionControls {
  gate: LearnExcludeGate;
  // visible is true only for a licensed runtime:manage session — the list-page
  // render decision (hidden from viewers, absent on community / OSS).
  visible: boolean;
  // excludedServices is the org's current fully-ignored service names — the
  // source for the Services page "Ignored services" table. Empty on
  // community / viewer (the policy GET is disabled / unauthorized).
  excludedServices: string[];
  // busy is true while the policy is (re)loading or a mutation is in flight, so
  // the cells can disable themselves against a double submit.
  busy: boolean;
  isServiceExcluded: (service: string) => boolean;
  isSignalExcluded: (signal: string) => boolean;
  isPatternExcluded: (patternKey: string) => boolean;
  toggleService: (service: string, exclude: boolean) => void;
  toggleSignal: (signal: string, exclude: boolean) => void;
  togglePattern: (patternKey: string, exclude: boolean) => void;
  // Batch variants for the bulk-action bar. Both fold MANY entries into ONE
  // whole-list PUT (per-entry PUTs would race on the same stale list, the last
  // write dropping the rest): toggleSignals over the metric grain,
  // togglePatterns over the log-pattern grain.
  toggleSignals: (signals: string[], exclude: boolean) => void;
  togglePatterns: (patternKeys: string[], exclude: boolean) => void;
}

// useLearnExclusions wires the shared control state for one list page. It reads
// the org's Disable-Learn policy ONCE (a single ["learn-exclusions"] query all
// cells share) and exposes the two write paths. The policy GET is enabled ONLY
// for a licensed admin ("editable"), so a viewer / community caller never fires
// a doomed request. `licensed` is supplied by the caller — its own baselines
// query success on the metrics / traces pages, or useIntelLicensed() on logs.
export function useLearnExclusions(licensed: boolean): LearnExclusionControls {
  const qc = useQueryClient();
  const toast = useToast();
  const access = useEffectiveRole();
  const gate = learnExcludeGate({ licensed, canManage: access.isAdmin });
  const editable = gate === "editable";

  const ex = useQuery({
    queryKey: LEARN_EXCLUSIONS_KEY,
    queryFn: () => api.getLearnExclusions(),
    enabled: editable,
    retry: (count, err) => {
      if (isTerminalAuthStatus(err)) return false;
      return count < 1;
    },
  });

  // Both write paths adopt the authoritative post-change lists returned by the
  // server, then invalidate so every mounted control (this page's other rows,
  // the ServiceDetailPage) reflects the change; it bites on the next worker
  // tick with no restart.
  const onWritten = (next: LearnExclusions) => {
    qc.setQueryData(LEARN_EXCLUSIONS_KEY, next);
    qc.invalidateQueries({ queryKey: LEARN_EXCLUSIONS_KEY });
  };
  const onWriteError = (err: unknown) =>
    toast.push({
      tone: "error",
      title: "Couldn't update learning policy",
      description: err instanceof Error ? err.message : String(err),
    });

  const serviceMut = useMutation({
    mutationFn: (v: { service: string; exclude: boolean }) =>
      api.setServiceLearnExclusion(v.service, v.exclude),
    onSuccess: onWritten,
    onError: onWriteError,
  });

  const signalMut = useMutation({
    mutationFn: (next: LearnExclusions) => api.setLearnExclusions(next),
    onSuccess: onWritten,
    onError: onWriteError,
  });

  const busy =
    ex.isFetching ||
    serviceMut.isPending ||
    signalMut.isPending ||
    !ex.data;

  return {
    gate,
    visible: listExcludeControlVisible(gate),
    excludedServices: ex.data?.services ?? [],
    busy,
    isServiceExcluded: (service) => serviceExcluded(service, ex.data?.services),
    isSignalExcluded: (signal) => metricExcluded(signal, ex.data?.metrics),
    isPatternExcluded: (patternKey) =>
      patternExcluded(patternKey, ex.data?.patterns),
    toggleService: (service, exclude) => serviceMut.mutate({ service, exclude }),
    toggleSignal: (signal, exclude) =>
      signalMut.mutate({
        services: ex.data?.services ?? [],
        metrics: toggleMetricExclusion(ex.data?.metrics ?? [], signal, exclude),
        patterns: ex.data?.patterns ?? [],
      }),
    togglePattern: (patternKey, exclude) =>
      signalMut.mutate({
        services: ex.data?.services ?? [],
        metrics: ex.data?.metrics ?? [],
        patterns: toggleLogPatternExclusion(
          ex.data?.patterns ?? [],
          patternKey,
          exclude,
        ),
      }),
    toggleSignals: (signals, exclude) =>
      signalMut.mutate({
        services: ex.data?.services ?? [],
        metrics: toggleMetricExclusions(
          ex.data?.metrics ?? [],
          signals,
          exclude,
        ),
        patterns: ex.data?.patterns ?? [],
      }),
    togglePatterns: (patternKeys, exclude) =>
      signalMut.mutate({
        services: ex.data?.services ?? [],
        metrics: ex.data?.metrics ?? [],
        patterns: toggleLogPatternExclusions(
          ex.data?.patterns ?? [],
          patternKeys,
          exclude,
        ),
      }),
  };
}
