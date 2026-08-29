import { describe, expect, it } from "vitest";
import type { ChatEvent } from "./api";
import { emptyChatStream, reduceChatEvent } from "./chatStream";

function event(seq: number, kind: ChatEvent["kind"], extra = {}): ChatEvent {
  return { seq, kind, at: "2026-08-28T00:00:00Z", ...extra };
}

describe("reduceChatEvent", () => {
  it("preserves whitespace deltas and applies events incrementally", () => {
    const first = reduceChatEvent(emptyChatStream, event(1, "model_delta", { delta: "Hello" }));
    const second = reduceChatEvent(first, event(2, "model_delta", { delta: " \nworld" }));
    expect(second.blocks[0]).toMatchObject({ text: "Hello \nworld" });
    expect(first.blocks[0]).toMatchObject({ text: "Hello" });
  });

  it("replaces a running tool by call ID without moving it", () => {
    const started = reduceChatEvent(
      emptyChatStream,
      event(2, "tool_started", { tool: "query_metrics", call_id: "call-1" }),
    );
    const finished = reduceChatEvent(
      started,
      event(3, "tool_finished", { tool: "query_metrics", call_id: "call-1", output: "ok" }),
    );
    expect(finished.blocks).toHaveLength(1);
    expect(finished.blocks[0]).toMatchObject({
      kind: "tool",
      key: "tool-call-1",
      event: { kind: "tool_finished", output: "ok" },
    });
  });

  it("correlates repeated tools with out-of-order finishes", () => {
    const first = reduceChatEvent(emptyChatStream, event(1, "tool_started", { tool: "query_metrics", call_id: "first" }));
    const second = reduceChatEvent(first, event(2, "tool_started", { tool: "query_metrics", call_id: "second" }));
    const secondDone = reduceChatEvent(second, event(3, "tool_finished", { tool: "query_metrics", call_id: "second", output: "second output" }));
    const bothDone = reduceChatEvent(secondDone, event(4, "tool_finished", { tool: "query_metrics", call_id: "first", output: "first output" }));
    expect(bothDone.blocks.map((block) => block.kind === "tool" ? block.event.output : "")).toEqual(["first output", "second output"]);
  });

  it("finishes the newest matching tool when the finish omits its call ID", () => {
    const first = reduceChatEvent(emptyChatStream, event(1, "tool_started", { tool: "query_metrics", call_id: "first" }));
    const second = reduceChatEvent(first, event(2, "tool_started", { tool: "query_metrics", call_id: "second" }));
    const finished = reduceChatEvent(second, event(3, "tool_finished", { tool: "query_metrics", output: "latest output" }));

    expect(finished.blocks).toHaveLength(2);
    expect(finished.blocks[0]).toMatchObject({ event: { kind: "tool_started", call_id: "first" } });
    expect(finished.blocks[1]).toMatchObject({ event: { kind: "tool_finished", output: "latest output" } });
  });

  it("keeps a full answer across more than 200 model deltas and accepts success", () => {
    let state = emptyChatStream;
    const answer = "x".repeat(8192);
    const chunks = answer.match(/.{1,32}/g) ?? [];
    for (const [index, delta] of chunks.entries()) {
      state = reduceChatEvent(state, event(index + 1, "model_delta", { delta }));
    }
    state = reduceChatEvent(state, event(chunks.length + 1, "run_finished"));

    expect(chunks.length).toBeGreaterThan(200);
    expect(state.blocks).toMatchObject([{ kind: "text", text: answer }]);
    expect(state.eventCount).toBe(0);
    expect(state.terminal?.kind).toBe("run_finished");
  });

  it("renders compaction and records the first terminal only", () => {
    const compacted = reduceChatEvent(
      emptyChatStream,
      event(4, "compacted", { output: "3 older turns omitted" }),
    );
    const done = reduceChatEvent(compacted, event(5, "run_finished"));
    const duplicate = reduceChatEvent(done, event(6, "run_failed", { error: "late" }));
    expect(compacted.blocks[0]).toMatchObject({ kind: "compaction" });
    expect(duplicate.terminal?.kind).toBe("run_finished");
  });

  it("bounds terminal error payloads", () => {
    const failed = reduceChatEvent(emptyChatStream, event(9, "run_failed", { error: "x".repeat(20_000) }));
    expect(new TextEncoder().encode(failed.terminal?.error ?? "").byteLength).toBe(8192);
  });

  it("renders persisted trace elision markers", () => {
    const elided = reduceChatEvent(emptyChatStream, event(7, "events_elided", { output: "42 events omitted" }));
    const compacted = reduceChatEvent(elided, event(8, "trace_compacted", { output: "older traces compacted" }));
    expect(compacted.blocks).toMatchObject([
      { kind: "compaction", text: "42 events omitted" },
      { kind: "compaction", text: "older traces compacted" },
    ]);
  });

  it("resets all live state before a new run", () => {
    const active = reduceChatEvent(
      emptyChatStream,
      event(1, "model_delta", { delta: "old" }),
    );
    expect(reduceChatEvent(active, { type: "reset" })).toEqual(emptyChatStream);
  });
});