import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { BarChart3 } from "lucide-react";
import clsx from "clsx";
import { api, type Capabilities } from "@/lib/api";
import {
  canReport,
  defaultReportChannel,
  defaultReportWindow,
  hasReportChannel,
  reportChannels,
  REPORT_WINDOWS,
  summarizeReportOutcome,
  type ReportWindow,
} from "@/lib/reportAnalytics";
import { Modal } from "./Modal";
import { ChannelIcon } from "./ChannelIcon";
import { ErrorBox, Spinner } from "./feedback";
import { useToast } from "./toastContext";

// ReportsButton renders the incidents-analytics "Report" action for the
// Incidents page. It is window-scoped (not per-incident) and spans BOTH
// AI-detect and webhook incidents. Hidden entirely when the server reports the
// feature disabled — the UI never guesses, it reads the capabilities probe.
export function ReportsButton({ className }: { className?: string }) {
  const [open, setOpen] = useState(false);
  const cap = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    staleTime: 60_000,
  });

  if (!canReport(cap.data)) return null;

  return (
    <>
      <button
        className={clsx("btn", className)}
        onClick={() => setOpen(true)}
        aria-label="Generate an incidents analytics report"
        title="Render a shareable incidents-analytics dashboard for a window and send it to a channel"
      >
        <BarChart3 size={12} />
        Report
      </button>
      {open && <ReportsDialog cap={cap.data} onClose={() => setOpen(false)} />}
    </>
  );
}

function ReportsDialog({
  cap,
  onClose,
}: {
  cap: Capabilities | undefined;
  onClose: () => void;
}) {
  const toast = useToast();
  const channels = reportChannels(cap);
  const hasChannel = hasReportChannel(cap);
  const [channel, setChannel] = useState(() => defaultReportChannel(cap));
  const [window, setWindow] = useState<ReportWindow>(() =>
    defaultReportWindow(cap),
  );

  // Preview: fetch the rendered PNG with auth (an <img src> can't carry the
  // gateway-secret header), then render it via an object URL. Re-fetches when
  // the window changes.
  const preview = useQuery({
    queryKey: ["report-preview", window],
    queryFn: () => api.fetchIncidentsReportImage(window),
    staleTime: 30_000,
    retry: false,
  });
  const previewURL = useMemo(
    () => (preview.data ? URL.createObjectURL(preview.data) : ""),
    [preview.data],
  );
  useEffect(() => {
    return () => {
      if (previewURL) URL.revokeObjectURL(previewURL);
    };
  }, [previewURL]);

  const send = useMutation({
    mutationFn: () => api.sendIncidentsReport(window, channel),
    onSuccess: (result) => {
      const s = summarizeReportOutcome(result);
      toast.push({ tone: s.tone, title: s.title, description: s.description });
      onClose();
    },
    onError: (err) => {
      toast.push({
        tone: "error",
        title: "Report failed",
        description: err instanceof Error ? err.message : String(err),
      });
    },
  });

  return (
    <Modal
      title="Incidents report"
      onClose={onClose}
      closeDisabled={send.isPending}
      size="lg"
      footer={
        <>
          <button className="btn" onClick={onClose} disabled={send.isPending}>
            Cancel
          </button>
          <button
            className="btn btn-primary"
            onClick={() => send.mutate()}
            disabled={send.isPending || !hasChannel || !channel}
          >
            {send.isPending ? (
              <>
                <Spinner /> Sending…
              </>
            ) : (
              "Send"
            )}
          </button>
        </>
      }
    >
      <div className="space-y-4">
        {/* Window picker */}
        <div>
          <label className="field-label" htmlFor="report-window">
            Window
          </label>
          <select
            id="report-window"
            className="input"
            value={window}
            onChange={(e) => setWindow(e.target.value as ReportWindow)}
          >
            {REPORT_WINDOWS.map((w) => (
              <option key={w.value} value={w.value}>
                {w.label}
              </option>
            ))}
          </select>
        </div>

        {/* Preview */}
        <div>
          <span className="field-label">Preview</span>
          <div className="mt-1 flex min-h-40 items-center justify-center overflow-hidden rounded-control border border-ink-600 bg-surface-sunken p-2">
            {preview.isLoading ? (
              <span className="inline-flex items-center gap-2 text-xs text-ink-300">
                <Spinner /> Rendering dashboard…
              </span>
            ) : preview.isError ? (
              <ErrorBox error={preview.error} />
            ) : previewURL ? (
              <img
                src={previewURL}
                alt="Incidents report preview"
                className="max-h-[360px] w-full object-contain"
              />
            ) : (
              <span className="text-xs text-ink-400">No preview available.</span>
            )}
          </div>
        </div>

        {/* Channel picker / no-channel degrade */}
        {hasChannel ? (
          <div>
            <label className="field-label" htmlFor="report-channel">
              Channel
            </label>
            <div className="flex items-center gap-2">
              <select
                id="report-channel"
                className="input"
                value={channel}
                onChange={(e) => setChannel(e.target.value)}
              >
                {channels.map((c) => (
                  <option key={c} value={c}>
                    {c}
                  </option>
                ))}
              </select>
              {channel && <ChannelIcon id={channel} size={14} />}
            </div>
            <p className="mt-1 text-2xs text-ink-400">
              Image-capable channels (Slack, Telegram, Email) receive the
              dashboard; others get a redacted text summary.
            </p>
          </div>
        ) : (
          <p className="rounded-control border border-ink-600 bg-surface-sunken/60 p-3 text-xs text-ink-300">
            No notification channel is enabled. Configure a channel (Slack,
            Telegram, Email, …) to send this report. The preview above is still
            downloadable.
          </p>
        )}
      </div>
    </Modal>
  );
}

