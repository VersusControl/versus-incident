import { expect, test, type Page } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { harnessRuntimeStateDir } from "./support/capture";

type LicensedIdentity = { id: string; subject: string; role: string; clusters: string[]; expected_mfa: boolean; login_url: string };
type LicensedContract = { configured: boolean; base_url: string; org: string; identities: LicensedIdentity[] };
type LicensedBrowserHelper = {
  loadBrowserFixture: (file: string) => Promise<unknown>;
  loginLicensedIdentity: (page: Page, contract: LicensedContract, fixture: unknown, id: string) => Promise<void>;
};

const settingsBody = { learning: { logs: true, metrics: true, traces: true, scope: { include: ["east-shop"], exclude: [] } }, break_logs: { linked_source: "", fields: {} } };

test.describe("Kubernetes agent settings real licensed RBAC", () => {
  test.skip(process.env.E2E_KUBERNETES_LICENSED !== "1", "requires the real licensed Keycloak browser fixture");
  let contract: LicensedContract;
  let helper: LicensedBrowserHelper;
  let browserFixture: unknown;

  test.beforeAll(async () => {
    const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../../..");
    const state = harnessRuntimeStateDir();
    contract = JSON.parse(fs.readFileSync(path.join(state, "licensed-kubernetes.json"), "utf8"));
    expect(contract.configured, "a completed real Enterprise bootstrap is required").toBe(true);
    const browserFile = process.env.HARNESS_KUBERNETES_BROWSER_FILE;
    if (!browserFile) throw new Error("HARNESS_KUBERNETES_BROWSER_FILE must point to the private mode-0600 browser fixture");
    helper = await import(pathToFileURL(path.join(root, "plans/harness-run/tests/enterprise/licensed-keycloak-browser.mjs")).href) as LicensedBrowserHelper;
    browserFixture = await helper.loadBrowserFixture(browserFile);
  });

  for (const id of ["east", "all", "viewer"]) {
    test(`real ${id} session reads but cannot write or test agent settings`, async ({ browser }, testInfo) => {
      test.setTimeout(180_000);
      const context = await browser.newContext({ baseURL: contract.base_url });
      const page = await context.newPage();
      const origin = { Origin: contract.base_url };
      try {
        await helper.loginLicensedIdentity(page, contract, browserFixture, id);
        const sessionResponse = await context.request.get(`/enterprise/api/sso/${encodeURIComponent(contract.org)}/session`);
        expect(sessionResponse.status()).toBe(200);
        const session = await sessionResponse.json();
        expect(session.local ?? false).toBe(false);
        expect(session.subject).toBe(contract.identities.find((identity) => identity.id === id)?.subject);
        const me = await (await context.request.get("/enterprise/api/rbac/me")).json();
        expect(me.permissions).not.toContain("infrastructure:settings:write");
        const read = await context.request.get("/api/admin/kubernetes/agent-settings?cluster=east");
        const logs = await context.request.get("/api/admin/kubernetes/break-logs?cluster=east&namespace=east-shop&root_uid=absent-root&since=" + encodeURIComponent(new Date(Date.now() - 600_000).toISOString()) + "&until=" + encodeURIComponent(new Date().toISOString()));
        const write = await context.request.put("/api/admin/kubernetes/agent-settings?cluster=east", { data: settingsBody, headers: origin });
        const writeText = await write.text();
        const link = await context.request.post("/api/admin/kubernetes/agent-settings/test-link?cluster=east", { data: { source: "absent", fields: {} }, headers: origin });
        const linkText = await link.text();
        const after = await context.request.get("/api/admin/kubernetes/agent-settings?cluster=east");
        const crossCluster = await context.request.get("/api/admin/kubernetes/agent-settings?cluster=west");
        const issueAlertsRead = await context.request.get("/api/admin/kubernetes/issue-alerts?cluster=east");
        const issueAlertsWrite = await context.request.put("/api/admin/kubernetes/issue-alerts?cluster=east", { data: { enabled: true, level: "warning" }, headers: origin });
        const issueAlertsWriteText = await issueAlertsWrite.text();
        const issueAlertsAfter = await context.request.get("/api/admin/kubernetes/issue-alerts?cluster=east");
        const issueAlertsWest = await context.request.get("/api/admin/kubernetes/issue-alerts?cluster=west");
        const evidence = { id, role: me.role, read: read.status(), break_logs: logs.status(), write: write.status(), write_body: writeText.slice(0, 200), test_link: link.status(), test_link_body: linkText.slice(0, 200), west: crossCluster.status(), issue_alerts_read: issueAlertsRead.status(), issue_alerts_write: issueAlertsWrite.status(), issue_alerts_write_body: issueAlertsWriteText.slice(0, 200), issue_alerts_west: issueAlertsWest.status() };
        await testInfo.attach(`agent-settings-rbac-${id}`, { body: JSON.stringify(evidence), contentType: "application/json" });
        console.log(`AGENT_SETTINGS_RBAC ${JSON.stringify({ ...evidence, subject_matches: session.subject === contract.identities.find((identity) => identity.id === id)?.subject, mfa: session.mfa, local: session.local ?? false })}`);
        expect(write.status()).toBe(403);
        expect(link.status()).toBe(403);
        expect(issueAlertsWrite.status()).toBe(403);
        expect(writeText).not.toMatch(/csrf|origin/i);
        expect(linkText).not.toMatch(/csrf|origin/i);
        expect(issueAlertsWriteText).not.toMatch(/csrf|origin/i);
        if (id === "viewer") {
          expect(read.status()).toBe(403);
          expect(logs.status()).toBe(403);
          expect(issueAlertsRead.status()).toBe(403);
          return;
        }
        expect(read.status()).toBe(200);
        expect(logs.status()).toBe(200);
        expect(issueAlertsRead.status()).toBe(200);
        const settings = await read.json();
        expect(settings.can_write).toBe(false);
        expect(await after.json()).toEqual(settings);
        expect(crossCluster.status()).toBe(id === "east" ? 404 : 200);
        const issueAlerts = await issueAlertsRead.json();
        expect(issueAlerts.can_write).toBe(false);
        expect(await issueAlertsAfter.json()).toEqual(issueAlerts);
        expect(issueAlertsWest.status()).toBe(id === "east" ? 404 : 200);
      } finally {
        await context.close();
      }
    });
  }
});
