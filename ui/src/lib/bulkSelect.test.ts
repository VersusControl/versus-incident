import { describe, it, expect } from "vitest";
import {
  allState,
  buildLogsBulkActions,
  buildSignalBulkActions,
  selectedInView,
  withAll,
  withToggled,
} from "@/lib/bulkSelect";
import type { RowActionId } from "@/lib/rowActions";

// Pure-logic tests for the unified select-all + action model shared by the
// three learned-signal admin pages. The console has no default DOM harness, so
// the contracts that matter are pinned here:
//   • select-all tri-state (none / some / all) against the visible keys;
//   • toggling one key and toggling all are immutable and only touch given keys;
//   • the action item SET per page and per scope — including that Assign-to-
//     service (reassign) is reachable via the selection on ALL pages, and the
//     gate that hides Ignore/Resume for a community / viewer session
//     (excludeVisible=false);
//   • a bulk action only ever operates on the keys currently on screen.

const ids = (specs: { id: RowActionId }[]) => specs.map((s) => s.id);

describe("selection maths", () => {
  it("withToggled flips a key in/out without mutating the input", () => {
    const a = new Set<string>(["x"]);
    const b = withToggled(a, "y");
    expect([...b].sort()).toEqual(["x", "y"]);
    const c = withToggled(b, "x");
    expect([...c]).toEqual(["y"]);
    // input untouched
    expect([...a]).toEqual(["x"]);
  });

  it("withAll adds or removes every given key, leaving others alone", () => {
    const base = new Set<string>(["keep"]);
    const added = withAll(base, ["a", "b"], true);
    expect([...added].sort()).toEqual(["a", "b", "keep"]);
    const removed = withAll(added, ["a", "b"], false);
    expect([...removed]).toEqual(["keep"]);
  });

  it("allState reports the header checkbox tri-state", () => {
    const keys = ["a", "b", "c"];
    expect(allState(new Set(), keys)).toBe("none");
    expect(allState(new Set(["a"]), keys)).toBe("some");
    expect(allState(new Set(["a", "b", "c"]), keys)).toBe("all");
    // an empty visible list is never "all" (nothing to select)
    expect(allState(new Set(["a"]), [])).toBe("none");
  });

  it("selectedInView returns only the visible selected keys, in visible order", () => {
    const sel = new Set(["c", "a", "gone"]);
    expect(selectedInView(sel, ["a", "b", "c"])).toEqual(["a", "c"]);
    // a key selected but no longer visible (page changed) is excluded
    expect(selectedInView(sel, ["a"])).toEqual(["a"]);
  });
});

describe("buildLogsBulkActions", () => {
  it("Active + admin: relabel (mark/clear) + gated Ignore + Assign-to-service", () => {
    const specs = buildLogsBulkActions({ scope: "active", excludeVisible: true });
    expect(ids(specs)).toEqual([
      "mark-known",
      "clear-verdict",
      "ignore",
      "reassign",
    ]);
    expect(specs.find((s) => s.id === "ignore")?.danger).toBe(true);
    // Assign-to-service reads plainly (not a danger action).
    expect(specs.find((s) => s.id === "reassign")?.label).toBe(
      "Assign to service",
    );
  });

  it("Active + community/viewer: relabel + Assign-to-service, Ignore gated out", () => {
    // Log attribution override is OSS, so Assign-to-service stays reachable even
    // without the enterprise exclude surface; only Ignore is gated away.
    const specs = buildLogsBulkActions({ scope: "active", excludeVisible: false });
    expect(ids(specs)).toEqual(["mark-known", "clear-verdict", "reassign"]);
  });

  it("Ignored + admin: Resume + Assign-to-service", () => {
    const specs = buildLogsBulkActions({ scope: "ignored", excludeVisible: true });
    expect(ids(specs)).toEqual(["resume", "reassign"]);
  });

  it("Ignored + community/viewer: Assign-to-service only", () => {
    const specs = buildLogsBulkActions({ scope: "ignored", excludeVisible: false });
    expect(ids(specs)).toEqual(["reassign"]);
  });
});

describe("buildSignalBulkActions", () => {
  it("Active + admin: gated Ignore + Assign-to-service (baselines are read-only)", () => {
    const specs = buildSignalBulkActions({ scope: "active", excludeVisible: true });
    expect(ids(specs)).toEqual(["ignore", "reassign"]);
    expect(specs.find((s) => s.id === "ignore")?.danger).toBe(true);
  });

  it("Ignored + admin: Resume + Assign-to-service", () => {
    const specs = buildSignalBulkActions({ scope: "ignored", excludeVisible: true });
    expect(ids(specs)).toEqual(["resume", "reassign"]);
  });

  it("community / viewer: NO actions at all (metric/trace surface is fully gated)", () => {
    // Unlike logs, metric/trace attribution override is enterprise-only, so the
    // whole set — including Assign-to-service — vanishes without the surface.
    expect(buildSignalBulkActions({ scope: "active", excludeVisible: false })).toEqual([]);
    expect(buildSignalBulkActions({ scope: "ignored", excludeVisible: false })).toEqual([]);
  });
});
