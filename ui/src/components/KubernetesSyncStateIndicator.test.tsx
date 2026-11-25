// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { KubernetesSyncStateIndicator } from "@/components/KubernetesSyncStateIndicator";
import type { KubernetesKindProblem } from "@/lib/kubernetesSyncHealth";

afterEach(() => { cleanup(); vi.useRealTimers(); });

const published = "2026-10-27T08:00:00Z";
const hhmm = (iso: string) => new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hourCycle: "h23" });
const minutesAgo = (value: number) => new Date(Date.now() - value * 60_000).toISOString();
const problem = (kind: string, reason: string, since?: string): KubernetesKindProblem => ({ kind, error: "forbidden", reason, skipped: 0, records: 0, since, sinceBasis: "observed" });
// The status as shown on a wide screen: without its screen-reader-only words
// and without the short phone label.
const shownText = (element: Element | null | undefined) => {
  if (!element) return undefined;
  const copy = element.cloneNode(true) as Element;
  copy.querySelectorAll(".sr-only, [data-sync-label-short]").forEach((node) => node.remove());
  return copy.textContent;
};
const label = () => shownText(document.querySelector("[data-sync-state]"));

describe("Sync state indicator", () => {
  it("says which namespaces a namespace-only identity shows, with the freshness and docs in its details", () => {
    render(<KubernetesSyncStateIndicator state="ready" streamState="live" ageSeconds={4} firstReadyAt={published} readableNamespaces={["payments", "shop"]} />);
    expect(label()).toBe("Namespace access — showing payments, shop");
    fireEvent.mouseEnter(screen.getByRole("button", { name: "Namespace access — showing payments, shop" }));
    const details = screen.getByRole("region", { name: "Namespace access details" });
    expect(details.textContent).toContain("Last synced 4s ago");
    expect(within(details).getByRole("link").getAttribute("target")).toBe("_blank");
  });

  it("lets a real failure and a disconnected feed outrank namespace access", () => {
    const { rerender } = render(<KubernetesSyncStateIndicator state="failed" streamState="live" firstReadyAt={published} readableNamespaces={["shop"]} />);
    expect(label()).toMatch(/^Sync failed/);
    rerender(<KubernetesSyncStateIndicator state="ready" streamState="reconnecting" firstReadyAt={published} readableNamespaces={["shop"]} />);
    expect(label()).toBe("Updates disconnected — refreshing every 30s");
  });

  it.each([
    ["ready", "Last synced 4s ago"],
    ["preparing", "Preparing"],
    ["degraded", "Sync degraded"],
    ["failed", "Sync failed"],
  ] as const)("writes out the %s state at every width", (state, expected) => {
    const { container } = render(<KubernetesSyncStateIndicator state={state} streamState="live" ageSeconds={4} />);
    const indicator = container.querySelector("[data-sync-state]")!;
    expect(indicator.getAttribute("data-sync-state")).toBe(state);
    expect(shownText(indicator)).toBe(expected);
    // One responsive rule for all four states: no width-scoped hiding, so a
    // narrow viewport never leaves the dot alone.
    const tokens = indicator.className.split(/\s+/);
    expect(tokens).not.toContain("hidden");
    expect(tokens.some((token) => /^(sm|md|lg):(inline|block|flex)$/.test(token))).toBe(false);
  });

  it.each([
    ["connecting", "Last synced 4s ago"],
    ["reconnecting", "Updates disconnected — refreshing every 30s"],
    ["resyncing", "Updates disconnected — refreshing every 30s"],
  ] as const)("names the %s record feed while the sync is ready", (streamState, expected) => {
    const { container } = render(<KubernetesSyncStateIndicator state="ready" streamState={streamState} ageSeconds={4} />);
    expect(shownText(container.querySelector("[data-sync-state]"))).toBe(expected);
  });

  it("keeps the dot decorative so state is never carried by colour alone", () => {
    const { container } = render(<KubernetesSyncStateIndicator state="degraded" streamState="live" />);
    const indicator = container.querySelector("[data-sync-state]")!;
    const decoration = indicator.querySelector("[aria-hidden='true']");
    expect(decoration).toBeTruthy();
    expect(decoration?.textContent).toBe("");
    expect(indicator.getAttribute("role")).toBe("status");
    expect(indicator.getAttribute("aria-live")).toBe("polite");
  });
});

it.each([
  ["failed", "Sync failed"],
  ["degraded", "Sync degraded"],
] as const)("dates the data on screen when a published cluster's sync is %s", (state, head) => {
  const lastSyncedAt = "2026-11-02T09:00:00Z";
  render(<KubernetesSyncStateIndicator state={state} firstReadyAt={published} streamState="live" lastSyncedAt={lastSyncedAt} />);
  // The data is from an earlier day, so its date is part of the label.
  const day = new Date(lastSyncedAt).toLocaleDateString([], { month: "short", day: "numeric" });
  expect(label()).toBe(`${head} — showing data from ${day}, ${hhmm(lastSyncedAt)}`);
  expect(document.querySelector("[data-sync-state]")?.className).toContain(state === "failed" ? "text-sev-critical-text" : "text-sev-warning-text");
});

it("says last saved data when a published cluster's data has no known time", () => {
  render(<KubernetesSyncStateIndicator state="failed" firstReadyAt={published} streamState="live" />);
  expect(label()).toBe("Sync failed — showing last saved data");
});

it("does not claim to show data when nothing is on screen", () => {
  render(<KubernetesSyncStateIndicator state="failed" firstReadyAt={published} streamState="live" ageSeconds={60} dataOnScreen={false} />);
  expect(label()).toBe("Sync failed");
});

it("names published preparing as Syncing", () => {
  render(<KubernetesSyncStateIndicator state="preparing" firstReadyAt={published} streamState="live" />);
  expect(label()).toBe("Syncing");
});

it("names credential renewal without claiming a successful live refresh", () => {
  render(<KubernetesSyncStateIndicator state="ready" streamState="live" syncState="refreshing" syncReason="credential_refresh" />);
  expect(label()).toBe("Refreshing credentials…");
});

it("does not describe a credential denial as an active renewal", () => {
  render(<KubernetesSyncStateIndicator state="failed" streamState="live" syncState="error" syncReason="credential_unavailable" />);
  expect(label()).toBe("Sync failed");
});

it("words an unreachable cluster the same way as any other failed sync", () => {
  render(<KubernetesSyncStateIndicator state="degraded" streamState="live" firstReadyAt={published} syncReason="cluster_unreachable" ageSeconds={60} />);
  expect(label()).toMatch(/^Sync degraded — showing data from \d{2}:\d{2}/);
  expect(label()).not.toContain("unreachable");
});

it.each(["syncing", "refreshing"])("does not let stale %s progress conceal a current failure or advance its saved timestamp", syncState => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2026-11-02T09:10:00Z"));
  const lastSyncedAt = "2026-11-02T09:00:00Z";
  const props = { state: "failed" as const, streamState: "live" as const, firstReadyAt: lastSyncedAt,
    syncState, syncReason: "request_timeout", lastSyncedAt, ageSeconds: 0 };
  const { container, rerender } = render(<KubernetesSyncStateIndicator {...props} />);
  const expected = `Sync failed — showing data from ${hhmm(lastSyncedAt)}`;
  expect(container.querySelector("[data-sync-state]")?.textContent).toBe(expected);
  vi.advanceTimersByTime(600000);
  rerender(<KubernetesSyncStateIndicator {...props} />);
  expect(container.querySelector("[data-sync-state]")?.textContent).toBe(expected);
});

it("does not mask a current credential denial with stale credential-renewal progress", () => {
  render(<KubernetesSyncStateIndicator state="failed" streamState="live" syncState="refreshing" syncReason="credential_refresh" />);
  expect(label()).toBe("Sync failed");
});

describe("sync failure details", () => {
  it("opens on hover with one line per failing kind and Retry sync", () => {
    const onRetry = vi.fn();
    render(<KubernetesSyncStateIndicator state="failed" firstReadyAt={published} streamState="live"
      problems={[problem("Secret", "permission denied", minutesAgo(5)), problem("Node", "the cluster read failed")]} onRetry={onRetry} />);
    expect(screen.queryByRole("region", { name: "Sync status details" })).toBeNull();
    fireEvent.mouseEnter(screen.getByRole("button", { name: /^Sync failed/ }));
    const details = screen.getByRole("region", { name: "Sync status details" });
    expect(within(details).getByText("Sync failed")).toBeTruthy();
    const rows = details.querySelectorAll("[data-sync-problem]");
    expect(rows).toHaveLength(2);
    expect(rows[0].textContent).toBe("Secret: permission denied · since 5 minutes ago");
    expect(rows[1].textContent).toBe("Node: the cluster read failed");
    fireEvent.click(within(details).getByRole("button", { name: "Retry sync" }));
    expect(onRetry).toHaveBeenCalledTimes(1);
    // The label itself stays the live region; the panel lives outside it.
    expect(document.querySelector("[role=status][data-sync-state]")?.contains(details)).toBe(false);
  });

  it("opens on keyboard focus, lets Tab reach Retry, and closes on Escape back to the trigger", () => {
    render(<>
      <KubernetesSyncStateIndicator state="degraded" firstReadyAt={published} streamState="live" staleSeconds={420} onRetry={vi.fn()} />
      <button type="button">Next control</button>
    </>);
    const trigger = screen.getByRole("button", { name: /^Sync degraded/ });
    act(() => trigger.focus());
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    const details = screen.getByRole("region", { name: "Sync status details" });
    expect(trigger.getAttribute("aria-controls")).toBe(details.id);
    expect(within(details).getByText("Sync degraded")).toBeTruthy();
    expect(details.textContent).toContain("The last snapshot is 7 minutes old.");
    fireEvent.keyDown(trigger, { key: "Tab" });
    const retry = within(details).getByRole("button", { name: "Retry sync" });
    expect(document.activeElement).toBe(retry);
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("region", { name: "Sync status details" })).toBeNull();
    expect(document.activeElement).toBe(trigger);
  });

  it("moves on past the panel to the control after the trigger", () => {
    render(<>
      <KubernetesSyncStateIndicator state="failed" firstReadyAt={published} streamState="live" onRetry={vi.fn()} />
      <button type="button">Next control</button>
    </>);
    const trigger = screen.getByRole("button", { name: /^Sync failed/ });
    act(() => trigger.focus());
    fireEvent.keyDown(trigger, { key: "Tab" });
    const retry = screen.getByRole("button", { name: "Retry sync" });
    fireEvent.keyDown(retry, { key: "Tab" });
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Next control" }));
    expect(screen.queryByRole("region", { name: "Sync status details" })).toBeNull();
  });

  it("scrolls a long list of failing kinds instead of growing past the viewport", () => {
    const many = Array.from({ length: 9 }, (_, index) => problem(`Kind${index}`, "permission denied"));
    render(<KubernetesSyncStateIndicator state="failed" firstReadyAt={published} streamState="live" problems={many} />);
    fireEvent.click(screen.getByRole("button", { name: /^Sync failed/ }));
    const list = screen.getByRole("list", { name: "Resource types that could not sync" });
    expect(list.querySelectorAll("li")).toHaveLength(9);
    expect(list.className).toContain("overflow-y-auto");
    expect(list.getAttribute("tabindex")).toBe("0");
    expect(screen.getByRole("region", { name: "Sync status details" }).className).toContain("overflow-auto");
  });

  it("offers no details trigger while the sync is healthy", () => {
    render(<KubernetesSyncStateIndicator state="ready" streamState="live" ageSeconds={4} />);
    expect(screen.queryByRole("button")).toBeNull();
  });
});

it("keeps a long status on one line that truncates in place, with the full words in its title", () => {
  const { container } = render(<KubernetesSyncStateIndicator state="ready" streamState="reconnecting" ageSeconds={4} firstReadyAt={published} />);
  const text = container.querySelector<HTMLElement>("[data-sync-label]")!;
  expect(text.textContent).toBe("Updates disconnected — refreshing every 30s");
  expect(text.getAttribute("title")).toBe("Updates disconnected — refreshing every 30s");
  expect(text.className.split(/\s+/)).toEqual(expect.arrayContaining(["min-w-0", "truncate"]));
  expect(label()).toBe("Updates disconnected — refreshing every 30s");
});

it.each([
  ["reconnecting", undefined, "text-ink-300"],
  ["live", "syncing", "text-sev-ok-text"],
] as const)("writes the %s status in a tone that meets contrast on the top bar", (streamState, syncState, tone) => {
  const { container } = render(<KubernetesSyncStateIndicator state="ready" streamState={streamState} syncState={syncState} ageSeconds={4} firstReadyAt={published} />);
  const tokens = container.querySelector("[data-sync-state]")!.className.split(/\s+/);
  expect(tokens).toContain(tone);
  expect(tokens).not.toContain("text-ink-400");
  expect(tokens).not.toContain("text-sev-ok");
});

it("keeps the refresh note reachable on a phone through an icon that holds its place", () => {
  vi.useFakeTimers();
  const note = "Couldn't refresh status — try again in 5s";
  const { container } = render(<KubernetesSyncStateIndicator state="ready" streamState="reconnecting" ageSeconds={4} firstReadyAt={published} refreshNote={note} />);
  // From sm up the words follow the status; below sm they are hidden and the
  // icon carries them, so the top bar never wraps.
  const words = container.querySelector<HTMLElement>("[data-sync-refresh-note]")!;
  expect(words.className.split(/\s+/)).toEqual(expect.arrayContaining(["hidden", "sm:inline", "truncate"]));
  const trigger = screen.getByRole("button", { name: note });
  expect(trigger.className.split(/\s+/)).toEqual(expect.arrayContaining(["sm:hidden", "shrink-0"]));
  expect(trigger.getAttribute("title")).toBe(note);
  fireEvent.click(trigger);
  act(() => { vi.advanceTimersByTime(50); });
  expect(screen.getByRole("region", { name: "Status refresh" }).textContent).toBe(note);
});

it.each([
  [{ statusStaleSeconds: 200 }, "degraded", "Status may be out of date · last snapshot 3 minutes ago", "text-sev-warning-text"],
  [{ statusUnavailable: true }, "preparing", "Couldn't load sync status — try again", "text-ink-300"],
] as const)("words a status that could not be read for too long without a failed sync", (props, attribute, text, tone) => {
  const { container } = render(<KubernetesSyncStateIndicator state={"statusUnavailable" in props ? "preparing" : "ready"} streamState="live" ageSeconds={4} refreshNote="Couldn't refresh status — try again in 5s" {...props} />);
  const status = container.querySelector<HTMLElement>("[data-sync-state]")!;
  expect(status.getAttribute("data-sync-state")).toBe(attribute);
  expect(shownText(status.querySelector("[data-sync-label]"))).toBe(text);
  expect(status.className).toContain(tone);
  expect(status.textContent).not.toMatch(/Sync failed/);
});

it("counts the refresh note down to the next status read, then says it is retrying", () => {
  vi.useFakeTimers();
  vi.setSystemTime(Date.parse("2026-10-26T09:00:00Z"));
  const retryAt = Date.now() + 5_000;
  const { container } = render(<KubernetesSyncStateIndicator state="ready" streamState="live" ageSeconds={4} firstReadyAt={published} refreshNote="Couldn't refresh status" refreshRetryAt={retryAt} />);
  const note = () => container.querySelector("[data-sync-refresh-note]")?.textContent;
  expect(note()).toBe("· Couldn't refresh status — try again in 5s");
  act(() => { vi.advanceTimersByTime(1_000); });
  expect(note()).toBe("· Couldn't refresh status — try again in 4s");
  act(() => { vi.advanceTimersByTime(4_000); });
  expect(note()).toBe("· Couldn't refresh status — retrying…");
});

it("gives the phone refresh icon a touch target larger than 24 px", () => {
  render(<KubernetesSyncStateIndicator state="ready" streamState="live" ageSeconds={4} firstReadyAt={published} refreshNote="The history database is not responding" />);
  const size = screen.getByRole("button", { name: "The history database is not responding" }).className.match(/\bsize-(\d+)\b/);
  expect(Number(size?.[1]) * 4).toBeGreaterThan(24);
});

it("announces only the refresh cause: the countdown never changes the live region or the icon's label", () => {
  vi.useFakeTimers();
  vi.setSystemTime(Date.parse("2026-10-26T09:00:00Z"));
  const { container } = render(<KubernetesSyncStateIndicator state="ready" streamState="live" ageSeconds={4} firstReadyAt={published} refreshNote="Couldn't refresh status" refreshRetryAt={Date.now() + 5_000} />);
  const live = container.querySelector<HTMLElement>('[role="status"][aria-live="polite"]')!;
  // What a screen reader reads from the live region: its text without the
  // parts hidden from assistive technology.
  const announced = () => {
    const copy = live.cloneNode(true) as HTMLElement;
    copy.querySelectorAll('[aria-hidden="true"]').forEach((node) => node.remove());
    return copy.textContent;
  };
  const icon = () => container.querySelector("[data-sync-refresh-note-trigger]")!;
  const before = { text: announced(), label: icon().getAttribute("aria-label"), visible: container.querySelector("[data-sync-refresh-countdown]")?.textContent };
  expect(before.text).toContain("Couldn't refresh status");
  expect(before.text).not.toMatch(/try again in|retrying/);
  expect(before.label).toBe("Couldn't refresh status");
  act(() => { vi.advanceTimersByTime(2_000); });
  expect(announced()).toBe(before.text);
  expect(icon().getAttribute("aria-label")).toBe(before.label);
  // The countdown itself still moves on screen.
  expect(container.querySelector("[data-sync-refresh-countdown]")?.textContent).not.toBe(before.visible);
});

// What a screen reader reads from the status live region: its text without
// the parts hidden from assistive technology.
const announcedText = (container: HTMLElement) => {
  const copy = container.querySelector('[role="status"][aria-live="polite"]')!.cloneNode(true) as HTMLElement;
  copy.querySelectorAll('[aria-hidden="true"]').forEach((node) => node.remove());
  return copy.textContent;
};

it.each([
  ["a healthy cluster", {}],
  ["failing status reads", { refreshNote: "Couldn't refresh status", retry: true }],
] as const)("announces nothing while only time passes, for %s", (_name, extra) => {
  vi.useFakeTimers();
  vi.setSystemTime(Date.parse("2026-10-26T09:00:00Z"));
  const retryAt = Date.now() + 20_000;
  const view = (age: number) => <KubernetesSyncStateIndicator state="ready" streamState="live" ageSeconds={age} firstReadyAt={published}
    refreshNote={"refreshNote" in extra ? extra.refreshNote : undefined} refreshRetryAt={"retry" in extra ? retryAt : undefined} />;
  const { container, rerender } = render(view(39));
  const seen = new Set([announcedText(container)]);
  // The page re-renders with a growing age as time passes; the shown age and
  // the countdown both move every second.
  for (let second = 1; second <= 15; second++) {
    act(() => { vi.advanceTimersByTime(1_000); });
    rerender(view(39 + second));
    seen.add(announcedText(container));
  }
  expect(shownText(container.querySelector("[data-sync-label]"))).toBe("Last synced 54s ago");
  expect([...seen]).toHaveLength(1);
  expect([...seen][0]).toBe("refreshNote" in extra ? "Last status known; Couldn't refresh status" : "Synced");
});

it("words a stale status as possibly out of date, never as a snapshot that is too old", () => {
  const { container } = render(<KubernetesSyncStateIndicator state="ready" streamState="live" ageSeconds={95} firstReadyAt={published} statusStaleSeconds={95} />);
  const line = shownText(container.querySelector("[data-sync-label]"));
  expect(line).toBe("Status may be out of date · last snapshot 2 minutes ago");
  expect(line).not.toMatch(/\bold\b|is \d/);
  expect(announcedText(container)).toBe("Status may be out of date");
});

it("shows a short stale label on a phone with the full line one tap away", () => {
  vi.useFakeTimers();
  const { container } = render(<KubernetesSyncStateIndicator state="ready" streamState="live" ageSeconds={130} firstReadyAt={published} statusStaleSeconds={130} />);
  const short = container.querySelector<HTMLElement>("[data-sync-label-short]")!;
  expect(short.textContent).toBe("May be out of date");
  expect(short.className.split(/\s+/)).toContain("sm:hidden");
  const long = short.previousElementSibling as HTMLElement;
  expect(long.className.split(/\s+/)).toEqual(expect.arrayContaining(["hidden", "sm:inline"]));
  expect(container.querySelector("[data-sync-label]")!.className.split(/\s+/)).toEqual(expect.arrayContaining(["min-w-0", "truncate"]));
  const trigger = screen.getByRole("button", { name: "Status details" });
  expect(trigger.className.split(/\s+/)).toEqual(expect.arrayContaining(["sm:hidden", "shrink-0"]));
  fireEvent.click(trigger);
  act(() => { vi.advanceTimersByTime(50); });
  expect(screen.getByRole("region", { name: "Status details" }).textContent).toBe("Status may be out of date · last snapshot 2 minutes ago");
});

it.each([
  ["a healthy status", {}, "Last synced 39s ago"],
  ["a stale status", { statusStaleSeconds: 130 }, "Status may be out of date · last snapshot 2 minutes ago"],
] as const)("describes %s with its full line, read on demand from outside the live region", (_name, extra, line) => {
  const { container } = render(<KubernetesSyncStateIndicator state="ready" streamState="live" ageSeconds={39} firstReadyAt={published} {...extra} />);
  const live = container.querySelector<HTMLElement>('[role="status"]')!;
  const id = live.getAttribute("aria-describedby");
  expect(id).toBeTruthy();
  const description = document.getElementById(id!)!;
  expect(description.textContent).toBe(line);
  expect(live.contains(description)).toBe(false);
});

it("says the last status is known, not synced, while status reads fail", () => {
  const { container } = render(<KubernetesSyncStateIndicator state="ready" streamState="live" ageSeconds={39} firstReadyAt={published} refreshNote="Couldn't refresh status" refreshRetryAt={Date.now() + 5_000} />);
  expect(announcedText(container)).toBe("Last status known; Couldn't refresh status");
  expect(announcedText(container)).not.toMatch(/Synced/);
});

it("keeps the phone detail buttons beside the status, outside its live region", () => {
  const { container } = render(<>
    <KubernetesSyncStateIndicator state="ready" streamState="live" ageSeconds={130} firstReadyAt={published} statusStaleSeconds={130} refreshNote="Couldn't refresh status" />
  </>);
  const live = container.querySelector<HTMLElement>('[role="status"]')!;
  for (const name of ["Status details", "Couldn't refresh status"]) {
    const button = screen.getByRole("button", { name });
    expect(live.contains(button), `${name} sits outside the live region`).toBe(false);
    expect(button.parentElement).toBe(live.parentElement);
    expect(button.className.split(/\s+/)).toEqual(expect.arrayContaining(["sm:hidden", "shrink-0"]));
  }
  expect(live.querySelector("button")).toBeNull();
});
