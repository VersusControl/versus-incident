import { useRef, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Sparkles } from "lucide-react";
import clsx from "clsx";
import { api, type AnalyzeEvent } from "@/lib/api";
import { Spinner } from "@/components/feedback";
import { AnalysisStream } from "@/components/AnalysisStream";
import { AnalysisCard } from "@/components/AnalysisCard";
import { Modal } from "@/components/Modal";
import { useToast } from "@/components/toastContext";

// RunAnalysisButton fires the analyze agent and streams its progress live.
// Disabled with an explanation while AI is off (agent.ai.enable); outcomes
// always surface via toast — never silent. Shared by the incident detail page
// and the incidents list so both trigger analysis identically.
export function RunAnalysisButton({
  incidentID,
  onRan,
  className,
}: {
  incidentID: string;
  onRan?: () => void;
  className?: string;
}) {
  const cfg = useQuery({
    queryKey: ["agent-config"],
    queryFn: () => api.getAgentConfig(),
    staleTime: 60_000,
  });
  const qc = useQueryClient();
  const toast = useToast();
  const [running, setRunning] = useState(false);
  const [events, setEvents] = useState<AnalyzeEvent[]>([]);
  const [startedAt, setStartedAt] = useState(0);
  const [open, setOpen] = useState(false);
  const abortRef = useRef<AbortController | null>(null);
  // Token deltas arrive one SSE frame at a time. Appending per frame would be
  // one O(n) copy and one render PER TOKEN, so they are coalesced into at most
  // one state update per animation frame.
  const pending = useRef<AnalyzeEvent[]>([]);
  const rafRef = useRef<number | null>(null);

  const flush = () => {
    rafRef.current = null;
    if (!pending.current.length) return;
    const batch = pending.current;
    pending.current = [];
    setEvents((prev) => [...prev, ...batch]);
  };

  const push = (ev: AnalyzeEvent) => {
    pending.current.push(ev);
    if (rafRef.current == null) {
      rafRef.current = requestAnimationFrame(flush);
    }
  };

  const finish = (ok: boolean, detail?: string) => {
    qc.invalidateQueries({ queryKey: ["analyses", incidentID] });
    if (ok) {
      toast.push({ tone: "ok", title: "Analysis complete" });
      onRan?.();
    } else {
      toast.push({
        tone: "error",
        title: "Analysis failed",
        description: detail,
        action: { label: "Retry", onClick: () => void run() },
      });
    }
  };

  const run = async () => {
    if (running) return;
    setEvents([]);
    setStartedAt(Date.now());
    setRunning(true);
    setOpen(true);
    const ctrl = new AbortController();
    abortRef.current = ctrl;
    try {
      const term = await api.streamAnalysis(incidentID, push, {
        signal: ctrl.signal,
      });
      finish(term?.kind !== "run_failed", term?.error);
    } catch {
      // The stream can fail for reasons unrelated to the analysis — a proxy
      // that strips text/event-stream, for one. Fall back to the plain call so
      // the feature degrades instead of breaking.
      try {
        await api.runAnalysis(incidentID);
        finish(true);
      } catch (e2) {
        finish(false, e2 instanceof Error ? e2.message : String(e2));
      }
    } finally {
      // Land any tokens still buffered when the stream ended, or the tail of
      // the answer would never render.
      if (rafRef.current != null) cancelAnimationFrame(rafRef.current);
      flush();
      setRunning(false);
      abortRef.current = null;
    }
  };

  const aiOff = cfg.isSuccess && !cfg.data.ai?.enable;
  // The terminal event carries the persisted record's id, so the modal can
  // hand the operator straight to it instead of making them hunt for it.
  const analysisID = events.find((e) => e.analysis_id)?.analysis_id;

  // The model's final answer is a JSON finding. Once it is persisted, fetch it
  // and render it through the same card the analysis pages use, rather than
  // leaving the operator reading raw JSON.
  const finished = useQuery({
    queryKey: ["analysis", analysisID],
    queryFn: () => api.getAnalysis(analysisID as string),
    enabled: Boolean(analysisID) && !running,
    staleTime: 60_000,
  });

  return (
    <>
      <span className="inline-flex items-center gap-2">
        <button
          className={clsx("btn", className)}
          disabled={running || cfg.isLoading || aiOff}
          onClick={() => void run()}
          aria-label={
            aiOff
              ? "Run AI analysis — unavailable: AI is not enabled"
              : "Run AI analysis"
          }
          title={
            aiOff
              ? "AI is not enabled — configure it to run analyses."
              : "Run a fresh analysis. Past analyses stay available below."
          }
        >
          {running ? (
            <>
              <Spinner /> Analysing…
            </>
          ) : (
            <>
              <Sparkles size={11} /> Run analysis
            </>
          )}
        </button>
        {aiOff && (
          <span className="hidden text-2xs text-ink-400 sm:inline">
            AI not enabled
          </span>
        )}
      </span>

      {/* The shared Modal, not an inline panel or a fixed dock: this button
          sits inside action rows on the list peek, the detail header and the
          mobile action bar. A panel in the flow stretches those rows, and a
          fixed one is clipped by the detail header's backdrop-blur, which
          makes that header a containing block. Modal portals to <body>. */}
      {open && (
        <Modal
          title={running ? "Analysing incident" : "Analysis complete"}
          size="xl"
          onClose={() => setOpen(false)}
          footer={
            <>
              {analysisID && !running && (
                <Link
                  to={`/incidents/${incidentID}/analyses/${analysisID}`}
                  className="btn"
                  onClick={() => setOpen(false)}
                >
                  View full analysis
                </Link>
              )}
              <button className="btn" onClick={() => setOpen(false)}>
                {running ? "Run in background" : "Close"}
              </button>
            </>
          }
        >
          {finished.data ? (
            <AnalysisCard rec={finished.data} title="Analysis" />
          ) : (
            <AnalysisStream
              events={events}
              running={running}
              startedAt={startedAt}
            />
          )}
          {running && (
            <p className="mt-3 text-2xs text-ink-400">
              Closing this leaves the analysis running — it finishes and is
              saved either way.
            </p>
          )}
        </Modal>
      )}
    </>
  );
}
