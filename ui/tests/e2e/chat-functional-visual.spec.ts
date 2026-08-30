import { expect, test, type Page, type Route } from "@playwright/test";

type Theme = "dark" | "light";

interface ChatTurn {
  id: string;
  role: "user" | "assistant" | "compaction";
  content: string;
  created_at: string;
  attachment?: Record<string, unknown>;
  tool_calls?: Array<{ Name: string; Args: string; Output: string; DurationMs: number; Error: string }>;
  citations?: Array<{ tool: string; label?: string; locator?: string }>;
}

interface Session {
  id: string;
  status: "idle" | "running" | "failed";
  seeded: boolean;
  created_at: string;
  updated_at: string;
  turns: ChatTurn[];
}

interface MockOptions {
  sessions?: Session[];
  listStatus?: number;
  getStatus?: number;
  createStatus?: number;
  deleteStatus?: number;
  deleteStatuses?: number[];
  streamStatus?: number;
  cancelStatus?: number;
  delayList?: boolean;
  delayStreamUntilCancel?: boolean;
}

const now = "2026-08-28T12:00:00Z";
const assistant = "Checkout latency rose after deployment v42.";

function populatedSession(): Session {
  return {
    id: "session-1",
    status: "idle",
    seeded: true,
    created_at: now,
    updated_at: now,
    turns: [
      { id: "u1", role: "user", content: "Investigate checkout latency", created_at: now },
      { id: "a1", role: "assistant", content: assistant, created_at: now },
    ],
  };
}

function toolSession(): Session {
  const session = populatedSession();
  session.turns[1] = {
    id: "a1",
    role: "assistant",
    content: "Latency is elevated [1].\n\n```json\n{\"p99\":820}\n```",
    created_at: now,
    tool_calls: [{
      Name: "query_metrics",
      Args: "{\"service\":\"checkout\"}",
      Output: "{\"p99\":820,\"unit\":\"ms\"}",
      DurationMs: 12,
      Error: "",
    }],
    citations: [{ tool: "query_metrics", label: "Latency series", locator: "checkout/p99" }],
  };
  return session;
}

async function json(route: Route, value: unknown, status = 200) {
  await route.fulfill({ status, contentType: "application/json", body: JSON.stringify(value) });
}

async function installApi(page: Page, options: MockOptions = {}) {
  const state = {
    sessions: structuredClone(options.sessions ?? []),
    calls: [] as string[],
    messageBodies: [] as Array<Record<string, unknown>>,
    deleteAttempts: 0,
    gatewayExchanges: 0,
    authenticatedConfigProbes: 0,
  };

  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const method = request.method();

    if (path === "/api/auth/gateway-session" && method === "POST") {
      if (request.headers()["x-gateway-secret"] !== "qa-secret") {
        return json(route, { error: "unauthorized" }, 401);
      }
      state.gatewayExchanges += 1;
      return route.fulfill({
        status: 204,
        headers: {
          "Set-Cookie": "versus_gateway_session=mock-signed-session; Path=/; HttpOnly; SameSite=Strict",
        },
        body: "",
      });
    }
    if (path === "/api/admin/config/agent") {
      if (!request.headers().cookie?.includes("versus_gateway_session=mock-signed-session")) {
        return json(route, { error: "unauthorized" }, 401);
      }
      state.authenticatedConfigProbes += 1;
      return json(route, {});
    }
    if (path === "/api/enterprise/deployment" || path.endsWith("/deployment")) return json(route, { error: "community" }, 403);

    const sessionMatch = path.match(/^\/api\/admin\/chat\/sessions\/([^/]+)$/);
    const messageMatch = path.match(/^\/api\/admin\/chat\/sessions\/([^/]+)\/messages$/);
    const cancelMatch = path.match(/^\/api\/admin\/chat\/sessions\/([^/]+)\/cancel$/);

    if (path === "/api/admin/chat/sessions" && method === "GET") {
      state.calls.push("list");
      if (options.delayList) await new Promise((resolve) => setTimeout(resolve, 3_000));
      if (options.listStatus) return json(route, { error: `list error ${options.listStatus}` }, options.listStatus);
      return json(route, {
        sessions: state.sessions.map((session) => ({
          id: session.id,
          status: session.status,
          seeded: session.seeded,
          created_at: session.created_at,
          updated_at: session.updated_at,
        })),
      });
    }
    if (path === "/api/admin/chat/sessions" && method === "POST") {
      state.calls.push("create");
      if (options.createStatus) return json(route, { error: `create error ${options.createStatus}` }, options.createStatus);
      const created: Session = {
        id: "session-created",
        status: "idle",
        seeded: false,
        created_at: now,
        updated_at: now,
        turns: [],
      };
      state.sessions.unshift(created);
      return json(route, created, 201);
    }
    if (sessionMatch && method === "GET") {
      state.calls.push("get");
      if (options.getStatus) return json(route, { error: `get error ${options.getStatus}` }, options.getStatus);
      const found = state.sessions.find((session) => session.id === decodeURIComponent(sessionMatch[1]));
      return found ? json(route, found) : json(route, { error: "session not found" }, 404);
    }
    if (sessionMatch && method === "DELETE") {
      state.calls.push("delete");
      const deleteStatus = options.deleteStatuses?.[state.deleteAttempts] ?? options.deleteStatus;
      state.deleteAttempts += 1;
      if (deleteStatus) return json(route, { error: `delete error ${deleteStatus}` }, deleteStatus);
      state.sessions = state.sessions.filter((session) => session.id !== decodeURIComponent(sessionMatch[1]));
      return route.fulfill({ status: 204, body: "" });
    }
    if (cancelMatch && method === "POST") {
      state.calls.push("cancel");
      if (options.cancelStatus) return json(route, { error: `cancel error ${options.cancelStatus}` }, options.cancelStatus);
      return route.fulfill({ status: 204, body: "" });
    }
    if (messageMatch && method === "POST") {
      state.calls.push("message");
      state.messageBodies.push(request.postDataJSON());
      if (options.delayStreamUntilCancel) {
        while (!state.calls.includes("cancel")) await new Promise((resolve) => setTimeout(resolve, 20));
      }
      if (options.streamStatus) return json(route, { error: `stream error ${options.streamStatus}` }, options.streamStatus);
      const id = decodeURIComponent(messageMatch[1]);
      const body = state.messageBodies.at(-1) ?? {};
      const found = state.sessions.find((session) => session.id === id);
      if (found) {
        found.turns = [
          ...found.turns,
          { id: `u-${found.turns.length}`, role: "user", content: String(body.message), created_at: now },
          { id: `a-${found.turns.length}`, role: "assistant", content: assistant, created_at: now },
        ];
      }
      const stream = [
        "event: model_delta",
        `data: {"seq":1,"at":"${now}","kind":"model_delta",`,
        `data: "delta":"${assistant}"}`,
        "",
        "event: run_finished",
        `data: {"seq":2,"at":"${now}","kind":"run_finished"}`,
        "",
        "",
      ].join("\n");
      try {
        await route.fulfill({ status: 200, contentType: "text/event-stream", body: stream });
      } catch (error) {
        if (!options.delayStreamUntilCancel) throw error;
      }
      return;
    }

    return json(route, {});
  });
  return state;
}

async function signIn(page: Page) {
  await page.getByLabel("Gateway secret").fill("qa-secret");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByTestId("app-authenticated")).toBeVisible();
}

async function openChat(page: Page, query = "") {
  await page.goto(`/agent/chat${query}`);
  await signIn(page);
  await expect(page.locator("header").first()).toBeVisible();
}

async function assertViewportIntegrity(page: Page) {
  const dimensions = await page.evaluate(() => ({
    innerWidth: window.innerWidth,
    bodyWidth: document.body.scrollWidth,
    documentWidth: document.documentElement.scrollWidth,
  }));
  expect(dimensions.bodyWidth).toBeLessThanOrEqual(dimensions.innerWidth);
  expect(dimensions.documentWidth).toBeLessThanOrEqual(dimensions.innerWidth);

  const topBar = page.locator("header").first();
  if (await topBar.isVisible()) {
    const box = await topBar.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(dimensions.innerWidth + 1);
  }
}

test("session lifecycle, deep link, exact stream result, attachment, summary, and deletion", async ({ page }) => {
  const state = await installApi(page);
  await openChat(page);

  await page.getByRole("button", { name: "Attach context" }).click();
  await page.getByLabel("Service context").fill("checkout");
  await page.getByLabel("Incident ID context").fill("inc-42");
  await page.getByLabel("Message").fill("What changed?");
  await page.getByLabel("Message").press("Enter");

  await expect(page).toHaveURL(/session=session-created/);
  await expect(page.getByText(assistant, { exact: true })).toHaveCount(1);
  expect(state.calls).toEqual(expect.arrayContaining(["list", "create", "message", "get"]));
  expect(state.messageBodies[0]).toEqual({
    message: "What changed?",
    attachment: { service: "checkout", incident: { id: "inc-42" } },
  });
  await expect(page.getByText("What changed?", { exact: true })).toHaveCount(2);

  await page.reload();
  await expect(page.getByTestId("app-authenticated")).toBeVisible();
  await expect(page.getByLabel("Gateway secret")).toHaveCount(0);
  expect(state.gatewayExchanges).toBe(1);
  expect(state.authenticatedConfigProbes).toBeGreaterThanOrEqual(1);
  await expect(page.getByText(assistant, { exact: true })).toHaveCount(1);
  await page.getByRole("button", { name: "Delete thread" }).click();
  await expect(page).not.toHaveURL(/session=/);
  await expect(page.getByRole("heading", { name: "Versus DevOps Chat" })).toBeVisible();
  expect(state.calls).toContain("delete");
});

test("keyboard composer and cancel request ordering", async ({ page }) => {
  const session = populatedSession();
  const state = await installApi(page, { sessions: [session], delayStreamUntilCancel: true });
  await openChat(page, "?session=session-1");

  const composer = page.getByLabel("Message");
  await composer.fill("line one");
  await composer.press("Shift+Enter");
  await expect(composer).toHaveValue("line one\n");
  expect(state.calls).not.toContain("message");
  await composer.press("Enter");
  await expect(page.getByRole("button", { name: "Stop" })).toBeVisible();
  await page.getByRole("button", { name: "Stop" }).click();
  await expect.poll(() => state.calls.filter((call) => call === "cancel").length).toBe(1);
  expect(state.calls.indexOf("cancel")).toBeGreaterThan(state.calls.indexOf("message"));
});

test("stream conflict is actionable", async ({ page }) => {
  const session = populatedSession();
  const state = await installApi(page, { sessions: [session], streamStatus: 409 });
  await openChat(page, "?session=session-1");
  const composer = page.getByLabel("Message");
  await composer.fill("conflicting run");
  await composer.press("Enter");
  await expect.poll(() => state.calls.filter((call) => call === "get").length).toBeGreaterThanOrEqual(2);
  await expect(page.getByText("conflicting run", { exact: true })).toBeVisible();
  await expect(page.getByRole("alert")).toContainText("stream error 409");
  await expect(page.getByRole("alert").getByRole("button", { name: "Retry" })).toBeVisible();
  await expect(page.getByRole("alert").getByRole("button", { name: "Dismiss" })).toBeVisible();
});

test("delete failures are actionable without a live run and successful retry clears them", async ({ page }) => {
  for (const status of [409, 500, 401]) {
    const state = await installApi(page, { sessions: [populatedSession()], deleteStatuses: [status] });
    await openChat(page, "?session=session-1");
    await page.getByRole("button", { name: "Delete thread" }).click();
    const alert = page.getByRole("alert");
    await expect(alert).toContainText(new RegExp(`delete error ${status}|Session expired`));
    if (status === 401) {
      await page.getByRole("dialog", { name: "Session expired" }).getByRole("button", { name: "Close dialog" }).click();
    }
    await alert.getByRole("button", { name: "Retry" }).click();
    await expect.poll(() => state.calls.filter((call) => call === "delete").length).toBe(2);
    await expect(page).not.toHaveURL(/session=/);
    await expect(page.getByRole("heading", { name: "Versus DevOps Chat" })).toBeVisible();
    await expect(alert).toHaveCount(0);
    await page.unrouteAll({ behavior: "wait" });
  }
});

test("HTTP failure states are actionable for unauthorized, missing, unavailable, and server errors", async ({ page }) => {
  const cases = [
    { options: { sessions: [populatedSession()], getStatus: 404 }, query: "?session=session-1", text: "get error 404" },
    { options: { listStatus: 503 }, query: "", text: "Chat is not enabled" },
    { options: { listStatus: 500 }, query: "", text: "list error 500" },
  ];

  for (const entry of cases) {
    await installApi(page, entry.options);
    await openChat(page, entry.query);
    await expect(page.getByText(entry.text, { exact: false }).first()).toBeVisible();
    await page.unrouteAll({ behavior: "wait" });
  }

  await installApi(page, { listStatus: 401 });
  await openChat(page);
  await expect(page.getByText(/list error 401|Session expired/).first()).toBeVisible();
});

test("tool output, evidence, summaries, sticky bottom, markdown policy, and copy fallback", async ({ page }) => {
  const long = toolSession();
  long.turns = [
    ...Array.from({ length: 18 }, (_, index): ChatTurn => ({
      id: `u${index}`,
      role: index % 2 ? "assistant" : "user",
      content: `History line ${index}: ${"diagnostic detail ".repeat(10)}`,
      created_at: now,
    })),
    { id: "summary", role: "compaction", content: "Earlier investigation summarized for context.", created_at: now },
    toolSession().turns[1],
    {
      id: "unsafe",
      role: "assistant",
      content: '<script>window.__unsafe = true</script> ![beacon](https://attacker.invalid/leak) [bad](javascript:alert(1))',
      created_at: now,
    },
    {
      id: "safe-link",
      role: "assistant",
      content: "[docs](https://example.com)",
      created_at: now,
    },
  ];
  await page.addInitScript(() => {
    Object.defineProperty(navigator, "clipboard", { value: undefined, configurable: true });
    Object.defineProperty(document, "execCommand", {
      value: (command: string) => {
        (window as typeof window & { __copyCommand?: string }).__copyCommand = command;
        return true;
      },
      configurable: true,
    });
  });
  await installApi(page, { sessions: [long] });
  await openChat(page, "?session=session-1");

  await expect(page.getByText("Earlier investigation summarized for context.")).toBeVisible();
  await expect(page.getByText(/^History line 0:/).first()).toBeVisible();
  await page.getByRole("button", { name: /finished query metrics/ }).click();
  await expect(page.getByText(/"p99": 820/)).toBeVisible();
  await page.getByRole("button", { name: /Latency series/ }).click();
  await expect(page.getByRole("complementary", { name: "Evidence details" })).toContainText("checkout/p99");

  expect(await page.locator("article img").count()).toBe(0);
  expect(await page.locator("script").count()).toBeGreaterThan(0);
  expect(await page.locator("article script").count()).toBe(0);
  await expect(page.locator("article").filter({ hasText: "[bad](javascript:alert(1))" })).toBeVisible();
  await expect(page.getByRole("link", { name: "bad" })).toHaveCount(0);
  await expect(page.getByRole("link", { name: "docs" })).toHaveAttribute("rel", "noopener noreferrer");
  await page.getByRole("button", { name: "Copy code" }).click();
  await expect.poll(() => page.evaluate(() => (window as typeof window & { __copyCommand?: string }).__copyCommand)).toBe("copy");

  await page.getByLabel("Conversation").evaluate((element) => { element.scrollTop = 0; element.dispatchEvent(new Event("scroll")); });
  await expect(page.getByRole("button", { name: "Scroll to bottom" })).toBeVisible();
  await page.getByRole("button", { name: "Scroll to bottom" }).click();
  const gap = await page.getByLabel("Conversation").evaluate((element) => element.scrollHeight - element.scrollTop - element.clientHeight);
  expect(gap).toBeLessThanOrEqual(1);
});

test("mobile history and evidence drawers support interaction, focus containment, and escape", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await installApi(page, { sessions: [toolSession()] });
  await openChat(page, "?session=session-1");

  await page.getByRole("button", { name: "Open chat history" }).click();
  const history = page.getByRole("dialog", { name: "Thread history" });
  await expect(history).toBeVisible();
  const historyFocused = await history.evaluate((dialog) => dialog.contains(document.activeElement));
  await page.keyboard.press("Escape");
  await expect(history).toBeHidden();

  await page.getByRole("button", { name: /Latency series/ }).click();
  const evidence = page.getByRole("dialog", { name: "Evidence details" });
  await expect(evidence).toBeVisible();
  const evidenceFocused = await evidence.evaluate((dialog) => dialog.contains(document.activeElement));
  await evidence.getByRole("button", { name: "Close evidence" }).last().click();
  await expect(evidence).toBeHidden();
  expect.soft(historyFocused, "history drawer should receive focus when opened").toBe(true);
  expect.soft(evidenceFocused, "evidence drawer should receive focus when opened").toBe(true);
});

for (const theme of ["dark", "light"] as const) {
  for (const viewport of [
    { name: "desktop", width: 1440, height: 900 },
    { name: "mobile", width: 390, height: 844 },
  ] as const) {
    for (const visualState of ["empty", "populated", "tool", "evidence", "history", "loading", "error", "running"] as const) {
      test(`visual ${theme} ${viewport.name} ${visualState}`, async ({ page }) => {
        await page.setViewportSize({ width: viewport.width, height: viewport.height });
        await page.addInitScript((selectedTheme: Theme) => {
          localStorage.setItem("versus.theme", selectedTheme);
          if (selectedTheme === "light") document.documentElement.setAttribute("data-theme", "light");
        }, theme);

        const options: MockOptions = {};
        let query = "";
        if (["populated", "tool", "evidence", "history", "running"].includes(visualState)) {
          options.sessions = [visualState === "populated" ? populatedSession() : toolSession()];
          query = "?session=session-1";
        }
        if (visualState === "loading") options.delayList = true;
        if (visualState === "error") options.listStatus = 503;
        if (visualState === "running") options.delayStreamUntilCancel = true;
        await installApi(page, options);
        await page.goto(`/agent/chat${query}`);
        await signIn(page);

        if (visualState === "loading") {
          if (viewport.name === "mobile") {
            await page.getByRole("button", { name: "Open chat history" }).click();
            await expect(page.getByRole("dialog", { name: "Thread history" }).getByLabel("Loading history")).toBeVisible();
          } else {
            await expect(page.getByLabel("Loading history")).toBeVisible();
          }
          await page.waitForTimeout(100);
        } else if (visualState === "error") {
          await expect(page.getByRole("heading", { name: "Chat is not enabled" })).toBeVisible();
        } else {
          await expect(page.getByLabel("Message")).toBeVisible();
        }
        if (visualState === "evidence") {
          await page.getByRole("button", { name: /Latency series/ }).click();
          await expect(page.getByRole(viewport.name === "mobile" ? "dialog" : "complementary", { name: "Evidence details" })).toBeVisible();
          await page.waitForTimeout(400);
        }
        if (visualState === "history" && viewport.name === "mobile") {
          await page.getByRole("button", { name: "Open chat history" }).click();
          await expect(page.getByRole("dialog", { name: "Thread history" })).toBeVisible();
          await page.waitForTimeout(100);
        }
        if (visualState === "running") {
          await page.getByLabel("Message").fill("Continue investigating checkout");
          await page.getByLabel("Message").press("Enter");
          await expect(page.getByRole("button", { name: "Stop" })).toBeVisible();
        }

        await assertViewportIntegrity(page);
        await page.screenshot({
          path: `tests/e2e/screenshots/chat/${theme}/${viewport.name}/${visualState}.png`,
          fullPage: false,
        });
        if (visualState === "running") {
          await page.getByRole("button", { name: "Stop" }).click();
        }
      });
    }
  }
}