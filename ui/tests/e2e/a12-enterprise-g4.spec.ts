// A12 gate step G4 (plans/status/aws-harness/a12-enterprise-a8-a11.md §4): the
// browser pass on the Enterprise gate image, desktop 1440, dark and light.
//
//   Run A (E2E_A12_G4=a): the image booted without a key. The OSS screens
//   (Changes, FinOps, AWS Settings) are unchanged; 7d and the compare past
//   24 h read as locked; there is no extra Enterprise UI.
//   Run B (E2E_A12_G4=b): the same image with an active key. The built-in
//   admin signs in through the real login form (HARNESS_ADMIN_ENV); the SSO
//   users the QA Integration driver granted (viewer, finance on all accounts,
//   finance limited to Staging, responder limited to Staging) sign in by
//   clicking "Sign in with QA IdP" against a loopback OIDC IdP this spec runs
//   with the driver's key (qa-idp-key.pem in the stack folder).
//
// The stack is plans/harness-run/tests/aws/aws-changes.sh browser-up with
// AWS_CHANGES_EDITION=enterprise (fakeaws, file storage). Nothing is
// intercepted: every assertion is on what the server sent and the page
// rendered. Captures: capturePath("g4-run<a|b>-<persona>-<page>", 1440, theme,
// state). Findings: $HARNESS_RESULTS_DIR/a12-g4.jsonl.
//
// Run (traces off: B209):
//   HARNESS_OSS_PORT=18180 AWS_CHANGES_LICENSE=on|off harness.sh e2e oss \
//     a12-enterprise-g4.spec.ts --trace off
//   with E2E_A12_G4=a|b and AWS_CHANGES_BROWSER_DIR=<stack folder>.
import { expect, test, type Browser, type BrowserContext, type Page } from "@playwright/test";
import { createHash, createPrivateKey, createPublicKey, randomBytes, sign as signWith } from "node:crypto";
import * as fs from "node:fs";
import { createServer, type Server } from "node:http";
import * as path from "node:path";
import { openApp } from "./helpers";
import { capturePath, harnessResultsDir } from "./support/capture";

const RUN = (process.env.E2E_A12_G4 ?? "").toLowerCase();
test.skip(RUN !== "a" && RUN !== "b", "set E2E_A12_G4=a (no key) or b (key) against the aws-changes Enterprise browser stack");

const BROWSER_DIR = process.env.AWS_CHANGES_BROWSER_DIR ?? "";
const IDP_PORT = Number(process.env.GATE_IDP_PORT || 18098);
const THEMES = ["dark", "light"] as const;
type Theme = (typeof THEMES)[number];
const VIEWPORT = { width: 1440, height: 900 };
const PROD = "111122223333";
const STAGING = "444455556666";
const H = 3_600_000;
const APP_ORIGIN = new URL(process.env.E2E_BASE_URL ?? "http://127.0.0.1:18180").origin;

// The container reaches the IdP as host.docker.internal; so does the browser.
test.use({ launchOptions: { args: ["--host-resolver-rules=MAP host.docker.internal 127.0.0.1"] } });

// AWS_CHANGES_MASK_FILE (real-AWS runs, aws-changes.sh real-up): the file with
// the account ID. Every capture masks each element whose text holds it, and
// Playwright's own screenshot, video and trace (which take no mask) are off.
const MASK_TEXT = (() => {
  const file = process.env.AWS_CHANGES_MASK_FILE ?? "";
  if (!file) return "";
  const id = fs.readFileSync(file, "utf8").trim();
  if (!/^\d{12}$/.test(id)) throw new Error("AWS_CHANGES_MASK_FILE does not hold a 12-digit account ID");
  return id;
})();
const masks = (page: Page) => (MASK_TEXT ? [page.getByText(MASK_TEXT)] : []);
if (MASK_TEXT) test.use({ screenshot: "off", video: "off", trace: "off" });

// Wire codes and leaks that must never reach the screen as text.
const RAW = [
  "cost_scope_limited", "window_locked", "forbidden", "finops:view", "infrastructure:view", "rbac.denied",
  "[object Object]", "undefined", "NaN", "Internal Server Error",
];

const contexts: BrowserContext[] = [];
test.afterEach(async () => {
  for (const c of contexts.splice(0)) await c.close().catch(() => {});
});

interface Watch {
  phase: string;
  console: string[];
  http: string[];
}

function record(name: string, data: Record<string, unknown>) {
  const dir = harnessResultsDir();
  fs.mkdirSync(dir, { recursive: true });
  fs.appendFileSync(path.join(dir, "a12-g4.jsonl"), JSON.stringify({ run: RUN, test: name, at: new Date().toISOString(), ...data }) + "\n");
}

async function newPage(browser: Browser, theme: Theme, watch: Watch): Promise<Page> {
  const context = await browser.newContext({ viewport: VIEWPORT, colorScheme: theme });
  contexts.push(context);
  await context.addInitScript((value) => localStorage.setItem("versus.theme", value), theme);
  const page = await context.newPage();
  page.on("console", (m) => {
    if (m.type() === "error") watch.console.push(`[${watch.phase}] ${m.text().slice(0, 240)} @ ${(m.location()?.url ?? "").replace(/^https?:\/\/[^/]+/, "")}`);
  });
  page.on("response", (r) => {
    const u = new URL(r.url());
    if (r.status() >= 400 && (u.origin === APP_ORIGIN || u.hostname === "host.docker.internal")) {
      watch.http.push(`[${watch.phase}] ${r.status()} ${r.request().method()} ${u.pathname}${u.search}`);
    }
  });
  await page.emulateMedia({ colorScheme: theme });
  return page;
}

async function shot(page: Page, persona: string, surface: string, theme: Theme, state: string) {
  await page.waitForTimeout(400);
  await page.screenshot({ path: capturePath(`${process.env.G4_CAPTURE_PREFIX || "g4"}-run${RUN}-${persona}-${surface}`, VIEWPORT.width, theme, state), fullPage: true, mask: masks(page) });
}

async function mainText(page: Page): Promise<string> {
  return (await page.locator("main").first().innerText().catch(() => "")).replace(/\s+\n/g, "\n");
}

function rawCodes(text: string): string[] {
  return RAW.filter((c) => text.includes(c));
}

/** Options of the listbox a trigger button opens (closed again with Escape). */
async function listOptions(page: Page, trigger: ReturnType<Page["locator"]>): Promise<string[]> {
  if ((await trigger.count()) === 0) return ["<no trigger>"];
  await trigger.first().click();
  const box = page.getByRole("listbox").first();
  await box.waitFor({ state: "visible", timeout: 5_000 }).catch(() => {});
  const opts = (await box.getByRole("option").allInnerTexts().catch(() => [])).map((s) => s.replace(/\s+/g, " ").trim());
  await page.keyboard.press("Escape");
  return opts;
}

async function navTo(page: Page, label: string) {
  const nav = page.locator('nav[aria-label="Primary"]');
  const href = { Changes: "/aws/changes", FinOps: "/aws/finops", Settings: "/aws/settings" }[label] ?? "";
  const link = nav.locator(`a[href="${href}"]`);
  if (!(await link.isVisible().catch(() => false))) await nav.getByRole("button", { name: /^AWS/ }).first().click();
  await link.click();
}

async function changesSettled(page: Page) {
  await page.locator("[data-summary-card], [data-lock-note], main .card").first().waitFor({ state: "visible", timeout: 30_000 }).catch(() => {});
  await page.waitForLoadState("networkidle").catch(() => {});
}

async function settle(page: Page) {
  await page.waitForLoadState("networkidle").catch(() => {});
  await page.waitForTimeout(800);
}

// ---------------------------------------------------------------- SSO (Run B)

const USERS: Record<string, { sub: string; email: string; name: string }> = {
  viewer: { sub: "qa-viewer", email: "viewer@qa.example", name: "QA Viewer" },
  financeAll: { sub: "qa-finance-all", email: "finance-all@qa.example", name: "QA Finance All" },
  financeLimited: { sub: "qa-finance-limited", email: "finance-limited@qa.example", name: "QA Finance Staging" },
  responderLimited: { sub: "qa-responder-limited", email: "responder-limited@qa.example", name: "QA Responder Staging" },
};
let idpUser = "";
let idp: Server | null = null;

// A minimal OIDC provider with the driver's key (discovery, JWKS, authorize,
// token with PKCE S256, RS256 ID tokens). The authorize step signs in the
// persona the test chose (idpUser) instead of showing a form.
function startIdp(): Promise<Server> {
  const keyFile = path.join(BROWSER_DIR, "qa-idp-key.pem");
  const privateKey = createPrivateKey(fs.readFileSync(keyFile));
  const publicKey = createPublicKey(privateKey);
  const kid = `qa-${createHash("sha256").update(publicKey.export({ type: "spki", format: "der" })).digest("hex").slice(0, 8)}`;
  const jwk = { ...publicKey.export({ format: "jwk" }), kid, alg: "RS256", use: "sig" };
  const issuer = `http://host.docker.internal:${IDP_PORT}`;
  const codes = new Map<string, { user: (typeof USERS)[string]; nonce: string | null; clientID: string | null; challenge: string | null }>();
  const b64 = (v: string) => Buffer.from(v).toString("base64url");
  const server = createServer((req, res) => {
    const url = new URL(req.url ?? "/", "http://idp");
    const send = (status: number, body: unknown, headers: Record<string, string> = {}) => {
      res.writeHead(status, { "content-type": "application/json", ...headers });
      res.end(typeof body === "string" ? body : JSON.stringify(body));
    };
    if (url.pathname === "/.well-known/openid-configuration") {
      return send(200, { issuer, authorization_endpoint: `${issuer}/authorize`, token_endpoint: `${issuer}/token`, jwks_uri: `${issuer}/jwks`,
        response_types_supported: ["code"], subject_types_supported: ["public"], id_token_signing_alg_values_supported: ["RS256"],
        code_challenge_methods_supported: ["S256"] });
    }
    if (url.pathname === "/jwks") return send(200, { keys: [jwk] });
    if (url.pathname === "/authorize") {
      const user = USERS[idpUser];
      if (!user) return send(400, { error: "unknown user" });
      const code = randomBytes(16).toString("hex");
      codes.set(code, { user, nonce: url.searchParams.get("nonce"), clientID: url.searchParams.get("client_id"), challenge: url.searchParams.get("code_challenge") });
      const back = new URL(url.searchParams.get("redirect_uri") ?? "http://invalid/");
      back.searchParams.set("code", code);
      back.searchParams.set("state", url.searchParams.get("state") ?? "");
      res.writeHead(302, { location: back.toString() });
      return res.end();
    }
    if (url.pathname === "/token" && req.method === "POST") {
      let body = "";
      req.on("data", (d) => (body += d));
      req.on("end", () => {
        const form = new URLSearchParams(body);
        const entry = codes.get(form.get("code") ?? "");
        codes.delete(form.get("code") ?? "");
        if (!entry) return send(400, { error: "invalid_grant" });
        if (createHash("sha256").update(form.get("code_verifier") ?? "").digest("base64url") !== entry.challenge) return send(400, { error: "invalid_grant" });
        const now = Math.floor(Date.now() / 1000);
        const header = b64(JSON.stringify({ alg: "RS256", typ: "JWT", kid }));
        const claims = b64(JSON.stringify({ iss: issuer, sub: entry.user.sub, aud: entry.clientID, exp: now + 300, iat: now, nonce: entry.nonce,
          email: entry.user.email, email_verified: true, name: entry.user.name, auth_time: now, amr: ["pwd"] }));
        const signature = signWith("sha256", Buffer.from(`${header}.${claims}`), privateKey).toString("base64url");
        send(200, { access_token: randomBytes(12).toString("hex"), token_type: "Bearer", expires_in: 300, id_token: `${header}.${claims}.${signature}` });
      });
      return undefined;
    }
    return send(404, { error: "not found" });
  });
  return new Promise((resolve) => server.listen(IDP_PORT, "127.0.0.1", () => resolve(server)));
}

/** Sign in the way the persona does: the gateway secret (Run A), the
 *  built-in admin's login form, or the "Sign in with QA IdP" button. */
async function signIn(page: Page, persona: string, target: string, watch: Watch) {
  watch.phase = "sign-in";
  if (persona === "operator" || persona === "admin") {
    await openApp(page, target);
  } else {
    idpUser = persona;
    await page.goto(target);
    const button = page.getByRole("button", { name: "Sign in with QA IdP" });
    await expect(button).toBeVisible({ timeout: 30_000 });
    await button.click();
    await expect(page.getByTestId("app-authenticated")).toBeVisible({ timeout: 30_000 });
  }
  watch.phase = "landing";
  await page.goto(target);
  await settle(page);
}

// The IdP runs only where the QA Integration driver left its key (the fakeaws
// gate); a real-AWS run has no SSO users and signs in as the built-in admin.
if (RUN === "b" && fs.existsSync(path.join(BROWSER_DIR, "qa-idp-key.pem"))) {
  test.beforeAll(async () => {
    idp = await startIdp();
  });
  test.afterAll(async () => {
    await new Promise<void>((r) => (idp ? idp.close(() => r()) : r()));
  });
}

const iso = (msAgo: number) => new Date(Date.now() - msAgo).toISOString().replace(/\.\d{3}Z$/, "Z");

// ---------------------------------------------------------------- Run A: no key

for (const theme of THEMES) {
  test(`A changes (${theme})`, async ({ browser }) => {
    test.skip(RUN !== "a", "Run A only");
    test.setTimeout(150_000);
    const watch: Watch = { phase: "", console: [], http: [] };
    const page = await newPage(browser, theme, watch);
    await signIn(page, "operator", "/now", watch);
    const navText = (await page.locator('nav[aria-label="Primary"]').innerText()).replace(/\s+/g, " ");
    watch.phase = "changes";
    await navTo(page, "Changes");
    await expect(page).toHaveURL(/\/aws\/changes/);
    await expect(page.locator("[data-summary-card]")).toBeVisible({ timeout: 30_000 });
    await changesSettled(page);
    const problems: string[] = [];
    const seven = page.getByRole("button", { name: "7d, Enterprise" });
    const sevenState = { visible: await seven.isVisible(), disabled: await seven.getAttribute("aria-disabled"), title: await seven.getAttribute("title") };
    if (!sevenState.visible || sevenState.disabled !== "true") problems.push(`7d does not read as locked: ${JSON.stringify(sevenState)}`);
    for (const [name, loc] of [["Services filter", page.locator("[data-services-filter]")], ["grid", page.locator("[data-grid-scroll]")],
      ["list", page.locator('[data-panel="changes"]')], ["compare", page.locator('[data-panel="compare"]')]] as const) {
      if (!(await loc.first().isVisible())) problems.push(`${name} not visible`);
    }
    const text = await mainText(page);
    problems.push(...rawCodes(text).map((c) => `raw "${c}" on Changes`));
    await shot(page, "operator", "changes", theme, "populated");
    // Services filter panel open.
    await page.locator("[data-services-filter]").getByRole("button").first().click();
    await page.waitForTimeout(400);
    await shot(page, "operator", "changes", theme, "services-open");
    await page.keyboard.press("Escape");
    // Every service: the grid and the list populated.
    await page.getByRole("button", { name: "Clear all" }).first().click().catch(() => {});
    await changesSettled(page);
    await page.waitForTimeout(1000);
    const rowsAll = await page.locator("[data-item]").count();
    if (rowsAll === 0) problems.push("no rows with every service");
    await shot(page, "operator", "changes", theme, "all-services");
    // 7d: a quiet lock note, not an error.
    watch.phase = "7d";
    await seven.click({ force: true });
    await expect(page.locator("[data-lock-note]").first()).toBeVisible();
    const lockNote = (await page.locator("[data-lock-note]").first().innerText()).trim();
    if (/Couldn't load/i.test(await mainText(page))) problems.push("7d shows an error panel");
    await shot(page, "operator", "changes", theme, "7d-locked");
    const api7d = (await page.request.get("/api/admin/aws/changes?window=7d")).status();
    // Compare from 30 h ago (deep link): the Enterprise note.
    watch.phase = "compare-30h";
    await page.goto(`/aws/changes?from=${encodeURIComponent(iso(30 * H))}`);
    await changesSettled(page);
    const compareLocked = page.locator('[data-compare="locked"]');
    await compareLocked.waitFor({ state: "visible", timeout: 20_000 }).catch(() => {});
    const compareText = (await page.locator('[data-panel="compare"]').innerText().catch(() => "")).replace(/\s+/g, " ").slice(0, 400);
    if (!(await compareLocked.isVisible())) problems.push(`compare from 30 h ago is not shown locked: "${compareText}"`);
    await shot(page, "operator", "changes", theme, "compare-30h-locked");
    const comparePresets = await listOptions(page, page.getByRole("button", { name: "Compare from" }));
    // The shell: the OSS Enterprise zone, the gateway-secret sign-in.
    // Expected without a key: the shell's SSO probe (QA-447), the 7d and the
    // compare-past-24h refusals (window_locked, shown as notes), and the Now
    // page's Enterprise baselines probe.
    const unexplained = watch.http.filter((h) => !/\/enterprise\/api\/sso\/deployment|window=7d|sign-in\]|\[compare-30h\] 403 GET \/api\/admin\/aws\/changes\/compare|\/api\/agent\/baselines/.test(h));
    record(`A changes ${theme}`, { navText, sevenState, lockNote, api7d, compareText, comparePresets, problems, console: watch.console, http: watch.http, text: text.slice(0, 1500) });
    expect.soft(api7d, "API refuses 7d without a key").toBe(403);
    expect.soft(problems).toEqual([]);
    expect.soft(unexplained, "no unexplained 4xx/5xx").toEqual([]);
  });

  test(`A finops and settings (${theme})`, async ({ browser }) => {
    test.skip(RUN !== "a", "Run A only");
    test.setTimeout(150_000);
    const watch: Watch = { phase: "", console: [], http: [] };
    const page = await newPage(browser, theme, watch);
    await signIn(page, "operator", "/now", watch);
    watch.phase = "finops";
    await navTo(page, "FinOps");
    await expect(page).toHaveURL(/\/aws\/finops/);
    await settle(page);
    await page.getByText(/Month to date|This month|Spend|Forecast/i).first().waitFor({ timeout: 30_000 }).catch(() => {});
    await settle(page);
    const finText = await mainText(page);
    const problems: string[] = rawCodes(finText).map((c) => `raw "${c}" on FinOps`);
    for (const word of [/chargeback/i, /retention/i, /scheduled (pdf|report)/i, /branded/i, /finance role/i, /\bEnterprise\b/]) {
      if (word.test(finText)) problems.push(`FinOps shows Enterprise text ${word}`);
    }
    const accounts = await listOptions(page, page.locator('button[aria-haspopup="listbox"]').filter({ hasText: /All \d/ }));
    const periods = await listOptions(page, page.locator('button[aria-haspopup="listbox"]').first());
    await shot(page, "operator", "finops", theme, "populated");
    watch.phase = "settings";
    await navTo(page, "Settings");
    await expect(page).toHaveURL(/\/aws\/settings/);
    await settle(page);
    await page.getByText(/Production/).first().waitFor({ timeout: 20_000 }).catch(() => {});
    const setText = await mainText(page);
    problems.push(...rawCodes(setText).map((c) => `raw "${c}" on Settings`));
    const enterpriseMentions = setText.split("\n").filter((l) => /Enterprise/.test(l));
    await shot(page, "operator", "settings", theme, "populated");
    // Advanced (budget, optional data, tag allowlist) open.
    const advanced = page.getByText("Budget, optional data, tag allowlist");
    if (await advanced.isVisible().catch(() => false)) {
      await advanced.click();
      await page.waitForTimeout(400);
      await shot(page, "operator", "settings", theme, "advanced-open");
    }
    const unexplained = watch.http.filter((h) => !/\/enterprise\/api\/sso\/deployment|sign-in\]|\/api\/agent\/baselines/.test(h));
    record(`A finops+settings ${theme}`, { accounts, periods, enterpriseMentions, problems, console: watch.console, http: watch.http,
      finText: finText.slice(0, 2000), setText: setText.slice(0, 1200) });
    expect.soft(problems).toEqual([]);
    expect.soft(unexplained, "no unexplained 4xx/5xx").toEqual([]);
  });
}

// A Services filter switch: while the new answer loads, the card must not
// show the previous filter's numbers as the new scope's (no flash of "No
// changes" for every service). Samples the card every 100 ms.
test("A services switch shows no stale answer (dark)", async ({ browser }) => {
  test.skip(RUN !== "a" && process.env.G4_SWITCH !== "1", "Run A (or G4_SWITCH=1)");
  test.setTimeout(120_000);
  const watch: Watch = { phase: "", console: [], http: [] };
  const page = await newPage(browser, "dark", watch);
  await signIn(page, RUN === "b" ? "admin" : "operator", "/aws/changes", watch);
  const result: Array<Record<string, unknown>> = [];
  for (const [from, to] of [["rds,iam", "all"], ["all", "rds,iam"], ["rds,iam", "all"]] as const) {
    // Start from `from` as an explicit choice, saved like a user's.
    await page.goto(`/aws/changes${from === "all" ? "" : `?service=${from}`}`);
    await expect(page.locator("[data-summary-card]")).toBeVisible({ timeout: 30_000 });
    await settle(page);
    const before = (await page.locator("[data-summary-card]").innerText()).replace(/\s+/g, " ").slice(0, 160);
    const inflight: string[] = [];
    const done: string[] = [];
    const onReq = (r: { url: () => string }) => { if (r.url().includes("/api/admin/aws/changes?")) inflight.push(`${Date.now()} ${new URL(r.url()).search}`); };
    const onRes = (r: { url: () => string; status: () => number }) => { if (r.url().includes("/api/admin/aws/changes?")) done.push(`${Date.now()} ${r.status()} ${new URL(r.url()).search}`); };
    page.on("request", onReq);
    page.on("response", onRes);
    const t0 = Date.now();
    if (to === "all") await page.getByRole("button", { name: "Clear all" }).first().click();
    else {
      await page.locator("[data-services-filter]").getByRole("button").first().click();
      for (const name of ["RDS", "IAM"]) await page.getByRole("checkbox", { name: new RegExp(`^${name}`) }).first().check().catch(() => {});
      await page.keyboard.press("Escape");
    }
    const frames: string[] = [];
    for (let i = 0; i < 40; i++) {
      const card = (await page.locator("[data-summary-card]").innerText().catch(() => "")).replace(/\s+/g, " ").slice(0, 160);
      if (frames.length === 0 || frames[frames.length - 1].slice(frames[frames.length - 1].indexOf(" ") + 1) !== card) frames.push(`${Date.now() - t0}ms ${card}`);
      if (i === 2) await shot(page, "operator", "changes-switch", "dark", `${from.replace(",", "-")}-to-${to.replace(",", "-")}-t300ms`);
      await page.waitForTimeout(100);
    }
    page.off("request", onReq);
    page.off("response", onRes);
    result.push({ from, to, before, frames, inflight: inflight.map((s) => `${Number(s.split(" ")[0]) - t0}ms ${s.split(" ")[1]}`),
      done: done.map((s) => `${Number(s.split(" ")[0]) - t0}ms ${s.split(" ").slice(1).join(" ")}`) });
  }
  record("A services switch", { result });
  const stale = result.filter((r) => (r.frames as string[]).some((f) => (r.to === "all" ? /No changes in the last/ : /14,9\d\d|1[0-9],\d{3}/).test(f)));
  expect.soft(stale.map((r) => `${r.from} -> ${r.to}: ${(r.frames as string[]).join(" || ")}`), "no frame shows the previous filter's answer as the new scope's").toEqual([]);
});

// ---------------------------------------------------------------- Run B: key

for (const theme of THEMES) {
  test(`B admin changes (${theme})`, async ({ browser }) => {
    test.skip(RUN !== "b", "Run B only");
    test.setTimeout(150_000);
    const watch: Watch = { phase: "", console: [], http: [] };
    const page = await newPage(browser, theme, watch);
    await signIn(page, "admin", "/now", watch);
    const navText = (await page.locator('nav[aria-label="Primary"]').innerText()).replace(/\s+/g, " ");
    watch.phase = "changes";
    await navTo(page, "Changes");
    await expect(page.locator("[data-summary-card]")).toBeVisible({ timeout: 30_000 });
    await changesSettled(page);
    await shot(page, "admin", "changes", theme, "24h");
    const problems: string[] = [];
    const seven = page.getByRole("button", { name: /^7d/ });
    const sevenState = { label: await seven.getAttribute("aria-label"), disabled: await seven.getAttribute("aria-disabled"), title: await seven.getAttribute("title") };
    if (sevenState.disabled === "true" || /Enterprise/.test(sevenState.title ?? "")) problems.push(`licensed: 7d still reads as locked: ${JSON.stringify(sevenState)}`);
    watch.phase = "7d";
    const answer7d = page.waitForResponse((r) => r.url().includes("/api/admin/aws/changes") && r.url().includes("window=7d"), { timeout: 10_000 }).catch(() => null);
    await seven.click({ force: true });
    const r7 = await answer7d;
    await changesSettled(page);
    const lockShown = await page.locator("[data-lock-note]").first().isVisible().catch(() => false);
    const lockText = lockShown ? (await page.locator("[data-lock-note]").first().innerText()).trim() : "";
    if (lockShown) problems.push(`licensed: 7d shows the lock note "${lockText}"`);
    if (!r7) problems.push("licensed: clicking 7d sent no window=7d request");
    await shot(page, "admin", "changes", theme, "7d");
    const api7d = await page.request.get("/api/admin/aws/changes?window=7d");
    const api7dJson = api7d.ok() ? await api7d.json() : null;
    const deep = await (async () => {
      await page.goto("/aws/changes?window=7d");
      await changesSettled(page);
      return { url: page.url(), lock: await page.locator("[data-lock-note]").first().isVisible().catch(() => false), card: await page.locator("[data-summary-card]").isVisible().catch(() => false) };
    })();
    if (deep.lock || !deep.card) problems.push(`licensed: deep link ?window=7d → ${JSON.stringify(deep)}`);
    await shot(page, "admin", "changes", theme, "7d-deep-link");
    // Compare from 30 h ago: past 24 h, inside the history the licence allows.
    watch.phase = "compare-30h";
    await page.goto(`/aws/changes?from=${encodeURIComponent(iso(30 * H))}`);
    await changesSettled(page);
    await page.locator('[data-panel="compare"] .sk').first().waitFor({ state: "detached", timeout: 20_000 }).catch(() => {});
    const compareText = (await page.locator('[data-panel="compare"]').innerText().catch(() => "")).replace(/\s+/g, " ").slice(0, 500);
    const compareLocked = await page.locator('[data-compare="locked"]').isVisible().catch(() => false);
    if (compareLocked) problems.push(`licensed: compare from 30 h ago shows the Enterprise lock: "${compareText}"`);
    if (/Couldn't load/i.test(compareText)) problems.push(`licensed: compare from 30 h ago errors: "${compareText}"`);
    await shot(page, "admin", "changes", theme, "compare-30h");
    const comparePresets = await listOptions(page, page.getByRole("button", { name: "Compare from" }));
    const apiCompare = await page.request.get(`/api/admin/aws/changes/compare?from=${encodeURIComponent(iso(30 * H))}`);
    const text = await mainText(page);
    problems.push(...rawCodes(text).map((c) => `raw "${c}"`));
    record(`B admin changes ${theme}`, { navText, sevenState, r7: r7 ? r7.status() : null, lockText, api7d: api7d.status(),
      api7dWindow: api7dJson?.window, deep, compareText, comparePresets, apiCompare: { status: apiCompare.status(), body: (await apiCompare.text()).slice(0, 400) },
      problems, console: watch.console, http: watch.http });
    expect.soft(api7d.status(), "API serves 7d with a key").toBe(200);
    expect.soft(problems).toEqual([]);
  });

  test(`B admin finops and settings (${theme})`, async ({ browser }) => {
    test.skip(RUN !== "b", "Run B only");
    test.setTimeout(150_000);
    const watch: Watch = { phase: "", console: [], http: [] };
    const page = await newPage(browser, theme, watch);
    await signIn(page, "admin", "/now", watch);
    watch.phase = "finops";
    await navTo(page, "FinOps");
    await settle(page);
    await page.getByText(/Month to date|This month|Spend|Forecast/i).first().waitFor({ timeout: 30_000 }).catch(() => {});
    await settle(page);
    const finText = await mainText(page);
    const accounts = await listOptions(page, page.locator('button[aria-haspopup="listbox"]').filter({ hasText: /All \d/ }));
    const periods = await listOptions(page, page.locator('button[aria-haspopup="listbox"]').first());
    await shot(page, "admin", "finops", theme, "populated");
    const trend = await page.request.get("/api/admin/aws/finops/trend");
    const trendJson = trend.ok() ? await trend.json() : null;
    const months = JSON.stringify(trendJson ?? {}).match(/20\d\d-\d\d/g) ?? [];
    watch.phase = "settings";
    await navTo(page, "Settings");
    await settle(page);
    await page.getByText(/Production/).first().waitFor({ timeout: 20_000 }).catch(() => {});
    await shot(page, "admin", "settings", theme, "populated");
    const problems = [...rawCodes(finText), ...rawCodes(await mainText(page))];
    record(`B admin finops+settings ${theme}`, { accounts, periods, trendStatus: trend.status(), monthsSeen: [...new Set(months)].sort(),
      problems, console: watch.console, http: watch.http, finText: finText.slice(0, 2000) });
    expect.soft(problems).toEqual([]);
    expect.soft(watch.http.filter((h) => !/sign-in\]/.test(h)), "no 4xx/5xx for the admin").toEqual([]);
  });

  test(`B viewer (${theme})`, async ({ browser }) => {
    test.skip(RUN !== "b", "Run B only");
    test.setTimeout(150_000);
    const watch: Watch = { phase: "", console: [], http: [] };
    const page = await newPage(browser, theme, watch);
    await signIn(page, "viewer", "/aws/changes", watch);
    await changesSettled(page);
    await page.waitForTimeout(1500);
    const text = await mainText(page);
    const problems: string[] = rawCodes(text).map((c) => `raw "${c}"`);
    const denial = /permission|not allowed|access|role|ask an admin|administrator/i.test(text);
    if (!denial) problems.push(`viewer on Changes: no clear denial: "${text.slice(0, 300).replace(/\s+/g, " ")}"`);
    if (/Couldn't load|Try again|Retry/i.test(text)) problems.push(`viewer on Changes: an error/retry panel instead of a denial: "${text.slice(0, 300).replace(/\s+/g, " ")}"`);
    await shot(page, "viewer", "changes", theme, "denied");
    const navText = (await page.locator('nav[aria-label="Primary"]').innerText().catch(() => "")).replace(/\s+/g, " ");
    watch.phase = "finops";
    await page.goto("/aws/finops");
    await settle(page);
    const fin = await mainText(page);
    await shot(page, "viewer", "finops", theme, "as-viewer");
    record(`B viewer ${theme}`, { navText, problems, console: watch.console, http: watch.http, text: text.slice(0, 1200), fin: fin.slice(0, 800) });
    expect.soft(problems).toEqual([]);
  });

  test(`B finance all accounts (${theme})`, async ({ browser }) => {
    test.skip(RUN !== "b", "Run B only");
    test.setTimeout(240_000);
    const watch: Watch = { phase: "", console: [], http: [] };
    const page = await newPage(browser, theme, watch);
    await signIn(page, "financeAll", "/", watch);
    await page.waitForTimeout(2500);
    const landing = { url: page.url(), shell: await page.getByTestId("app-authenticated").isVisible(), text: (await mainText(page)).slice(0, 600) };
    await shot(page, "finance-all", "shell", theme, "landing");
    const shellHttp = [...watch.http];
    const shellConsole = [...watch.console];
    const navText = (await page.locator('nav[aria-label="Primary"]').innerText().catch(() => "")).replace(/\s+/g, " ");
    watch.phase = "finops";
    await navTo(page, "FinOps").catch(async () => page.goto("/aws/finops"));
    await settle(page);
    await page.getByText(/Month to date|This month|Spend|Forecast/i).first().waitFor({ timeout: 30_000 }).catch(() => {});
    await settle(page);
    const fin = await mainText(page);
    const accounts = await listOptions(page, page.locator('button[aria-haspopup="listbox"]').filter({ hasText: /All \d/ }));
    await shot(page, "finance-all", "finops", theme, "populated");
    const problems: string[] = rawCodes(fin).map((c) => `raw "${c}" on FinOps`);
    if (/Couldn't load|Forbidden|insufficient permissions/i.test(fin)) problems.push(`finance-all FinOps shows an error panel: "${fin.slice(0, 300).replace(/\s+/g, " ")}"`);
    const finopsHttp = watch.http.filter((h) => h.startsWith("[finops]"));
    // Nav entries it cannot use (desktop dark walks them all; light the AWS ones).
    const links = await page.locator('nav[aria-label="Primary"] a[href^="/"]').evaluateAll((els) => [...new Set(els.map((e) => e.getAttribute("href") ?? ""))]);
    const walk: Array<Record<string, unknown>> = [];
    const toWalk = theme === "dark" ? links : links.filter((l) => l.startsWith("/aws"));
    for (const href of toWalk.slice(0, 30)) {
      watch.phase = `nav ${href}`;
      const before = watch.http.length;
      const cBefore = watch.console.length;
      await page.goto(href);
      await settle(page);
      const t = await mainText(page);
      const entry = { href, url: page.url(), http: watch.http.slice(before), console: watch.console.slice(cBefore).length,
        errorPanel: /Couldn't load|Something went wrong|Forbidden|\b403\b/i.test(t), denial: /permission|not allowed|access|role/i.test(t), head: t.slice(0, 160).replace(/\s+/g, " ") };
      walk.push(entry);
      if (/\/aws\/changes|\/aws\/settings|\/incidents|\/now/.test(href)) await shot(page, "finance-all", `nav${href.replace(/\//g, "-")}`, theme, "refused");
    }
    record(`B finance-all ${theme}`, { landing, navText, accounts, shellHttp, shellConsole, finopsHttp, problems, walk, console: watch.console });
    expect.soft(landing.shell, "app shell loads").toBe(true);
    expect.soft(problems).toEqual([]);
    expect.soft(finopsHttp, "FinOps page has no 4xx/5xx").toEqual([]);
    expect.soft(shellHttp.filter((h) => !h.startsWith("[sign-in]")), "shell load: no unexplained 403s (QA-528)").toEqual([]);
    expect.soft(shellConsole.filter((h) => !h.startsWith("[sign-in]")), "shell load: no console errors (QA-528)").toEqual([]);
  });

  test(`B finance limited to Staging (${theme})`, async ({ browser }) => {
    test.skip(RUN !== "b", "Run B only");
    test.setTimeout(240_000);
    const watch: Watch = { phase: "", console: [], http: [] };
    const page = await newPage(browser, theme, watch);
    await signIn(page, "financeLimited", "/aws/finops", watch);
    await page.getByText(/Month to date|This month|Spend|Forecast/i).first().waitFor({ timeout: 30_000 }).catch(() => {});
    await settle(page);
    const fin = await mainText(page);
    const problems: string[] = rawCodes(fin).map((c) => `raw "${c}"`);
    if (fin.includes(PROD) || /Production/.test(fin)) problems.push("FinOps names Production");
    const pickers = await page.locator('button[aria-haspopup="listbox"]').allInnerTexts();
    const accountPicker = page.locator('button[aria-haspopup="listbox"]').filter({ hasText: /All \d|Staging/ });
    const accounts = (await accountPicker.count()) ? await listOptions(page, accountPicker) : ["<no picker>"];
    if (accounts.some((a) => /Production|111122223333/.test(a))) problems.push(`account picker lists Production: ${accounts.join(" | ")}`);
    const scopeLine = (await page.locator("main").getByText(/^Account|Accounts/).first().innerText().catch(() => "")).trim();
    await shot(page, "finance-limited", "finops", theme, "populated");
    // Budgets: the Forecast tile carries the budget line.
    const budgetLines = fin.split("\n").filter((l) => /budget/i.test(l)).slice(0, 8);
    // Report page.
    watch.phase = "report";
    await page.goto("/aws/finops/report");
    await settle(page);
    const report = await page.locator("body").innerText();
    if (report.includes(PROD) || /Production/.test(report)) problems.push("report names Production");
    problems.push(...rawCodes(report).map((c) => `raw "${c}" on report`));
    await shot(page, "finance-limited", "finops-report", theme, "populated");
    let saveToast = "";
    let chat = "";
    if (theme === "dark") {
      // Save the monthly report (a whole-bill write): a clear refusal.
      watch.phase = "save-report";
      await page.goto("/aws/finops");
      await settle(page);
      const exportButton = page.getByRole("button", { name: /^Export/ }).first();
      if (await exportButton.isVisible().catch(() => false)) {
        await exportButton.click();
        const save = page.getByRole("menuitem", { name: /Save monthly report/i }).or(page.getByRole("button", { name: /Save monthly report/i }));
        if (await save.first().isVisible().catch(() => false)) {
          await save.first().click();
          await page.waitForTimeout(1500);
          saveToast = (await page.locator('[role="status"], [role="alert"], [data-toast]').allInnerTexts().catch(() => [])).join(" | ").replace(/\s+/g, " ");
          await shot(page, "finance-limited", "finops", theme, "save-report-refused");
        } else saveToast = "<no Save monthly report action shown>";
        await page.keyboard.press("Escape");
      } else saveToast = "<no Export button>";
      if (saveToast && !saveToast.startsWith("<") && !/permission|not allowed|limited|account|access/i.test(saveToast)) problems.push(`save report refusal is unclear: "${saveToast}"`);
      problems.push(...rawCodes(saveToast).map((c) => `raw "${c}" in toast`));
      // Chat: the cost tools answer for Staging only.
      watch.phase = "chat";
      await page.goto("/agent/chat");
      await settle(page);
      const box = page.getByPlaceholder("Ask about your system");
      if (await box.isVisible().catch(() => false)) {
        await box.fill("use cost_accounts to list cost by account");
        await page.getByRole("button", { name: "Send" }).click();
        await page.waitForTimeout(8000);
        await settle(page);
        chat = (await mainText(page)).slice(-1500);
        if (chat.includes(PROD) || /Production/.test(chat)) problems.push("chat answer names Production");
        await shot(page, "finance-limited", "chat", theme, "cost-accounts");
      } else {
        chat = `<no chat box> ${(await mainText(page)).slice(0, 200)}`;
        await shot(page, "finance-limited", "chat", theme, "no-chat");
      }
    }
    record(`B finance-limited ${theme}`, { pickers, accounts, scopeLine, budgetLines, saveToast, chat, problems, console: watch.console, http: watch.http, fin: fin.slice(0, 1800) });
    expect.soft(problems).toEqual([]);
  });

  test(`B responder limited to Staging (${theme})`, async ({ browser }) => {
    test.skip(RUN !== "b", "Run B only");
    test.setTimeout(180_000);
    const watch: Watch = { phase: "", console: [], http: [] };
    const page = await newPage(browser, theme, watch);
    await signIn(page, "responderLimited", "/aws/changes", watch);
    await changesSettled(page);
    const ch = await mainText(page);
    const problems: string[] = rawCodes(ch).map((c) => `raw "${c}" on Changes`);
    const chAccounts = await listOptions(page, page.getByRole("button", { name: "Accounts" }));
    if (chAccounts.some((a) => /Production|111122223333/.test(a))) problems.push(`Changes Accounts lists Production: ${chAccounts.join(" | ")}`);
    if (/Production/.test(ch)) problems.push("Changes text names Production");
    await shot(page, "responder-limited", "changes", theme, "populated");
    watch.phase = "finops";
    await page.goto("/aws/finops");
    await page.getByText(/Month to date|This month|Spend|Forecast/i).first().waitFor({ timeout: 30_000 }).catch(() => {});
    await settle(page);
    const fin = await mainText(page);
    const finAccounts = await listOptions(page, page.locator('button[aria-haspopup="listbox"]').filter({ hasText: /All \d|Staging/ }));
    if (fin.includes(PROD) || /Production/.test(fin)) problems.push("FinOps names Production");
    if (finAccounts.some((a) => /Production|111122223333/.test(a))) problems.push(`FinOps Accounts lists Production: ${finAccounts.join(" | ")}`);
    problems.push(...rawCodes(fin).map((c) => `raw "${c}" on FinOps`));
    await shot(page, "responder-limited", "finops", theme, "populated");
    record(`B responder-limited ${theme}`, { chAccounts, finAccounts, problems, console: watch.console, http: watch.http, ch: ch.slice(0, 1200), fin: fin.slice(0, 1200) });
    expect.soft(problems).toEqual([]);
  });
}

// Follow-up for the finance personas: chat (allowed for finance), the Save
// as monthly report refusal on a closed month, and the Details view.
for (const theme of THEMES) {
  test(`B finance follow-up (${theme})`, async ({ browser }) => {
    test.skip(RUN !== "b", "Run B only");
    test.setTimeout(300_000);
    const watch: Watch = { phase: "", console: [], http: [] };
    const page = await newPage(browser, theme, watch);
    await signIn(page, "financeLimited", "/aws/finops", watch);
    await page.getByText(/Month to date|This month|to date/i).first().waitFor({ timeout: 30_000 }).catch(() => {});
    const out: Record<string, unknown> = {};
    const problems: string[] = [];
    // Ask AI from FinOps, as a finance user would.
    watch.phase = "chat";
    await page.getByRole("link", { name: "Ask AI" }).first().click();
    const box = page.getByPlaceholder("Ask about your system");
    const checking = page.getByText("Checking AI Agent status…");
    const t0 = Date.now();
    await box.waitFor({ state: "visible", timeout: 60_000 }).catch(() => {});
    out.chatBoxAfterMs = (await box.isVisible()) ? Date.now() - t0 : null;
    out.chatChecking = await checking.isVisible().catch(() => false);
    await shot(page, "finance-limited", "chat", theme, out.chatBoxAfterMs === null ? "stuck-60s" : "open");
    if (out.chatBoxAfterMs === null) problems.push(`chat never opens for finance: "${(await mainText(page)).slice(0, 120).replace(/\s+/g, " ")}" after 60 s`);
    else if (theme === "dark") {
      await box.fill("use cost_accounts to list cost by account");
      await page.getByRole("button", { name: "Send" }).click();
      await page.getByText(/444455556666|Staging|couldn't|error|denied/i).last().waitFor({ timeout: 60_000 }).catch(() => {});
      await settle(page);
      const chat = (await mainText(page)).slice(-1500);
      out.chat = chat;
      if (chat.includes(PROD) || /Production/.test(chat)) problems.push("chat answer names Production");
      await shot(page, "finance-limited", "chat", theme, "cost-accounts-answer");
    }
    // Save as monthly report on a closed month: a clear refusal.
    watch.phase = "save-report";
    await page.goto("/aws/finops?period=2026-10");
    await page.getByText(/October/).first().waitFor({ timeout: 30_000 }).catch(() => {});
    await settle(page);
    await page.getByRole("button", { name: /^Export/ }).first().click();
    const items = await page.getByRole("menu", { name: "Export" }).getByRole("menuitem").allInnerTexts().catch(() => []);
    out.exportItems = items.map((s) => s.replace(/\s+/g, " "));
    const save = page.getByRole("menuitem", { name: /Save as monthly report/ });
    if (await save.isVisible().catch(() => false)) {
      await save.click();
      await page.waitForTimeout(1500);
      out.saveToast = (await page.locator('[role="status"], [role="alert"]').allInnerTexts().catch(() => [])).join(" | ").replace(/\s+/g, " ");
      await shot(page, "finance-limited", "finops", theme, "save-report-refused");
    } else {
      out.saveToast = "<no Save as monthly report item: hidden for this role>";
      await shot(page, "finance-limited", "finops", theme, "export-menu");
      await page.keyboard.press("Escape");
    }
    // Details view (accounts, budgets).
    watch.phase = "details";
    await page.goto("/aws/finops");
    await settle(page);
    await page.getByRole("tab", { name: "Details" }).or(page.getByRole("button", { name: "Details", exact: true })).first().click().catch(() => {});
    await settle(page);
    const details = await mainText(page);
    out.detailsBudget = details.split("\n").filter((l) => /budget/i.test(l)).slice(0, 10);
    if (details.includes(PROD) || /Production/.test(details)) problems.push("Details names Production");
    problems.push(...rawCodes(details).map((c) => `raw "${c}" on Details`));
    await shot(page, "finance-limited", "finops-details", theme, "populated");
    record(`B finance follow-up ${theme}`, { ...out, problems, http: watch.http.filter((h) => !/config\/agent|incidents\/counts|baselines|sign-in\]/.test(h)) });
    expect.soft(problems).toEqual([]);
  });
}

test("B admin details (dark)", async ({ browser }) => {
  test.skip(RUN !== "b", "Run B only");
  test.setTimeout(120_000);
  const watch: Watch = { phase: "", console: [], http: [] };
  const page = await newPage(browser, "dark", watch);
  await signIn(page, "admin", "/aws/finops", watch);
  await page.getByRole("button", { name: "Details", exact: true }).first().click().catch(() => {});
  await settle(page);
  const details = await mainText(page);
  record("B admin details", { rawDrivers: details.split("\n").filter((l) => /^(usage_|discount_|price_|new_|removed_|one_off|other \()/.test(l)).slice(0, 8) });
  await shot(page, "admin", "finops-details", "dark", "populated");
});

// ---------------------------------------------------------------- watch (Run B)

test("B watch Changes over time (admin, dark)", async ({ browser }) => {
  test.skip(RUN !== "b" || process.env.G4_WATCH !== "1", "set G4_WATCH=1 on Run B");
  const minutes = Number(process.env.G4_WATCH_MIN || "5");
  test.setTimeout((minutes + 3) * 60_000);
  const watch: Watch = { phase: "", console: [], http: [] };
  const page = await newPage(browser, "dark", watch);
  await signIn(page, "admin", "/aws/changes", watch);
  await expect(page.locator("[data-summary-card]")).toBeVisible({ timeout: 30_000 });
  // Every service, so the counts and the list move with each poll.
  await page.getByRole("button", { name: "Clear all" }).first().click().catch(() => {});
  await page.locator("[data-item]").first().waitFor({ timeout: 30_000 }).catch(() => {});
  watch.phase = "watch";
  const samples: Array<Record<string, unknown>> = [];
  const server5xx: string[] = [];
  page.on("response", (r) => {
    if (r.status() >= 500) server5xx.push(`${new Date().toISOString()} ${r.status()} ${new URL(r.url()).pathname}`);
  });
  const ticks = Math.round((minutes * 60) / 30);
  for (let tick = 0; tick <= ticks; tick++) {
    const numbers = await page.locator("[data-number]").evaluateAll((els) => els.map((e) => `${e.getAttribute("data-number")}=${(e.querySelector("dd")?.textContent ?? "").trim()}`));
    const warning = (await page.locator("[data-summary-card]").innerText().catch(() => "")).split("\n").filter((l) => /not complete|Reading|missing/i.test(l)).join(" | ");
    const errors = await page.getByText(/Couldn't load|Something went wrong/i).count();
    const lock = await page.locator("[data-lock-note]").count();
    const rows = await page.locator("[data-item]").count();
    samples.push({ at: new Date().toISOString(), tick, numbers, warning, errors, lock, rows });
    await page.screenshot({ path: capturePath("g4-runb-admin-changes-watch", VIEWPORT.width, "dark", `t${String(tick).padStart(2, "0")}`), mask: masks(page) });
    if (tick < ticks) await page.waitForTimeout(30_000);
  }
  record("B watch", { minutes, samples, server5xx, console: watch.console, http: watch.http });
  expect.soft(server5xx, "no 5xx while the page stays open").toEqual([]);
  expect.soft(samples.filter((s) => (s.errors as number) > 0 || (s.lock as number) > 0).map((s) => s.at), "no error panel or lock note flashes").toEqual([]);
});

// ---------------------------------------------------------------- G4 recheck (QA-530..535)
//
// The second browser pass after the Front-End fixes (G4b). Tests are named
// "R<run> …" so `-g "^RA "` / `-g "^RB "` select them. Captures carry the
// prefix g4b-. Every request the page sends to the API is logged per phase,
// so "never sends window=7d" and "no retry loop" are read off the wire.

interface Recheck extends Watch {
  requests: string[];
}

async function recheckPage(browser: Browser, theme: Theme, viewport = VIEWPORT): Promise<{ page: Page; watch: Recheck }> {
  const watch: Recheck = { phase: "", console: [], http: [], requests: [] };
  const context = await browser.newContext({ viewport, colorScheme: theme });
  contexts.push(context);
  await context.addInitScript((value) => localStorage.setItem("versus.theme", value), theme);
  const page = await context.newPage();
  page.on("console", (m) => {
    if (m.type() === "error") watch.console.push(`[${watch.phase}] ${m.text().slice(0, 240)} @ ${(m.location()?.url ?? "").replace(/^https?:\/\/[^/]+/, "")}`);
  });
  page.on("request", (r) => {
    const u = new URL(r.url());
    if (u.origin === APP_ORIGIN && /^\/(api|enterprise\/api)\//.test(u.pathname)) watch.requests.push(`[${watch.phase}] ${Date.now()} ${r.method()} ${u.pathname}${u.search}`);
  });
  page.on("response", (r) => {
    const u = new URL(r.url());
    if (r.status() >= 400 && (u.origin === APP_ORIGIN || u.hostname === "host.docker.internal")) {
      watch.http.push(`[${watch.phase}] ${r.status()} ${r.request().method()} ${u.pathname}${u.search}`);
    }
  });
  await page.emulateMedia({ colorScheme: theme });
  return { page, watch };
}

async function rshot(page: Page, persona: string, surface: string, theme: Theme, state: string, width = VIEWPORT.width) {
  await page.waitForTimeout(400);
  await page.screenshot({ path: capturePath(`${process.env.G4_CAPTURE_PREFIX || "g4b"}-run${RUN}-${persona}-${surface}`, width, theme, state), fullPage: true, mask: masks(page) });
}

const inPhase = (lines: string[], phase: string) => lines.filter((l) => l.startsWith(`[${phase}]`));
const navHrefs = (page: Page) =>
  page.locator('nav[aria-label="Primary"] a[href^="/"]').evaluateAll((els) => [...new Set(els.map((e) => e.getAttribute("href") ?? ""))]);

/** Layout of the "What moved the cost" table (QA-535): overflow, clipped cells, empty bars, overlapping axis labels. */
function bridgeLayout(page: Page) {
  return page.evaluate(() => {
    const table = document.querySelector<HTMLElement>("[data-bridge-table]");
    if (!table) return { table: false } as Record<string, unknown>;
    const wrap = table.parentElement as HTMLElement;
    const card = table.closest<HTMLElement>(".card");
    const cardBox = card?.getBoundingClientRect();
    const cells = [...table.querySelectorAll<HTMLElement>("th, td")];
    const clipped = cells
      .filter((c) => c.scrollWidth > c.clientWidth + 1 || [...c.querySelectorAll<HTMLElement>("*")].some((k) => getComputedStyle(k).textOverflow === "ellipsis" && k.scrollWidth > k.clientWidth + 1))
      .map((c) => c.innerText.replace(/\s+/g, " ").slice(0, 60));
    const offCard = cardBox ? cells.filter((c) => c.getBoundingClientRect().right > cardBox.right + 1 && c.innerText.trim()).map((c) => c.innerText.replace(/\s+/g, " ").slice(0, 40)) : [];
    const rects = [...table.querySelectorAll<SVGRectElement>("[data-bar-cell] rect")].map((r) => r.getBoundingClientRect().width);
    const barCells = [...table.querySelectorAll<HTMLElement>("[data-bar-cell]")].map((b) => Math.round(b.getBoundingClientRect().width));
    const overlaps: string[] = [];
    const axisOutside: string[] = [];
    for (const axis of table.querySelectorAll<HTMLElement>("div.relative.h-4")) {
      const box = axis.getBoundingClientRect();
      const spans = [...axis.querySelectorAll<HTMLElement>("span")].map((s) => ({ t: s.innerText, r: s.getBoundingClientRect() }));
      for (let i = 0; i < spans.length; i++) {
        if (spans[i].r.left < box.left - 1 || spans[i].r.right > box.right + 1) axisOutside.push(spans[i].t);
        for (let j = i + 1; j < spans.length; j++) {
          if (spans[i].r.left < spans[j].r.right - 0.5 && spans[j].r.left < spans[i].r.right - 0.5) overlaps.push(`${spans[i].t}|${spans[j].t}`);
        }
      }
    }
    return {
      table: true,
      viewport: window.innerWidth,
      wrapScroll: wrap.scrollWidth,
      wrapClient: wrap.clientWidth,
      tableWidth: Math.round(table.getBoundingClientRect().width),
      cardWidth: Math.round(cardBox?.width ?? 0),
      rows: table.querySelectorAll("tbody tr").length,
      barCells,
      emptyBars: rects.filter((w) => w < 1).length,
      bars: rects.length,
      clipped,
      offCard,
      overlaps,
      axisOutside,
    } as Record<string, unknown>;
  });
}

function layoutProblems(label: string, l: Record<string, unknown>): string[] {
  const out: string[] = [];
  if (!l.table) return [`${label}: no bridge table`];
  if ((l.wrapScroll as number) > (l.wrapClient as number) + 1) out.push(`${label}: table scrolls sideways (${l.wrapScroll} > ${l.wrapClient})`);
  if ((l.clipped as string[]).length) out.push(`${label}: clipped cells ${JSON.stringify(l.clipped)}`);
  if ((l.offCard as string[]).length) out.push(`${label}: cells past the card edge ${JSON.stringify(l.offCard)}`);
  if ((l.bars as number) === 0 || (l.emptyBars as number) === (l.bars as number)) out.push(`${label}: no bars render`);
  if ((l.barCells as number[]).some((w) => w < 150)) out.push(`${label}: a bar cell under 10rem ${JSON.stringify(l.barCells)}`);
  if ((l.overlaps as string[]).length) out.push(`${label}: axis labels overlap ${JSON.stringify(l.overlaps)}`);
  if ((l.axisOutside as string[]).length) out.push(`${label}: axis labels outside the column ${JSON.stringify(l.axisOutside)}`);
  return out;
}

async function openDetails(page: Page) {
  await page.getByText(/Month to date|This month|to date/i).first().waitFor({ timeout: 30_000 }).catch(() => {});
  await page.getByRole("button", { name: "Details", exact: true }).or(page.getByRole("tab", { name: "Details" })).first().click();
  await page.locator("[data-bridge-table]").waitFor({ timeout: 30_000 }).catch(() => {});
  await settle(page);
}

/** Chat: wait for the composer, ask the cost question, wait for the answer. */
async function askCost(page: Page, watch: Recheck): Promise<{ openedMs: number | null; answer: string }> {
  const box = page.getByPlaceholder("Ask about your system");
  const t0 = Date.now();
  await box.waitFor({ state: "visible", timeout: 30_000 }).catch(() => {});
  if (!(await box.isVisible())) return { openedMs: null, answer: `<no composer> ${(await mainText(page)).slice(0, 200)}` };
  const openedMs = Date.now() - t0;
  watch.phase = "chat-turn";
  await box.fill("use cost_accounts to list cost by account");
  await page.getByRole("button", { name: "Send" }).click();
  await page.locator("main").getByText(/444455556666|111122223333|couldn't|error|denied/i).last().waitFor({ timeout: 60_000 }).catch(() => {});
  await page.waitForTimeout(3000);
  await settle(page);
  return { openedMs, answer: (await mainText(page)).slice(-2000) };
}

// ---- Run A (no key)

for (const theme of THEMES) {
  test(`RA 7d stays locked without a key (${theme})`, async ({ browser }) => {
    test.skip(RUN !== "a", "Run A only");
    test.setTimeout(150_000);
    const { page, watch } = await recheckPage(browser, theme);
    await signIn(page, "operator", "/aws/changes", watch);
    await expect(page.locator("[data-summary-card]")).toBeVisible({ timeout: 30_000 });
    await changesSettled(page);
    const problems: string[] = [];
    const seven = page.getByRole("group", { name: "Window" }).getByRole("button", { name: /^7d/ });
    const sevenState = { label: await seven.getAttribute("aria-label"), disabled: await seven.getAttribute("aria-disabled"), title: await seven.getAttribute("title"),
      lockIcon: await seven.locator("svg").count() };
    if (sevenState.disabled !== "true" || !sevenState.lockIcon) problems.push(`7d does not read as locked: ${JSON.stringify(sevenState)}`);
    watch.phase = "click-7d";
    await seven.click({ force: true });
    await page.waitForTimeout(2000);
    const clickNote = await page.locator("[data-lock-note]").first().isVisible().catch(() => false);
    if (!clickNote) problems.push("clicking 7d shows no lock note");
    await rshot(page, "operator", "changes", theme, "7d-clicked-locked");
    watch.phase = "deep-7d";
    await page.goto("/aws/changes?window=7d");
    await changesSettled(page);
    await page.waitForTimeout(5000);
    const deep = { url: page.url(), note: await page.locator("[data-lock-note]").first().isVisible().catch(() => false),
      noteText: (await page.locator("[data-lock-note]").first().innerText().catch(() => "")).trim(), card: (await page.locator("[data-summary-card]").innerText().catch(() => "")).replace(/\s+/g, " ").slice(0, 200) };
    if (!deep.note) problems.push(`?window=7d shows no lock note: ${JSON.stringify(deep)}`);
    await rshot(page, "operator", "changes", theme, "7d-deep-link-locked");
    const comparePresets = await listOptions(page, page.getByRole("button", { name: "Compare from" }));
    const sent7d = watch.requests.filter((r) => /window=7d|window=168h/.test(r));
    // After the page checks: the limits the OSS-mode answer publishes.
    const limits = await page.request.get("/api/admin/aws/changes?window=24h&limit=1").then(async (r) => ({ status: r.status(), limits: r.ok() ? (await r.json()).limits : null }));
    const unexplained = watch.http.filter((h) => !/\/enterprise\/api\/sso\/deployment|sign-in\]|\/api\/agent\/baselines/.test(h));
    record(`RA 7d ${theme}`, { sevenState, clickNote, deep, comparePresets, sent7d, limits, problems, console: watch.console, http: watch.http,
      changesRequests: watch.requests.filter((r) => r.includes("/api/admin/aws/changes")).slice(0, 40) });
    expect.soft(sent7d, "the page never sends window=7d without a key").toEqual([]);
    expect.soft(problems).toEqual([]);
    expect.soft(unexplained, "no unexplained 4xx/5xx").toEqual([]);
    expect.soft(comparePresets.some((o) => /\b(2|3|7|14|30) ?d\b|days? ago/.test(o)), `no compare preset past 24 h: ${comparePresets.join(" | ")}`).toBe(false);
  });

  // QA-530: after "Clear all" the card reads "Loading…", never "No changes".
  test(`RA services clear-all shows loading, not "No changes" (${theme})`, async ({ browser }) => {
    test.skip(RUN !== "a" && process.env.G4_SWITCH !== "1", "Run A (or G4_SWITCH=1)");
    test.setTimeout(180_000);
    const result: Array<Record<string, unknown>> = [];
    for (let attempt = 1; attempt <= 2; attempt++) {
      // A fresh context each time, so no cached every-service answer hides the gap.
      const { page, watch } = await recheckPage(browser, theme);
      await signIn(page, RUN === "b" ? "admin" : "operator", "/aws/changes?service=rds,iam", watch);
      await expect(page.locator("[data-summary-card]")).toBeVisible({ timeout: 30_000 });
      await settle(page);
      const before = (await page.locator("[data-summary-card]").innerText()).replace(/\s+/g, " ").slice(0, 160);
      watch.phase = "switch";
      const t0 = Date.now();
      await page.getByRole("button", { name: "Clear all" }).first().click();
      const frames: string[] = [];
      let last = "";
      let shotLoading = false;
      for (let i = 0; i < 80; i++) {
        const card = (await page.locator("[data-summary-card]").innerText().catch(() => "")).replace(/\s+/g, " ").slice(0, 160);
        const list = (await page.locator('[data-panel="changes"]').innerText().catch(() => "")).replace(/\s+/g, " ");
        const busy = await page.locator("[data-summary-card]").getAttribute("aria-busy").catch(() => null);
        const listState = /Nothing to list\./.test(list) ? "list:nothing" : /Loading…/.test(list) ? "list:loading" : "list:rows";
        const frame = `${card} | ${listState} | busy=${busy}`;
        if (frame !== last) frames.push(`${Date.now() - t0}ms ${frame}`);
        last = frame;
        // The first loading frame, captured at once (it lasts a few hundred ms).
        if (busy === "true" && !shotLoading) {
          shotLoading = true;
          await page.screenshot({ path: capturePath(`${process.env.G4_CAPTURE_PREFIX || "g4b"}-run${RUN}-operator-changes-clear-all`, VIEWPORT.width, theme, `loading-${attempt}`), mask: masks(page) });
        }
        if (/Changes [\d,]+/.test(card) && i > 5 && busy !== "true") break;
        await page.waitForTimeout(100);
      }
      await rshot(page, "operator", "changes-clear-all", theme, `settled-${attempt}`);
      const changesCalls = inPhase(watch.requests, "switch").filter((r) => r.includes("/api/admin/aws/changes?")).map((r) => `${Number(r.split(" ")[1]) - t0}ms ${r.split(" ").slice(2).join(" ")}`);
      result.push({ attempt, before, frames, changesCalls, console: watch.console });
      await page.context().close();
    }
    record(`RA services clear-all ${theme}`, { result });
    // Frames still labelled with the old filter are the page before the click
    // landed (old answer, old label: correct). Any frame under the new label
    // must not say "No changes" or "Nothing to list." until the new answer lands.
    const falseEmpty = result.flatMap((r) => (r.frames as string[])
      .filter((f) => !/Service filter: RDS, IAM|No RDS or IAM changes/.test(f))
      .filter((f) => /No changes in/.test(f.split(" | ")[0]) || /list:nothing/.test(f)).map((f) => `#${r.attempt} ${f}`));
    const sawLoading = result.map((r) => (r.frames as string[]).some((f) => /Loading…/.test(f)));
    expect.soft(falseEmpty, "no frame says No changes / Nothing to list while the every-service answer loads").toEqual([]);
    expect.soft(sawLoading, "the card shows Loading… while it reads").toEqual(result.map(() => true));
  });
}

// ---- Run B (key)

for (const theme of THEMES) {
  test(`RB admin changes 7d and compare (${theme})`, async ({ browser }) => {
    test.skip(RUN !== "b", "Run B only");
    test.setTimeout(180_000);
    const { page, watch } = await recheckPage(browser, theme);
    await signIn(page, "admin", "/aws/changes", watch);
    await expect(page.locator("[data-summary-card]")).toBeVisible({ timeout: 30_000 });
    await changesSettled(page);
    await page.waitForTimeout(1500);
    const problems: string[] = [];
    const seven = page.getByRole("group", { name: "Window" }).getByRole("button", { name: /^7d/ });
    const sevenState = { label: await seven.getAttribute("aria-label"), disabled: await seven.getAttribute("aria-disabled"), title: await seven.getAttribute("title"),
      lockIcon: await seven.locator("svg").count(), text: (await seven.innerText()).trim() };
    if (sevenState.disabled === "true" || sevenState.lockIcon || /Enterprise/.test(`${sevenState.title ?? ""} ${sevenState.label ?? ""}`)) problems.push(`7d still reads as locked: ${JSON.stringify(sevenState)}`);
    await rshot(page, "admin", "changes", theme, "24h-licensed");
    watch.phase = "7d";
    const answer7d = page.waitForResponse((r) => r.url().includes("/api/admin/aws/changes?") && r.url().includes("window=7d"), { timeout: 15_000 }).catch(() => null);
    await seven.click();
    const r7 = await answer7d;
    const r7json = r7 && r7.ok() ? await r7.json().catch(() => null) : null;
    await changesSettled(page);
    await page.locator('[data-summary-card][aria-busy="true"]').waitFor({ state: "detached", timeout: 30_000 }).catch(() => {});
    await page.waitForTimeout(1000);
    const card7 = (await page.locator("[data-summary-card]").innerText()).replace(/\s+/g, " ").slice(0, 300);
    const main7 = await mainText(page);
    const pressed7 = await seven.getAttribute("aria-pressed");
    if (!r7) problems.push("clicking 7d sent no window=7d request");
    if (pressed7 !== "true") problems.push(`7d not pressed after the click (${pressed7})`);
    if (/part of Enterprise|About Enterprise/i.test(main7)) problems.push("an Enterprise note shows on the licensed 7d view");
    if (!/7 ?d|7 days/i.test(card7)) problems.push(`the card does not say 7 days: "${card7}"`);
    await rshot(page, "admin", "changes", theme, "7d");
    watch.phase = "deep-7d";
    await page.goto("/aws/changes?window=7d");
    await changesSettled(page);
    await page.waitForTimeout(3000);
    const deep = { url: page.url(), pressed: await seven.getAttribute("aria-pressed"), note: await page.locator("[data-lock-note]").first().isVisible().catch(() => false),
      card: (await page.locator("[data-summary-card]").innerText().catch(() => "")).replace(/\s+/g, " ").slice(0, 200),
      sent: inPhase(watch.requests, "deep-7d").filter((r) => r.includes("/api/admin/aws/changes?")).map((r) => r.split(" ").slice(2).join(" ")) };
    if (deep.note || deep.pressed !== "true" || !deep.sent.some((s) => s.includes("window=7d"))) problems.push(`deep link ?window=7d: ${JSON.stringify(deep)}`);
    await rshot(page, "admin", "changes", theme, "7d-deep-link");
    // Compare: the from picker reaches back past 24 h.
    watch.phase = "compare";
    await page.goto("/aws/changes");
    await changesSettled(page);
    const trigger = page.getByRole("button", { name: "Compare from" });
    await trigger.click();
    const box = page.getByRole("listbox").first();
    await box.waitFor({ state: "visible", timeout: 5_000 }).catch(() => {});
    const comparePresets = (await box.getByRole("option").allInnerTexts().catch(() => [])).map((s) => s.replace(/\s+/g, " ").trim());
    await rshot(page, "admin", "changes", theme, "compare-from-open");
    const past = box.getByRole("option").filter({ hasText: /\b2 ?d\b|2 days/ }).first();
    let compareAfter = "";
    let compareLocked = false;
    if (await past.count()) {
      await past.click();
      await page.waitForTimeout(500);
      await page.locator('[data-panel="compare"] .sk').first().waitFor({ state: "detached", timeout: 20_000 }).catch(() => {});
      await settle(page);
      compareAfter = (await page.locator('[data-panel="compare"]').innerText().catch(() => "")).replace(/\s+/g, " ").slice(0, 500);
      compareLocked = await page.locator('[data-compare="locked"]').isVisible().catch(() => false);
      await rshot(page, "admin", "changes", theme, "compare-2d");
    } else {
      await page.keyboard.press("Escape");
      problems.push(`Compare from offers nothing past 24 h: ${comparePresets.join(" | ")}`);
    }
    if (compareLocked || /part of Enterprise/i.test(compareAfter)) problems.push(`compare 2 d ago shows the Enterprise lock: "${compareAfter}"`);
    if (/Couldn't load/i.test(compareAfter)) problems.push(`compare 2 d ago errors: "${compareAfter}"`);
    const compareSent = inPhase(watch.requests, "compare").filter((r) => r.includes("/compare")).map((r) => r.split(" ").slice(2).join(" "));
    const limits = await page.request.get("/api/admin/aws/changes?window=24h&limit=1").then(async (r) => ({ status: r.status(), limits: r.ok() ? (await r.json()).limits : null }));
    problems.push(...rawCodes(await mainText(page)).map((c) => `raw "${c}"`));
    record(`RB admin changes ${theme}`, { sevenState, r7: r7?.status() ?? null, r7window: r7json?.window, r7buckets: Array.isArray(r7json?.buckets) ? r7json.buckets.length : null,
      card7, deep, comparePresets, compareAfter, compareSent, limits, problems, console: watch.console, http: watch.http });
    expect.soft(problems).toEqual([]);
    expect.soft(watch.http.filter((h) => !/sign-in\]/.test(h)), "no 4xx/5xx for the admin").toEqual([]);
    expect.soft(watch.console.filter((h) => !/sign-in\]/.test(h)), "no console errors for the admin").toEqual([]);
  });

  test(`RB viewer denials (${theme})`, async ({ browser }) => {
    test.skip(RUN !== "b", "Run B only");
    test.setTimeout(180_000);
    const { page, watch } = await recheckPage(browser, theme);
    await signIn(page, "viewer", "/now", watch);
    const nav = await navHrefs(page);
    const problems: string[] = [];
    if (nav.some((h) => h.startsWith("/aws"))) problems.push(`viewer nav offers AWS pages: ${nav.filter((h) => h.startsWith("/aws")).join(", ")}`);
    const pages: Array<Record<string, unknown>> = [];
    for (const [name, href] of [["changes", "/aws/changes"], ["finops", "/aws/finops"], ["settings", "/aws/settings"]] as const) {
      watch.phase = name;
      await page.goto(href);
      await settle(page);
      await page.waitForTimeout(8000);
      const text = await mainText(page);
      const entry = {
        href,
        denied: await page.locator("[data-access-denied]").first().isVisible().catch(() => false),
        title: (await page.locator("#access-denied-title").first().innerText().catch(() => "")).trim(),
        retry: await page.getByRole("button", { name: /Retry|Try again/i }).count(),
        alert: await page.locator('main [role="alert"]').count(),
        errorPanel: /Couldn't load|Something went wrong/i.test(text),
        raw: rawCodes(text),
        requests: inPhase(watch.requests, name).map((r) => r.split(" ").slice(2).join(" ")),
        http: inPhase(watch.http, name),
        text: text.slice(0, 300).replace(/\s+/g, " "),
      };
      pages.push(entry);
      if (!entry.denied) problems.push(`${href}: no plain denial: "${entry.text}"`);
      if (entry.retry || entry.alert || entry.errorPanel) problems.push(`${href}: error styling or Retry (retry=${entry.retry} alert=${entry.alert} error=${entry.errorPanel})`);
      if (entry.raw.length) problems.push(`${href}: raw ${entry.raw.join(",")}`);
      const refused = (entry.http as string[]).filter((h) => / 403 /.test(h));
      if (refused.length > 1) problems.push(`${href}: ${refused.length} refused requests in 8 s: ${refused.join(" ; ")}`);
      await rshot(page, "viewer", name, theme, "denied");
    }
    record(`RB viewer ${theme}`, { nav, pages, problems, console: watch.console, http: watch.http });
    expect.soft(problems).toEqual([]);
  });

  test(`RB finance all accounts (${theme})`, async ({ browser }) => {
    test.skip(RUN !== "b", "Run B only");
    test.setTimeout(240_000);
    const { page, watch } = await recheckPage(browser, theme);
    await signIn(page, "financeAll", "/", watch);
    await page.waitForTimeout(2500);
    const problems: string[] = [];
    const landing = page.url();
    if (!/\/aws\/finops/.test(new URL(landing).pathname)) problems.push(`lands on ${landing}, not FinOps`);
    const nav = await navHrefs(page);
    const navText = (await page.locator('nav[aria-label="Primary"]').innerText().catch(() => "")).replace(/\s+/g, " ");
    const unexpected = nav.filter((h) => !/^\/agent\/chat|^\/aws\/finops/.test(h));
    if (unexpected.length || !nav.some((h) => h.startsWith("/agent/chat")) || !nav.some((h) => h.startsWith("/aws/finops"))) problems.push(`nav: ${nav.join(", ")}`);
    const badge = (await page.getByTestId("topbar-role").innerText().catch(() => "")).trim();
    if (!/^finance$/i.test(badge)) problems.push(`badge reads "${badge}"`);
    const topbar = (await page.locator("header").first().innerText().catch(() => "")).replace(/\s+/g, " ").slice(0, 300);
    await rshot(page, "finance-all", "landing", theme, "finops");
    // Normal use: FinOps summary, Details, back, then Ask AI.
    watch.phase = "finops";
    await page.getByText(/Month to date|This month|to date/i).first().waitFor({ timeout: 30_000 }).catch(() => {});
    const fin = await mainText(page);
    if (/Couldn't load|insufficient permissions/i.test(fin)) problems.push("FinOps shows an error panel");
    await openDetails(page);
    await page.getByRole("button", { name: "Summary", exact: true }).or(page.getByRole("tab", { name: "Summary" })).first().click().catch(() => {});
    await settle(page);
    watch.phase = "chat-open";
    await page.getByRole("link", { name: "Ask AI" }).first().click();
    const chat = await askCost(page, watch);
    if (chat.openedMs === null) problems.push(`chat never opens: ${chat.answer}`);
    else if (!/111122223333|444455556666|Production|Staging/.test(chat.answer)) problems.push(`no cost_accounts answer: "${chat.answer.slice(-400)}"`);
    await rshot(page, "finance-all", "chat", theme, "cost-accounts-answer");
    // Watch the wire for 20 s after the answer: no retry loop.
    watch.phase = "chat-idle";
    await page.waitForTimeout(20_000);
    const idle = inPhase(watch.requests, "chat-idle").map((r) => r.split(" ").slice(2).join(" "));
    const chatOpenReqs = [...inPhase(watch.requests, "chat-open"), ...inPhase(watch.requests, "chat-turn")].map((r) => r.split(" ").slice(2).join(" "));
    const loopy = (list: string[]) => Object.entries(list.reduce<Record<string, number>>((m, r) => ((m[r.replace(/\?.*$/, "")] = (m[r.replace(/\?.*$/, "")] ?? 0) + 1), m), {})).filter(([, n]) => n > 3);
    if (loopy(idle).length) problems.push(`requests repeat while chat is idle: ${JSON.stringify(loopy(idle))}`);
    const normal = watch.console.filter((c) => !c.startsWith("[sign-in]"));
    const refused = watch.http.filter((h) => !h.startsWith("[sign-in]"));
    record(`RB finance-all ${theme}`, { landing, nav, navText, badge, topbar, chatOpenedMs: chat.openedMs, answer: chat.answer.slice(-1200), idle, chatOpenReqs,
      problems, console: watch.console, http: watch.http });
    expect.soft(problems).toEqual([]);
    expect.soft(refused, "no 4xx/5xx in normal use").toEqual([]);
    expect.soft(normal, "no console errors in normal use").toEqual([]);
  });

  test(`RB finance limited to Staging (${theme})`, async ({ browser }) => {
    test.skip(RUN !== "b", "Run B only");
    test.setTimeout(240_000);
    const { page, watch } = await recheckPage(browser, theme);
    await signIn(page, "financeLimited", "/aws/finops", watch);
    const problems: string[] = [];
    watch.phase = "details-1440";
    await openDetails(page);
    const l1440 = await bridgeLayout(page);
    problems.push(...layoutProblems("finance-limited 1440", l1440));
    await rshot(page, "finance-limited", "finops-details", theme, "populated");
    await page.setViewportSize({ width: 1280, height: 900 });
    watch.phase = "details-1280";
    await settle(page);
    const l1280 = await bridgeLayout(page);
    problems.push(...layoutProblems("finance-limited 1280", l1280));
    await rshot(page, "finance-limited", "finops-details", theme, "populated", 1280);
    await page.setViewportSize(VIEWPORT);
    const details = await mainText(page);
    if (details.includes(PROD) || /Production/.test(details)) problems.push("Details names Production");
    watch.phase = "chat-open";
    await page.goto("/aws/finops");
    await settle(page);
    await page.getByRole("link", { name: "Ask AI" }).first().click();
    const chat = await askCost(page, watch);
    if (chat.openedMs === null) problems.push(`chat never opens: ${chat.answer}`);
    else {
      if (chat.answer.includes(PROD) || /Production/.test(chat.answer)) problems.push("chat answer names Production");
      if (!/444455556666|Staging/.test(chat.answer)) problems.push(`chat answer does not name Staging: "${chat.answer.slice(-400)}"`);
    }
    await rshot(page, "finance-limited", "chat", theme, "cost-accounts-answer");
    record(`RB finance-limited ${theme}`, { l1440, l1280, chatOpenedMs: chat.openedMs, answer: chat.answer.slice(-1200), problems, console: watch.console, http: watch.http });
    expect.soft(problems).toEqual([]);
    expect.soft(watch.console.filter((c) => !c.startsWith("[sign-in]")), "no console errors").toEqual([]);
  });

  test(`RB admin finops details layout (${theme})`, async ({ browser }) => {
    test.skip(RUN !== "b", "Run B only");
    test.setTimeout(150_000);
    const { page, watch } = await recheckPage(browser, theme);
    await signIn(page, "admin", "/aws/finops", watch);
    const problems: string[] = [];
    await page.getByText(/Month to date|This month|to date/i).first().waitFor({ timeout: 30_000 }).catch(() => {});
    await settle(page);
    await rshot(page, "admin", "finops", theme, "summary");
    watch.phase = "details-1440";
    await openDetails(page);
    const l1440 = await bridgeLayout(page);
    problems.push(...layoutProblems("admin 1440", l1440));
    await rshot(page, "admin", "finops-details", theme, "populated");
    await page.setViewportSize({ width: 1280, height: 900 });
    watch.phase = "details-1280";
    await settle(page);
    const l1280 = await bridgeLayout(page);
    problems.push(...layoutProblems("admin 1280", l1280));
    await rshot(page, "admin", "finops-details", theme, "populated", 1280);
    problems.push(...rawCodes(await mainText(page)).filter((c) => !/forbidden/.test(c)).map((c) => `raw "${c}"`));
    record(`RB admin details ${theme}`, { l1440, l1280, problems, console: watch.console, http: watch.http });
    expect.soft(problems).toEqual([]);
    expect.soft(watch.http.filter((h) => !/sign-in\]/.test(h)), "no 4xx/5xx for the admin").toEqual([]);
    expect.soft(watch.console.filter((h) => !/sign-in\]/.test(h)), "no console errors for the admin").toEqual([]);
  });
}

// ---- Run B follow-up: the 7 day grid with every service, and the whole
// Details card in a tall viewport (the page scrolls inside main, so a
// full-page capture stops at the fold).

for (const theme of THEMES) {
  test(`RB2 admin 7d grid every service (${theme})`, async ({ browser }) => {
    test.skip(RUN !== "b", "Run B only");
    test.setTimeout(120_000);
    const { page, watch } = await recheckPage(browser, theme);
    await signIn(page, "admin", "/aws/changes?window=7d", watch);
    await expect(page.locator("[data-summary-card]")).toBeVisible({ timeout: 30_000 });
    await page.getByRole("button", { name: "Clear all" }).first().click().catch(() => {});
    await page.locator('[data-summary-card][aria-busy="true"]').waitFor({ state: "detached", timeout: 30_000 }).catch(() => {});
    await settle(page);
    const grid = await page.evaluate(() => {
      const scroll = document.querySelector<HTMLElement>("[data-grid-scroll]");
      const table = scroll?.querySelector<HTMLElement>('[role="table"]');
      const firstRow = table?.querySelector<HTMLElement>('[role="row"]');
      const cells = firstRow ? [...firstRow.children[1].children].map((c) => c.getBoundingClientRect()) : [];
      const axis = table ? [...table.querySelectorAll<HTMLElement>(":scope > div[aria-hidden='true'] > div.flex-1 > div")] : [];
      const sb = scroll?.getBoundingClientRect();
      const labels = axis.map((a, i) => ({ i, t: a.innerText.trim(), x: Math.round(a.getBoundingClientRect().left), cellX: Math.round(cells[i]?.left ?? -1),
        visible: !!sb && a.getBoundingClientRect().left >= sb.left && a.getBoundingClientRect().left < sb.right })).filter((l) => l.t);
      return { scrollWidth: scroll?.scrollWidth, clientWidth: scroll?.clientWidth, tableWidth: Math.round(table?.getBoundingClientRect().width ?? 0),
        buckets: cells.length, cellWidth: cells.length ? Math.round(cells[0].width * 10) / 10 : 0, axisCells: axis.length,
        axisRowWidth: Math.round((axis[0]?.parentElement?.scrollWidth ?? 0)), labels };
    });
    const card = (await page.locator("[data-summary-card]").innerText()).replace(/\s+/g, " ").slice(0, 300);
    await rshot(page, "admin", "changes", theme, "7d-every-service");
    record(`RB2 admin 7d grid ${theme}`, { card, grid, console: watch.console, http: watch.http });
    const misplaced = (grid.labels as Array<{ i: number; x: number; cellX: number; visible: boolean; t: string }>).filter((l) => Math.abs(l.x - l.cellX) > 2);
    expect.soft(misplaced, "each axis label sits over its own hour").toEqual([]);
    expect.soft((grid.labels as Array<{ visible: boolean }>).filter((l) => !l.visible).length, "every axis label is on screen").toBe(0);
  });

  test(`RB2 details card tall (${theme})`, async ({ browser }) => {
    test.skip(RUN !== "b", "Run B only");
    test.setTimeout(150_000);
    for (const persona of ["admin", "financeLimited"] as const) {
      for (const width of [1440, 1280]) {
        const { page, watch } = await recheckPage(browser, theme, { width, height: 2600 });
        await signIn(page, persona, "/aws/finops", watch);
        await openDetails(page);
        const card = page.locator("section.card, div.card").filter({ has: page.locator("[data-bridge-table]") }).first();
        await card.screenshot({ path: capturePath(`g4b-runb-${persona === "admin" ? "admin" : "finance-limited"}-finops-details-card`, width, theme, "full"), mask: masks(page) });
        const layout = await bridgeLayout(page);
        const rowHeights = await page.locator("[data-bridge-table] tbody tr").evaluateAll((rs) => rs.map((r) => Math.round(r.getBoundingClientRect().height)));
        const colWidths = await page.locator("[data-bridge-table] thead th").evaluateAll((hs) => hs.map((h) => `${(h as HTMLElement).innerText.trim() || "-"}:${Math.round(h.getBoundingClientRect().width)}`));
        record(`RB2 details tall ${persona} ${width} ${theme}`, { layout, rowHeights, colWidths });
        await page.context().close();
      }
    }
  });
}

// ---------------------------------------------------------------- G4 final recheck (QA-536..539)
//
// The third browser pass (G4c), after the Amendment 13 server (6 h buckets
// past 24 h, `bucket_seconds`) and the Front-End fixes. Desktop 1280 and
// 1440, dark and light. Tests are named "RC …"; run them with
// G4_CAPTURE_PREFIX=g4c so every capture, old tests' included, carries g4c-.

const WIDTHS = [1280, 1440] as const;
const CHANGES_API = "/api/admin/aws/changes?";

interface Answer {
  window: { since: string; until: string; settled_until?: string };
  bucket_seconds?: number;
  buckets: Array<Record<string, unknown> & { start: string; state: string }>;
  gaps: Array<{ account: string; region?: string; reason: string; from: string; to: string }>;
  alarms: Array<{ id: string; start?: string; start_known?: boolean }>;
  summary: { total: number; errors: number; by_actor_class: Record<string, number> };
}

/** The Activity grid against the answer it draws: overflow, columns, axis, tooltips, overlays, counts. */
function gridGeometry(page: Page, ans: Answer) {
  return page.evaluate((ans) => {
    const out: Record<string, unknown> = {};
    const scroll = document.querySelector<HTMLElement>("[data-grid-scroll]");
    const card = scroll?.closest<HTMLElement>("section");
    const table = scroll?.querySelector<HTMLElement>('[role="table"]');
    if (!scroll || !card || !table) return { missing: true } as Record<string, unknown>;
    // In view, so the QA-547 elementFromPoint probe sees the painted layers.
    scroll.scrollIntoView({ block: "center" });
    const r = (e: Element) => e.getBoundingClientRect();
    const round = (n: number) => Math.round(n * 10) / 10;
    const rows = [...table.querySelectorAll<HTMLElement>('[role="row"]')];
    const cellEls = [...rows[0].querySelectorAll<HTMLElement>('[role="cell"]')];
    const cells = cellEls.map((c) => r(c));
    const until = Date.parse(ans.window.until);
    const since = Date.parse(ans.window.since);
    const starts = ans.buckets.map((b) => Date.parse(b.start));
    const ends = starts.map((_, i) => starts[i + 1] ?? until);
    const xAt = (t: number) => {
      if (t <= starts[0]) return cells[0].left;
      if (t >= until) return cells[cells.length - 1].right;
      let i = starts.length - 1;
      while (i > 0 && starts[i] > t) i--;
      return cells[i].left + ((t - starts[i]) / (ends[i] - starts[i])) * cells[i].width;
    };
    const page = document.documentElement;
    const main = document.querySelector<HTMLElement>("main");
    out.overflow = {
      gridScroll: scroll.scrollWidth, gridClient: scroll.clientWidth,
      tableRight: round(r(table).right), cardRight: round(r(card).right), lastCellRight: round(cells[cells.length - 1]?.right ?? 0),
      pageScroll: page.scrollWidth, pageClient: page.clientWidth, mainScroll: main?.scrollWidth ?? 0, mainClient: main?.clientWidth ?? 0,
    };
    out.columns = { attr: Number(table.getAttribute("data-columns")), step: Number(table.getAttribute("data-step-seconds")), cells: cellEls.length,
      api: ans.buckets.length, apiStep: ans.bucket_seconds ?? null, cellWidth: round(cells[0]?.width ?? 0), label: table.getAttribute("aria-label"),
      // Every column's width: equal time slots must draw equal widths, or
      // every overlay placed by time (axis, hatch, alarm lines) drifts.
      widths: cells.map((c) => round(c.width)), widthSpread: round(Math.max(...cells.map((c) => c.width)) - Math.min(...cells.map((c) => c.width))) };
    // State per column: the UI cell against the answer's bucket.
    // Count text wider than its column (it spills into the neighbours).
    out.textSpill = rows.flatMap((row) => [...row.querySelectorAll<HTMLElement>('[role="cell"]')].map((c) => {
      const t = c.querySelector<HTMLElement>("span.relative");
      return t && r(t).width > r(c).width - 2 ? `${row.getAttribute("data-row")} "${t.innerText}" ${round(r(t).width)}px in ${round(r(c).width)}px` : "";
    }).filter(Boolean));
    out.stateMismatch = cellEls.map((c, i) => [i, c.getAttribute("data-state"), ans.buckets[i]?.state]).filter(([, a, b]) => a !== b);
    // Tooltips (long columns): the span "Nov 19 18:27–00:27: …" starts at the bucket's local clock and lasts the step.
    const pad = (n: number) => String(n).padStart(2, "0");
    const clock = (t: number) => `${pad(new Date(t).getHours())}:${pad(new Date(t).getMinutes())}`;
    const step = (ans.bucket_seconds ?? 3600) / 60;
    const tips = cellEls.map((c, i) => ({ i, title: c.getAttribute("title") ?? "", aria: c.getAttribute("aria-label") ?? "" }));
    out.tipSample = tips.filter((_, i) => i === 0 || i === tips.length - 1 || i === Math.floor(tips.length / 2)).map((t) => `${t.title} | ${t.aria}`);
    if (step > 60) {
      out.tipProblems = tips.map((t) => {
        const m = /^(?:[A-Z][a-z]{2} \d{1,2} )?(\d\d):(\d\d)–(?:[A-Z][a-z]{2} \d{1,2} )?(\d\d):(\d\d): /.exec(t.title);
        if (!m) return `#${t.i} title "${t.title}"`;
        const mins = ((Number(m[3]) * 60 + Number(m[4]) - Number(m[1]) * 60 - Number(m[2])) % 1440 + 1440) % 1440;
        const expectMins = Math.round((ends[t.i] - starts[t.i]) / 60000);
        if (mins !== expectMins) return `#${t.i} spans ${mins} min, bucket is ${expectMins}: "${t.title}"`;
        if (`${m[1]}:${m[2]}` !== clock(starts[t.i])) return `#${t.i} starts ${m[1]}:${m[2]}, bucket starts ${clock(starts[t.i])}`;
        if (!t.aria.includes(t.title.slice(0, t.title.indexOf(": ")))) return `#${t.i} aria-label "${t.aria}" lacks the span`;
        return "";
      }).filter(Boolean);
    }
    // The overlay box spans the columns exactly.
    const overlay = table.querySelector<HTMLElement>('[role="rowgroup"] > div[aria-hidden="true"]');
    out.overlayBox = overlay ? { left: round(r(overlay).left), firstCell: round(cells[0].left), right: round(r(overlay).right), lastCell: round(cells[cells.length - 1].right) } : null;
    // Hatch bands: each one's edges at a gap's from/to, measured on the columns.
    const bands = [...table.querySelectorAll<HTMLElement>("[data-band]")].map((b) => ({ left: r(b).left, right: r(b).right, reason: b.getAttribute("data-band"), title: b.getAttribute("title") }));
    out.bands = bands.length;
    out.bandMisplaced = bands.filter((b) => !ans.gaps.some((g) => {
      const x0 = xAt(Math.max(Date.parse(g.from), since));
      const x1 = xAt(Math.min(Date.parse(g.to), until));
      return Math.abs(b.left - x0) <= 2 && (Math.abs(b.right - x1) <= 2 || x1 - x0 < 8);
    })).map((b) => `${b.reason} ${round(b.left)}–${round(b.right)} "${b.title}"`);
    // QA-547: each band's edges against its gap's exact time (best match, px).
    out.bandOffsets = bands.map((b) => {
      const best = ans.gaps.map((g) => {
        const x0 = xAt(Math.max(Date.parse(g.from), since));
        const x1 = xAt(Math.min(Date.parse(g.to), until));
        // dr: right edge against the gap's end; a gap under 8 px is drawn at a
        // minimum width by design, so its overrun is kept apart in "past".
        return { dl: round(b.left - x0), dr: x1 - x0 < 8 ? 0 : round(b.right - x1), past: round(b.right - x1) };
      }).sort((p, q) => Math.abs(p.dl) + Math.abs(p.dr) - Math.abs(q.dl) - Math.abs(q.dr))[0];
      return { reason: b.reason, left: round(b.left), right: round(b.right), ...(best ?? { dl: null, dr: null }) };
    });
    // QA-547: a count in a column a band reaches sits on an opaque chip that
    // paints above the hatch and reads against it. Probed with elementFromPoint
    // across the text, after the grid is scrolled into view.
    {
      const rgba = (c: string) => {
        const cv = document.createElement("canvas"); cv.width = cv.height = 1;
        const cx = cv.getContext("2d") as CanvasRenderingContext2D;
        cx.clearRect(0, 0, 1, 1); cx.fillStyle = "#000"; cx.fillStyle = c; cx.fillRect(0, 0, 1, 1);
        const d = cx.getImageData(0, 0, 1, 1).data; return [d[0], d[1], d[2], d[3] / 255];
      };
      const lum = ([R, G, B]: number[]) => [R, G, B].map((v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); })
        .reduce((s, v, i) => s + v * [0.2126, 0.7152, 0.0722][i], 0);
      const contrast = (a: number[], b: number[]) => { const [x, y] = [lum(a), lum(b)].sort((p, q) => q - p); return Math.round(((x + 0.05) / (y + 0.05)) * 100) / 100; };
      const boxes = [...table.querySelectorAll<HTMLElement>("[data-band]")].map((b) => r(b));
      out.bandCounts = rows.flatMap((row) => [...row.querySelectorAll<HTMLElement>('[role="cell"]')].map((c, i) => {
        const t = c.querySelector<HTMLElement>("[data-count]");
        if (!t || !t.innerText.trim()) return null;
        const cb = r(c);
        const reached = boxes.filter((bb) => bb.left < cb.right - 0.5 && bb.right > cb.left + 0.5);
        if (!reached.length) return null;
        const tb = r(t);
        const ys = tb.top + tb.height / 2;
        const xs = [tb.left + 1, tb.left + tb.width / 2, tb.right - 1];
        const hits = xs.map((x) => { const e = document.elementFromPoint(x, ys); return e === t || (e !== null && t.contains(e)); });
        const cs = getComputedStyle(t);
        const bg = rgba(cs.backgroundColor);
        const fg = rgba(cs.color);
        const edgeInText = reached.some((bb) => [bb.left, bb.right].some((x) => x > tb.left + 0.5 && x < tb.right - 0.5));
        // A band box over any part of the text (hatch or its 1 px edge).
        const overText = reached.some((bb) => bb.left < tb.right - 0.5 && bb.right > tb.left + 0.5);
        // Union of the bands' spans inside the column (bands repeat per account and Region).
        const spans = reached.map((bb) => [Math.max(bb.left, cb.left), Math.min(bb.right, cb.right)]).sort((p, q) => p[0] - q[0]);
        let union = 0, end = -Infinity;
        for (const [a, b] of spans) { if (b > end) { union += b - Math.max(a, end); end = b; } }
        return { row: row.getAttribute("data-row"), column: i, text: t.innerText.trim(), onTop: hits.every(Boolean), hits,
          bgAlpha: bg[3], contrast: bg[3] > 0.99 ? contrast(fg, bg) : null, edgeInText, overText, coveredPct: round((union / cb.width) * 100),
          textBox: [round(tb.left), round(tb.right)], cellBox: [round(cb.left), round(cb.right)], bandRight: round(Math.max(...reached.map((bb) => bb.right))) };
      }).filter(Boolean));
    }
    // Hatched columns sit where the answer says nothing was read.
    out.gapColumns = cellEls.map((c, i) => (c.getAttribute("data-state") === "gap" ? i : -1)).filter((i) => i >= 0);
    // The arriving outline starts at the left edge of the column that holds
    // settled_until and runs to the last column's edge (QA-545 fix); none of
    // its borders crosses a count in those columns.
    const arriving = table.querySelector<HTMLElement>("[data-arriving-outline]");
    out.arriving = arriving && ans.window.settled_until ? (() => {
      const su = Date.parse(ans.window.settled_until as string);
      let ci = starts.length - 1;
      while (ci > 0 && starts[ci] > su) ci--;
      const ab = r(arriving);
      const cs = getComputedStyle(arriving);
      const bw = ["Left", "Right", "Top", "Bottom"].map((s) => parseFloat(cs.getPropertyValue(`border-${s.toLowerCase()}-width`)) || 0);
      const strips = [
        [ab.left, ab.left + bw[0], ab.top, ab.bottom], [ab.right - bw[1], ab.right, ab.top, ab.bottom],
        [ab.left, ab.right, ab.top, ab.top + bw[2]], [ab.left, ab.right, ab.bottom - bw[3], ab.bottom],
      ].filter(([x0, x1, y0, y1]) => x1 > x0 && y1 > y0);
      const covered = rows.flatMap((row) => [...row.querySelectorAll<HTMLElement>('[role="cell"]')].slice(ci).map((c) => {
        const t = c.querySelector<HTMLElement>("span.relative");
        if (!t || !t.innerText.trim()) return "";
        const tb = r(t);
        const hit = strips.some(([x0, x1, y0, y1]) => x0 < tb.right - 0.2 && x1 > tb.left + 0.2 && y0 < tb.bottom - 0.2 && y1 > tb.top + 0.2);
        return hit ? `${row.getAttribute("data-row")} "${t.innerText}" ${round(tb.left)}–${round(tb.right)}` : "";
      }).filter(Boolean));
      return { left: round(ab.left), expect: round(cells[ci].left), column: ci, settledX: round(xAt(su)), right: round(ab.right), lastCell: round(cells[cells.length - 1].right),
        settledUntil: ans.window.settled_until, borders: bw, covered, textsChecked: rows.length };
    })() : arriving ? { left: round(r(arriving).left), settledUntil: null } : null;
    // Alarm lines centred on the alarm's start.
    const lines = [...table.querySelectorAll<HTMLElement>("[data-alarm-line]")];
    out.alarms = lines.map((l) => {
      const a = ans.alarms.find((x) => x.id === l.getAttribute("data-alarm-line"));
      const c = r(l).left + r(l).width / 2;
      return { id: l.getAttribute("data-alarm-line"), x: round(c), expect: a?.start ? round(xAt(Date.parse(a.start))) : null };
    });
    out.apiAlarms = ans.alarms.length;
    // Axis.
    const dayAxis = table.querySelector<HTMLElement>("[data-day-axis]");
    if (dayAxis) {
      const ticks = [...dayAxis.querySelectorAll<HTMLElement>("[data-minor-tick]")].map((t) => r(t).left);
      const labels = [...dayAxis.querySelectorAll<HTMLElement>("[data-day-label]")].map((l) => ({ t: l.innerText.trim(), left: r(l).left, right: r(l).right }));
      const mids: number[] = [];
      const d = new Date(since);
      let m = new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
      if (m <= since) m = new Date(d.getFullYear(), d.getMonth(), d.getDate() + 1).getTime();
      while (m < until) { mids.push(m); const n = new Date(m); m = new Date(n.getFullYear(), n.getMonth(), n.getDate() + 1).getTime(); }
      const sb = r(scroll);
      out.axis = {
        kind: "day", ticks: ticks.length, tickMisplaced: ticks.filter((x, i) => Math.abs(x - (cells[i]?.left ?? -99)) > 1.5).length,
        labels: labels.map((l) => l.t),
        labelProblems: labels.map((l, i) => {
          const x = mids[i] !== undefined ? xAt(mids[i]) : NaN;
          const probs: string[] = [];
          if (!/^[A-Z][a-z]{2} \d{1,2}$/.test(l.t)) probs.push(`"${l.t}" is not a day`);
          if (!(Math.min(Math.abs(l.left - x), Math.abs(l.right - x)) <= 1.5)) probs.push(`"${l.t}" at ${round(l.left)}–${round(l.right)}, midnight at ${round(x)}`);
          if (l.left < sb.left - 1 || l.right > sb.right + 1) probs.push(`"${l.t}" off screen`);
          if (i > 0 && l.left < labels[i - 1].right - 0.5) probs.push(`"${l.t}" overlaps "${labels[i - 1].t}"`);
          return probs.join("; ");
        }).filter(Boolean),
        midnights: mids.length,
      };
    } else {
      const axisRow = [...table.children].find((c) => c.getAttribute("aria-hidden") === "true");
      const texts = axisRow ? [...axisRow.querySelectorAll<HTMLElement>("div.flex-1 > div")].map((e) => e.innerText.trim()).filter(Boolean) : [];
      out.axis = { kind: "clock", labels: texts, allClock: texts.every((t) => /^\d\d:\d\d$/.test(t)) };
    }
    // Counts: the cells, the row totals and the answer.
    const counts: Record<string, unknown> = {};
    for (const row of rows) {
      const key = row.getAttribute("data-row") ?? "";
      // A cell of 1,000 or more shows a short form ("2.6k"); its aria-label
      // ("Automation 14:38: 2,611, not settled yet") keeps the exact count.
      const exact = (c: HTMLElement) => {
        const m = /: ([\d,]+)(?:,|$)/.exec(c.getAttribute("aria-label") ?? "");
        return m ? Number(m[1].replace(/,/g, "")) : Number((c.innerText || "0").replace(/[^\d]/g, "") || 0);
      };
      const cellSum = [...row.querySelectorAll<HTMLElement>('[role="cell"]')].reduce((s, c) => s + exact(c), 0);
      // The shown text agrees with the exact count: the same digits under
      // 1,000, a short form within rounding at or above it.
      for (const c of row.querySelectorAll<HTMLElement>('[role="cell"]')) {
        const n = exact(c);
        const t = (c.innerText || "").trim();
        const short = /^(\d*\.?\d+)([kMB])$/.exec(t);
        const shown = short ? Number(short[1]) * { k: 1e3, M: 1e6, B: 1e9 }[short[2] as "k" | "M" | "B"] : Number(t.replace(/,/g, "") || 0);
        const ok = n < 1000 ? shown === n && !short : Boolean(short) && Math.abs(shown - n) <= n * 0.05 + 50;
        if (!ok) ((out.shortForm ??= []) as string[]).push(`${row.getAttribute("data-row")} "${t}" for ${n}`);
        else if (short) ((out.shortOk ??= []) as string[]).push(`"${t}"=${n}`);
      }
      const header = (row.querySelector("[data-row-total]") as HTMLElement | null)?.innerText.trim() ?? "";
      const apiCells = ans.buckets.reduce((s, b) => s + Number((b as Record<string, unknown>)[key] ?? 0), 0);
      const apiSummary = key === "errors" ? ans.summary.errors : ans.summary.by_actor_class[key];
      counts[key] = { cells: cellSum, header, apiCells, apiSummary };
    }
    out.counts = counts;
    out.apiTotal = ans.summary.total;
    out.apiBucketTotal = ans.buckets.reduce((s, b) => s + ["pipeline", "person", "aws_service", "unknown"].reduce((t, k) => t + Number((b as Record<string, unknown>)[k] ?? 0), 0), 0);
    return out;
  }, ans);
}

function gridProblems(label: string, win: string, g: Record<string, unknown>): string[] {
  const p: string[] = [];
  if (g.missing) return [`${label}: no grid`];
  const o = g.overflow as Record<string, number>;
  if (o.gridScroll > o.gridClient + 1) p.push(`${label}: grid scrolls sideways (${o.gridScroll} > ${o.gridClient})`);
  if (o.lastCellRight > o.cardRight + 0.5) p.push(`${label}: last column past the card (${o.lastCellRight} > ${o.cardRight})`);
  if (o.pageScroll > o.pageClient + 1 || o.mainScroll > o.mainClient + 1) p.push(`${label}: page scrolls sideways ${JSON.stringify(o)}`);
  const c = g.columns as Record<string, number | string | null>;
  const want = { "1h": [12, 300], "24h": [24, 3600], "7d": [28, 21600] }[win] ?? [0, 0];
  if (c.attr !== want[0] || c.cells !== want[0] || c.api !== want[0]) p.push(`${label}: columns ${JSON.stringify(c)}, want ${want[0]}`);
  if ((c.widthSpread as number) > 1.5) p.push(`${label}: columns of one step differ in width by ${c.widthSpread}px ${JSON.stringify(c.widths)}`);
  if (c.step !== want[1] || c.apiStep !== want[1]) p.push(`${label}: step ${c.step} / bucket_seconds ${c.apiStep}, want ${want[1]}`);
  if (((g.shortForm as string[]) ?? []).length) p.push(`${label}: cell text disagrees with its exact count ${JSON.stringify((g.shortForm as string[]).slice(0, 6))}`);
  if (((g.textSpill as string[]) ?? []).length) p.push(`${label}: count text wider than its column ${JSON.stringify((g.textSpill as string[]).slice(0, 6))}`);
  if ((g.stateMismatch as unknown[]).length) p.push(`${label}: cell states differ from the answer ${JSON.stringify(g.stateMismatch)}`);
  if (((g.tipProblems as string[]) ?? []).length) p.push(`${label}: tooltips ${JSON.stringify((g.tipProblems as string[]).slice(0, 5))}`);
  const ob = g.overlayBox as Record<string, number> | null;
  if (ob && (Math.abs(ob.left - ob.firstCell) > 1 || Math.abs(ob.right - ob.lastCell) > 1)) p.push(`${label}: overlay box ${JSON.stringify(ob)} is not the columns' box`);
  if ((g.bandMisplaced as string[]).length) p.push(`${label}: hatch bands off their gap ${JSON.stringify(g.bandMisplaced)}`);
  const offs = (g.bandOffsets as Array<{ dl: number | null; dr: number | null }>) ?? [];
  const offBad = offs.filter((o) => o.dl === null || Math.abs(o.dl) > 1 || Math.abs(o.dr ?? 0) > 1);
  if (offBad.length) p.push(`${label}: QA-547 band edge more than 1 px from its gap time ${JSON.stringify(offBad)}`);
  // Fails where a band is drawn over the count's text without the chip above
  // it, or a chip reads below 4.5:1. A count in a reached column that no band
  // crosses may go without a chip.
  const bc = (g.bandCounts as Array<{ onTop: boolean; bgAlpha: number; contrast: number | null; overText: boolean }>) ?? [];
  const bcBad = bc.filter((c) => (c.overText && (!c.onTop || c.bgAlpha < 0.99)) || (c.bgAlpha >= 0.99 && (c.contrast ?? 0) < 4.5));
  if (bcBad.length) p.push(`${label}: QA-547 count under a band not readable ${JSON.stringify(bcBad.slice(0, 6))}`);
  const a = g.arriving as Record<string, number | string | string[] | null> | null;
  if (a && a.expect !== undefined && (Math.abs((a.left as number) - (a.expect as number)) > 1.5 || Math.abs((a.right as number) - (a.lastCell as number)) > 1.5)) p.push(`${label}: arriving outline ${JSON.stringify(a)}`);
  if (a && ((a.covered as string[]) ?? []).length) p.push(`${label}: arriving outline border over counts ${JSON.stringify(a.covered)}`);
  for (const al of g.alarms as Array<{ id: string; x: number; expect: number | null }>) {
    if (al.expect === null || Math.abs(al.x - al.expect) > 2) p.push(`${label}: alarm line ${JSON.stringify(al)}`);
  }
  const ax = g.axis as Record<string, unknown>;
  if (win === "7d") {
    if (ax.kind !== "day") p.push(`${label}: 7d axis is ${ax.kind}`);
    else {
      if (ax.ticks !== 28 || ax.tickMisplaced) p.push(`${label}: ticks ${ax.ticks}, misplaced ${ax.tickMisplaced}`);
      if ((ax.labels as string[]).length !== ax.midnights || (ax.labels as string[]).length < 6) p.push(`${label}: day labels ${JSON.stringify(ax.labels)} for ${ax.midnights} midnights`);
      if ((ax.labelProblems as string[]).length) p.push(`${label}: day labels ${JSON.stringify(ax.labelProblems)}`);
    }
  } else if (ax.kind !== "clock" || !ax.allClock || !(ax.labels as string[]).length) p.push(`${label}: ${win} axis ${JSON.stringify(ax)}`);
  for (const [row, v] of Object.entries(g.counts as Record<string, { cells: number; header: string; apiCells: number; apiSummary: number }>)) {
    const header = Number(v.header.replace(/[^\d]/g, ""));
    if (v.cells !== v.apiCells || header !== v.apiSummary) p.push(`${label}: ${row} cells ${v.cells} / answer cells ${v.apiCells} / header ${v.header} / summary ${v.apiSummary}`);
  }
  return p;
}

for (const theme of THEMES) {
  for (const width of WIDTHS) {
    // QA-536: the grid at every window, against the answer it draws.
    test(`RC grid windows ${width} (${theme})`, async ({ browser }) => {
      test.setTimeout(240_000);
      const { page, watch } = await recheckPage(browser, theme, { width, height: 900 });
      const answers = new Map<string, Answer>();
      page.on("response", async (res) => {
        const u = new URL(res.url());
        if (u.origin !== APP_ORIGIN || !res.url().includes(CHANGES_API) || !res.ok()) return;
        const json = (await res.json().catch(() => null)) as Answer | null;
        if (json?.buckets && !u.searchParams.get("cursor")) answers.set(u.searchParams.get("window") ?? "24h", json);
      });
      const persona = RUN === "b" ? "admin" : "operator";
      await signIn(page, persona, "/aws/changes", watch);
      await expect(page.locator("[data-summary-card]")).toBeVisible({ timeout: 30_000 });
      await page.getByRole("button", { name: "Clear all" }).first().click().catch(() => {});
      const problems: string[] = [];
      const results: Record<string, unknown> = {};
      for (const win of RUN === "b" ? ["1h", "24h", "7d"] : ["1h", "24h"]) {
        watch.phase = `grid-${win}`;
        answers.delete(win);
        await page.getByRole("group", { name: "Window" }).getByRole("button", { name: new RegExp(`^${win}`) }).click();
        await expect.poll(() => answers.has(win), { timeout: 30_000 }).toBe(true);
        await page.locator('[data-summary-card][aria-busy="true"]').waitFor({ state: "detached", timeout: 30_000 }).catch(() => {});
        await settle(page);
        const ans = answers.get(win) as Answer;
        const g = await gridGeometry(page, ans);
        const card = (await page.locator("[data-summary-card]").innerText()).replace(/\s+/g, " ").slice(0, 300);
        const cardTotal = Number((/Changes\s*([\d,]+)/.exec(card)?.[1] ?? "-1").replace(/,/g, ""));
        const busiest = (await page.locator('section[aria-labelledby="changes-grid-title"]').innerText()).match(/Busiest (hour|span)[^\n]*/)?.[0] ?? "";
        const label = `${width} ${theme} ${win}`;
        // While the first read runs the card and the row totals read "—" by design (QA-512): no count comparison then.
        const firstRead = /^Changes —/.test(card);
        problems.push(...gridProblems(label, win, g).filter((p) => !firstRead || !/ cells \d+ \/ answer cells | card -1, /.test(p)));
        const dom = Object.entries(g.counts as Record<string, { cells: number }>).filter(([k]) => k !== "errors").reduce((s, [, v]) => s + v.cells, 0);
        if (!firstRead && (cardTotal !== g.apiTotal || dom !== g.apiTotal)) problems.push(`${label}: card ${cardTotal}, cells ${dom}, answer total ${g.apiTotal}, answer bucket sum ${g.apiBucketTotal}`);
        results[win] = { card, cardTotal, firstRead, busiest, grid: g };
        await rshot(page, persona, "changes-grid", theme, `${win}-every-service`, width);
        await page.locator('section[aria-labelledby="changes-grid-title"]').screenshot({ path: capturePath(`${process.env.G4_CAPTURE_PREFIX || "g4c"}-run${RUN}-${persona}-changes-grid-card`, width, theme, win), mask: masks(page) });
      }
      record(`RC grid ${width} ${theme}`, { results, problems, console: watch.console, http: watch.http });
      expect.soft(problems).toEqual([]);
      expect.soft(watch.http.filter((h) => !/sign-in\]|\/enterprise\/api\/sso\/deployment|\/api\/agent\/baselines/.test(h)), "no 4xx/5xx").toEqual([]);
    });

    // QA-539: every Compare "from" option reads in full.
    test(`RC compare from list ${width} (${theme})`, async ({ browser }) => {
      test.setTimeout(120_000);
      const { page, watch } = await recheckPage(browser, theme, { width, height: 900 });
      const persona = RUN === "b" ? "admin" : "operator";
      await signIn(page, persona, "/aws/changes", watch);
      await changesSettled(page);
      await page.getByRole("button", { name: "Compare from" }).click();
      const box = page.getByRole("listbox").first();
      await box.waitFor({ state: "visible", timeout: 5_000 });
      const list = await box.evaluate((el) => {
        const lb = el.getBoundingClientRect();
        const card = el.closest("section")?.getBoundingClientRect();
        const opts = [...el.querySelectorAll<HTMLElement>('[role="option"]')].map((o) => {
          const cut = [o, ...o.querySelectorAll<HTMLElement>("*")].some((k) => k.scrollWidth > k.clientWidth + 1 || (getComputedStyle(k).textOverflow === "ellipsis" && k.scrollWidth > k.clientWidth));
          const rr = o.getBoundingClientRect();
          return { text: o.innerText.replace(/\s+/g, " ").trim(), cut, height: Math.round(rr.height), outside: rr.right > lb.right + 0.5 || rr.left < lb.left - 0.5 };
        });
        return { listLeft: Math.round(lb.left), listRight: Math.round(lb.right), listWidth: Math.round(lb.width), viewport: window.innerWidth,
          cardRight: card ? Math.round(card.right) : null, trigger: Math.round((el.parentElement?.querySelector("button")?.getBoundingClientRect().width) ?? 0), opts };
      });
      await rshot(page, persona, "changes-compare-from", theme, "open", width);
      const problems: string[] = [];
      const cut = list.opts.filter((o) => o.cut || o.outside);
      if (cut.length) problems.push(`options cut: ${JSON.stringify(cut)}`);
      if (list.listRight > list.viewport || list.listLeft < 0) problems.push(`list past the viewport: ${list.listLeft}–${list.listRight} of ${list.viewport}`);
      const texts = list.opts.map((o) => o.text);
      if (RUN === "b" && !texts.some((t) => /\(2 d ago\)$/.test(t))) problems.push(`no option ends "(2 d ago)": ${texts.join(" | ")}`);
      if (!texts.some((t) => /\(24 h ago\)$/.test(t))) problems.push(`no option ends "(24 h ago)": ${texts.join(" | ")}`);
      await page.keyboard.press("Escape");
      record(`RC compare from ${width} ${theme}`, { list, problems, console: watch.console, http: watch.http });
      expect.soft(problems).toEqual([]);
    });

    // QA-543: Run B (key on Run A's data). Compare from a time inside Run A
    // (G4_UPGRADE_FROM, ISO) to now reads the same resources as unchanged:
    // the panel and the answer it draws both say 0 added and 0 removed.
    test(`RC compare across the upgrade ${width} (${theme})`, async ({ browser }) => {
      // G4_UPGRADE_TO (optional ISO) and G4_UPGRADE_LABEL (capture state, default
      // across-upgrade) give the "before only" and "after only" compares too.
      const from = process.env.G4_UPGRADE_FROM ?? "";
      const to = process.env.G4_UPGRADE_TO ?? "";
      const stateLabel = process.env.G4_UPGRADE_LABEL || "across-upgrade";
      test.skip(RUN !== "b" || !from, "Run B with G4_UPGRADE_FROM=<ISO time inside Run A>");
      test.setTimeout(120_000);
      const { page, watch } = await recheckPage(browser, theme, { width, height: 900 });
      let answer: Record<string, unknown> | null = null;
      page.on("response", async (res) => {
        if (new URL(res.url()).origin === APP_ORIGIN && res.url().includes("/api/admin/aws/changes/compare?") && res.ok()) answer = (await res.json().catch(() => null)) as Record<string, unknown> | null;
      });
      await signIn(page, "admin", "/now", watch);
      watch.phase = "compare-upgrade";
      await page.goto(`/aws/changes?from=${encodeURIComponent(from)}${to ? `&to=${encodeURIComponent(to)}` : ""}`);
      await changesSettled(page);
      await expect.poll(() => answer !== null, { timeout: 30_000 }).toBe(true);
      await page.locator('[data-panel="compare"] .sk').first().waitFor({ state: "detached", timeout: 20_000 }).catch(() => {});
      await settle(page);
      const panel = (await page.locator('[data-panel="compare"]').innerText().catch(() => "")).replace(/\s+/g, " ").slice(0, 600);
      const a = (answer ?? {}) as Record<string, unknown[] | string | undefined>;
      const n = (k: string) => (Array.isArray(a[k]) ? (a[k] as unknown[]).length : -1);
      const counts = { status: a.status, added: n("added"), removed: n("removed"), changed: n("changed"), partial: n("partial") };
      const problems: string[] = [];
      if (counts.added !== 0 || counts.removed !== 0 || counts.changed !== 0) problems.push(`compare ${from} → ${to || "now"}: answer ${JSON.stringify(counts)}`);
      if (counts.status === "unavailable") problems.push(`compare from ${from} is unavailable`);
      for (const kind of ["added", "removed", "changed"]) {
        const shown = new RegExp(`(\\d[\\d,]*) ${kind}`).exec(panel)?.[1];
        if (shown !== undefined && shown !== "0") problems.push(`panel shows ${shown} ${kind}: "${panel}"`);
      }
      if (/Couldn't load|part of Enterprise/i.test(panel)) problems.push(`panel: "${panel}"`);
      await rshot(page, "admin", "changes-compare", theme, stateLabel, width);
      record(`RC compare ${stateLabel} ${width} ${theme}`, { from, to, counts, panel, problems, console: watch.console, http: watch.http });
      expect.soft(problems).toEqual([]);
      expect.soft(watch.http.filter((h) => !/sign-in\]/.test(h)), "no 4xx/5xx").toEqual([]);
    });
  }

  // QA-537: a viewer's deep link or reload of a refused page sends no refused request.
  test(`RC viewer deep links (${theme})`, async ({ browser }) => {
    test.skip(RUN !== "b", "Run B only");
    test.setTimeout(180_000);
    const { page, watch } = await recheckPage(browser, theme);
    await signIn(page, "viewer", "/now", watch);
    const problems: string[] = [];
    const pages: Array<Record<string, unknown>> = [];
    for (const [name, href] of [["changes", "/aws/changes"], ["finops", "/aws/finops"], ["settings", "/aws/settings"]] as const) {
      for (const how of ["load", "reload"] as const) {
        const phase = `${name}-${how}`;
        watch.phase = phase;
        if (how === "load") await page.goto(href);
        else await page.reload();
        await settle(page);
        await page.waitForTimeout(5000);
        const entry = {
          phase,
          denied: await page.locator("[data-access-denied]").first().isVisible().catch(() => false),
          retry: await page.getByRole("button", { name: /Retry|Try again/i }).count(),
          requests: inPhase(watch.requests, phase).map((r) => r.split(" ").slice(2).join(" ")),
          http: inPhase(watch.http, phase),
          console: inPhase(watch.console, phase),
        };
        pages.push(entry);
        if (!entry.denied || entry.retry) problems.push(`${phase}: denied=${entry.denied} retry=${entry.retry}`);
        const pageReqs = entry.requests.filter((r) => /\/api\/admin\/aws\//.test(r));
        if (pageReqs.length) problems.push(`${phase}: sent ${pageReqs.join(" ; ")}`);
        if (entry.http.length) problems.push(`${phase}: refused ${entry.http.join(" ; ")}`);
        if (entry.console.length) problems.push(`${phase}: console ${entry.console.join(" ; ")}`);
        if (how === "load") await rshot(page, "viewer", name, theme, "deep-link-denied");
      }
    }
    record(`RC viewer ${theme}`, { pages, problems, console: watch.console, http: watch.http });
    expect.soft(problems).toEqual([]);
  });

  // QA-538: finance opens chat without the session list, and chat answers.
  test(`RC finance chat (${theme})`, async ({ browser }) => {
    test.skip(RUN !== "b", "Run B only");
    test.setTimeout(300_000);
    const problems: string[] = [];
    const out: Record<string, unknown> = {};
    for (const persona of ["financeAll", "financeLimited"] as const) {
      const { page, watch } = await recheckPage(browser, theme);
      await signIn(page, persona, "/", watch);
      await page.waitForTimeout(2500);
      const landing = new URL(page.url()).pathname;
      if (!/^\/aws\/finops/.test(landing)) problems.push(`${persona}: lands on ${landing}`);
      watch.phase = "chat-open";
      await page.getByText(/Month to date|This month|to date/i).first().waitFor({ timeout: 30_000 }).catch(() => {});
      await page.getByRole("link", { name: "Ask AI" }).first().click();
      const chat = await askCost(page, watch);
      if (chat.openedMs === null) problems.push(`${persona}: chat never opens: ${chat.answer.slice(0, 200)}`);
      else if (!/444455556666|Staging/.test(chat.answer)) problems.push(`${persona}: no cost_accounts answer: "${chat.answer.slice(-300)}"`);
      if (persona === "financeLimited" && (chat.answer.includes(PROD) || /Production/.test(chat.answer))) problems.push(`${persona}: answer names Production`);
      await rshot(page, persona === "financeAll" ? "finance-all" : "finance-limited", "chat", theme, "answer");
      watch.phase = "chat-reload";
      await page.reload();
      await settle(page);
      await page.getByPlaceholder("Ask about your system").waitFor({ state: "visible", timeout: 30_000 }).catch(() => {});
      await page.waitForTimeout(3000);
      const reloadText = await mainText(page);
      if (/Couldn't load|Retry|Try again/i.test(reloadText)) problems.push(`${persona}: chat shows an error panel after reload`);
      await rshot(page, persona === "financeAll" ? "finance-all" : "finance-limited", "chat", theme, "reloaded");
      const list = watch.requests.filter((r) => / GET \/api\/admin\/chat\/sessions(\?|$)/.test(r));
      if (list.length) problems.push(`${persona}: asked for the session list: ${list.join(" ; ")}`);
      const refused = watch.http.filter((h) => !h.startsWith("[sign-in]"));
      if (refused.length) problems.push(`${persona}: refused ${refused.join(" ; ")}`);
      const errors = watch.console.filter((c) => !c.startsWith("[sign-in]"));
      if (errors.length) problems.push(`${persona}: console ${errors.join(" ; ")}`);
      out[persona] = { landing, chatOpenedMs: chat.openedMs, answer: chat.answer.slice(-800),
        chatRequests: watch.requests.filter((r) => /^\[chat-/.test(r)).map((r) => `${r.split(" ")[0]} ${r.split(" ").slice(2).join(" ")}`), console: watch.console, http: watch.http };
      await page.context().close();
    }
    record(`RC finance chat ${theme}`, { ...out, problems });
    expect.soft(problems).toEqual([]);
  });
}

// ---------------------------------------------------------------- G6: one real AWS account
//
// A12 gate step G6: the Enterprise gate image on one real account
// (aws-changes.sh real-up with AWS_CHANGES_EDITION=enterprise; the quickstart
// config, a default-chain account, Cost Explorer off). E2E_A12_G4=a is the run
// without a key (and the lapse, booted again without it on the licensed
// data), b the run with one. They run only with AWS_CHANGES_MASK_FILE set, so
// the account ID is masked in every capture. Tests are named "G6 …"; with
// G4_CAPTURE_PREFIX=g6 the RA/RB/RC tests that hold on any data join them.

const G6_ON = Boolean(MASK_TEXT);
const g6Persona = () => (RUN === "b" ? "admin" : "operator");

// eslint-disable-next-line @typescript-eslint/no-explicit-any
async function getJson(page: Page, p: string): Promise<{ status: number; json: any; text: string }> {
  const r = await page.request.get(p);
  const text = await r.text();
  let json = null;
  try {
    json = JSON.parse(text);
  } catch {
    json = null;
  }
  return { status: r.status(), json, text: text.slice(0, 300) };
}

const itemKey = (i: Record<string, unknown>) => (i.id ? `id:${i.id}` : `${i.kind}|${i.account}|${i.region}|${i.event_name}|${i.time}|${i.actor}`);

/** A Changes listing read page by page, as Load more does: keys, repeats, totals. */
async function listing(page: Page, query: string) {
  const first = await getJson(page, `/api/admin/aws/changes?${query}`);
  const items: Array<Record<string, unknown>> = [...(first.json?.items ?? [])];
  let cursor = first.json?.next_cursor;
  let pages = 1;
  let error = first.status === 200 ? "" : `${first.status} ${first.text}`;
  while (cursor && pages < 150 && !error) {
    const next = await getJson(page, `/api/admin/aws/changes?${query}&cursor=${encodeURIComponent(cursor)}`);
    if (next.status !== 200) error = `page ${pages + 1}: ${next.status} ${next.text}`;
    items.push(...(next.json?.items ?? []));
    cursor = next.json?.next_cursor;
    pages++;
  }
  const seen = new Map<string, number>();
  for (const it of items) seen.set(itemKey(it), (seen.get(itemKey(it)) ?? 0) + 1);
  const repeats = [...seen.entries()].filter(([, n]) => n > 1).map(([k, n]) => `${k} x${n}`);
  const kinds: Record<string, number> = {};
  for (const it of items) kinds[String(it.kind)] = (kinds[String(it.kind)] ?? 0) + 1;
  return { status: first.status, error, pages, items: items.length, kinds, repeats, summary: first.json?.summary, window: first.json?.window,
    complete: first.json?.complete, sync: first.json?.sync, limits: first.json?.limits, regions: first.json?.regions, gaps: (first.json?.gaps ?? []).length };
}

/** The account ID's elements on screen (all masked in captures). */
async function maskedCount(page: Page): Promise<number> {
  return MASK_TEXT ? page.getByText(MASK_TEXT).count() : 0;
}

/** Lines that read as a warning or a failure. */
function alarmLines(text: string): string[] {
  return text.split("\n").map((l) => l.trim()).filter((l) => /denied|failed|error|expired|invalid|couldn't|could not|unable|not connected|disconnected|warning/i.test(l));
}

async function waitFirstRead(page: Page): Promise<Record<string, unknown>> {
  let last: Record<string, unknown> = {};
  for (let i = 0; i < 30; i++) {
    const r = await getJson(page, "/api/admin/aws/changes?window=24h&limit=1");
    last = { status: r.status, pending: r.json?.sync?.first_read_pending, label: r.json?.sync?.label };
    if (r.status === 200 && !r.json?.sync?.first_read_pending) return last;
    await page.waitForTimeout(5_000);
  }
  return last;
}

for (const theme of THEMES) {
  test(`G6 settings connected (${theme})`, async ({ browser }) => {
    test.skip(!G6_ON, "real-AWS run only (AWS_CHANGES_MASK_FILE)");
    test.setTimeout(150_000);
    const { page, watch } = await recheckPage(browser, theme);
    await signIn(page, g6Persona(), "/now", watch);
    watch.phase = "settings";
    await navTo(page, "Settings");
    await expect(page).toHaveURL(/\/aws\/settings/);
    await page.getByText(/Connected/).first().waitFor({ timeout: 30_000 }).catch(() => {});
    await settle(page);
    const api = await getJson(page, "/api/admin/aws/settings");
    const acct = api.json?.accounts?.[0] ?? {};
    const text = await mainText(page);
    const problems: string[] = [];
    if (api.status !== 200 || acct.status !== "connected") problems.push(`API account status ${api.status} ${acct.status} ${acct.error_code ?? ""}`);
    if (!/\bConnected\b/.test(text)) problems.push("Settings does not say Connected");
    problems.push(...rawCodes(text).map((c) => `raw "${c}" on Settings`));
    const alarms = alarmLines(text);
    if (alarms.length) problems.push(`warning or failure lines: ${JSON.stringify(alarms.slice(0, 8))}`);
    const notEnabled: string[] = acct.regions?.not_enabled ?? [];
    if (!notEnabled.includes("ap-southeast-7")) problems.push(`ap-southeast-7 (opt-in, off) not reported not enabled: ${JSON.stringify(acct.regions)}`);
    if (notEnabled.some((r) => r !== "ap-southeast-7")) problems.push(`an enabled Region reported not enabled: ${JSON.stringify(notEnabled)}`);
    const masked = await maskedCount(page);
    await rshot(page, g6Persona(), "settings", theme, "connected");
    record(`G6 settings ${theme}`, { status: acct.status, error_code: acct.error_code ?? null, regions: acct.regions, services: acct.services, sync: acct.sync,
      api_use: acct.api_use, masked, alarms, problems, console: watch.console, http: watch.http, text: text.slice(0, 2500) });
    expect.soft(problems).toEqual([]);
    expect.soft(watch.http.filter((h) => !/sign-in\]|\/enterprise\/api\/sso\/deployment|\/api\/agent\/baselines/.test(h)), "no 4xx/5xx").toEqual([]);
  });

  test(`G6 changes real data (${theme})`, async ({ browser }) => {
    test.skip(!G6_ON, "real-AWS run only (AWS_CHANGES_MASK_FILE)");
    test.setTimeout(300_000);
    const { page, watch } = await recheckPage(browser, theme);
    await signIn(page, g6Persona(), "/now", watch);
    watch.phase = "changes";
    await navTo(page, "Changes");
    await expect(page.locator("[data-summary-card]")).toBeVisible({ timeout: 30_000 });
    const firstRead = await waitFirstRead(page);
    await page.reload();
    await changesSettled(page);
    await page.getByRole("button", { name: "Clear all" }).first().click().catch(() => {});
    await page.locator('[data-summary-card][aria-busy="true"]').waitFor({ state: "detached", timeout: 30_000 }).catch(() => {});
    await settle(page);
    const problems: string[] = [];
    const card = (await page.locator("[data-summary-card]").innerText()).replace(/\s+/g, " ").slice(0, 400);
    const text = await mainText(page);
    problems.push(...rawCodes(text).map((c) => `raw "${c}" on Changes`));
    if (/Couldn't load/i.test(text)) problems.push("error panel on Changes");
    await rshot(page, g6Persona(), "changes", theme, "24h-every-service");
    // Load more until 200 rows (or the end): no row twice on screen.
    watch.phase = "load-more";
    for (let i = 0; i < 4; i++) {
      const more = page.getByRole("button", { name: /Load more/i });
      if (!(await more.isVisible().catch(() => false))) break;
      await more.click();
      await settle(page);
    }
    const rows = await page.locator("[data-item]").evaluateAll((els) => els.map((e) => e.getAttribute("data-item") ?? (e as HTMLElement).innerText.slice(0, 120)));
    const rowSeen = new Map<string, number>();
    for (const r of rows) rowSeen.set(r, (rowSeen.get(r) ?? 0) + 1);
    const rowRepeats = [...rowSeen.entries()].filter(([, n]) => n > 1).map(([k, n]) => `${k} x${n}`);
    if (rows.length === 0) problems.push("no rows listed");
    if (rowRepeats.length) problems.push(`rows on screen twice: ${JSON.stringify(rowRepeats.slice(0, 6))}`);
    await rshot(page, g6Persona(), "changes", theme, "24h-load-more");
    // Expand the first row.
    await page.locator("[data-item]").first().click().catch(() => {});
    await page.waitForTimeout(500);
    await rshot(page, g6Persona(), "changes", theme, "24h-row-open");
    // The API listing, page by page: each item once; the card is the answer's total.
    const day = await listing(page, "window=24h&limit=200");
    const cardTotal = Number((/Changes\s*([\d,]+)/.exec(card)?.[1] ?? "-1").replace(/,/g, ""));
    if (day.error) problems.push(`24 h listing: ${day.error}`);
    if (day.repeats.length) problems.push(`24 h listing repeats: ${JSON.stringify(day.repeats.slice(0, 6))}`);
    // The card and the API are read seconds apart on a live account: equal, or the API newer by a few.
    if (Math.abs(cardTotal - (day.summary?.total ?? -1)) > 25) problems.push(`card ${cardTotal} vs API total ${day.summary?.total}`);
    let week = null;
    if (RUN === "b") {
      week = await listing(page, "window=7d&limit=200");
      if (week.error) problems.push(`7 d listing: ${week.error}`);
      if (week.repeats.length) problems.push(`7 d listing repeats: ${JSON.stringify(week.repeats.slice(0, 6))}`);
      if ((week.summary?.total ?? 0) < (day.summary?.total ?? 0)) problems.push(`7 d total ${week.summary?.total} below 24 h total ${day.summary?.total}`);
    }
    const masked = await maskedCount(page);
    record(`G6 changes ${theme}`, { firstRead, card, cardTotal, rowsOnScreen: rows.length, rowRepeats, day, week, masked, problems, console: watch.console, http: watch.http });
    expect.soft(problems).toEqual([]);
    expect.soft(watch.http.filter((h) => !/sign-in\]|\/enterprise\/api\/sso\/deployment|\/api\/agent\/baselines|window=7d/.test(h)), "no 4xx/5xx").toEqual([]);
  });

  test(`G6 finops real data (${theme})`, async ({ browser }) => {
    test.skip(!G6_ON, "real-AWS run only (AWS_CHANGES_MASK_FILE)");
    test.setTimeout(150_000);
    const { page, watch } = await recheckPage(browser, theme);
    await signIn(page, g6Persona(), "/now", watch);
    watch.phase = "finops";
    await navTo(page, "FinOps");
    await expect(page).toHaveURL(/\/aws\/finops/);
    await settle(page);
    await page.getByText(/Month to date|This month|to date|Spend|Forecast/i).first().waitFor({ timeout: 30_000 }).catch(() => {});
    await settle(page);
    const text = await mainText(page);
    const problems: string[] = rawCodes(text).map((c) => `raw "${c}" on FinOps`);
    if (/Couldn't load/i.test(text)) problems.push("error panel on FinOps");
    const apis: Record<string, unknown> = {};
    for (const p of ["/api/admin/aws/finops/summary", "/api/admin/aws/finops/accounts"]) {
      const r = await getJson(page, p);
      apis[p] = { status: r.status, keys: r.json && typeof r.json === "object" ? Object.keys(r.json).slice(0, 20) : null, total_micros: r.json?.total_micros ?? null,
        accounts: (r.json?.accounts ?? []).length };
      if (r.status !== 200) problems.push(`${p} answers ${r.status}`);
    }
    const masked = await maskedCount(page);
    await rshot(page, g6Persona(), "finops", theme, "populated");
    record(`G6 finops ${theme}`, { apis, masked, problems, console: watch.console, http: watch.http, text: text.slice(0, 2500) });
    expect.soft(problems).toEqual([]);
    expect.soft(watch.http.filter((h) => !/sign-in\]|\/enterprise\/api\/sso\/deployment|\/api\/agent\/baselines/.test(h)), "no 4xx/5xx").toEqual([]);
  });
}

// Run B: viewing Changes writes audit rows (one per Changes read the page sends).
test("G6 audit rows for viewing Changes (dark)", async ({ browser }) => {
  test.skip(!G6_ON || RUN !== "b", "real-AWS Run B only");
  test.setTimeout(150_000);
  const { page, watch } = await recheckPage(browser, "dark");
  await signIn(page, "admin", "/now", watch);
  const org = (await getJson(page, "/enterprise/api/sso/deployment")).json?.org ?? "";
  const audit = async () => {
    const r = await getJson(page, `/enterprise/api/audit/${org}?action=aws.changes.viewed&limit=500`);
    const list = Array.isArray(r.json) ? r.json : r.json?.entries ?? r.json?.events ?? r.json?.items ?? [];
    return { status: r.status, list: list as Array<Record<string, unknown>> };
  };
  const before = await audit();
  watch.phase = "view";
  await page.goto("/aws/changes");
  await changesSettled(page);
  await page.getByRole("group", { name: "Window" }).getByRole("button", { name: /^1h/ }).click();
  await changesSettled(page);
  await page.waitForTimeout(1500);
  const reads = watch.requests.filter((r) => r.startsWith("[view]") && / GET \/api\/admin\/aws\/changes\?/.test(r) && !/cursor=/.test(r)).length;
  const after = await audit();
  const added = after.list.length - before.list.length;
  const fresh = after.list.slice(0, Math.max(0, added)).map((e) => ({ actor: e.actor ?? e.Actor, result: e.result ?? e.Result, target: e.target ?? e.Target, at: e.at ?? e.time ?? e.created_at }));
  const problems: string[] = [];
  if (before.status !== 200 || after.status !== 200) problems.push(`audit API ${before.status}/${after.status}`);
  if (added < 1) problems.push(`no audit row for viewing Changes (page sent ${reads} reads)`);
  if (fresh.some((e) => /denied/i.test(String(e.result)))) problems.push(`a viewing row reads denied: ${JSON.stringify(fresh.slice(0, 3))}`);
  record("G6 audit", { org, before: before.list.length, after: after.list.length, added, reads, fresh: fresh.slice(0, 8), problems });
  expect.soft(problems).toEqual([]);
});

// Run B: Compare past 24 h. The tier allows it; the inventory history of a
// fresh store starts at Run A's boot, so the answer names that start.
test("G6 compare past 24 h within history (dark)", async ({ browser }) => {
  test.skip(!G6_ON || RUN !== "b", "real-AWS Run B only");
  test.setTimeout(150_000);
  const { page, watch } = await recheckPage(browser, "dark");
  await signIn(page, "admin", "/aws/changes", watch);
  await changesSettled(page);
  const problems: string[] = [];
  const probes: Record<string, unknown> = {};
  for (const hours of [25, 30, 72]) {
    const r = await getJson(page, `/api/admin/aws/changes/compare?from=${encodeURIComponent(iso(hours * H))}`);
    probes[`${hours}h`] = { status: r.status, compare_status: r.json?.status, history_starts: r.json?.history_starts, error: r.json?.error, limits: r.json?.limits };
    if (r.status !== 200) problems.push(`compare from ${hours} h ago answers ${r.status} ${r.json?.error ?? r.text}`);
  }
  const starts = (probes["30h"] as Record<string, unknown>).history_starts as string | undefined;
  const startMs = starts ? Date.parse(starts) : NaN;
  const reach = Number.isFinite(startMs) ? (Date.now() - startMs) / H : null;
  let within: Record<string, unknown> | null = null;
  if (Number.isFinite(startMs)) {
    const from = new Date(startMs + 2 * 60_000).toISOString().replace(/\.\d{3}Z$/, "Z");
    const r = await getJson(page, `/api/admin/aws/changes/compare?from=${encodeURIComponent(from)}`);
    within = { from, status: r.status, compare_status: r.json?.status, added: (r.json?.added ?? []).length, removed: (r.json?.removed ?? []).length,
      changed: (r.json?.changed ?? []).length, changes_between_total: r.json?.changes_between_total, partial: (r.json?.partial ?? []).length };
    if (r.status !== 200 || r.json?.status === "unavailable") problems.push(`compare from the history start answers ${JSON.stringify(within)}`);
    watch.phase = "ui-from-history";
    await page.goto(`/aws/changes?from=${encodeURIComponent(from)}`);
    await changesSettled(page);
    await page.locator('[data-panel="compare"] .sk').first().waitFor({ state: "detached", timeout: 20_000 }).catch(() => {});
    await settle(page);
    const panel = (await page.locator('[data-panel="compare"]').innerText().catch(() => "")).replace(/\s+/g, " ").slice(0, 500);
    within.panel = panel;
    if (/Couldn't load|Enterprise/i.test(panel)) problems.push(`compare panel from the history start: "${panel}"`);
    await rshot(page, "admin", "changes-compare", "dark", "from-history-start");
  }
  watch.phase = "ui-30h";
  await page.goto(`/aws/changes?from=${encodeURIComponent(iso(30 * H))}`);
  await changesSettled(page);
  await page.locator('[data-panel="compare"] .sk').first().waitFor({ state: "detached", timeout: 20_000 }).catch(() => {});
  await settle(page);
  const panel30 = (await page.locator('[data-panel="compare"]').innerText().catch(() => "")).replace(/\s+/g, " ").slice(0, 500);
  if (await page.locator('[data-compare="locked"]').isVisible().catch(() => false)) problems.push(`compare from 30 h ago shows the lock: "${panel30}"`);
  await rshot(page, "admin", "changes-compare", "dark", "from-30h");
  const comparePresets = await listOptions(page, page.getByRole("button", { name: "Compare from" }));
  record("G6 compare", { probes, historyReachHours: reach, within, panel30, comparePresets, problems, console: watch.console, http: watch.http });
  expect.soft(problems).toEqual([]);
});
