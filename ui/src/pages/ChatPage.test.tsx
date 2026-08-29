// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError, type ChatSession } from "@/lib/api";
import { ChatPage } from "./ChatPage";

vi.mock("@/components/TopBar", () => ({
  TopBar: ({ title, actions }: { title: string; actions?: React.ReactNode }) => (
    <header><span>{title}</span>{actions}</header>
  ),
}));

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      listChatSessions: vi.fn(),
      getChatSession: vi.fn(),
      createChatSession: vi.fn(),
      deleteChatSession: vi.fn(),
      cancelChatRun: vi.fn(),
      streamChatMessage: vi.fn(),
    },
  };
});

const baseSession: ChatSession = {
  id: "session-1",
  status: "idle",
  seeded: true,
  created_at: "2026-08-28T10:00:00Z",
  updated_at: "2026-08-28T10:05:00Z",
  turns: [],
};

function renderPage(url = "/agent/chat") {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  const rendered = render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[url]} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
        <ChatPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { ...rendered, client };
}

beforeEach(() => {
  Object.defineProperty(window, "matchMedia", {
    value: vi.fn(() => ({ matches: false, addEventListener: vi.fn(), removeEventListener: vi.fn() })),
    configurable: true,
  });
  vi.mocked(api.listChatSessions).mockResolvedValue([]);
  vi.mocked(api.getChatSession).mockResolvedValue(baseSession);
  vi.mocked(api.createChatSession).mockResolvedValue(baseSession);
  vi.mocked(api.deleteChatSession).mockResolvedValue(undefined);
  vi.mocked(api.cancelChatRun).mockResolvedValue(undefined);
  vi.mocked(api.streamChatMessage).mockResolvedValue({
    seq: 2,
    at: "2026-08-28T10:06:00Z",
    kind: "run_finished",
  });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("ChatPage", () => {
  it("loads a deep-linked session and derives its history title", async () => {
    const full = {
      ...baseSession,
      turns: [{ id: "u1", role: "user" as const, content: "Investigate checkout latency", created_at: baseSession.created_at }],
    };
    vi.mocked(api.listChatSessions).mockResolvedValue([baseSession]);
    vi.mocked(api.getChatSession).mockResolvedValue(full);
    renderPage("/agent/chat?session=session-1");
    expect(await screen.findAllByText("Investigate checkout latency")).toHaveLength(2);
    expect(api.getChatSession).toHaveBeenCalledWith("session-1");
  });

  it("creates, streams, refetches, and renders the final assistant once", async () => {
    const finalSession: ChatSession = {
      ...baseSession,
      turns: [
        { id: "u1", role: "user", content: "What changed?", created_at: baseSession.created_at },
        { id: "a1", role: "assistant", content: "Deployment v42 changed checkout.", created_at: baseSession.updated_at },
      ],
    };
    vi.mocked(api.getChatSession).mockResolvedValue(finalSession);
    vi.mocked(api.streamChatMessage).mockImplementation(async (_id, _message, _attachment, onEvent) => {
      onEvent({ seq: 1, at: "", kind: "model_delta", delta: "Deployment v42 changed checkout." });
      return { seq: 2, at: "", kind: "run_finished" };
    });
    renderPage();
    fireEvent.change(screen.getByRole("textbox", { name: "Message" }), { target: { value: "What changed?" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() => expect(api.createChatSession).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(screen.getAllByText("Deployment v42 changed checkout.")).toHaveLength(1));
    expect(api.streamChatMessage).toHaveBeenCalledWith("session-1", "What changed?", undefined, expect.any(Function), expect.any(AbortSignal));
  });

  it("sends context attachments without file upload", async () => {
    renderPage("/agent/chat?session=session-1");
    await screen.findByLabelText("Message");
    fireEvent.click(screen.getByRole("button", { name: "Attach context" }));
    fireEvent.change(screen.getByLabelText("Service context"), { target: { value: "checkout" } });
    fireEvent.change(screen.getByLabelText("Incident ID context"), { target: { value: "inc-42" } });
    fireEvent.change(screen.getByLabelText("Message"), { target: { value: "Explain this" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(api.streamChatMessage).toHaveBeenCalled());
    expect(vi.mocked(api.streamChatMessage).mock.calls[0][2]).toEqual({
      service: "checkout",
      incident: { id: "inc-42" },
    });
    expect(screen.queryByLabelText(/upload/i)).toBeNull();
  });

  it("requests cancellation and keeps reading through run_cancelled", async () => {
    const order: string[] = [];
    let finish: (() => void) | undefined;
    vi.mocked(api.cancelChatRun).mockImplementation(async () => { order.push("cancel"); finish?.(); });
    vi.mocked(api.streamChatMessage).mockImplementation(async (_id, _message, _attachment, onEvent, signal) => {
      await new Promise<void>((resolve) => { finish = resolve; });
      expect(signal?.aborted).toBe(false);
      const terminal = { seq: 2, at: "", kind: "run_cancelled" as const };
      onEvent(terminal);
      order.push("terminal");
      return terminal;
    });
    renderPage("/agent/chat?session=session-1");
    fireEvent.change(await screen.findByLabelText("Message"), { target: { value: "Keep looking" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    fireEvent.click(await screen.findByRole("button", { name: "Stop" }));
    await waitFor(() => expect(order).toEqual(["cancel", "terminal"]));
  });

  it("prevents duplicate sends while a new session is being created", async () => {
    let finishCreate: ((session: ChatSession) => void) | undefined;
    vi.mocked(api.createChatSession).mockImplementation(() => new Promise((resolve) => { finishCreate = resolve; }));
    renderPage();
    const message = screen.getByLabelText("Message");
    fireEvent.change(message, { target: { value: "Only once" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    fireEvent.submit(message.closest("form") as HTMLFormElement);
    expect(api.createChatSession).toHaveBeenCalledTimes(1);
    finishCreate?.(baseSession);
    await waitFor(() => expect(api.streamChatMessage).toHaveBeenCalledTimes(1));
  });

  it("retains a fully streamed answer when detail refetch fails", async () => {
    vi.mocked(api.getChatSession)
      .mockResolvedValueOnce(baseSession)
      .mockRejectedValueOnce(new Error("refetch failed"))
      .mockRejectedValueOnce(new Error("refetch failed"))
      .mockResolvedValue({
        ...baseSession,
        turns: [
          { id: "u1", role: "user", content: "Question", created_at: baseSession.created_at },
          { id: "a1", role: "assistant", content: "Retained answer", created_at: baseSession.updated_at },
        ],
      });
    vi.mocked(api.streamChatMessage).mockImplementation(async (_id, _message, _attachment, onEvent) => {
      onEvent({ seq: 1, at: "", kind: "model_delta", delta: "Retained answer" });
      return { seq: 2, at: "", kind: "run_finished" };
    });
    renderPage("/agent/chat?session=session-1");
    fireEvent.change(await screen.findByLabelText("Message"), { target: { value: "Question" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    expect(await screen.findByText("Retained answer")).toBeTruthy();
    expect((await screen.findAllByText("refetch failed")).length).toBeGreaterThan(0);
    fireEvent.click(screen.getAllByRole("button", { name: "Retry" }).at(-1) as HTMLButtonElement);
    await waitFor(() => expect(screen.getAllByText("Retained answer")).toHaveLength(1));
    expect(api.streamChatMessage).toHaveBeenCalledTimes(1);
  });

  it("blocks one-sided and reversed time ranges with actionable errors", async () => {
    renderPage("/agent/chat?session=session-1");
    fireEvent.click(await screen.findByRole("button", { name: "Attach context" }));
    fireEvent.change(screen.getByLabelText("Context start"), { target: { value: "2026-08-28T10:00" } });
    expect(screen.getByRole("alert").textContent).toContain("both a start and an end");
    fireEvent.change(screen.getByLabelText("Context end"), { target: { value: "2026-08-28T09:00" } });
    expect(screen.getByRole("alert").textContent).toContain("after start");
    fireEvent.change(screen.getByLabelText("Message"), { target: { value: "Do not send" } });
    expect((screen.getByRole("button", { name: "Send" }) as HTMLButtonElement).disabled).toBe(true);
    expect(api.streamChatMessage).not.toHaveBeenCalled();
  });

  it("clears displayed time context before a second send", async () => {
    renderPage("/agent/chat?session=session-1");
    fireEvent.click(await screen.findByRole("button", { name: "Attach context" }));
    fireEvent.change(screen.getByLabelText("Context start"), { target: { value: "2026-08-28T10:00" } });
    fireEvent.change(screen.getByLabelText("Context end"), { target: { value: "2026-08-28T11:00" } });
    fireEvent.change(screen.getByLabelText("Message"), { target: { value: "First" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(api.streamChatMessage).toHaveBeenCalledTimes(1));
    expect((screen.getByLabelText("Context start") as HTMLInputElement).value).toBe("");
    expect((screen.getByLabelText("Context end") as HTMLInputElement).value).toBe("");

    fireEvent.change(screen.getByLabelText("Message"), { target: { value: "Second" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(api.streamChatMessage).toHaveBeenCalledTimes(2));
    expect(vi.mocked(api.streamChatMessage).mock.calls[0][2]).toMatchObject({ time_range: expect.any(Object) });
    expect(vi.mocked(api.streamChatMessage).mock.calls[1][2]).toBeUndefined();
  });

  it("hides the optimistic turn after a mid-run refetch persists it", async () => {
    let finish: (() => void) | undefined;
    vi.mocked(api.streamChatMessage).mockImplementation(async () => {
      await new Promise<void>((resolve) => { finish = resolve; });
      return { seq: 2, at: "", kind: "run_finished" };
    });
    const { client } = renderPage("/agent/chat?session=session-1");
    fireEvent.change(await screen.findByLabelText("Message"), { target: { value: "Persist me once" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(screen.getAllByText("Persist me once")).toHaveLength(1));
    client.setQueryData(["chat-session", "session-1"], {
      ...baseSession,
      status: "running",
      turns: [{ id: "server-user", role: "user", content: "Persist me once", created_at: new Date().toISOString() }],
    });
    await waitFor(() => expect(screen.getAllByText("Persist me once")).toHaveLength(1));
    finish?.();
  });

  it("does not show a stream failure in another thread", async () => {
    const sessionA = { ...baseSession, id: "session-a" };
    const sessionB = { ...baseSession, id: "session-b", turns: [{ id: "b-user", role: "user" as const, content: "Thread B", created_at: baseSession.created_at }] };
    vi.mocked(api.listChatSessions).mockResolvedValue([sessionA, sessionB]);
    vi.mocked(api.getChatSession).mockImplementation(async (id) => {
      if (id === "session-a" && vi.mocked(api.streamChatMessage).mock.calls.length > 0) throw new Error("resync failed");
      return id === "session-a" ? sessionA : sessionB;
    });
    vi.mocked(api.streamChatMessage).mockRejectedValue(new Error("stream failed in A"));
    renderPage("/agent/chat?session=session-a");
    fireEvent.change(await screen.findByLabelText("Message"), { target: { value: "Question A" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    expect(await screen.findByText("stream failed in A")).toBeTruthy();
    fireEvent.click(screen.getAllByRole("button", { name: /Chat ·/ }).at(-1) as HTMLButtonElement);
    expect((await screen.findAllByText("Thread B")).length).toBeGreaterThan(0);
    expect(screen.queryByText("stream failed in A")).toBeNull();
  });

  it("keeps the failed optimistic turn and error after a successful idle resync", async () => {
    vi.mocked(api.streamChatMessage).mockRejectedValue(new ApiError(409, "stream conflict"));
    renderPage("/agent/chat?session=session-1");
    fireEvent.change(await screen.findByLabelText("Message"), { target: { value: "Keep this question" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() => expect(api.getChatSession).toHaveBeenCalledTimes(2));
    expect(screen.getByText("Keep this question")).toBeTruthy();
    expect(screen.getByRole("alert").textContent).toContain("stream conflict");
  });

  it("dedupes a persisted failed-send turn while retaining its error", async () => {
    const persisted = {
      ...baseSession,
      turns: [{ id: "persisted-user", role: "user" as const, content: "Persisted question", created_at: new Date().toISOString() }],
    };
    vi.mocked(api.getChatSession).mockResolvedValueOnce(baseSession).mockResolvedValue(persisted);
    vi.mocked(api.streamChatMessage).mockRejectedValue(new ApiError(409, "stream conflict"));
    renderPage("/agent/chat?session=session-1");
    fireEvent.change(await screen.findByLabelText("Message"), { target: { value: "Persisted question" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() => expect(api.getChatSession).toHaveBeenCalledTimes(2));
    expect(screen.getAllByText("Persisted question")).toHaveLength(1);
    expect(screen.getByRole("alert").textContent).toContain("stream conflict");
  });

  it("caps composer input by UTF-8 bytes", async () => {
    renderPage("/agent/chat?session=session-1");
    const input = await screen.findByLabelText("Message");
    fireEvent.change(input, { target: { value: "😀".repeat(3000) } });
    const value = (input as HTMLTextAreaElement).value;
    expect(new TextEncoder().encode(value).byteLength).toBe(8192);
    expect(screen.getByText("0 bytes left")).toBeTruthy();
  });

  it("renders live output only in its session and restores it when switching back", async () => {
    const sessionA = { ...baseSession, id: "session-a", turns: [{ id: "a-user", role: "user" as const, content: "Thread A", created_at: baseSession.created_at }] };
    const sessionB = { ...baseSession, id: "session-b", turns: [{ id: "b-user", role: "user" as const, content: "Thread B", created_at: baseSession.created_at }] };
    vi.mocked(api.listChatSessions).mockResolvedValue([sessionA, sessionB]);
    vi.mocked(api.getChatSession).mockImplementation(async (id) => id === "session-a" ? sessionA : sessionB);
    let emit: ((event: Parameters<Parameters<typeof api.streamChatMessage>[3]>[0]) => void) | undefined;
    let finish: (() => void) | undefined;
    vi.mocked(api.streamChatMessage).mockImplementation(async (_id, _message, _attachment, onEvent) => {
      emit = onEvent;
      await new Promise<void>((resolve) => { finish = resolve; });
      return { seq: 3, at: "", kind: "run_finished" };
    });
    renderPage("/agent/chat?session=session-a");
    fireEvent.change(await screen.findByLabelText("Message"), { target: { value: "Question A" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(emit).toBeTruthy());
    fireEvent.click(screen.getAllByRole("button", { name: /Chat ·/ })[0]);
    emit?.({ seq: 2, at: "", kind: "model_delta", delta: "Only for A" });
    expect((await screen.findAllByText("Thread B")).length).toBeGreaterThan(0);
    expect(screen.queryByText("Only for A")).toBeNull();
    fireEvent.click(screen.getAllByRole("button", { name: /Chat ·/ })[0]);
    expect(await screen.findByText("Only for A")).toBeTruthy();
    finish?.();
  });

  it("contains mobile drawer focus, makes background inert, and restores the trigger", async () => {
    const listeners = new Set<() => void>();
    const media = { matches: true, addEventListener: (_type: string, listener: () => void) => listeners.add(listener), removeEventListener: (_type: string, listener: () => void) => listeners.delete(listener) };
    Object.defineProperty(window, "matchMedia", { value: vi.fn(() => media), configurable: true });
    renderPage();
    const trigger = screen.getByRole("button", { name: "Open chat history" });
    const background = screen.getByRole("log", { name: "Conversation" }).parentElement?.parentElement as HTMLElement;
    trigger.focus();
    fireEvent.click(trigger);
    const dialog = await screen.findByRole("dialog", { name: "Thread history" });
    expect(document.activeElement).toBe(dialog);
    expect(background.inert).toBe(true);
    expect(background.getAttribute("aria-hidden")).toBe("true");
    fireEvent.keyDown(dialog, { key: "Tab" });
    expect(dialog.contains(document.activeElement)).toBe(true);
    fireEvent.keyDown(dialog, { key: "Escape" });
    await waitFor(() => expect(document.activeElement).toBe(trigger));
  });

  it("opens citation evidence with matching tool output", async () => {
    vi.mocked(api.listChatSessions).mockResolvedValue([baseSession]);
    vi.mocked(api.getChatSession).mockResolvedValue({
      ...baseSession,
      turns: [{
        id: "a1",
        role: "assistant",
        content: "Latency is elevated [1].",
        created_at: baseSession.updated_at,
        citations: [{ tool: "query_metrics", label: "Latency series", locator: "checkout/p99" }],
        tool_calls: [{ Name: "query_metrics", Args: "{}", Output: '{"p99":820}', DurationMs: 12, Error: "" }],
      }],
    });
    renderPage("/agent/chat?session=session-1");
    fireEvent.click(await screen.findByRole("button", { name: /Latency series/ }));
    const panel = screen.getByRole("complementary", { name: "Evidence details" });
    expect(within(panel).getByText("checkout/p99")).toBeTruthy();
    expect(within(panel).getByText(/"p99": 820/)).toBeTruthy();
  });

  it("matches reloaded repeated-tool citations by persisted call ID", async () => {
    vi.mocked(api.listChatSessions).mockResolvedValue([baseSession]);
    vi.mocked(api.getChatSession).mockResolvedValue({
      ...baseSession,
      turns: [{
        id: "a1",
        role: "assistant",
        content: "The second query found the spike [1].",
        created_at: baseSession.updated_at,
        citations: [{ tool: "query_metrics", label: "Second query", locator: "tool-call-second" }],
        tool_calls: [
          { call_id: "first", Name: "query_metrics", Args: "{}", Output: '{"value":"first"}', DurationMs: 20, Error: "" },
          { call_id: "second", Name: "query_metrics", Args: "{}", Output: '{"value":"second"}', DurationMs: 10, Error: "" },
        ],
      }],
    });
    renderPage("/agent/chat?session=session-1");
    fireEvent.click(await screen.findByRole("button", { name: /Second query/ }));
    const panel = screen.getByRole("complementary", { name: "Evidence details" });
    expect(within(panel).getByText(/"value": "second"/)).toBeTruthy();
    expect(within(panel).queryByText(/"value": "first"/)).toBeNull();
  });

  it("matches ID-less repeated-tool citations by one-based ordinal", async () => {
    vi.mocked(api.listChatSessions).mockResolvedValue([baseSession]);
    vi.mocked(api.getChatSession).mockResolvedValue({
      ...baseSession,
      turns: [{
        id: "a1",
        role: "assistant",
        content: "The second query found the spike [1].",
        created_at: baseSession.updated_at,
        citations: [{ tool: "query_metrics", label: "Second ordinal query", locator: "tool-call-2" }],
        tool_calls: [
          { Name: "query_metrics", Args: "{}", Output: '{"value":"first"}', DurationMs: 20, Error: "" },
          { Name: "query_metrics", Args: "{}", Output: '{"value":"second"}', DurationMs: 10, Error: "" },
        ],
      }],
    });
    renderPage("/agent/chat?session=session-1");
    fireEvent.click(await screen.findByRole("button", { name: /Second ordinal query/ }));
    const panel = screen.getByRole("complementary", { name: "Evidence details" });
    expect(within(panel).getByText(/"value": "second"/)).toBeTruthy();
    expect(within(panel).queryByText(/"value": "first"/)).toBeNull();
  });

  it("does not render absent or zero persisted tool durations", async () => {
    vi.mocked(api.listChatSessions).mockResolvedValue([baseSession]);
    vi.mocked(api.getChatSession).mockResolvedValue({
      ...baseSession,
      turns: [{
        id: "a1",
        role: "assistant",
        content: "Tool results",
        created_at: baseSession.updated_at,
        tool_calls: [
          { Name: "query_metrics", Args: "{}", Output: "{}", DurationMs: 0, Error: "" },
          { Name: "query_logs", Args: "{}", Output: "{}", DurationMs: undefined as unknown as number, Error: "" },
        ],
      }],
    });
    renderPage("/agent/chat?session=session-1");
    await screen.findByRole("button", { name: /finished query metrics/ });
    expect(screen.queryByText("0ms")).toBeNull();
  });

  it("deletes a thread and returns the selected view to empty state", async () => {
    vi.mocked(api.listChatSessions).mockResolvedValue([baseSession]);
    renderPage("/agent/chat?session=session-1");
    fireEvent.click(await screen.findByRole("button", { name: "Delete thread" }));
    await waitFor(() => expect(api.deleteChatSession).toHaveBeenCalledWith("session-1"));
    expect(await screen.findByText("Versus DevOps Chat")).toBeTruthy();
  });

  it("shows a delete failure without a live run and clears it after retry", async () => {
    vi.mocked(api.listChatSessions).mockResolvedValue([baseSession]);
    vi.mocked(api.deleteChatSession)
      .mockRejectedValueOnce(new ApiError(500, "delete failed"))
      .mockResolvedValueOnce(undefined);
    renderPage("/agent/chat?session=session-1");
    fireEvent.click(await screen.findByRole("button", { name: "Delete thread" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("delete failed");
    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(api.deleteChatSession).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.queryByText("delete failed")).toBeNull());
  });

  it("shows an actionable disabled state for 503", async () => {
    vi.mocked(api.listChatSessions).mockRejectedValue(new ApiError(503, "chat not enabled"));
    renderPage();
    expect(await screen.findByRole("heading", { name: "Chat is not enabled" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });
});