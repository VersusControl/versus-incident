import {
  useEffect,
  useRef,
  useState,
  type FormEvent,
  type KeyboardEvent,
} from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useSearchParams } from "react-router-dom";
import {
  ArrowDown,
  Bot,
  CheckCircle2,
  ChevronDown,
  ChevronRight,
  CircleStop,
  Clock3,
  History,
  LoaderCircle,
  Paperclip,
  PanelLeftClose,
  PanelLeftOpen,
  Plus,
  Send,
  Trash2,
  Wrench,
  X,
  XCircle,
} from "lucide-react";
import clsx from "clsx";
import { TopBar } from "@/components/TopBar";
import { RetryableError } from "@/components/RetryableError";
import { MarkdownText } from "@/components/chat/MarkdownText";
import {
  api,
  ApiError,
  type ChatAttachment,
  type ChatCitation,
  type ChatEvent,
  type ChatSession,
  type ChatSessionSummary,
  type ChatToolCall,
  type ChatTurn,
} from "@/lib/api";
import {
  emptyChatStream,
  reduceChatEvent,
  type ChatStreamBlock,
} from "@/lib/chatStream";
import { fmtRel, truncate } from "@/lib/format";
import { capChatMessage, MAX_CHAT_MESSAGE_BYTES } from "@/lib/markdownPolicy";
import { useStickyBottom } from "@/lib/useStickyBottom";

const suggestions = [
  "What changed before the latest incident?",
  "Which services have the highest error rate?",
  "Summarize unresolved incidents from the last 24 hours.",
];

interface EvidenceSelection {
  citation: ChatCitation;
  output?: string;
}

interface LiveRun {
  active: boolean;
  optimistic: ChatTurn;
  persistedTurnIds: string[];
  stream: typeof emptyChatStream;
  error: Error | null;
  stopping: boolean;
}

const maxAttachmentSpanMs = 32 * 24 * 60 * 60 * 1000;
const messageEncoder = new TextEncoder();

function trapDialogFocus(event: KeyboardEvent<HTMLElement>, onClose: () => void) {
  if (event.key === "Escape") {
    event.preventDefault();
    onClose();
    return;
  }
  if (event.key !== "Tab") return;
  const controls = Array.from(event.currentTarget.querySelectorAll<HTMLElement>(
    'button:not([disabled]), [href], input:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
  )).filter((control) => control.offsetParent !== null);
  if (controls.length === 0) {
    event.preventDefault();
    event.currentTarget.focus();
    return;
  }
  const first = controls[0];
  const last = controls[controls.length - 1];
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault();
    first.focus();
  }
}

function sessionTitle(summary: ChatSessionSummary, full?: ChatSession) {
  const firstUser = full?.turns.find((turn) => turn.role === "user")?.content.trim();
  return firstUser ? truncate(firstUser, 54) : `Chat · ${fmtRel(summary.updated_at)}`;
}

function toolProse(event: ChatEvent) {
  const label =
    event.tool_display ||
    event.tool?.replaceAll("_", " ").replaceAll("-", " ") ||
    "Checking another signal";
  const activity = label.charAt(0).toLowerCase() + label.slice(1);
  if (event.kind === "tool_started") return `I'm ${activity} to gather evidence…`;
  if (event.error) return `I couldn't finish ${activity}.`;
  return `I finished ${activity} and added the result.`;
}

function pretty(value?: string) {
  if (!value) return "";
  try {
    return JSON.stringify(JSON.parse(value), null, 2);
  } catch {
    return value;
  }
}

function ToolActivity({ event }: { event: ChatEvent }) {
  const [open, setOpen] = useState(false);
  const running = event.kind === "tool_started";
  const failed = Boolean(event.error);
  const expandable = Boolean(event.args || event.output || event.error);
  return (
    <div className="my-2 border-l-2 border-ink-500/60 pl-2">
      <button
        type="button"
        disabled={!expandable}
        onClick={() => setOpen((value) => !value)}
        aria-expanded={expandable ? open : undefined}
        className="flex min-h-8 w-full items-center gap-2 rounded-control px-1.5 text-left text-xs text-ink-300 hover:bg-ink-700/50 disabled:cursor-default"
      >
        {expandable ? open ? <ChevronDown size={13} /> : <ChevronRight size={13} /> : <span className="w-[13px]" />}
        <Wrench size={13} aria-hidden />
        <span className="min-w-0 flex-1">{toolProse(event)}</span>
        {running ? (
          <LoaderCircle size={13} className="animate-spin text-accent" aria-label="Running" />
        ) : failed ? (
          <XCircle size={13} className="text-sev-critical" aria-label="Error" />
        ) : (
          <CheckCircle2 size={13} className="text-sev-ok" aria-label="Success" />
        )}
        {event.duration_ms != null && event.duration_ms > 0 && <span className="tabular-nums text-ink-400">{event.duration_ms}ms</span>}
      </button>
      {open && (
        <div className="mt-1 space-y-2 pl-6">
          {event.args && <ToolPayload label="Arguments" value={event.args} />}
          {(event.output || event.error) && <ToolPayload label={failed ? "Error" : "Output"} value={event.output || event.error || ""} />}
        </div>
      )}
    </div>
  );
}

function ToolPayload({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0">
      <div className="mb-1 text-2xs font-medium uppercase text-ink-400">{label}</div>
      <pre className="max-h-56 max-w-full overflow-auto whitespace-pre-wrap break-words rounded-control bg-ink-900 p-2 text-[11px] leading-5 text-ink-200">{pretty(value)}</pre>
    </div>
  );
}

function traceEvent(call: ChatToolCall, seq: number): ChatEvent {
  return {
    seq,
    at: "",
    kind: "tool_finished",
    tool: call.Name,
    call_id: call.call_id,
    tool_display: call.Name.replaceAll("_", " "),
    args: call.Args,
    output: call.Output,
    duration_ms: call.DurationMs > 0 ? call.DurationMs : undefined,
    error: call.Error,
  };
}

function citedToolCall(citation: ChatCitation, calls: ChatToolCall[] | undefined) {
  if (!calls?.length) return undefined;
  const byCallID = calls.find((call) =>
    call.call_id && citation.locator === `tool-call-${call.call_id}`
  );
  if (byCallID) return byCallID;
  const ordinal = citation.locator?.match(/^tool-call-(\d+)$/);
  if (ordinal) {
    const indexed = calls[Number(ordinal[1]) - 1];
    if (indexed) return indexed;
  }
  return calls.find((call) => call.Name === citation.tool);
}

function CitationList({ citations, onOpen }: { citations?: ChatCitation[]; onOpen: (value: EvidenceSelection) => void }) {
  if (!citations?.length) return null;
  return (
    <div className="mt-3 flex flex-wrap gap-2" aria-label="Sources">
      {citations.map((citation, index) => (
        <button
          key={`${citation.tool}-${citation.locator ?? index}`}
          type="button"
          onClick={() => onOpen({ citation })}
          className="inline-flex min-h-8 items-center gap-1.5 rounded-control border border-ink-500/60 bg-surface-raised px-2 text-2xs text-link hover:border-link/60"
        >
          [{index + 1}] {citation.label || citation.tool.replaceAll("_", " ")}
        </button>
      ))}
    </div>
  );
}

function TurnView({ turn, onEvidence }: { turn: ChatTurn; onEvidence: (value: EvidenceSelection) => void }) {
  if (turn.role === "compaction") {
    return <div className="my-4 text-center text-2xs italic text-ink-400">{turn.content}</div>;
  }
  if (turn.role === "user") {
    return (
      <div className="my-4 flex justify-end">
        <div className="max-w-[85%] rounded-2xl bg-ink-700 px-4 py-2.5 text-sm leading-6 text-ink-50">{turn.content}</div>
      </div>
    );
  }
  return (
    <article className="my-6 min-w-0">
      {turn.tool_calls?.map((call, index) => <ToolActivity key={`${turn.id}-${index}`} event={traceEvent(call, index)} />)}
      <MarkdownText>{turn.content}</MarkdownText>
      <CitationList
        citations={turn.citations}
        onOpen={({ citation }) =>
          onEvidence({
            citation,
            output: citedToolCall(citation, turn.tool_calls)?.Output,
          })
        }
      />
    </article>
  );
}

function LiveBlocks({ blocks, terminal }: { blocks: ChatStreamBlock[]; terminal: ChatEvent | null }) {
  return (
    <div className="my-6 min-w-0">
      {blocks.map((block) => {
        if (block.kind === "tool") return <ToolActivity key={block.key} event={block.event} />;
        if (block.kind === "compaction") return <div key={block.key} className="my-3 text-center text-2xs italic text-ink-400">{block.text}</div>;
        return <MarkdownText key={block.key}>{block.text}</MarkdownText>;
      })}
      {terminal && terminal.kind !== "run_finished" && (
        <div
          role="status"
          className={clsx(
            "mt-3 flex items-center gap-2 text-xs",
            terminal.kind === "run_cancelled" ? "text-ink-400" : "text-sev-critical",
          )}
        >
          {terminal.kind === "run_cancelled" ? <CircleStop size={13} /> : <XCircle size={13} />}
          {terminal.error ||
            (terminal.kind === "run_throttled"
              ? "Rate limit reached; retry later."
              : "Chat run failed.")}
        </div>
      )}
    </div>
  );
}

function HistoryRail({ sessions, selected, selectedFull, loading, deleteErrors, onSelect, onDelete, onDismissDeleteError, onClose }: {
  sessions: ChatSessionSummary[];
  selected: string | null;
  selectedFull?: ChatSession;
  loading: boolean;
  deleteErrors: Record<string, Error>;
  onSelect: (id: string) => void;
  onDelete: (id: string) => void;
  onDismissDeleteError: (id: string) => void;
  onClose: () => void;
}) {
  return (
    <div className="flex h-full w-[300px] flex-col border-r border-ink-500/50 bg-surface-sunken">
      <div className="flex h-12 items-center justify-between border-b border-ink-500/40 px-3">
        <span className="text-xs font-semibold text-ink-100">Thread history</span>
        <button type="button" onClick={onClose} aria-label="Close history" title="Close history" className="rounded-control p-2 text-ink-300 hover:bg-ink-700"><PanelLeftClose size={16} /></button>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto p-2">
        {loading ? (
          <div className="space-y-2" aria-label="Loading history">{[0, 1, 2, 3].map((value) => <div key={value} className="sk h-11 rounded-control" />)}</div>
        ) : sessions.length === 0 ? (
          <div className="px-2 py-8 text-center text-xs text-ink-400">No conversations yet</div>
        ) : sessions.map((session) => (
          <div key={session.id} className="mb-1">
            <div className={clsx("group flex items-center rounded-control", selected === session.id ? "bg-accent-subtle" : "hover:bg-ink-700/50")}>
              <button type="button" onClick={() => onSelect(session.id)} className="min-w-0 flex-1 px-2 py-2 text-left" aria-current={selected === session.id ? "page" : undefined}>
                <div className="truncate text-xs text-ink-100">{sessionTitle(session, selected === session.id ? selectedFull : undefined)}</div>
                <div className="mt-0.5 flex items-center gap-1 text-2xs text-ink-400"><Clock3 size={10} />{fmtRel(session.updated_at)}{session.status === "running" && <span> · running</span>}</div>
              </button>
              <button type="button" onClick={() => onDelete(session.id)} aria-label="Delete thread" title="Delete thread" className="mr-1 rounded-control p-2 text-ink-400 opacity-100 hover:bg-sev-critical/10 hover:text-sev-critical lg:opacity-0 lg:group-hover:opacity-100 lg:focus:opacity-100"><Trash2 size={13} /></button>
            </div>
            {deleteErrors[session.id] && (
              <div role="alert" className="mt-1 rounded-control border border-sev-critical/40 bg-sev-critical/10 p-2 text-2xs text-sev-critical">
                <div>{deleteErrors[session.id].message}</div>
                <div className="mt-2 flex gap-2">
                  <button type="button" className="btn" onClick={() => onDelete(session.id)}>Retry</button>
                  <button type="button" className="btn" onClick={() => onDismissDeleteError(session.id)}>Dismiss</button>
                </div>
              </div>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}

function Composer({ value, onChange, onSubmit, running, stopping, onStop, attachment, onAttachmentChange, resetGeneration }: {
  value: string;
  onChange: (value: string) => void;
  onSubmit: () => void;
  running: boolean;
  stopping: boolean;
  onStop: () => void;
  attachment: ChatAttachment;
  onAttachmentChange: (value: ChatAttachment) => void;
  resetGeneration: number;
}) {
  const [contextOpen, setContextOpen] = useState(false);
  const [start, setStart] = useState(attachment.time_range?.start ?? "");
  const [end, setEnd] = useState(attachment.time_range?.end ?? "");
  const [timeError, setTimeError] = useState("");
  useEffect(() => {
    setStart("");
    setEnd("");
    setTimeError("");
  }, [resetGeneration]);
  const messageBytes = messageEncoder.encode(value).byteLength;
  const submit = (event: FormEvent) => { event.preventDefault(); if (!running && !timeError && value.trim()) onSubmit(); };
  const keyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key === "Enter" && !event.shiftKey) { event.preventDefault(); if (!running && !timeError && value.trim()) onSubmit(); }
  };
  const setTime = (nextStart: string, nextEnd: string) => {
    setStart(nextStart); setEnd(nextEnd);
    if (!nextStart && !nextEnd) {
      setTimeError("");
      onAttachmentChange({ ...attachment, time_range: undefined });
      return;
    }
    if (!nextStart || !nextEnd) {
      setTimeError("Choose both a start and an end time.");
      onAttachmentChange({ ...attachment, time_range: undefined });
      return;
    }
    const startDate = new Date(nextStart);
    const endDate = new Date(nextEnd);
    if (!Number.isFinite(startDate.getTime()) || !Number.isFinite(endDate.getTime())) {
      setTimeError("Enter valid start and end times.");
      onAttachmentChange({ ...attachment, time_range: undefined });
      return;
    }
    if (startDate >= endDate) {
      setTimeError("End time must be after start time.");
      onAttachmentChange({ ...attachment, time_range: undefined });
      return;
    }
    if (endDate.getTime() - startDate.getTime() > maxAttachmentSpanMs) {
      setTimeError("Choose a time window of 32 days or less.");
      onAttachmentChange({ ...attachment, time_range: undefined });
      return;
    }
    setTimeError("");
    onAttachmentChange({ ...attachment, time_range: { start: startDate.toISOString(), end: endDate.toISOString() } });
  };
  const chips = [
    attachment.service ? { key: "service", label: `Service: ${attachment.service}` } : null,
    attachment.incident?.id ? { key: "incident", label: `Incident: ${attachment.incident.id}` } : null,
    attachment.time_range ? { key: "time", label: "Time window" } : null,
  ].filter(Boolean) as { key: string; label: string }[];
  const remove = (key: string) => {
    if (key === "service") onAttachmentChange({ ...attachment, service: undefined });
    if (key === "incident") onAttachmentChange({ ...attachment, incident: undefined });
    if (key === "time") { setStart(""); setEnd(""); onAttachmentChange({ ...attachment, time_range: undefined }); }
  };
  return (
    <form onSubmit={submit} className="rounded-xl border border-ink-500/70 bg-surface shadow-card">
      {contextOpen && (
        <div className="grid gap-2 border-b border-ink-500/40 p-3 sm:grid-cols-2">
          <label className="text-2xs text-ink-300">Service<input aria-label="Service context" value={attachment.service ?? ""} onChange={(event) => onAttachmentChange({ ...attachment, service: event.target.value || undefined })} className="input mt-1 w-full" placeholder="checkout-api" /></label>
          <label className="text-2xs text-ink-300">Incident ID<input aria-label="Incident ID context" value={attachment.incident?.id ?? ""} onChange={(event) => onAttachmentChange({ ...attachment, incident: event.target.value ? { id: event.target.value } : undefined })} className="input mt-1 w-full" placeholder="inc_..." /></label>
          <label className="text-2xs text-ink-300">Start<input aria-label="Context start" aria-invalid={Boolean(timeError)} aria-describedby={timeError ? "chat-time-error" : undefined} type="datetime-local" value={start} onChange={(event) => setTime(event.target.value, end)} className="input mt-1 w-full" /></label>
          <label className="text-2xs text-ink-300">End<input aria-label="Context end" aria-invalid={Boolean(timeError)} aria-describedby={timeError ? "chat-time-error" : undefined} type="datetime-local" value={end} onChange={(event) => setTime(start, event.target.value)} className="input mt-1 w-full" /></label>
          {timeError && <p id="chat-time-error" role="alert" className="text-2xs text-sev-critical sm:col-span-2">{timeError}</p>}
        </div>
      )}
      {chips.length > 0 && <div className="flex flex-wrap gap-1.5 px-3 pt-2">{chips.map((chip) => <span key={chip.key} className="pill max-w-full"><span className="truncate">{chip.label}</span><button type="button" onClick={() => remove(chip.key)} aria-label={`Remove ${chip.label}`}><X size={11} /></button></span>)}</div>}
      <textarea
        value={value}
        onChange={(event) => onChange(capChatMessage(event.target.value))}
        onKeyDown={keyDown}
        rows={2}
        placeholder="Ask about your system"
        aria-label="Message"
        aria-describedby="chat-message-budget"
        className="max-h-40 min-h-[72px] w-full resize-none bg-transparent px-4 py-3 text-base leading-6 text-ink-50 outline-none placeholder:text-ink-400 lg:text-sm"
      />
      <div className="flex min-h-12 items-center justify-between gap-2 px-2 pb-2">
        <button type="button" onClick={() => setContextOpen((open) => !open)} aria-expanded={contextOpen} aria-label="Attach context" title="Attach service, incident, or time context" className="inline-flex min-h-11 min-w-11 items-center justify-center rounded-control text-ink-300 hover:bg-ink-700"><Paperclip size={17} /></button>
        <span id="chat-message-budget" className="ml-auto text-2xs tabular-nums text-ink-400">{MAX_CHAT_MESSAGE_BYTES - messageBytes} bytes left</span>
        {running ? (
          <button type="button" onClick={onStop} disabled={stopping} className="inline-flex min-h-11 items-center gap-2 rounded-control bg-ink-100 px-3 text-xs font-semibold text-ink-900 disabled:opacity-60"><CircleStop size={16} />{stopping ? "Stopping" : "Stop"}</button>
        ) : (
          <button type="submit" disabled={!value.trim() || Boolean(timeError)} className="inline-flex min-h-11 items-center gap-2 rounded-control bg-accent px-3 text-xs font-semibold text-white hover:bg-accent-hover disabled:opacity-40"><Send size={16} />Send</button>
        )}
      </div>
    </form>
  );
}

export function ChatPage() {
  const queryClient = useQueryClient();
  const [searchParams, setSearchParams] = useSearchParams();
  const selectedId = searchParams.get("session");
  const [historyOpen, setHistoryOpen] = useState(true);
  const [mobileHistory, setMobileHistory] = useState(false);
  const [evidence, setEvidence] = useState<EvidenceSelection | null>(null);
  const [draft, setDraft] = useState("");
  const [attachment, setAttachment] = useState<ChatAttachment>({});
  const [composerResetGeneration, setComposerResetGeneration] = useState(0);
  const [liveRuns, setLiveRuns] = useState<Record<string, LiveRun>>({});
  const [deleteErrors, setDeleteErrors] = useState<Record<string, Error>>({});
  const abortRefs = useRef(new Map<string, AbortController>());
  const submittingRef = useRef(false);
  const historyTriggerRef = useRef<HTMLButtonElement | null>(null);
  const evidenceTriggerRef = useRef<HTMLElement | null>(null);
  const historyDialogRef = useRef<HTMLDivElement | null>(null);
  const evidenceDialogRef = useRef<HTMLDivElement | null>(null);
  const conversationRef = useRef<HTMLDivElement | null>(null);
  const [isMobile, setIsMobile] = useState(false);

  const sessionsQ = useQuery({
    queryKey: ["chat-sessions"],
    queryFn: api.listChatSessions,
    retry: (failureCount, error) =>
      !(error instanceof ApiError && error.status === 503) && failureCount < 1,
  });
  const sessionQ = useQuery({ queryKey: ["chat-session", selectedId], queryFn: () => api.getChatSession(selectedId as string), enabled: Boolean(selectedId), retry: 1 });
  const turns = sessionQ.data?.turns ?? [];
  const selectedRun = selectedId ? liveRuns[selectedId] : undefined;
  const active = Boolean(selectedRun?.active);
  const stream = selectedRun?.stream ?? emptyChatStream;
  const optimistic = selectedRun?.optimistic ?? null;
  const liveTail = stream.blocks.at(-1);
  const scroll = useStickyBottom(`${turns.length}:${stream.blocks.length}:${liveTail?.kind === "text" ? liveTail.text.length : 0}`);
  const optimisticPersisted = optimistic && turns.some((turn) =>
    turn.role === "user" &&
    !selectedRun?.persistedTurnIds.includes(turn.id) &&
    turn.content === optimistic.content &&
    Math.abs(new Date(turn.created_at).getTime() - new Date(optimistic.created_at).getTime()) <= 5 * 60 * 1000
  );
  const displayTurns = optimistic && !optimisticPersisted ? [...turns, optimistic] : turns;

  useEffect(() => {
    if (typeof window.matchMedia !== "function") return;
    const media = window.matchMedia("(max-width: 1023px)");
    const update = () => setIsMobile(media.matches);
    update();
    media.addEventListener("change", update);
    return () => media.removeEventListener("change", update);
  }, []);

  useEffect(() => {
    const modalOpen = isMobile && (mobileHistory || Boolean(evidence));
    const content = conversationRef.current;
    if (!content) return;
    content.inert = modalOpen;
    if (modalOpen) content.setAttribute("aria-hidden", "true");
    else content.removeAttribute("aria-hidden");
  }, [evidence, isMobile, mobileHistory]);

  useEffect(() => {
    if (mobileHistory) historyDialogRef.current?.focus();
  }, [mobileHistory]);

  useEffect(() => {
    if (evidence && isMobile) evidenceDialogRef.current?.focus();
  }, [evidence, isMobile]);

  useEffect(() => () => {
    for (const controller of abortRefs.current.values()) controller.abort();
    abortRefs.current.clear();
  }, []);

  const newThread = () => {
    setSearchParams({}); setDraft(""); setAttachment({}); setComposerResetGeneration((value) => value + 1); setEvidence(null); setMobileHistory(false);
  };
  const selectThread = (id: string) => { setSearchParams({ session: id }); setMobileHistory(false); setEvidence(null); };
  const closeHistory = () => {
    setMobileHistory(false);
    queueMicrotask(() => historyTriggerRef.current?.focus());
  };
  const openEvidence = (value: EvidenceSelection) => {
    evidenceTriggerRef.current = document.activeElement as HTMLElement | null;
    setEvidence(value);
  };
  const closeEvidence = () => {
    setEvidence(null);
    queueMicrotask(() => evidenceTriggerRef.current?.focus());
  };
  const deleteThread = async (id: string) => {
    try {
      await api.deleteChatSession(id);
      setDeleteErrors((errors) => {
        if (!errors[id]) return errors;
        const next = { ...errors };
        delete next[id];
        return next;
      });
      await queryClient.invalidateQueries({ queryKey: ["chat-sessions"] });
      queryClient.removeQueries({ queryKey: ["chat-session", id] });
      setLiveRuns((runs) => {
        if (!runs[id]) return runs;
        const next = { ...runs };
        delete next[id];
        return next;
      });
      if (selectedId === id) newThread();
    } catch (error) {
      const nextError = error instanceof Error ? error : new Error(String(error));
      setDeleteErrors((errors) => ({ ...errors, [id]: nextError }));
    }
  };

  const send = async () => {
    const message = draft.trim();
    if (!message || active || submittingRef.current) return;
    submittingRef.current = true;
    setDraft("");
    let id = selectedId;
    try {
      if (!id) {
        const created = await api.createChatSession();
        id = created.id;
        queryClient.setQueryData(["chat-session", id], created);
        setSearchParams({ session: id });
      }
      const optimisticTurn: ChatTurn = { id: `local-${Date.now()}`, role: "user", content: message, created_at: new Date().toISOString(), attachment };
      setLiveRuns((runs) => ({
        ...runs,
        [id as string]: { active: true, optimistic: optimisticTurn, persistedTurnIds: turns.map((turn) => turn.id), stream: emptyChatStream, error: null, stopping: false },
      }));
      submittingRef.current = false;
      setAttachment({});
      setComposerResetGeneration((value) => value + 1);
      const controller = new AbortController();
      abortRefs.current.set(id, controller);
      await api.streamChatMessage(id, message, Object.keys(attachment).length ? attachment : undefined, (event) => {
        setLiveRuns((runs) => {
          const run = runs[id as string];
          return run ? { ...runs, [id as string]: { ...run, stream: reduceChatEvent(run.stream, event) } } : runs;
        });
      }, controller.signal);
      const refreshed = await queryClient.fetchQuery({ queryKey: ["chat-session", id], queryFn: () => api.getChatSession(id as string), staleTime: 0 });
      if (refreshed.status !== "running") setLiveRuns((runs) => {
          const next = { ...runs };
          delete next[id as string];
          return next;
        });
    } catch (error) {
      const nextError = error instanceof Error ? error : new Error(String(error));
      if (!(error instanceof DOMException && error.name === "AbortError")) {
        if (id) setLiveRuns((runs) => {
          const run = runs[id as string];
          return run ? { ...runs, [id as string]: { ...run, active: false, error: nextError } } : runs;
        });
      }
      try {
        if (id) {
          let refreshed: ChatSession | undefined;
          for (let attempt = 0; attempt < 6; attempt += 1) {
            refreshed = await queryClient.fetchQuery({ queryKey: ["chat-session", id], queryFn: () => api.getChatSession(id as string), staleTime: 0 });
            if (refreshed?.status !== "running") break;
            await new Promise((resolve) => setTimeout(resolve, 500));
          }
          if (refreshed?.status !== "running") setLiveRuns((runs) => {
            const run = runs[id as string];
            return run ? { ...runs, [id as string]: { ...run, active: false, stopping: false } } : runs;
          });
        }
      } catch { /* retain the visible stream until retry */ }
    } finally {
      submittingRef.current = false;
      if (id) {
        abortRefs.current.delete(id);
        setLiveRuns((runs) => {
          const run = runs[id as string];
          return run ? { ...runs, [id as string]: { ...run, active: false, stopping: false } } : runs;
        });
      }
      await queryClient.invalidateQueries({ queryKey: ["chat-sessions"] });
    }
  };

  const stop = async () => {
    if (!selectedId || !selectedRun?.active || selectedRun.stopping) return;
    setLiveRuns((runs) => ({ ...runs, [selectedId]: { ...runs[selectedId], stopping: true } }));
    try {
      await api.cancelChatRun(selectedId);
      const cancelledID = selectedId;
      void (async () => {
        await new Promise((resolve) => setTimeout(resolve, 1500));
        let refreshed: ChatSession | undefined;
        for (let attempt = 0; attempt < 6; attempt += 1) {
          try {
            refreshed = await queryClient.fetchQuery({ queryKey: ["chat-session", cancelledID], queryFn: () => api.getChatSession(cancelledID), staleTime: 0 });
          } catch {
            // Keep the partial stream and try again within the bounded window.
          }
          if (refreshed && refreshed.status !== "running") {
            abortRefs.current.get(cancelledID)?.abort();
            setLiveRuns((runs) => {
              const next = { ...runs };
              delete next[cancelledID];
              return next;
            });
            return;
          }
          await new Promise((resolve) => setTimeout(resolve, 500));
        }
        abortRefs.current.get(cancelledID)?.abort();
        setLiveRuns((runs) => {
          const run = runs[cancelledID];
          const error = new Error("Cancellation is still being confirmed. Refresh the conversation to resync.");
          return run ? { ...runs, [cancelledID]: { ...run, active: false, stopping: false, error } } : runs;
        });
      })();
    } catch (error) {
      const nextError = error instanceof Error ? error : new Error(String(error));
      setLiveRuns((runs) => ({ ...runs, [selectedId]: { ...runs[selectedId], stopping: false, error: nextError } }));
    }
  };

  const retryResync = async () => {
    if (!selectedId || !selectedRun) {
      return;
    }
    setLiveRuns((runs) => ({ ...runs, [selectedId]: { ...runs[selectedId], error: null } }));
    try {
      const refreshed = await queryClient.fetchQuery({ queryKey: ["chat-session", selectedId], queryFn: () => api.getChatSession(selectedId), staleTime: 0 });
      if (refreshed.status === "running") {
        throw new Error("The run is still active. Retry shortly to resync.");
      }
      setLiveRuns((runs) => {
        const next = { ...runs };
        delete next[selectedId];
        return next;
      });
    } catch (error) {
      const nextError = error instanceof Error ? error : new Error(String(error));
      setLiveRuns((runs) => ({ ...runs, [selectedId]: { ...runs[selectedId], error: nextError } }));
    }
  };

  const unavailable = sessionsQ.error instanceof ApiError && sessionsQ.error.status === 503;
  const conversationStarted = displayTurns.length > 0 || active;
  const rail = <HistoryRail sessions={sessionsQ.data ?? []} selected={selectedId} selectedFull={sessionQ.data} loading={sessionsQ.isLoading} deleteErrors={deleteErrors} onSelect={selectThread} onDelete={deleteThread} onDismissDeleteError={(id) => setDeleteErrors((errors) => {
    if (!errors[id]) return errors;
    const next = { ...errors };
    delete next[id];
    return next;
  })} onClose={() => { setHistoryOpen(false); closeHistory(); }} />;

  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden bg-surface-sunken">
      <TopBar title="DevOps Agent" subtitle={sessionQ.data?.status === "running" ? "Running" : ""} actions={<><button ref={historyTriggerRef} type="button" onClick={() => setMobileHistory(true)} aria-label="Open chat history" title="Open chat history" className="inline-flex min-h-11 min-w-11 items-center justify-center rounded-control text-ink-300 hover:bg-ink-700 lg:hidden"><History size={16} /></button><button type="button" onClick={newThread} aria-label="New thread" title="New thread" className="inline-flex min-h-11 min-w-11 items-center justify-center rounded-control text-ink-300 hover:bg-ink-700"><Plus size={16} /></button></>} />
      <div className="relative flex min-h-0 flex-1 overflow-hidden">
        <div className={clsx("hidden shrink-0 overflow-hidden transition-[width] duration-300 lg:block", historyOpen ? "w-[300px]" : "w-0")}>{rail}</div>
        {!historyOpen && <button type="button" onClick={() => setHistoryOpen(true)} aria-label="Open chat history" title="Open chat history" className="absolute left-3 top-3 z-sticky hidden rounded-control border border-ink-500/50 bg-surface p-2 text-ink-300 shadow-card hover:bg-ink-700 lg:block"><PanelLeftOpen size={16} /></button>}
        {mobileHistory && <div ref={historyDialogRef} className="fixed inset-0 z-overlay lg:hidden" role="dialog" aria-modal="true" aria-label="Thread history" tabIndex={-1} onKeyDown={(event) => trapDialogFocus(event, closeHistory)}><button aria-label="Close history" className="absolute inset-0 bg-black/70" onClick={closeHistory} /><div className="absolute inset-y-0 left-0 w-[min(300px,85vw)] shadow-overlay">{rail}</div></div>}

        <div
          ref={conversationRef}
          className="grid min-w-0 flex-1 transition-[grid-template-columns] duration-300 lg:grid"
          style={{
            gridTemplateColumns: evidence
              ? "minmax(0, 3fr) minmax(0, 2fr)"
              : "minmax(0, 1fr) 0fr",
          }}
        >
          <main className="relative min-w-0 overflow-hidden">
            {unavailable ? (
              <div className="mx-auto mt-16 max-w-lg px-4"><div role="alert" className="rounded-card border border-sev-warn/40 bg-sev-warn/10 p-5"><h2 className="font-semibold text-ink-50">Chat is not enabled</h2><p className="mt-2 text-sm text-ink-300">Enable the AI chat service and model configuration, then retry.</p><button className="btn mt-4" onClick={() => sessionsQ.refetch()}>Retry</button></div></div>
            ) : sessionsQ.isError ? (
              <div className="mx-auto mt-12 max-w-lg px-4"><RetryableError error={sessionsQ.error} onRetry={() => sessionsQ.refetch()} retrying={sessionsQ.isFetching} context="Couldn't load chat history" /></div>
            ) : (
              <>
                <div ref={scroll.scrollRef} onScroll={scroll.onScroll} className="h-full overflow-y-auto overscroll-contain px-4" role="log" aria-label="Conversation">
                  <div className={clsx("mx-auto max-w-3xl", conversationStarted ? "pb-52 pt-8" : "min-h-full pb-44 pt-[18vh]") }>
                    {!conversationStarted && <div className="mx-auto max-w-lg text-center"><Bot size={28} className="mx-auto text-accent" aria-hidden /><h2 className="mt-3 text-base font-semibold text-ink-50">Versus DevOps Chat</h2><p className="mt-1 text-sm text-ink-300">Investigate your system with evidence from connected sources.</p><div className="mt-5 flex flex-wrap justify-center gap-2">{suggestions.map((suggestion) => <button key={suggestion} type="button" onClick={() => setDraft(suggestion)} className="rounded-control border border-ink-500/60 bg-surface px-3 py-2 text-left text-xs text-ink-200 hover:border-accent/60 hover:bg-accent-subtle">{suggestion}</button>)}</div></div>}
                    {selectedId && sessionQ.isLoading && <div className="flex justify-center py-20 text-ink-400"><LoaderCircle className="animate-spin" aria-label="Loading conversation" /></div>}
                    {sessionQ.isError && <RetryableError error={sessionQ.error} onRetry={() => sessionQ.refetch()} retrying={sessionQ.isFetching} context="Couldn't load this conversation" />}
                    {displayTurns.map((turn) => <TurnView key={turn.id} turn={turn} onEvidence={openEvidence} />)}
                    {selectedRun && (selectedRun.active || stream.blocks.length > 0 || stream.terminal) && <LiveBlocks blocks={stream.blocks} terminal={stream.terminal} />}
                    {active && stream.blocks.length === 0 && <div className="flex items-center gap-2 py-4 text-xs text-ink-400"><LoaderCircle size={14} className="animate-spin" />Thinking</div>}
                    {selectedRun?.error && <div role="alert" className="my-4 flex items-center justify-between gap-3 rounded-control border border-sev-critical/40 bg-sev-critical/10 p-3 text-xs text-sev-critical"><span>{selectedRun.error.message}</span><div className="flex gap-2"><button type="button" onClick={retryResync} className="btn">Retry</button><button type="button" onClick={() => setLiveRuns((runs) => {
                      const run = runs[selectedId as string];
                      return run ? { ...runs, [selectedId as string]: { ...run, error: null } } : runs;
                    })} className="btn">Dismiss</button></div></div>}
                    <div className="sr-only" aria-live="polite">{active ? (selectedRun?.stopping ? "Stopping chat run" : "Chat run in progress") : stream.terminal ? "Chat run complete" : ""}</div>
                  </div>
                </div>
                {!scroll.following && <button type="button" onClick={scroll.scrollToBottom} className="absolute bottom-44 left-1/2 z-sticky flex -translate-x-1/2 items-center gap-2 rounded-full border border-ink-500/60 bg-surface px-3 py-2 text-xs text-ink-200 shadow-overlay"><ArrowDown size={14} />Scroll to bottom</button>}
                <div className={clsx("absolute left-0 right-0 z-sticky mx-auto max-w-3xl px-4 transition-[bottom,transform] duration-300", conversationStarted ? "bottom-4" : "bottom-[20vh]")}><Composer value={draft} onChange={setDraft} onSubmit={send} running={active} stopping={selectedRun?.stopping ?? false} onStop={stop} attachment={attachment} onAttachmentChange={setAttachment} resetGeneration={composerResetGeneration} /></div>
              </>
            )}
          </main>

          <aside className={clsx("hidden min-w-0 overflow-hidden border-l border-ink-500/50 bg-surface lg:block", !evidence && "invisible")} aria-label="Evidence details"><EvidencePanel evidence={evidence} onClose={closeEvidence} /></aside>
        </div>
        {evidence && <div ref={evidenceDialogRef} className="fixed inset-0 z-overlay lg:hidden" role="dialog" aria-modal="true" aria-label="Evidence details" tabIndex={-1} onKeyDown={(event) => trapDialogFocus(event, closeEvidence)}><button aria-label="Close evidence" className="absolute inset-0 bg-black/70" onClick={closeEvidence} /><div className="absolute inset-y-0 right-0 w-[min(360px,92vw)] bg-surface shadow-overlay"><EvidencePanel evidence={evidence} onClose={closeEvidence} /></div></div>}
      </div>
    </div>
  );
}

function EvidencePanel({ evidence, onClose }: { evidence: EvidenceSelection | null; onClose: () => void }) {
  return (
    <div className="flex h-full min-w-[320px] flex-col">
      <div className="flex h-12 items-center justify-between border-b border-ink-500/40 px-4"><h2 className="text-sm font-semibold text-ink-50">Evidence</h2><button type="button" onClick={onClose} aria-label="Close evidence" title="Close evidence" className="rounded-control p-2 text-ink-300 hover:bg-ink-700"><X size={16} /></button></div>
      {evidence && <div className="min-h-0 flex-1 overflow-y-auto p-4"><dl className="grid grid-cols-[80px_1fr] gap-x-3 gap-y-2 text-xs"><dt className="text-ink-400">Source</dt><dd className="break-words text-ink-100">{evidence.citation.tool.replaceAll("_", " ")}</dd><dt className="text-ink-400">Label</dt><dd className="break-words text-ink-100">{evidence.citation.label || "—"}</dd><dt className="text-ink-400">Locator</dt><dd className="break-all font-mono text-ink-200">{evidence.citation.locator || "—"}</dd></dl>{evidence.output && <div className="mt-5"><h3 className="mb-2 text-xs font-semibold text-ink-100">Matching tool output</h3><pre className="max-h-[60vh] overflow-auto whitespace-pre-wrap break-words rounded-control bg-ink-900 p-3 text-[11px] leading-5 text-ink-200">{pretty(evidence.output)}</pre></div>}</div>}
    </div>
  );
}