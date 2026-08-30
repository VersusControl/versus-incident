import type { ChatEvent } from "@/lib/api";
import { capChatMessage } from "@/lib/markdownPolicy";

const MAX_EVENTS = 256;
const MAX_BLOCKS = 256;
const MAX_FIELD_BYTES = 8192;
const MAX_TOTAL_BYTES = 64 * 1024;

export type ChatStreamBlock =
  | { kind: "text"; key: string; text: string; streaming: boolean }
  | { kind: "tool"; key: string; event: ChatEvent }
  | { kind: "compaction"; key: string; text: string };

export interface ChatStreamState {
  blocks: ChatStreamBlock[];
  terminal: ChatEvent | null;
  eventCount: number;
  totalBytes: number;
}

export type ChatStreamAction = ChatEvent | { type: "reset" };

export const emptyChatStream: ChatStreamState = { blocks: [], terminal: null, eventCount: 0, totalBytes: 0 };

const terminalKinds = new Set([
  "run_finished",
  "run_failed",
  "run_cancelled",
  "run_throttled",
]);

export function reduceChatEvent(
  state: ChatStreamState,
  event: ChatStreamAction,
): ChatStreamState {
  if ("type" in event) return emptyChatStream;
  const bounded: ChatEvent = {
    ...event,
    delta: event.delta == null ? undefined : capChatMessage(event.delta, MAX_FIELD_BYTES),
    args: event.args == null ? undefined : capChatMessage(event.args, MAX_FIELD_BYTES),
    output: event.output == null ? undefined : capChatMessage(event.output, MAX_FIELD_BYTES),
    error: event.error == null ? undefined : capChatMessage(event.error, MAX_FIELD_BYTES),
  };
  if (terminalKinds.has(bounded.kind)) {
    return state.terminal ? state : { ...state, terminal: bounded };
  }

  const semanticEvent = bounded.kind !== "model_delta";
  if ((semanticEvent && state.eventCount >= MAX_EVENTS) || state.totalBytes >= MAX_TOTAL_BYTES) return state;
  const eventBytes = new TextEncoder().encode(
    bounded.kind === "model_delta" ? bounded.delta ?? "" : JSON.stringify(bounded),
  ).byteLength;
  if (state.totalBytes + eventBytes > MAX_TOTAL_BYTES) return state;

  const blocks = state.blocks.slice();
  if (bounded.kind === "model_delta" && bounded.delta != null) {
    const last = blocks.at(-1);
    if (last?.kind === "text" && last.streaming) {
      blocks[blocks.length - 1] = { ...last, text: capChatMessage(last.text + bounded.delta, MAX_FIELD_BYTES) };
    } else {
      blocks.push({
        kind: "text",
        key: `text-${bounded.seq}`,
        text: bounded.delta,
        streaming: true,
      });
    }
  } else if (bounded.kind === "tool_started") {
    const last = blocks.at(-1);
    if (last?.kind === "text" && last.streaming) {
      blocks[blocks.length - 1] = { ...last, streaming: false };
    }
    blocks.push({ kind: "tool", key: `tool-${bounded.call_id || bounded.seq}`, event: bounded });
  } else if (bounded.kind === "tool_finished") {
    let index = -1;
    for (let candidate = blocks.length - 1; candidate >= 0; candidate -= 1) {
      const block = blocks[candidate];
      if (
        block.kind === "tool" &&
        block.event.kind === "tool_started" &&
        (bounded.call_id
          ? block.event.call_id === bounded.call_id
          : block.event.tool === bounded.tool)
      ) {
        index = candidate;
        break;
      }
    }
    if (index >= 0) blocks[index] = { ...blocks[index], event: bounded } as ChatStreamBlock;
    else blocks.push({ kind: "tool", key: `tool-${bounded.call_id || bounded.seq}`, event: bounded });
  } else if (bounded.kind === "compacted" || bounded.kind === "events_elided" || bounded.kind === "trace_compacted") {
    blocks.push({
      kind: "compaction",
      key: `compaction-${bounded.seq}`,
      text: bounded.output ?? (bounded.kind === "events_elided" ? "Some live trace events were omitted" : "Older context was compacted"),
    });
  }
  return {
    ...state,
    blocks: blocks.slice(-MAX_BLOCKS),
    eventCount: state.eventCount + (semanticEvent ? 1 : 0),
    totalBytes: state.totalBytes + eventBytes,
  };
}