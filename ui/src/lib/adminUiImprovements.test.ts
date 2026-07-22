import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

// Guards the admin-UI improvements that are best pinned against the real source
// (the console's default env is node, and mounting TopBar / Sidebar / the
// settings controls needs the full react-query + router context). Like
// catalogReset.test.ts / agentChannels.test.ts, these read the actual files so
// a regression that re-introduces the old behaviour fails here:
//   2. the top-bar incident count is the AI/webhook SPLIT (via the shared
//      origin helpers + the ["incidents","list"] cache) and the sidebar shows
//      NO count at all;
//   3. the notification-channels panel is TABBED with a channel icon per tab,
//      carries no "YAML" wording, and keeps its enterprise gating + masked
//      write-only secrets;
//   4. the incidents-report settings section carries a discoverable info icon
//      that links to the /agent/incident-report docs page.

const here = path.dirname(fileURLToPath(import.meta.url)); // src/lib
const read = (rel: string) => readFileSync(path.resolve(here, rel), "utf8");

const topbar = read("../components/TopBar.tsx");
const sidebar = read("../components/Sidebar.tsx");
const hooks = read("./hooks.ts");
const channels = read("../components/AgentChannelsSettingsControl.tsx");
const report = read("../components/ReportSettingsControl.tsx");
const infoHint = read("../components/InfoHint.tsx");
const patternsPage = read("../pages/PatternsPage.tsx");
const learnedSignals = read("../pages/LearnedSignalsView.tsx");
const servicesPage = read("../pages/ServicesPage.tsx");

describe("Item 2 — top-bar AI/webhook split + no sidebar count", () => {
  it("useOpenIncidentCount reads the AUTHORITATIVE server counts, not a loaded page", () => {
    // The badge must never tally a bounded, loaded array — it reads the cheap
    // per-origin × per-status server count endpoint and takes the OPEN slice.
    expect(hooks.includes("countByOrigin")).toBe(false);
    expect(hooks.includes("api.incidentCounts()")).toBe(true);
    // Shared counts cache key the Now page uses too — one fetch, no rows.
    expect(/queryKey:\s*\["incidents",\s*"counts"\]/.test(hooks)).toBe(true);
    // Open grand total + per-origin open both come from by_status.open.
    expect(/by_status\?\.open/.test(hooks)).toBe(true);
  });

  it("the top bar renders the AI/webhook split via formatOriginCounts", () => {
    expect(topbar.includes("formatOriginCounts")).toBe(true);
    expect(topbar.includes("originCounts")).toBe(true);
    // The single lumped count is replaced by the formatted split in the Link.
    expect(topbar.includes("{formatOriginCounts(originCounts)}")).toBe(true);
  });

  it("the sidebar shows NO incident count (badge removed entirely)", () => {
    // No open-count source, no badge field, no badge render on the nav item.
    expect(sidebar.includes("useOpenIncidentCount")).toBe(false);
    expect(sidebar.includes("badge")).toBe(false);
    // The Incidents item is a plain text row that ends right after its label —
    // no trailing badge/count field. (Per-item icons now live on group headers.)
    expect(/label:\s*"Incidents"\s*}/.test(sidebar)).toBe(true);
  });
});

describe("Item 3 — channels panel: tabbed, icons, no YAML, gating preserved", () => {
  it("is TABBED — a tab per channel, a single active card (not six stacked)", () => {
    expect(channels.includes('role="tablist"')).toBe(true);
    expect(channels.includes('role="tab"')).toBe(true);
    expect(channels.includes("setActiveChannel")).toBe(true);
    // Exactly one ChannelCard is rendered, keyed on the active channel.
    expect(channels.includes("channel={activeChannel}")).toBe(true);
    expect((channels.match(/<ChannelCard/g) ?? []).length).toBe(1);
  });

  it("shows a channel icon before each channel name", () => {
    expect(channels.includes("ChannelIcon")).toBe(true);
    expect(/import \{ ChannelIcon \}/.test(channels)).toBe(true);
    expect(channels.includes("<ChannelIcon id={channel}")).toBe(true);
  });

  it("carries no 'YAML' wording in the panel copy (the wire 'yaml' source value stays)", () => {
    // The removed user-facing copy — the display chip, the toast, the button
    // title and the upsell all named YAML; none may remain.
    expect(channels.includes('"yaml config"')).toBe(false);
    expect(channels.includes("reverted to YAML")).toBe(false);
    expect(channels.includes("follow the YAML config")).toBe(false);
    expect(channels.includes("editing YAML")).toBe(false);
    expect(channels.includes("without editing YAML")).toBe(false);
  });

  it("PRESERVES the enterprise gating and masked write-only secrets", () => {
    expect(channels.includes("adminGateState")).toBe(true);
    expect(channels.includes("useEffectiveRole")).toBe(true);
    expect(channels.includes("LockedBody")).toBe(true);
    expect(/enabled:\s*gate === "admin"/.test(channels)).toBe(true);
    expect(
      channels.includes('type={f.secret && !showSecret[f.name] ? "password" : "text"}'),
    ).toBe(true);
    expect(channels.includes("buildChannelPut")).toBe(true);
    expect(channels.includes("localStorage")).toBe(false);
  });
});

describe("Item 4 — incidents-report settings info icon → docs", () => {
  it("InfoHint supports an optional href link", () => {
    expect(infoHint.includes("href")).toBe(true);
    expect(infoHint.includes("linkLabel")).toBe(true);
  });

  it("the report section renders an InfoHint linking to /agent/incident-report", () => {
    expect(report.includes("InfoHint")).toBe(true);
    expect(report.includes("/#/agent/incident-report")).toBe(true);
    expect(/href=\{INCIDENT_REPORT_DOCS\}/.test(report)).toBe(true);
  });
});

// ---- 2026-07-04 action-model unification + sidebar + proxy reveal ----------

describe("Unified action model — the ⋯ RowActionMenu is gone from all 3 pages", () => {
  it("no page imports or renders RowActionMenu anymore", () => {
    for (const src of [patternsPage, learnedSignals, servicesPage]) {
      expect(src.includes("RowActionMenu")).toBe(false);
    }
  });

  it("the checkbox action bar drives actions on logs, metrics/traces AND services", () => {
    for (const src of [patternsPage, learnedSignals, servicesPage]) {
      expect(src.includes("BulkActionBar")).toBe(true);
      expect(src.includes("SelectAllCheckbox")).toBe(true);
      expect(src.includes("RowSelectCheckbox")).toBe(true);
    }
  });

  it("logs + metrics/traces route Assign-to-service (reassign) through the bar", () => {
    // The bulk action id "reassign" opens the shared ReassignModal for the
    // selection on both learned-signal pages.
    for (const src of [patternsPage, learnedSignals]) {
      expect(/case "reassign":|spec\.id === "reassign"/.test(src)).toBe(true);
      expect(src.includes("ReassignModal")).toBe(true);
      expect(src.includes("reassignMatches")).toBe(true);
    }
  });
});

describe("Services page — grace bar + Grace column + status fix", () => {
  it("moves grace control into the action bar (no inline End/Restart per row)", () => {
    expect(servicesPage.includes("graceActionsForSelection")).toBe(true);
    // Grace is routed through the unified bulk-action handler (which also
    // carries Ignore/Resume + manual-service CRUD), not a grace-only handler.
    expect(servicesPage.includes("onBulkAction")).toBe(true);
    // The old inline per-row grace buttons ("End" / "Restart" text) are gone.
    expect(/>\s*End\s*<\/button>/.test(servicesPage)).toBe(false);
    expect(/>\s*Restart\s*<\/button>/.test(servicesPage)).toBe(false);
  });

  it("adds a Grace column driven by the server grace status", () => {
    expect(servicesPage.includes("graceRemainingLabel")).toBe(true);
    expect(servicesPage.includes("info.grace_seconds_remaining")).toBe(true);
  });

  it("fixes Status to read the server in_grace (same source as the detail page)", () => {
    // The old hard-coded <Pill tone="good">tracked</Pill> for every row is gone;
    // status now branches on info.in_grace.
    expect(/info\.in_grace \?/.test(servicesPage)).toBe(true);
  });
});

describe("Channels — Use proxy on its own line + reveal on check", () => {
  it("pulls use_proxy out of the field grid onto its own line", () => {
    expect(channels.includes('PROXY_FIELD = "use_proxy"')).toBe(true);
    expect(channels.includes("gridFields")).toBe(true);
    // The grid maps gridFields (proxy filtered out), not the raw schema.
    expect(channels.includes("gridFields.map")).toBe(true);
  });

  it("reveals the proxy reference only when Use proxy is checked", () => {
    expect(channels.includes("proxyOn")).toBe(true);
    expect(channels.includes("{proxyOn && <ProxyReference")).toBe(true);
    expect(channels.includes("function ProxyReference")).toBe(true);
  });

  it("the reveal references the deployment `proxy:` config (read-only), keeps gating + masking", () => {
    expect(channels.includes("proxy:")).toBe(true);
    // Gating + masked secrets preserved (guarded again here for this change).
    expect(channels.includes("adminGateState")).toBe(true);
    expect(
      channels.includes('type={f.secret && !showSecret[f.name] ? "password" : "text"}'),
    ).toBe(true);
  });
});
