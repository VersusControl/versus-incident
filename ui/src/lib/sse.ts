export interface ServerSentEvent {
  event: string;
  data: string;
}

export interface SSELimits {
  maxLineBytes: number;
  maxFrameBytes: number;
  maxTotalBytes: number;
  maxEvents: number;
  maxDurationMs: number;
}

export const CHAT_SSE_LIMITS: SSELimits = {
  maxLineBytes: 16 * 1024,
  maxFrameBytes: 32 * 1024,
  maxTotalBytes: 2 * 1024 * 1024,
  maxEvents: 256,
  maxDurationMs: 10 * 60 * 1000,
};

export const ANALYSIS_SSE_LIMITS: SSELimits = {
  maxLineBytes: 16 * 1024,
  maxFrameBytes: 32 * 1024,
  maxTotalBytes: 2 * 1024 * 1024,
  maxEvents: 256,
  maxDurationMs: 10 * 60 * 1000,
};

export class SSELimitError extends Error {
  constructor(public readonly code: "line" | "frame" | "total" | "events" | "duration") {
    super(`Live stream exceeded its ${code} safety limit. Refresh the conversation to resync.`);
    this.name = "SSELimitError";
  }
}

export class SSEParser {
  private buffer = "";
  private event = "message";
  private data: string[] = [];
  private frameBytes = 0;
  private eventCount = 0;
  private readonly encoder = new TextEncoder();

  constructor(
    private readonly onEvent: (event: ServerSentEvent) => void,
    private readonly limits: SSELimits = CHAT_SSE_LIMITS,
  ) {}

  feed(chunk: string) {
    this.buffer += chunk;
    let newline = this.buffer.indexOf("\n");
    while (newline >= 0) {
      let line = this.buffer.slice(0, newline);
      this.buffer = this.buffer.slice(newline + 1);
      if (line.endsWith("\r")) line = line.slice(0, -1);
      this.processLine(line);
      newline = this.buffer.indexOf("\n");
    }
    if (this.encoder.encode(this.buffer).byteLength > this.limits.maxLineBytes) {
      throw new SSELimitError("line");
    }
  }

  finish() {
    if (this.buffer) {
      const line = this.buffer.endsWith("\r")
        ? this.buffer.slice(0, -1)
        : this.buffer;
      this.processLine(line);
      this.buffer = "";
    }
    this.dispatch();
  }

  private processLine(line: string) {
    if (!line) {
      this.dispatch();
      return;
    }
    if (line.startsWith(":")) return;

    const lineBytes = this.encoder.encode(line).byteLength + 1;
    if (lineBytes > this.limits.maxLineBytes) throw new SSELimitError("line");
    this.frameBytes += lineBytes;
    if (this.frameBytes > this.limits.maxFrameBytes) throw new SSELimitError("frame");

    const separator = line.indexOf(":");
    const field = separator < 0 ? line : line.slice(0, separator);
    let value = separator < 0 ? "" : line.slice(separator + 1);
    if (value.startsWith(" ")) value = value.slice(1);

    if (field === "event") this.event = value || "message";
    if (field === "data") this.data.push(value);
  }

  private dispatch() {
    if (this.data.length > 0) {
      if (this.event !== "model_delta") {
        this.eventCount += 1;
        if (this.eventCount > this.limits.maxEvents) throw new SSELimitError("events");
      }
      this.onEvent({ event: this.event, data: this.data.join("\n") });
    }
    this.event = "message";
    this.data = [];
    this.frameBytes = 0;
  }
}

export async function readEventStream(
  stream: ReadableStream<Uint8Array>,
  onEvent: (event: ServerSentEvent) => void,
  limits: SSELimits = CHAT_SSE_LIMITS,
) {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  const parser = new SSEParser(onEvent, limits);
  const startedAt = Date.now();
  let totalBytes = 0;
  try {
    while (true) {
      const remaining = limits.maxDurationMs - (Date.now() - startedAt);
      if (remaining <= 0) throw new SSELimitError("duration");
      let timer: ReturnType<typeof setTimeout> | undefined;
      const timeout = new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new SSELimitError("duration")), remaining);
      });
      const { done, value } = await Promise.race([reader.read(), timeout]).finally(() => clearTimeout(timer));
      if (done) break;
      totalBytes += value.byteLength;
      if (totalBytes > limits.maxTotalBytes) throw new SSELimitError("total");
      parser.feed(decoder.decode(value, { stream: true }));
    }
    parser.feed(decoder.decode());
    parser.finish();
  } catch (error) {
    await reader.cancel().catch(() => undefined);
    throw error;
  } finally {
    reader.releaseLock();
  }
}