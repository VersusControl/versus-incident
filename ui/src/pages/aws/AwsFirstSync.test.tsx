// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ToastProvider } from "@/components/Toast";
import type { AwsAccountSync, AwsSettings } from "@/lib/api";
import { accountStatusWords, firstSyncRunning, settingsPollMs, syncProgress } from "@/lib/awsSettings";
import { AwsAccountsCard } from "./AwsAccountsCard";
import { awsAccount, awsSettings } from "./awsFixtures";

// One Settings poll after another while a new account's first sync fills in
// (built by versus-incident/tests/ui/aws/firstSync.ts).
import { firstSyncFixture } from "../../../../tests/ui/aws/firstSync";

const fixture = firstSyncFixture();

const polls: AwsAccountSync[] = fixture.polls.map((p) => p.sync);
const NOW = new Date(fixture.now);

function settingsWith(sync: AwsAccountSync): AwsSettings {
  return awsSettings({ accounts: [awsAccount({ account_id: fixture.account_id, sync })] });
}

function card(settings: AwsSettings, canWrite = true) {
  return (
    <QueryClientProvider client={new QueryClient()}>
      <ToastProvider>
        <AwsAccountsCard settings={settings} canWrite={canWrite} onConnect={vi.fn()} onReconnect={vi.fn()} />
      </ToastProvider>
    </QueryClientProvider>
  );
}

afterEach(cleanup);
beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(NOW);
});
afterEach(() => vi.useRealTimers());

describe("first sync progress rules", () => {
  it("says Waiting while no service is listed yet, never 0 of 0", () => {
    const empty: AwsAccountSync = { state: "pending", synced_at: null, first_sync: true, units_done: 0, units_total: 0, services: [] };
    expect(syncProgress(empty).label).toBe("Waiting");
  });

  it("counts ready services and names the rest with a word, never a zero", () => {
    expect(syncProgress(polls[0]).label).toBe("0 of 9 ready");
    expect(syncProgress(polls[0]).rows.every((r) => r.word === "Waiting" && !r.detail)).toBe(true);
    const p2 = syncProgress(polls[1]);
    expect(p2.label).toBe("3 of 9 ready");
    expect(p2.rows.find((r) => r.service === "ecs")).toMatchObject({ word: "Loading", detail: "1 of 2 regions" });
    for (const p of polls) {
      for (const r of syncProgress(p).rows) expect(r.detail ?? "").not.toMatch(/^0 of/);
    }
  });

  it("names gaps in words", () => {
    const rows = syncProgress(polls[2]).rows;
    expect(rows.find((r) => r.service === "sns")?.word).toBe("Not checked: AWS throttled, retrying");
    expect(rows.find((r) => r.service === "elasticache")?.word).toBe("Denied");
  });

  it("never lowers the total between polls", () => {
    expect(syncProgress(polls[3]).total).toBe(7);
    expect(syncProgress(polls[3], 9).label).toBe("6 of 9 ready");
  });

  it("polls every second only while a first sync runs", () => {
    expect(polls.slice(0, 4).every((p) => firstSyncRunning(awsAccount({ sync: p })))).toBe(true);
    expect(firstSyncRunning(awsAccount({ sync: polls[4] }))).toBe(false);
    expect(settingsPollMs(settingsWith(polls[1]))).toBe(1_000);
    expect(settingsPollMs(settingsWith(polls[4]))).toBe(15_000);
    expect(settingsPollMs(awsSettings())).toBe(15_000);
  });

  it("switches freshness to Synced once data is committed", () => {
    expect(accountStatusWords(awsAccount({ sync: polls[0] }), NOW).detail).toMatch(/^checked /);
    expect(accountStatusWords(awsAccount({ sync: polls[4] }), NOW).detail).toBe("Synced 12 s ago");
    expect(accountStatusWords(awsAccount({ sync: { ...polls[4], state: "off" } }), NOW).detail).toMatch(/^checked /);
  });
});

describe("accounts row during the first sync", () => {
  it("fills in poll by poll with words and icons, keeping the total", () => {
    const view = render(card(settingsWith(polls[0])));
    expect(screen.getByRole("status", { name: "Production first sync: 0 of 9 ready" })).toBeTruthy();
    const list = () => screen.getByRole("list", { name: "Production services" });
    expect(within(list()).getAllByRole("listitem")).toHaveLength(9);
    expect(within(list()).getByRole("listitem", { name: "EC2: Waiting" })).toBeTruthy();

    view.rerender(card(settingsWith(polls[1])));
    expect(screen.getByRole("status").textContent).toContain("3 of 9 ready");
    expect(within(list()).getByRole("listitem", { name: "EC2: Ready" })).toBeTruthy();
    expect(within(list()).getByRole("listitem", { name: "ECS: Loading" }).textContent).toContain("1 of 2 regions");

    view.rerender(card(settingsWith(polls[2])));
    expect(screen.getByRole("status").textContent).toContain("6 of 9 ready");
    expect(within(list()).getByRole("listitem", { name: "ElastiCache: Denied" })).toBeTruthy();
    expect(list().textContent).toContain("Not checked: AWS throttled, retrying");

    view.rerender(card(settingsWith(polls[3])));
    expect(screen.getByRole("status").textContent).toContain("6 of 9 ready");

    expect(document.body.textContent).not.toMatch(/\b0 of \d+ regions/);
  });

  it("leaves progress for Synced freshness and named gaps when done", () => {
    render(card(settingsWith(polls[4])));
    expect(screen.queryByRole("status")).toBeNull();
    expect(screen.getByText("Synced 12 s ago")).toBeTruthy();
    expect(document.body.textContent).toContain("SNS: Not checked: AWS throttled, retrying · ElastiCache: Denied");
  });

  it("shows a viewer the same progress with no write controls", () => {
    render(card(settingsWith(polls[2]), false));
    expect(screen.getByRole("status").textContent).toContain("6 of 9 ready");
    expect(screen.queryByRole("button", { name: /Recheck|Add account|Rename|Remove/ })).toBeNull();
  });
});
