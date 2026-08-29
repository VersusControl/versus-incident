import { describe, expect, it } from "vitest";
import { readEventStream, SSELimitError, SSEParser, type ServerSentEvent, type SSELimits } from "./sse";

function parse(chunks: string[]) {
  const events: ServerSentEvent[] = [];
  const parser = new SSEParser((event) => events.push(event));
  chunks.forEach((chunk) => parser.feed(chunk));
  parser.finish();
  return events;
}

describe("SSEParser", () => {
  it("joins multiline data and honors event names", () => {
    expect(parse(["event: model_delta\ndata: first\ndata: second\n\n"])).toEqual([
      { event: "model_delta", data: "first\nsecond" },
    ]);
  });

  it("handles CRLF and arbitrary chunk boundaries", () => {
    expect(
      parse(["event: tool_", "finished\r\nda", "ta: {\"ok\":true}\r\n\r", "\n"]),
    ).toEqual([{ event: "tool_finished", data: '{"ok":true}' }]);
  });

  it("flushes a final frame without a trailing newline", () => {
    expect(parse(["event: done\ndata: final"])).toEqual([
      { event: "done", data: "final" },
    ]);
  });

  it("does not count model deltas against the semantic event limit", async () => {
    const frames = Array.from({ length: 220 }, (_, index) =>
      `event: model_delta\ndata: token-${index}\n\n`
    ).join("") + "event: run_finished\ndata: {}\n\n";
    const received: ServerSentEvent[] = [];
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode(frames));
        controller.close();
      },
    });
    const limits: SSELimits = { maxLineBytes: 1024, maxFrameBytes: 2048, maxTotalBytes: 64 * 1024, maxEvents: 1, maxDurationMs: 1000 };

    await readEventStream(stream, (next) => received.push(next), limits);

    expect(received).toHaveLength(221);
    expect(received.at(-1)?.event).toBe("run_finished");
  });

  it("rejects a malicious stream that never terminates a line", () => {
    const parser = new SSEParser(() => {}, { maxLineBytes: 8, maxFrameBytes: 32, maxTotalBytes: 64, maxEvents: 4, maxDurationMs: 1000 });
    expect(() => parser.feed("data: 123456789")).toThrow(SSELimitError);
  });

  it("cancels the reader when total bytes exceed the limit", async () => {
    let cancelled = false;
    const stream = new ReadableStream<Uint8Array>({
      pull(controller) { controller.enqueue(new TextEncoder().encode("data: 123456\n\n")); },
      cancel() { cancelled = true; },
    });
    const limits: SSELimits = { maxLineBytes: 32, maxFrameBytes: 32, maxTotalBytes: 8, maxEvents: 4, maxDurationMs: 1000 };
    await expect(readEventStream(stream, () => {}, limits)).rejects.toMatchObject({ code: "total" });
    expect(cancelled).toBe(true);
  });
});