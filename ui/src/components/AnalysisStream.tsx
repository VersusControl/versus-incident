import { useEffect, useMemo, useRef, useState } from "react";
import {
  CheckCircle2,
  ChevronDown,
  ChevronRight,
  Wrench,
  XCircle,
} from "lucide-react";
import clsx from "clsx";
import type { AnalyzeEvent } from "@/lib/api";

// Tool output is a JSON envelope; pretty-print it when it parses so the panel
// is readable, and fall back to the raw string when it does not.
function pretty(s: string): string {
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}

// The transcript is a flat, chronological sequence of blocks — prose the model
// wrote, and tools it reached for, in the order they happened.
//
// It deliberately does NOT expose the ReAct loop's turn structure. "Step 1 /
// Step 2" is our internal control flow; the reader is following an
// investigation, not a state machine.
type Block =
  | { kind: "text"; key: string; text: string; streaming: boolean }
  | { kind: "tool"; key: string; ev: AnalyzeEvent };

function buildBlocks(events: AnalyzeEvent[]): Block[] {
  const ordered = [...events].sort((a, b) => a.seq - b.seq);
  const blocks: Block[] = [];
  const toolIndex = new Map<number, number>();

  for (const ev of ordered) {
    switch (ev.kind) {
      case "model_delta": {
        if (!ev.output) break;
        const last = blocks[blocks.length - 1];
        // Consecutive deltas extend the current run of prose in place, so text
        // grows on screen exactly as the model writes it. A tool call between
        // them starts a fresh paragraph after it.
        if (last?.kind === "text" && last.streaming) {
          last.text += ev.output;
        } else {
          blocks.push({
            kind: "text",
            key: `t-${ev.seq}`,
            text: ev.output,
            streaming: true,
          });
        }
        break;
      }
      case "model_finished": {
        const last = blocks[blocks.length - 1];
        if (last?.kind === "text" && last.streaming) {
          last.streaming = false;
          break;
        }
        // A turn that produced prose without streaming it (the non-streaming
        // path, or a provider that emitted nothing incrementally).
        if (ev.output || ev.error) {
          blocks.push({
            kind: "text",
            key: `t-${ev.seq}`,
            text: ev.output || ev.error || "",
            streaming: false,
          });
        }
        break;
      }
      case "tool_started":
      case "tool_finished":
      case "tool_error": {
        const at = toolIndex.get(ev.seq);
        if (at == null) {
          toolIndex.set(ev.seq, blocks.length);
          blocks.push({ kind: "tool", key: `x-${ev.seq}`, ev });
        } else if (ev.kind !== "tool_started") {
          // A terminal event replaces its own "started" in place; a late
          // "started" never overwrites a terminal one.
          blocks[at] = { kind: "tool", key: `x-${ev.seq}`, ev };
        }
        break;
      }
      default:
        break;
    }
  }
  return blocks;
}

// The final answer is a JSON finding, not prose. Streaming it character by
// character would fill the panel with braces and quoted keys, so it shows as a
// status line here and the parsed result is rendered properly once the run
// ends. Intermediate reasoning, which IS prose, still streams as text.
function looksStructured(text: string): boolean {
  return text.trimStart().startsWith("{");
}

function Pending({ label }: { label: string }) {
  return (
    <div className="flex items-center gap-2 py-1 text-2xs text-ink-400">
      <span>{label}</span>
      <span className="inline-flex gap-0.5" aria-hidden>
        {[0, 1, 2].map((i) => (
          <span
            key={i}
            className="h-1 w-1 animate-pulse rounded-full bg-ink-400"
            style={{ animationDelay: `${i * 150}ms` }}
          />
        ))}
      </span>
    </div>
  );
}

function Elapsed({ since }: { since: number }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const iv = window.setInterval(() => setNow(Date.now()), 200);
    return () => window.clearInterval(iv);
  }, []);
  return (
    <span className="tabular-nums text-ink-500">
      {((now - since) / 1000).toFixed(1)}s
    </span>
  );
}

function toolThought(ev: AnalyzeEvent): string {
  const activity =
    ev.tool_display ||
    ev.tool?.replaceAll("_", " ").replaceAll("-", " ") ||
    "Checking another signal";
  const naturalActivity = activity.charAt(0).toLowerCase() + activity.slice(1);
  if (ev.kind === "tool_started") {
    return `I'm ${naturalActivity} to understand what happened…`;
  }
  if (ev.kind === "tool_error") {
    return `I couldn't finish ${naturalActivity}, so I'll continue with the evidence I have.`;
  }
  return `I finished ${naturalActivity} and added what I found to the investigation.`;
}

// ToolLine sits inline in the prose, the way a chat assistant mentions what it
// just did. Collapsed by default: the reader is following the answer, and the
// arguments/output are there for whoever wants to audit the step.
function ToolLine({ ev }: { ev: AnalyzeEvent }) {
  const [open, setOpen] = useState(false);
  const running = ev.kind === "tool_started";
  const body = ev.output || ev.error || "";
  const canOpen = Boolean(body || ev.args);

  return (
    <div className="my-1.5">
      <button
        type="button"
        onClick={() => canOpen && setOpen((v) => !v)}
        aria-expanded={canOpen ? open : undefined}
        disabled={!canOpen}
        className={clsx(
          "flex w-full items-center gap-2 rounded-control px-2 py-1 text-left text-2xs",
          canOpen ? "hover:bg-ink-800/60" : "cursor-default",
        )}
      >
        {canOpen ? (
          open ? (
            <ChevronDown size={11} className="shrink-0 text-ink-500" aria-hidden />
          ) : (
            <ChevronRight size={11} className="shrink-0 text-ink-500" aria-hidden />
          )
        ) : (
          <span className="w-[11px] shrink-0" aria-hidden />
        )}
        <Wrench size={11} className="shrink-0 text-ink-500" aria-hidden />
        <span className="text-ink-300">
          {toolThought(ev)}
        </span>
        {running ? (
          <span
            className="h-1.5 w-1.5 shrink-0 animate-pulse rounded-full bg-accent"
            aria-hidden
          />
        ) : ev.kind === "tool_error" ? (
          <XCircle size={11} className="shrink-0 text-danger" aria-hidden />
        ) : (
          <CheckCircle2 size={11} className="shrink-0 text-ok" aria-hidden />
        )}
        {!running && ev.duration_ms != null && (
          <span className="tabular-nums text-ink-500">{ev.duration_ms}ms</span>
        )}
      </button>

      {open && (
        <div className="mt-1 space-y-1 pl-6">
          {ev.args && (
            <pre className="max-h-40 max-w-full overflow-x-hidden overflow-y-auto whitespace-pre-wrap [overflow-wrap:anywhere] rounded bg-ink-900 p-2 text-[10px] leading-relaxed text-ink-300">
              {pretty(ev.args)}
            </pre>
          )}
          {body && (
            <pre
              className={clsx(
                "max-h-56 max-w-full overflow-x-hidden overflow-y-auto whitespace-pre-wrap [overflow-wrap:anywhere] rounded bg-ink-900 p-2 text-[10px] leading-relaxed",
                ev.kind === "tool_error" ? "text-danger" : "text-ink-300",
              )}
            >
              {pretty(body)}
            </pre>
          )}
        </div>
      )}
    </div>
  );
}

// AnalysisStream is the live transcript of an analyze run: the model's answer
// as it is written, with the tools it reached for appearing inline at the
// point it reached for them.
export function AnalysisStream({
  events,
  running,
  startedAt,
}: {
  events: AnalyzeEvent[];
  running: boolean;
  startedAt: number;
}) {
  const blocks = useMemo(() => buildBlocks(events), [events]);
  const endRef = useRef<HTMLDivElement>(null);
  const terminal = events.find(
    (e) => e.kind === "run_finished" || e.kind === "run_failed",
  );

  useEffect(() => {
    endRef.current?.scrollIntoView?.({ block: "nearest" });
  }, [events.length]);

  const last = blocks[blocks.length - 1];
  // Show the thinking indicator only when nothing else is moving: before the
  // first token, or after a tool returned while the model composes its next
  // sentence. While text is streaming the cursor already conveys it.
  const idle =
    running &&
    (blocks.length === 0 ||
      (last?.kind === "tool" && last.ev.kind !== "tool_started") ||
      (last?.kind === "text" && !last.streaming));

  if (!events.length && !running) return null;

  return (
    <div
      data-testid="analysis-stream"
      role="log"
      aria-live="polite"
      aria-label="Analysis progress"
    >
      {blocks.map((b) =>
        b.kind === "tool" ? (
          <ToolLine key={b.key} ev={b.ev} />
        ) : looksStructured(b.text) ? (
          b.streaming ? (
            <Pending key={b.key} label="Writing the analysis" />
          ) : null
        ) : (
          <p
            key={b.key}
            className="whitespace-pre-wrap break-words py-0.5 text-xs leading-relaxed text-ink-200"
          >
            {b.text}
            {b.streaming && (
              <span
                className="ml-0.5 inline-block h-3 w-1 animate-pulse bg-accent align-middle"
                aria-hidden
              />
            )}
          </p>
        ),
      )}

      {idle && <Pending label="Thinking" />}

      <div className="mt-2 text-2xs">
        {running ? (
          <Elapsed since={startedAt} />
        ) : terminal ? (
          <span
            className={clsx(
              "flex items-center gap-1.5",
              terminal.kind === "run_failed" ? "text-danger" : "text-ink-500",
            )}
          >
            {terminal.kind === "run_failed" ? (
              <>
                <XCircle size={11} aria-hidden />
                {terminal.error ?? "Analysis failed"}
              </>
            ) : (
              <>
                <CheckCircle2 size={11} className="text-ok" aria-hidden />
                Done
                {terminal.duration_ms != null && (
                  <span className="tabular-nums">
                    in {(terminal.duration_ms / 1000).toFixed(1)}s
                  </span>
                )}
              </>
            )}
          </span>
        ) : null}
      </div>
      <div ref={endRef} />
    </div>
  );
}
