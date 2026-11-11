// S10 — AWS Settings visual/browser acceptance (plans/status/aws-harness/a2-settings.md
// §5.3, §5.4, §6 "Browser e2e + screenshots").
//
// The browser drives the real SPA and signs in against the running instance like
// an operator. The AWS HTTP handlers are answered with page.route fixtures
// because the real handlers are covered by the aws-settings integration run;
// everything asserted here is rendered UI, never a mocked component.
//
// Captures use the shared capturePath helper (HARNESS_IMAGES_DIR through
// `run.sh exec`), named <surface>-<viewport>-<theme>-<state>.png. This file is
// fixture evidence, not live-AWS proof.
import { expect, test, type Browser, type BrowserContext, type Locator, type Page, type Route } from "@playwright/test";
import { openApp } from "./helpers";
import { capturePath } from "./support/capture";

// ---------------------------------------------------------------------------
// Viewports, themes and the fixture vocabulary.
// ---------------------------------------------------------------------------

const VIEWPORTS = [
  { name: "desktop", width: 1440, height: 900 },
  { name: "mobile", width: 390, height: 844 },
] as const;
const THEMES = ["dark", "light"] as const;
type Theme = (typeof THEMES)[number];
type Viewport = (typeof VIEWPORTS)[number];

const PRINCIPAL_ARN = "arn:aws:iam::999900001111:role/versus-server";
const WIZARD_ROLE_ARN = "arn:aws:iam::123456789012:role/versus-reader";

const contexts: BrowserContext[] = [];
test.afterEach(async () => {
  for (const context of contexts.splice(0)) await context.close().catch(() => {});
});

function secondsAgo(seconds: number): string {
  return new Date(Date.now() - seconds * 1000).toISOString();
}

function atLocalTime(hour: number, minute: number): string {
  const d = new Date();
  d.setHours(hour, minute, 0, 0);
  return d.toISOString();
}

// ---------------------------------------------------------------------------
// Fixtures — the product's own scenarios: three accounts, Staging denying
// ElastiCache, one service not on plan, one probe throttled.
// ---------------------------------------------------------------------------

function account(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    account_id: "111122223333",
    display_name: "Production",
    source: "wizard",
    status: "connected",
    checked_at: secondsAgo(12),
    disconnected_since: null,
    error_code: null,
    services: { total: 24, readable: 23, not_used: 1, not_checked: 0 },
    regions: { selected: ["us-east-1", "eu-west-1"], enabled: 2, not_enabled: [] },
    api_use: { used_rps: 3, budget_rps: 4 },
    role_arn: "arn:aws:iam::111122223333:role/versus-reader",
    denied_services: [],
    not_on_plan_services: [],
    ...overrides,
  };
}

function staging(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return account({
    account_id: "444455556666",
    display_name: "Staging",
    checked_at: secondsAgo(15),
    services: { total: 24, readable: 19, not_used: 3, not_checked: 0 },
    regions: {
      selected: ["us-east-1", "eu-west-1", "ap-southeast-1"],
      enabled: 2,
      not_enabled: ["ap-southeast-1"],
    },
    role_arn: "arn:aws:iam::444455556666:role/versus-reader",
    denied_services: ["elasticache"],
    not_on_plan_services: ["aws-health"],
    ...overrides,
  });
}

function settings(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  const accounts: Array<Record<string, unknown>> = (overrides.accounts as Array<Record<string, unknown>> | undefined) ?? [
    account(),
    staging(),
    account({
      account_id: "777788889999",
      display_name: "Shared services",
      checked_at: secondsAgo(9),
      regions: { selected: ["us-east-1", "eu-west-1", "ap-southeast-1"], enabled: 3, not_enabled: [] },
    }),
  ];
  return {
    enabled: true,
    principal_arn: PRINCIPAL_ARN,
    accounts,
    optional_data: {
      cost_explorer: { on: accounts.length, source: "config" },
      config: { on: 2, source: "config" },
      security_hub: { on: 1, source: "config" },
      trusted_advisor: { on: 0, source: "wizard" },
    },
    privacy: {
      sent: ["resource_names_types_counts", "alarm_names_states", "allowlisted_tag_values"],
      never_sent: ["secrets_env_user_data", "log_lines_request_bodies", "other_tag_values"],
      tag_value_allowlist: ["app", "team", "env", "owner", "cost-center"],
      tag_value_allowlist_source: "config",
      ai: { enabled: true, provider: "claude", model: "sonnet-4" },
      self_test: { passing: true, last_run_at: secondsAgo(3600), leaks_24h: 0 },
    },
    capabilities: { organizations: false, can_write: true },
    budget: { used_rps: 1.5, budget_rps: 2, source: "config" },
    ...overrides,
  };
}

function draft(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    draft_id: "draft-1",
    method: "cloudformation",
    external_id_masked: "vrs-ext-••••••••••••7f3a",
    principal_arn: PRINCIPAL_ARN,
    template_url: "https://downloads.example.com/versus-reader.yaml",
    terraform_snippet: 'module "versus_reader" {\n  source = "./versus-reader"\n}',
    role_arn: "",
    assume: null,
    services: [],
    fix_policy: null,
    probe_id: null,
    ...overrides,
  };
}

// The 24-service catalog, in grid order. The settled state matches the design:
// 19 readable · 3 not used · 1 denied · 1 not on plan.
const PARTIAL_SERVICES = [
  { service: "ec2", status: "readable" },
  { service: "lambda", status: "readable" },
  { service: "kinesis", status: "not_used" },
  { service: "elasticache", status: "denied", fix_actions: ["elasticache:Describe*"] },
  { service: "aws-health", status: "not_on_plan" },
  { service: "ecs", status: "readable" },
];

const SETTLED_SERVICES = [
  ...PARTIAL_SERVICES,
  { service: "api-gateway", status: "readable" },
  { service: "auto-scaling", status: "readable" },
  { service: "cloudformation", status: "readable" },
  { service: "cloudfront", status: "readable" },
  { service: "cloudtrail", status: "readable" },
  { service: "cloudwatch", status: "readable" },
  { service: "config", status: "readable" },
  { service: "dynamodb", status: "readable" },
  { service: "eks", status: "readable" },
  { service: "elbv2", status: "readable" },
  { service: "eventbridge", status: "readable" },
  { service: "iam", status: "readable" },
  { service: "rds", status: "readable" },
  { service: "route53", status: "readable" },
  { service: "s3", status: "not_used" },
  { service: "sns", status: "readable" },
  { service: "sqs", status: "not_used" },
  { service: "step-functions", status: "readable" },
];

const THROTTLED_SERVICES = SETTLED_SERVICES.map((service) =>
  service.service === "ecs"
    ? { service: "ecs", status: "not_checked", error_code: "throttled", retryable: true }
    : service,
);

function regions(): Array<Record<string, unknown>> {
  return [
    { region: "us-east-1", enabled: true, opt_in_status: "opt-in-not-required", resources_found: 241 },
    { region: "eu-west-1", enabled: true, opt_in_status: "opt-in-not-required", resources_found: 52 },
    { region: "ap-southeast-1", enabled: false, opt_in_status: "not-opted-in", resources_found: 0 },
  ];
}

interface Fixture {
  settings: Record<string, unknown>;
  /** Returned by POST /connect/drafts (method is read from the body). */
  draftBase?: Record<string, unknown>;
  /** GET draft before verify (assume not started). */
  draftBeforeVerify?: Record<string, unknown>;
  /** First GET draft after verify — the progressive state. */
  draftAfterVerify?: Record<string, unknown>;
  /** Every later GET draft — the settled state. */
  draftSettled?: Record<string, unknown>;
  regions?: Array<Record<string, unknown>>;
  /** 4xx on verify, exercising the panel-level mapped error. */
  verifyError?: { status: number; code: string };
  /** The account POST /accounts creates. */
  accountToCreate?: Record<string, unknown>;
}

interface Recorder {
  writes: Array<{ method: string; path: string; body?: unknown }>;
}

async function json(route: Route, body: unknown, status = 200) {
  await route.fulfill({
    status,
    contentType: "application/json",
    body: JSON.stringify(body),
  });
}

// installAws answers only the AWS admin API. Auth, the deployment probe and
// every other endpoint still hit the running instance.
async function installAws(page: Page, fixture: Fixture): Promise<Recorder> {
  const recorder: Recorder = { writes: [] };
  let current = structuredClone(fixture.settings);
  let verified = false;
  let servedAfterVerify = 0;
  let drafts = 0;
  const base = fixture.draftBase ?? draft();

  await page.route(
    (url) => url.pathname.startsWith("/api/admin/aws"),
    async (route) => {
      const request = route.request();
      const url = new URL(request.url());
      const method = request.method();
      const path = url.pathname;
      // Shape of the wizard write bodies the stub reads (draft method, account name/regions).
      let body: { method?: unknown; display_name?: unknown; regions?: string[] } | undefined;
      try {
        body = request.postData() ? JSON.parse(request.postData() as string) : undefined;
      } catch {
        body = undefined;
      }
      if (method !== "GET") recorder.writes.push({ method, path, body });

      if (path === "/api/admin/aws/settings" && method === "GET") {
        return json(route, current);
      }
      if (path === "/api/admin/aws/connect/drafts" && method === "POST") {
        drafts += 1;
        // Each draft has its own assume/probe lifecycle.
        verified = false;
        servedAfterVerify = 0;
        return json(route, { ...base, method: body?.method ?? base.method, draft_id: `draft-${drafts}` }, 201);
      }
      if (/^\/api\/admin\/aws\/connect\/drafts\/[^/]+\/verify$/.test(path) && method === "POST") {
        if (fixture.verifyError) {
          return json(route, { error: fixture.verifyError.code }, fixture.verifyError.status);
        }
        verified = true;
        servedAfterVerify = 0;
        return json(route, { probe_id: "probe-1" });
      }
      if (/^\/api\/admin\/aws\/connect\/drafts\/[^/]+\/external-id$/.test(path) && method === "GET") {
        return json(route, { external_id: "vrs-ext-7f3a91c2d4e6" });
      }
      if (/^\/api\/admin\/aws\/connect\/drafts\/[^/]+$/.test(path) && method === "GET") {
        if (!verified) return json(route, fixture.draftBeforeVerify ?? draft({ draft_id: base.draft_id }));
        // Serve the progressive state for two polls so a slow assertion cannot
        // race the 1 s refetch into the settled state.
        if (servedAfterVerify < 2) {
          servedAfterVerify += 1;
          return json(route, fixture.draftAfterVerify ?? draft({ draft_id: base.draft_id }));
        }
        return json(
          route,
          fixture.draftSettled ?? fixture.draftAfterVerify ?? draft({ draft_id: base.draft_id }),
        );
      }
      if (/^\/api\/admin\/aws\/connect\/drafts\/[^/]+\/regions$/.test(path) && method === "POST") {
        return json(route, fixture.regions ?? regions());
      }
      if (path === "/api/admin/aws/accounts" && method === "POST") {
        const created = fixture.accountToCreate ?? account({
          account_id: "123456789012",
          display_name: body?.display_name ?? "AWS account 123456789012",
          regions: { selected: body?.regions ?? [], enabled: (body?.regions ?? []).length, not_enabled: [] },
        });
        current = { ...current, accounts: [...(current.accounts as Array<Record<string, unknown>>), created] };
        return json(route, created, 201);
      }
      if (/^\/api\/admin\/aws\/accounts\/[^/]+\/recheck$/.test(path) && method === "POST") {
        return route.fulfill({ status: 204, body: "" });
      }
      return json(route, { error: "not found" }, 404);
    },
  );
  return recorder;
}

// ---------------------------------------------------------------------------
// Page helpers.
// ---------------------------------------------------------------------------

async function openFixture(
  browser: Browser,
  viewport: Viewport,
  theme: Theme,
  fixture: Fixture,
): Promise<{ page: Page; recorder: Recorder }> {
  const context = await browser.newContext({
    viewport: { width: viewport.width, height: viewport.height },
    colorScheme: theme,
  });
  contexts.push(context);
  await context.addInitScript((value) => {
    localStorage.setItem("versus.theme", value);
  }, theme);
  const page = await context.newPage();
  await page.emulateMedia({ colorScheme: theme });
  // Sign in against the real instance first; the fixture replaces only the AWS API.
  await openApp(page, "/now");
  const recorder = await installAws(page, fixture);
  await page.goto("/aws/settings");
  await expect(page.locator('[role="status"][data-answer-kind]')).toBeVisible();
  const htmlTheme = await page.locator("html").getAttribute("data-theme");
  if (theme === "light") {
    expect(htmlTheme, "light theme must set data-theme=light").toBe("light");
  } else {
    expect(htmlTheme, "dark is the default and must not set data-theme").not.toBe("light");
  }
  return { page, recorder };
}

async function capture(
  page: Page,
  viewport: Viewport,
  theme: Theme,
  state: string,
  fullPage = false,
): Promise<string> {
  const out = capturePath("aws-settings", viewport.width, theme, state);
  await page.screenshot({ path: out, animations: "disabled", fullPage });
  return out;
}

function accountsCard(page: Page): Locator {
  return page.locator('section[aria-labelledby="aws-accounts-title"]');
}

function privacyCard(page: Page): Locator {
  return page.locator('section[aria-labelledby="aws-privacy-title"]');
}

function connectPanel(page: Page): Locator {
  return page.locator('section[aria-labelledby="aws-connect-title"]');
}

async function expectNoPageOverflow(page: Page, findings: string[]) {
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - window.innerWidth,
  );
  if (overflow > 1) findings.push(`page overflows horizontally by ${overflow}px`);
}

async function expectNoCardOverflow(page: Page, findings: string[]) {
  const result = await page.evaluate(() =>
    Array.from(
      document.querySelectorAll<HTMLElement>('section.card, details.card, section[aria-labelledby="aws-connect-title"]'),
    )
      .map((el) => ({
        name: el.getAttribute("aria-labelledby") ?? el.querySelector("summary")?.textContent?.trim() ?? el.className,
        overflow: el.scrollWidth - el.clientWidth,
      }))
      .filter((r) => r.overflow > 1),
  );
  for (const row of result) findings.push(`card "${row.name}" overflows by ${row.overflow}px`);
}

async function clippedText(page: Page, findings: string[]) {
  const clipped = await page.evaluate(() =>
    Array.from(document.querySelectorAll<HTMLElement>(".truncate"))
      .filter((el) => el.offsetParent !== null && el.scrollWidth - el.clientWidth > 1)
      .map((el) => ({
        text: (el.textContent ?? "").trim().slice(0, 80),
        overflow: el.scrollWidth - el.clientWidth,
      })),
  );
  for (const row of clipped) findings.push(`truncated copy "${row.text}" is clipped by ${row.overflow}px`);
}

async function cardSpacing(page: Page, findings: string[]) {
  const boxes = await page.evaluate(() => {
    const pick = (el: Element | null) => {
      if (!el) return null;
      const r = el.getBoundingClientRect();
      return { top: r.top, bottom: r.bottom, left: r.left, right: r.right, width: r.width };
    };
    return {
      accounts: pick(document.querySelector('section[aria-labelledby="aws-accounts-title"]')),
      privacy: pick(document.querySelector('section[aria-labelledby="aws-privacy-title"]')),
      advanced: pick(document.querySelector("details.card")),
    };
  });
  if (boxes.accounts && boxes.privacy) {
    const gap = Math.round(boxes.privacy.top - boxes.accounts.bottom);
    if (gap !== 16) findings.push(`Accounts→Privacy gap is ${gap}px, expected 16px`);
  }
  if (boxes.privacy && boxes.advanced) {
    const gap = Math.round(boxes.advanced.top - boxes.privacy.bottom);
    if (gap !== 16) findings.push(`Privacy→Advanced gap is ${gap}px, expected 16px`);
  }
}

async function gridAlignment(page: Page, findings: string[]) {
  if (page.viewportSize()?.width === 390) return;
  const measurable = await page.locator(
    'section[aria-labelledby="aws-accounts-title"] ul[aria-label="AWS accounts"] > li',
  ).count();
  if (measurable === 0) return;
  const deltas = await page.evaluate(() => {
    const card = document.querySelector('section[aria-labelledby="aws-accounts-title"]');
    if (!card) return null;
    const head = card.querySelector('div[aria-hidden="true"]');
    const row = card.querySelector('ul[aria-label="AWS accounts"] > li');
    if (!head || !row) return null;
    const headCells = Array.from(head.children).map((el) => el.getBoundingClientRect().left);
    const rowCells = Array.from(row.querySelectorAll(':scope > div.grid > div')).map(
      (el) => el.getBoundingClientRect().left,
    );
    return headCells.map((x, i) => Math.round(x - (rowCells[i] ?? x)));
  });
  if (!deltas) {
    findings.push("accounts header/row grid could not be measured");
    return;
  }
  deltas.forEach((delta, i) => {
    if (Math.abs(delta) > 1) findings.push(`account column ${i + 1} is misaligned by ${delta}px`);
  });
}

// Effective background by walking ancestors and compositing translucent layers.
const CONTRAST_SCRIPT = `(() => {
  const parse = (value) => {
    const m = value.match(/rgba?\\(([^)]+)\\)/);
    if (!m) return null;
    const parts = m[1].split(",").map((v) => parseFloat(v.trim()));
    return { r: parts[0], g: parts[1], b: parts[2], a: parts[3] === undefined ? 1 : parts[3] };
  };
  const blend = (top, bottom) => ({
    r: Math.round(top.r * top.a + bottom.r * (1 - top.a)),
    g: Math.round(top.g * top.a + bottom.g * (1 - top.a)),
    b: Math.round(top.b * top.a + bottom.b * (1 - top.a)),
    a: 1,
  });
  const lum = (c) => {
    const f = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); };
    return 0.2126 * f(c.r) + 0.7152 * f(c.g) + 0.0722 * f(c.b);
  };
  const ratio = (a, b) => {
    const l1 = lum(a), l2 = lum(b);
    return (Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05);
  };
  const background = (el) => {
    const chain = [];
    let node = el;
    while (node && node instanceof Element) {
      const bg = parse(getComputedStyle(node).backgroundColor);
      if (bg && bg.a > 0) {
        chain.push(bg);
        if (bg.a >= 0.999) break;
      }
      node = node.parentElement;
    }
    if (chain.length === 0) return { r: 255, g: 255, b: 255, a: 1 };
    let out = chain[chain.length - 1];
    for (let i = chain.length - 2; i >= 0; i--) out = blend(chain[i], out);
    return out;
  };
  const samples = [];
  const push = (name, el) => {
    if (!el) return;
    const style = getComputedStyle(el);
    const fg = parse(style.color);
    if (!fg) return;
    const bg = background(el);
    const size = parseFloat(style.fontSize);
    const weight = parseInt(style.fontWeight, 10) || 400;
    const large = size >= 24 || (size >= 18.66 && weight >= 700);
    samples.push({
      name,
      text: (el.textContent ?? "").trim().slice(0, 48),
      ratio: Math.round(ratio(fg, bg) * 100) / 100,
      need: large ? 3 : 4.5,
    });
  };
  push("answer line", document.querySelector('[role="status"][data-answer-kind]'));
  push("card title", document.querySelector("#aws-accounts-title"));
  push("account name", document.querySelector('#aws-accounts-title')?.closest("section")?.querySelector("li span.font-medium"));
  push("secondary text", document.querySelector('#aws-accounts-title')?.closest("section")?.querySelector("p.font-mono"));
  push("status word", document.querySelector('#aws-accounts-title')?.closest("section")?.querySelector("li .text-sev-ok, li .text-sev-critical"));
  push("primary button", document.querySelector(".btn-primary"));
  push("legend", document.querySelector("#aws-connect-title")?.closest("section")?.querySelector("p.text-2xs"));
  push("grid status", document.querySelector('ul[aria-label="Service access"] li span.shrink-0.text-2xs'));
  return samples;
})()`;

async function contrast(page: Page, findings: string[], label: string) {
  const samples: Array<{ name: string; text: string; ratio: number; need: number }> =
    await page.evaluate(CONTRAST_SCRIPT);
  for (const sample of samples) {
    if (sample.ratio < sample.need) {
      findings.push(
        `${label}: "${sample.name}" (${sample.text}) is ${sample.ratio}:1, needs ${sample.need}:1`,
      );
    }
  }
}

async function measureLayout(page: Page, findings: string[], label: string) {
  await expectNoPageOverflow(page, findings);
  await expectNoCardOverflow(page, findings);
  await clippedText(page, findings);
  await cardSpacing(page, findings);
  await gridAlignment(page, findings);
  await contrast(page, findings, label);
}

async function tabUntil(page: Page, predicate: () => Promise<boolean>, max = 80): Promise<boolean> {
  for (let i = 0; i < max; i++) {
    await page.keyboard.press("Tab");
    if (await predicate()) return true;
  }
  return false;
}

// ---------------------------------------------------------------------------
// 1 + 2 + 4 — default screen, connect wizard, accessibility. Every viewport and
// theme.
// ---------------------------------------------------------------------------

for (const viewport of VIEWPORTS) {
  for (const theme of THEMES) {
    test(`AWS settings default + connect wizard (${viewport.name}, ${theme})`, async ({ browser }) => {
      const findings: string[] = [];
      const { page } = await openFixture(browser, viewport, theme, {
        settings: settings(),
        draftBase: draft(),
        draftBeforeVerify: draft(),
        draftAfterVerify: draft({
          assume: { ok: true, latency_ms: 212 },
          services: PARTIAL_SERVICES,
        }),
        draftSettled: draft({
          assume: { ok: true, latency_ms: 212 },
          services: SETTLED_SERVICES,
        }),
        regions: regions(),
        accountToCreate: account({
          account_id: "123456789012",
          display_name: "Payments",
          regions: { selected: ["us-east-1", "eu-west-1"], enabled: 2, not_enabled: [] },
        }),
      });

      // ---- 1. Default: answer line, Accounts, Privacy, Advanced collapsed.
      const answer = page.locator('[role="status"][data-answer-kind]');
      await expect(answer).toHaveAttribute("data-answer-kind", "denied");
      await expect(answer).toContainText("3 accounts connected");
      await expect(answer).toContainText("Staging is missing ElastiCache access");
      await expect(answer).toContainText("cost data on");

      const accounts = accountsCard(page);
      await expect(accounts.locator('ul[aria-label="AWS accounts"] > li')).toHaveCount(3);
      await expect(accounts).toContainText("Production");
      await expect(accounts).toContainText("111122223333");
      await expect(accounts).toContainText("Staging");
      await expect(accounts).toContainText("Shared services");
      await expect(accounts).toContainText("23 of 24 readable");
      await expect(accounts).toContainText("19 of 24 readable");
      await expect(accounts.getByText("ElastiCache: Denied", { exact: true })).toHaveCount(1);
      await expect(accounts).toContainText("2 of 3");
      await expect(accounts).toContainText("1 not enabled");
      await expect(accounts).toContainText("3 of 4 req/s");
      await expect(accounts).toContainText(
        "Optional data: Cost on in all accounts, Config in 2, Security Hub in 1",
      );

      const privacy = privacyCard(page);
      await expect(privacy).toContainText("Sent to AI");
      await expect(privacy).toContainText("Never sent");
      await expect(privacy).toContainText("Resource names, types and counts");
      await expect(privacy).toContainText("Secrets, env vars and user data");
      await expect(privacy.getByRole("list", { name: "Tag value allowlist" }).locator("li")).toHaveCount(5);
      await expect(privacy).toContainText("Leak check: passing");

      const advanced = page.locator("details.card");
      expect(await advanced.evaluate((el) => (el as HTMLDetailsElement).open)).toBe(false);
      await expect(advanced.locator("summary")).toContainText("Budget, optional data, tag allowlist");
      await expect(advanced.locator(".card-body")).toBeHidden();

      // Accessibility: the answer line is announced, not just coloured.
      await expect(answer).toHaveAttribute("aria-live", "polite");
      // Account status is a word + icon, never colour alone.
      const production = accounts.getByRole("listitem", { name: "Production 111122223333" });
      await expect(production).toContainText("Connected");
      const statusIcon = production.locator("svg").first();
      await expect(statusIcon).toHaveAttribute("aria-hidden", "true");

      await measureLayout(page, findings, `${viewport.name}/${theme}/default`);
      await capture(page, viewport, theme, "default", true);
      if (viewport.width === 390) {
        await page.locator("main").evaluate((el) => {
          el.scrollTop = el.scrollHeight;
        });
        await capture(page, viewport, theme, "default-privacy");
        await page.locator("main").evaluate((el) => {
          el.scrollTop = 0;
        });
      }

      // ---- 2. Connect wizard, step 1: methods; Organizations locked.
      await page.getByRole("button", { name: "Add account" }).click();
      const panel = connectPanel(page);
      await expect(panel).toBeVisible();
      await expect(panel.getByRole("radio")).toHaveCount(5);
      await expect(panel.getByRole("radio", { name: /CloudFormation quick-create/ })).toBeChecked();
      await expect(panel.getByRole("radio", { name: /Terraform/ })).toBeVisible();
      await expect(panel.getByRole("radio", { name: /Existing role ARN/ })).toBeVisible();
      await expect(panel.getByRole("radio", { name: /Local profile/ })).toBeVisible();
      await expect(panel.getByRole("radio", { name: /AWS Organizations/ })).toBeVisible();
      await expect(
        panel.locator("label").filter({ hasText: "AWS Organizations" }),
      ).toContainText("Enterprise");
      await expect(panel.getByRole("textbox", { name: "Versus principal ARN" })).toHaveValue(PRINCIPAL_ARN);
      await expect(panel.getByRole("link", { name: /Open CloudFormation quick-create/ })).toBeVisible();
      await expect(panel.getByRole("link", { name: /Download template/ })).toBeVisible();
      await expect(panel.getByRole("list", { name: "Connect account progress" })).toBeVisible();

      // Organizations is locked in OSS: selecting it explains the reason and
      // blocks Continue rather than offering a dead action.
      await panel.getByRole("radio", { name: /AWS Organizations/ }).check();
      await expect(page.getByTestId("enterprise-locked")).toBeVisible();
      await expect(page.getByText("AWS Organizations is an Enterprise feature")).toBeVisible();
      await expect(panel.getByRole("button", { name: "Continue", exact: true })).toBeDisabled();
      await panel.getByRole("radio", { name: /CloudFormation quick-create/ }).check();
      await expect(page.getByTestId("enterprise-locked")).toHaveCount(0);

      await capture(page, viewport, theme, "wizard-step1");
      await measureLayout(page, findings, `${viewport.name}/${theme}/step1`);

      // Step 2: role ARN, progressive grid, one denied service + fix box.
      await panel.getByRole("button", { name: "Continue", exact: true }).click();
      await expect(panel.locator("#aws-role-arn")).toBeVisible();
      await panel.locator("#aws-role-arn").fill(WIZARD_ROLE_ARN);
      await expect(panel.getByRole("button", { name: "Reveal External ID" })).toBeVisible();
      await panel.getByRole("button", { name: "Check access" }).click();

      await expect(panel).toContainText("Role assumed in 212 ms");
      await expect(panel).toContainText("18 of 24 services still checking.");
      const progress = panel.getByTestId("access-progress");
      await expect(progress).toHaveAttribute("role", "status");
      await expect(progress).toHaveAttribute("aria-live", "polite");
      const grid = panel.getByRole("list", { name: "Service access" });
      await expect(grid.getByRole("listitem", { name: "ElastiCache: Denied" })).toBeVisible();
      // Status is a word + icon: the icon only repeats the word.
      await expect(
        grid.getByRole("listitem", { name: "ElastiCache: Denied" }).locator("svg").first(),
      ).toHaveAttribute("aria-hidden", "true");

      // Settled: 19 readable · 3 not used · 1 denied · 1 not on plan.
      await expect(panel).toContainText("19 readable · 3 not used · 1 denied · 1 not on plan", {
        timeout: 15_000,
      });
      await expect(panel).toContainText("ElastiCache: Denied");
      await expect(panel).toContainText("Add elasticache:Describe* to versus-reader");
      const copyFix = panel.getByRole("button", { name: "Copy fix for ElastiCache" });
      await expect(copyFix).toBeVisible();
      await expect(copyFix).toContainText("Copy fix");
      await capture(page, viewport, theme, "wizard-step2-denied");
      await copyFix.scrollIntoViewIfNeeded();
      await capture(page, viewport, theme, "wizard-step2-fix");
      await measureLayout(page, findings, `${viewport.name}/${theme}/step2`);

      // Step 3: regions; a not-enabled region is shown and unselectable.
      await panel.getByRole("button", { name: "Continue to regions" }).click();
      await expect(panel.getByLabel("Account name")).toBeVisible();
      const regionList = panel.getByRole("list", { name: "Regions" });
      await expect(regionList.locator("li")).toHaveCount(3);
      await expect(panel.getByRole("checkbox", { name: "US East (N. Virginia) (us-east-1)" })).toBeChecked();
      await expect(panel.getByRole("checkbox", { name: "Europe (Ireland) (eu-west-1)" })).toBeChecked();
      const blockedRegion = panel.getByRole("checkbox", { name: "Asia Pacific (Singapore) (ap-southeast-1)" });
      await expect(blockedRegion).toBeDisabled();
      await expect(blockedRegion).not.toBeChecked();
      await expect(regionList).toContainText("Not enabled");
      await capture(page, viewport, theme, "wizard-step3-regions");
      await measureLayout(page, findings, `${viewport.name}/${theme}/step3`);

      // Commit: the account appears in the table at once and the panel closes.
      await panel.getByLabel("Account name").fill("Payments");
      await panel.getByRole("button", { name: "Connect account" }).click();
      await expect(panel).toHaveCount(0);
      await expect(accounts.getByRole("listitem", { name: "Payments 123456789012" })).toBeVisible();
      await expect(accounts.locator('ul[aria-label="AWS accounts"] > li')).toHaveCount(4);
      await expect(page.getByText("Account connected")).toBeVisible();
      await capture(page, viewport, theme, "committed-table", true);

      // ---- 4. Full keyboard path: methods, role ARN, check, forward.
      await page.getByRole("button", { name: "Add account" }).click();
      const kbPanel = connectPanel(page);
      await expect(kbPanel).toBeVisible();
      await page.getByRole("button", { name: "Add account" }).focus();
      const reachedRadio = await tabUntil(
        page,
        async () =>
          (await page.evaluate(() => {
            const el = document.activeElement as HTMLElement | null;
            return el?.getAttribute("name") === "aws-connect-method";
          })),
      );
      expect(reachedRadio, "Tab must reach the connection-method radios").toBe(true);
      await page.keyboard.press("ArrowDown");
      await expect(kbPanel.getByRole("radio", { name: /Terraform/ })).toBeChecked();

      // Focus is visible on the focused control.
      const focusRing = await page.evaluate(() => {
        const el = document.activeElement as HTMLElement | null;
        if (!el) return { style: "none", width: "0px" };
        const style = getComputedStyle(el);
        return { style: style.outlineStyle, width: style.outlineWidth };
      });
      if (focusRing.style === "none" || focusRing.width === "0px") {
        findings.push(`focused control has no visible outline (${focusRing.style}/${focusRing.width})`);
      }
      await expect(kbPanel.getByRole("radio", { name: /Terraform/ })).toBeFocused();

      const reachedContinue = await tabUntil(
        page,
        async () => (await page.getByRole("button", { name: "Continue", exact: true }).evaluate((el) => el === document.activeElement).catch(() => false)),
      );
      expect(reachedContinue, "Tab must reach Continue").toBe(true);
      await page.keyboard.press("Enter");
      await expect(kbPanel.locator("#aws-role-arn")).toBeVisible();
      await kbPanel.locator("#aws-role-arn").focus();
      await expect(kbPanel.locator("#aws-role-arn")).toBeFocused();
      await page.keyboard.insertText(WIZARD_ROLE_ARN);
      await expect(kbPanel.locator("#aws-role-arn")).toHaveValue(WIZARD_ROLE_ARN);
      const reachedCheck = await tabUntil(
        page,
        async () => (await page.getByRole("button", { name: "Check access" }).evaluate((el) => el === document.activeElement).catch(() => false)),
      );
      expect(reachedCheck, "Tab must reach Check access").toBe(true);
      await expect(page.getByRole("button", { name: "Check access" })).toBeFocused();
      await page.keyboard.press("Enter");
      await expect(kbPanel).toContainText("Role assumed in 212 ms");
      // Wait for the probes to settle before tabbing to the enabled footer action.
      await expect(kbPanel).toContainText(
        "19 readable · 3 not used · 1 denied · 1 not on plan",
        { timeout: 15_000 },
      );
      await kbPanel.getByRole("button", { name: "Check access" }).focus();

      const reachedRegions = await tabUntil(
        page,
        async () => (await kbPanel.getByRole("button", { name: "Continue to regions" }).evaluate((el) => el === document.activeElement).catch(() => false)),
      );
      expect(reachedRegions, "Tab must reach Continue to regions").toBe(true);
      await page.keyboard.press("Enter");
      await expect(kbPanel.getByRole("list", { name: "Regions" })).toBeVisible();
      await kbPanel.getByLabel("Account name").focus();
      const reachedConnect = await tabUntil(
        page,
        async () => (await kbPanel.getByRole("button", { name: "Connect account" }).evaluate((el) => el === document.activeElement).catch(() => false)),
      );
      expect(reachedConnect, "Tab must reach Connect account").toBe(true);

      // Esc closes the panel and returns focus to the page.
      await page.keyboard.press("Escape");
      await expect(connectPanel(page)).toHaveCount(0);

      expect(findings, findings.join("\n")).toEqual([]);
    });
  }
}

// ---------------------------------------------------------------------------
// 3 — Worst cases. Full set at 1440 dark; the mobile/light repeats cover the
// states whose layout changes materially.
// ---------------------------------------------------------------------------

test("AWS settings worst cases (desktop, dark)", async ({ browser }) => {
  const viewport = VIEWPORTS[0];
  const theme: Theme = "dark";
  const findings: string[] = [];

  // --- AWS off: the answer line and the exact config key + env var.
  {
    const { page, recorder } = await openFixture(browser, viewport, theme, {
      settings: settings({
        enabled: false,
        accounts: [],
        enable_hint: {
          config_key: "connectors.aws.enable",
          env_var: "VERSUS_CONNECTORS_AWS_ENABLE",
        },
      }),
    });
    await expect(page.locator('[data-answer-kind="off"]')).toContainText("AWS connector is off");
    await expect(page.getByRole("heading", { name: "AWS connector is off" })).toBeVisible();
    await expect(page.getByRole("textbox", { name: "Config key" })).toHaveValue("connectors.aws.enable: true");
    await expect(page.getByRole("textbox", { name: "Environment variable" })).toHaveValue(
      "VERSUS_CONNECTORS_AWS_ENABLE=true",
    );
    await expect(page.getByRole("heading", { name: "Accounts", exact: true })).toHaveCount(0);
    expect(recorder.writes, "AWS off must not fire a write").toEqual([]);
    await measureLayout(page, findings, "desktop/dark/off");
    await capture(page, viewport, theme, "worst-aws-off", true);
  }

  // --- No accounts: the panel opens at step 1, no dead end.
  {
    const { page } = await openFixture(browser, viewport, theme, {
      settings: settings({ accounts: [] }),
      draftBase: draft(),
    });
    await expect(page.locator('[data-answer-kind="no-accounts"]')).toContainText(
      "No AWS accounts connected",
    );
    const panel = connectPanel(page);
    await expect(panel).toBeVisible();
    await expect(panel.getByRole("radio", { name: /CloudFormation quick-create/ })).toBeChecked();
    await expect(panel.getByRole("button", { name: "Continue", exact: true })).toBeEnabled();
    await capture(page, viewport, theme, "worst-no-accounts");
    await measureLayout(page, findings, "desktop/dark/no-accounts");
  }

  // --- No server identity: step 1 explains it and Continue is disabled.
  {
    const { page } = await openFixture(browser, viewport, theme, {
      settings: settings({ principal_arn: null, accounts: [account()] }),
      draftBase: draft({ principal_arn: null }),
    });
    await page.getByRole("button", { name: "Add account" }).click();
    const panel = connectPanel(page);
    await expect(panel).toContainText("The Versus server has no AWS identity");
    await expect(panel).toContainText(/instance profile, an ECS task role or a shared profile/);
    await expect(panel.getByRole("button", { name: "Continue", exact: true })).toBeDisabled();
    await expect(panel.getByRole("button", { name: "Continue", exact: true })).toHaveAttribute(
      "title",
      "The server needs an AWS identity first",
    );
    await capture(page, viewport, theme, "worst-no-identity");
    await measureLayout(page, findings, "desktop/dark/no-identity");
  }

  // --- Assume fails: the mapped reason and the exact fix.
  {
    const { page } = await openFixture(browser, viewport, theme, {
      settings: settings({ accounts: [account()] }),
      draftBase: draft(),
      draftBeforeVerify: draft(),
      draftAfterVerify: draft({
        assume: { ok: false, error_code: "external_id_mismatch" },
        services: [],
      }),
    });
    await page.getByRole("button", { name: "Add account" }).click();
    const panel = connectPanel(page);
    await panel.getByRole("button", { name: "Continue", exact: true }).click();
    await panel.getByLabel("Role ARN").fill(WIZARD_ROLE_ARN);
    await panel.getByRole("button", { name: "Check access" }).click();
    await expect(panel.getByRole("alert")).toContainText("Trust policy does not allow this ExternalId");
    await expect(panel.getByRole("alert")).toContainText(
      "The role trusts a different ExternalId than the one this draft generated.",
    );
    await expect(panel.getByRole("alert")).toContainText(
      "Fix: Copy the ExternalId from this step into the role's trust condition (sts:ExternalId).",
    );
    await capture(page, viewport, theme, "worst-assume-failure");
    await measureLayout(page, findings, "desktop/dark/assume-failure");
  }

  // --- Throttled probe: "Not checked", never Denied and never a fake zero.
  {
    const { page } = await openFixture(browser, viewport, theme, {
      settings: settings({ accounts: [account()] }),
      draftBase: draft(),
      draftBeforeVerify: draft(),
      draftAfterVerify: draft({
        assume: { ok: true, latency_ms: 180 },
        services: THROTTLED_SERVICES,
      }),
      draftSettled: draft({
        assume: { ok: true, latency_ms: 180 },
        services: THROTTLED_SERVICES,
      }),
    });
    await page.getByRole("button", { name: "Add account" }).click();
    const panel = connectPanel(page);
    await panel.getByRole("button", { name: "Continue", exact: true }).click();
    await panel.getByLabel("Role ARN").fill(WIZARD_ROLE_ARN);
    await panel.getByRole("button", { name: "Check access" }).click();
    const ecs = panel.getByRole("listitem", { name: "ECS: Not checked: AWS throttled, retrying" });
    await expect(ecs).toBeVisible();
    await expect(ecs).toContainText("Not checked");
    await expect(ecs).toContainText("AWS throttled, retrying");
    await expect(ecs).not.toContainText("Denied");
    await expect(panel.getByRole("listitem", { name: /ECS: Denied/ })).toHaveCount(0);
    await expect(panel).toContainText("18 readable · 3 not used · 1 denied · 1 not on plan · 1 not checked");
    const gridText = await panel.getByRole("list", { name: "Service access" }).innerText();
    expect(gridText, "a throttled probe must not render a zero").not.toMatch(/\b0\b/);
    await capture(page, viewport, theme, "worst-throttled");
    await measureLayout(page, findings, "desktop/dark/throttled");
  }

  // --- Disconnected row: Reconnect, other rows unchanged.
  {
    const { page } = await openFixture(browser, viewport, theme, {
      settings: settings({
        accounts: [
          account(),
          staging({
            status: "disconnected",
            disconnected_since: atLocalTime(13, 5),
            checked_at: null,
            error_code: "trust_denied",
          }),
          account({ account_id: "777788889999", display_name: "Shared services" }),
        ],
      }),
    });
    await expect(page.locator('[data-answer-kind="disconnected"]')).toContainText(
      "Staging disconnected since 13:05",
    );
    await expect(page.locator('[data-answer-kind="disconnected"]')).toContainText(
      "2 accounts still connected",
    );
    const accounts = accountsCard(page);
    const row = accounts.getByRole("listitem", { name: "Staging 444455556666" });
    await expect(row).toContainText("Disconnected");
    await expect(row).toContainText("since 13:05");
    await expect(row.getByRole("button", { name: "Reconnect" })).toBeVisible();
    await expect(accounts.getByRole("listitem", { name: "Production 111122223333" })).toContainText(
      "Connected",
    );
    await capture(page, viewport, theme, "worst-disconnected", true);
    await measureLayout(page, findings, "desktop/dark/disconnected");
  }

  // --- Viewer: no Add account, Reveal, Recheck; no write request at all.
  {
    const { page, recorder } = await openFixture(browser, viewport, theme, {
      settings: settings({ capabilities: { organizations: false, can_write: false } }),
    });
    await expect(page.getByRole("button", { name: "Add account" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: /^Recheck/ })).toHaveCount(0);
    await expect(page.getByRole("button", { name: /^Reveal/ })).toHaveCount(0);
    await expect(page.getByRole("button", { name: /^Rename/ })).toHaveCount(0);
    await expect(page.getByRole("button", { name: /^Remove/ })).toHaveCount(0);
    await expect(page.getByText("Read-only: you can view AWS settings but not change them.")).toBeVisible();
    await expect(accountsCard(page).locator('ul[aria-label="AWS accounts"] > li')).toHaveCount(3);
    expect(recorder.writes, `viewer fired writes: ${JSON.stringify(recorder.writes)}`).toEqual([]);
    await capture(page, viewport, theme, "worst-viewer", true);
    await measureLayout(page, findings, "desktop/dark/viewer");
  }

  expect(findings, findings.join("\n")).toEqual([]);
});

test("AWS settings worst cases (mobile, dark)", async ({ browser }) => {
  const viewport = VIEWPORTS[1];
  const theme: Theme = "dark";
  const findings: string[] = [];

  // Off state stacks cleanly.
  {
    const { page } = await openFixture(browser, viewport, theme, {
      settings: settings({
        enabled: false,
        accounts: [],
        enable_hint: { config_key: "connectors.aws.enable", env_var: "VERSUS_CONNECTORS_AWS_ENABLE" },
      }),
    });
    await expect(page.getByRole("heading", { name: "AWS connector is off" })).toBeVisible();
    await expect(page.getByRole("textbox", { name: "Environment variable" })).toHaveValue(
      "VERSUS_CONNECTORS_AWS_ENABLE=true",
    );
    await capture(page, viewport, theme, "worst-aws-off", true);
    await measureLayout(page, findings, "mobile/dark/off");
  }

  // No accounts: the panel opens full-screen at step 1.
  {
    const { page } = await openFixture(browser, viewport, theme, {
      settings: settings({ accounts: [] }),
      draftBase: draft(),
    });
    const dialog = page.getByRole("dialog", { name: "Connect an AWS account" });
    await expect(dialog).toBeVisible();
    await expect(dialog.getByRole("heading", { name: "Connect account" }).first()).toBeVisible();
    await expect(dialog.getByRole("radio", { name: /CloudFormation quick-create/ })).toBeChecked();
    await expect(dialog.getByRole("button", { name: "Continue", exact: true })).toBeEnabled();
    await capture(page, viewport, theme, "worst-no-accounts");
    await measureLayout(page, findings, "mobile/dark/no-accounts");
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
  }

  // Viewer: no controls at mobile width either.
  {
    const { page, recorder } = await openFixture(browser, viewport, theme, {
      settings: settings({ capabilities: { organizations: false, can_write: false } }),
    });
    await expect(page.getByRole("button", { name: "Add account" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: /^Recheck/ })).toHaveCount(0);
    expect(recorder.writes).toEqual([]);
    await capture(page, viewport, theme, "worst-viewer", true);
    await measureLayout(page, findings, "mobile/dark/viewer");
  }

  expect(findings, findings.join("\n")).toEqual([]);
});

test("AWS settings worst cases (light theme)", async ({ browser }) => {
  const viewport = VIEWPORTS[0];
  const theme: Theme = "light";
  const findings: string[] = [];

  // Off, viewer and the default populated screen are enough to prove the light
  // tokens carry the same words and contrast.
  {
    const { page } = await openFixture(browser, viewport, theme, {
      settings: settings({
        enabled: false,
        accounts: [],
        enable_hint: { config_key: "connectors.aws.enable", env_var: "VERSUS_CONNECTORS_AWS_ENABLE" },
      }),
    });
    await expect(page.getByRole("heading", { name: "AWS connector is off" })).toBeVisible();
    await measureLayout(page, findings, "desktop/light/off");
    await capture(page, viewport, theme, "worst-aws-off", true);
  }

  {
    const { page } = await openFixture(browser, viewport, theme, {
      settings: settings(),
      draftBase: draft(),
    });
    await expect(page.locator('[data-answer-kind="denied"]')).toBeVisible();
    await measureLayout(page, findings, "desktop/light/default");
    await capture(page, viewport, theme, "default", true);
  }

  {
    const { page } = await openFixture(browser, viewport, theme, {
      settings: settings({ capabilities: { organizations: false, can_write: false } }),
    });
    await expect(page.getByText("Read-only: you can view AWS settings but not change them.")).toBeVisible();
    await capture(page, viewport, theme, "worst-viewer", true);
    await measureLayout(page, findings, "desktop/light/viewer");
  }

  expect(findings, findings.join("\n")).toEqual([]);
});
