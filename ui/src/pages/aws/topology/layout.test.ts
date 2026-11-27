import { describe, expect, it } from "vitest";
import type { TopologyLink, TopologyNode } from "@/lib/awsEstate";
import { accountsTopology, focusTopology, groupsTopology, TG } from "../shared/estateFixtures";
import { centredScroll, fitView, layoutAccounts, layoutFocus, layoutGroups, linkStyle, MIN_SCALE, type Box } from "./layout";

function overlaps(a: { x: number; y: number; w: number; h: number }, b: { x: number; y: number; w: number; h: number }): boolean {
  return a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h;
}

function noOverlap(boxes: Map<string, Box>) {
  const list = [...boxes.values()];
  for (let i = 0; i < list.length; i++)
    for (let j = i + 1; j < list.length; j++) expect(overlaps(list[i], list[j]), `${list[i].id} / ${list[j].id}`).toBe(false);
}

function manyAccounts(n: number): { nodes: TopologyNode[]; links: TopologyLink[] } {
  return {
    nodes: Array.from({ length: n }, (_, i) => ({ id: `account:${100000000000 + i}`, kind: "account" as const, name: `a${i}`, label: "connected" as const })),
    links: Array.from({ length: n - 1 }, (_, i) => ({
      from: `account:${100000000001 + i}`,
      to: "account:100000000000",
      type: "attached_to_transit",
      kind: "configured" as const,
      count: 1,
      both_sides: 1,
      seen_from: "both" as const,
    })),
  };
}

describe("Topology layouts", () => {
  it("keeps the Northwind accounts apart, with other accounts outside the organization frame", () => {
    const v = accountsTopology();
    const plan = layoutAccounts(v.nodes, v.links);
    noOverlap(plan.boxes);
    const frame = plan.frame as Box;
    for (const n of plan.inside) {
      const b = plan.boxes.get(n.id) as Box;
      expect(b.x >= frame.x && b.y >= frame.y && b.x + b.w <= frame.x + frame.w && b.y + b.h <= frame.y + frame.h, n.id).toBe(true);
    }
    for (const n of plan.outside) expect(overlaps(plan.boxes.get(n.id) as Box, frame), n.id).toBe(false);
  });

  it("keeps 1 to 40 accounts apart", () => {
    for (const n of [1, 2, 3, 4, 5, 6, 12, 40]) {
      const m = manyAccounts(n);
      const plan = layoutAccounts(m.nodes, m.links);
      noOverlap(plan.boxes);
      expect(plan.boxes.size).toBe(n);
    }
  });

  it("keeps groups, Ungrouped and the account pills apart", () => {
    const isGroup = (n: TopologyNode) => n.kind === "group";
    const v = groupsTopology();
    noOverlap(layoutGroups(v.nodes, v.links, isGroup).boxes);
    const many = Array.from({ length: 30 }, (_, i) => ({ id: `group:1/prefix:g${i}-`, kind: "group" as const, name: `g${i}`, group: `prefix:g${i}-`, rule: "prefix" }));
    const pills = Array.from({ length: 8 }, (_, i) => ({ id: `account:${i}`, kind: "account" as const, name: `x${i}` }));
    const links = pills.map((p, i) => ({ from: p.id, to: many[i].id, type: "trusts", kind: "may_access" as const, count: 1, both_sides: 0, seen_from: "one" as const }));
    noOverlap(layoutGroups([...many, ...pills], links, isGroup).boxes);
  });

  it("puts what points at the resource above it and what it points at below", () => {
    const v = focusTopology();
    const plan = layoutFocus(v.nodes, v.links, TG);
    noOverlap(plan.boxes);
    expect(plan.depth.get(TG)).toBe(0);
    expect((plan.boxes.get("lb") as Box).y).toBeLessThan((plan.boxes.get(TG) as Box).y);
    expect((plan.boxes.get("svc") as Box).y).toBeGreaterThan((plan.boxes.get(TG) as Box).y);
  });

  it("draws one side in amber over any kind, and may access unlike configured", () => {
    expect(linkStyle("configured", "one").tone).toBe("warn");
    expect(linkStyle("may_access", "both").dash).toBe("2 4");
    expect(linkStyle("configured", "both").dash).toBeUndefined();
    expect(linkStyle("inferred", "both").tone).toBe("plain");
  });
});

describe("dense maps draw clear of every box and label", () => {
  // Five kinds of link between the same two accounts, one-side links to a
  // column of other accounts, and links that would pass through a third
  // account: the shapes QA Visual saw on the real estate.
  const ids = ["platform", "prod", "network", "data"];
  const nodes: TopologyNode[] = [
    ...ids.map((n) => ({ id: `account:${n}`, kind: "account" as const, name: n, label: "connected" as const, role: n === "network" ? ("hub" as const) : undefined })),
    { id: "account:mgmt", kind: "account", name: "mgmt", label: "same_org" },
    ...[1, 2, 3, 4, 5].map((i) => ({ id: `account:u${i}`, kind: "account" as const, name: `u${i}`, label: "unknown" as const })),
    { id: "other:host", kind: "other", name: "host", dangling: true },
  ];
  const link = (from: string, to: string, type: string, seen: "both" | "one" = "both"): TopologyLink => ({
    from: `account:${from}`,
    to: to.startsWith("other:") ? to : `account:${to}`,
    type,
    kind: type === "trusts" ? "may_access" : "configured",
    count: 3,
    both_sides: seen === "both" ? 3 : 0,
    seen_from: seen,
  });
  const links: TopologyLink[] = [
    ...["shared_via", "trusts", "alias_of", "attached_to_transit", "may_access"].map((t) => link("platform", "network", t)),
    ...["shared_via", "attached_to_transit"].map((t) => link("prod", "network", t)),
    ...["shared_via", "attached_to_transit"].map((t) => link("data", "network", t)),
    link("prod", "data", "may_access"),
    link("platform", "mgmt", "trusts", "one"),
    ...[1, 2, 3, 4, 5].map((i) => link(i % 2 ? "prod" : "data", `u${i}`, "trusts", "one")),
    link("network", "other:host", "alias_of", "one"),
  ];

  it("bundles every pair into one line", () => {
    const plan = layoutAccounts(nodes, links);
    const pairs = new Set(plan.links.map((l) => [l.from, l.to].sort().join("|")));
    expect(pairs.size).toBe(plan.links.length);
    // Placement points lines away from the hub; either way, one line names all five kinds.
    const pn = plan.links.find((l) => [l.from, l.to].sort().join("|") === "account:network|account:platform");
    expect(pn?.title.split(", ")).toHaveLength(5);
  });

  it.each([
    ["Accounts", () => layoutAccounts(nodes, links)],
    ["Groups", () => {
      const v = groupsTopology();
      return layoutGroups(v.nodes, v.links, (n) => n.kind === "group");
    }],
    ["Focus", () => {
      const v = focusTopology();
      return layoutFocus(v.nodes, v.links, TG);
    }],
  ])("%s: no line crosses a card or a label, no label overlaps, nothing runs along the frame", (_name, make) => {
    const plan = make();
    // A line between two cards of one rank keeps its words in its hover title only.
    const labels = plan.links.filter((l) => l.labelAt).map((l) => ({ id: l.key, x: l.labelAt!.x, y: l.labelAt!.y, w: l.labelAt!.w, h: l.labelAt!.h }));
    for (const l of plan.links) {
      expect(l.points.length, l.key).toBeGreaterThanOrEqual(2);
      expect(l.title.length, l.key).toBeGreaterThan(0);
      for (let i = 1; i < l.points.length; i++) {
        const [p, q] = [l.points[i - 1], l.points[i]];
        for (const b of plan.boxes.values()) {
          if (b.id === l.from || b.id === l.to) continue;
          expect(segmentHits(p, q, b), `${l.key} through card ${b.id}`).toBe(false);
        }
        // A line's own label sits on it, as a gap; every other label stays clear.
        for (const r of labels) if (r.id !== l.key) expect(segmentHits(p, q, r), `${l.key} through label ${r.id}`).toBe(false);
        if (plan.frame) {
          const f = plan.frame;
          const along = (a: number, b: number, edge: number) => Math.abs(a - edge) < 3 && Math.abs(b - edge) < 3;
          expect(along(p[1], q[1], f.y) || along(p[1], q[1], f.y + f.h) || along(p[0], q[0], f.x) || along(p[0], q[0], f.x + f.w), `${l.key} along the frame`).toBe(false);
        }
      }
    }
    for (const r of labels) {
      for (const b of plan.boxes.values()) expect(overlaps(r, b), `${r.id} over ${b.id}`).toBe(false);
      for (const o of labels) if (o !== r) expect(overlaps(r, o), `${r.id} over ${o.id}`).toBe(false);
      expect(r.x + r.w).toBeLessThanOrEqual(plan.width);
      expect(r.y + r.h).toBeLessThanOrEqual(plan.height);
    }
    for (const b of plan.boxes.values()) {
      expect(b.x, b.id).toBeGreaterThanOrEqual(0);
      expect(b.x + b.w, b.id).toBeLessThanOrEqual(plan.width);
    }
  });

  // A larger estate built from a fixed seed: a dozen groups, pills for
  // other accounts and what cannot be seen, and links of mixed kinds.
  function seeded(seed: number) {
    let x = seed;
    const rnd = () => ((x = (x * 1103515245 + 12345) % 2147483648) / 2147483648);
    const groups = Array.from({ length: 12 }, (_, i) => ({ id: `group:a/g${i}`, kind: "group" as const, name: `g${i}`, group: i === 11 ? "ungrouped" : `g${i}` }));
    const pills = Array.from({ length: 5 }, (_, i) => ({ id: `account:p${i}`, kind: "account" as const, name: `p${i}` }));
    const all = [...groups, ...pills];
    const types = ["in_vpc", "in_subnet", "uses_security_group", "shared_via", "attached_to_transit", "trusts", "dead_letter"];
    const ls: TopologyLink[] = Array.from({ length: 28 }, () => {
      const from = all[Math.floor(rnd() * all.length)].id;
      const to = all[Math.floor(rnd() * all.length)].id;
      return { from, to, type: types[Math.floor(rnd() * types.length)], kind: "configured" as const, count: 1 + Math.floor(rnd() * 4), both_sides: 0, seen_from: rnd() > 0.7 ? ("one" as const) : ("" as const) };
    });
    return { nodes: all as TopologyNode[], links: ls };
  }

  it.each([1, 7, 42, 1234])("a seeded estate (%i) keeps every line clear of cards and labels", (seed) => {
    const { nodes: ns, links: ls } = seeded(seed);
    const plan = layoutGroups(ns, ls, (n) => n.kind === "group");
    noOverlap(plan.boxes);
    const labels = plan.links.filter((l) => l.labelAt).map((l) => ({ id: l.key, x: l.labelAt!.x, y: l.labelAt!.y, w: l.labelAt!.w, h: l.labelAt!.h }));
    for (const l of plan.links)
      for (let i = 1; i < l.points.length; i++) {
        const [p, q] = [l.points[i - 1], l.points[i]];
        for (const b of plan.boxes.values()) if (b.id !== l.from && b.id !== l.to) expect(segmentHits(p, q, b), `${l.key} through ${b.id}`).toBe(false);
        for (const r of labels) if (r.id !== l.key) expect(segmentHits(p, q, r), `${l.key} through label ${r.id}`).toBe(false);
      }
    for (const r of labels) {
      for (const b of plan.boxes.values()) expect(overlaps(r, b), `${r.id} over ${b.id}`).toBe(false);
      for (const o of labels) if (o !== r) expect(overlaps(r, o), `${r.id} over ${o.id}`).toBe(false);
    }
  });

  it("wraps the account pills above the groups inside the grid's width", () => {
    const pills = Array.from({ length: 9 }, (_, i) => ({ id: `account:p${i}`, kind: "account" as const, name: `p${i}` }));
    const groups = Array.from({ length: 6 }, (_, i) => ({ id: `group:a/g${i}`, kind: "group" as const, name: `g${i}`, group: `g${i}` }));
    const plan = layoutGroups([...groups, ...pills], [], (n) => n.kind === "group");
    noOverlap(plan.boxes);
    const gridRight = Math.max(...groups.map((g) => (plan.boxes.get(g.id) as Box).x + (plan.boxes.get(g.id) as Box).w));
    for (const p of pills) expect((plan.boxes.get(p.id) as Box).x + (plan.boxes.get(p.id) as Box).w).toBeLessThanOrEqual(gridRight);
  });

  it("centres a box in the view, held inside the content", () => {
    expect(centredScroll({ x: 900, y: 400, w: 200, h: 60 }, { width: 600, height: 300 }, { width: 2000, height: 1000 })).toEqual({ left: 700, top: 280 });
    expect(centredScroll({ x: 10, y: 10, w: 200, h: 60 }, { width: 600, height: 300 }, { width: 2000, height: 1000 })).toEqual({ left: 0, top: 0 });
    expect(centredScroll({ x: 1900, y: 950, w: 100, h: 50 }, { width: 600, height: 300 }, { width: 2000, height: 1000 })).toEqual({ left: 1400, top: 700 });
  });
});

/** Whether a segment passes through the inside of a box (Liang-Barsky clip against the box shrunk by 1 px). */
function segmentHits(p: [number, number], q: [number, number], b: { x: number; y: number; w: number; h: number }): boolean {
  const [x0, y0] = p;
  const dx = q[0] - x0;
  const dy = q[1] - y0;
  let t0 = 0;
  let t1 = 1;
  const edges: Array<[number, number]> = [
    [-dx, x0 - (b.x + 1)],
    [dx, b.x + b.w - 1 - x0],
    [-dy, y0 - (b.y + 1)],
    [dy, b.y + b.h - 1 - y0],
  ];
  for (const [pp, qq] of edges) {
    if (pp === 0) {
      if (qq < 0) return false;
      continue;
    }
    const t = qq / pp;
    if (pp < 0) t0 = Math.max(t0, t);
    else t1 = Math.min(t1, t);
    if (t0 > t1) return false;
  }
  return t1 - t0 > 1e-6;
}

describe("maps open fitted to their view", () => {
  // The two viewports QA measured: the map area at 1440 (1166 x 675) and at 390 with view=map.
  const VIEWS = [
    ["1440", { width: 1166, height: 675 }],
    ["390", { width: 342, height: 600 }],
  ] as const;
  const dense = () => {
    const ids = ["platform", "prod", "network", "data"];
    const ns: TopologyNode[] = [
      ...ids.map((n) => ({ id: `account:${n}`, kind: "account" as const, name: n, label: "connected" as const })),
      { id: "account:mgmt", kind: "account", name: "mgmt", label: "same_org" },
      ...[1, 2, 3, 4, 5].map((i) => ({ id: `account:u${i}`, kind: "account" as const, name: `u${i}`, label: "unknown" as const })),
      { id: "other:host", kind: "other", name: "host", dangling: true },
    ];
    const l = (from: string, to: string, type: string, seen: "both" | "one" = "both"): TopologyLink => ({
      from: `account:${from}`, to: to.startsWith("other:") ? to : `account:${to}`, type, kind: "configured", count: 3, both_sides: 0, seen_from: seen,
    });
    const ls = [
      ...["shared_via", "trusts", "alias_of", "attached_to_transit", "may_access"].map((t) => l("platform", "network", t)),
      ...["shared_via", "attached_to_transit"].map((t) => l("prod", "network", t)),
      ...["shared_via", "attached_to_transit"].map((t) => l("data", "network", t)),
      l("prod", "data", "may_access"), l("platform", "mgmt", "trusts", "one"),
      ...[1, 2, 3, 4, 5].map((i) => l(i % 2 ? "prod" : "data", `u${i}`, "trusts", "one")),
      l("network", "other:host", "alias_of", "one"),
    ];
    return { nodes: ns, links: ls };
  };
  const estates = [
    ["the dense estate", dense],
    ["the 4-account estate", () => {
      const v = accountsTopology();
      return { nodes: v.nodes, links: v.links };
    }],
    ["a first read with no lines yet", () => {
      const v = accountsTopology();
      return { nodes: v.nodes, links: [] };
    }],
  ] as const;

  for (const [estateName, make] of estates)
    for (const [viewName, view] of VIEWS)
      it(`${estateName} at ${viewName}: every connected account card is in view after the fit`, () => {
        const { nodes: ns, links: ls } = make();
        const plan = layoutAccounts(ns, ls, view);
        const at = fitView(plan, view);
        const inView = (b: Box) =>
          b.x * at.scale >= at.left - 1 &&
          (b.x + b.w) * at.scale <= at.left + view.width + 1 &&
          b.y * at.scale >= at.top - 1 &&
          (b.y + b.h) * at.scale <= at.top + view.height + 1;
        const connected = ns.filter((n) => n.label === "connected");
        const shown = connected.filter((n) => inView(plan.boxes.get(n.id) as Box)).map((n) => n.id);
        expect(shown).toEqual(connected.map((n) => n.id));
        expect(at.scale).toBeGreaterThanOrEqual(MIN_SCALE);
        if (viewName === "1440") {
          // Both headings are in view: the frame's title and the one over the first outside card.
          const frame = plan.frame as Box;
          expect(inView({ id: "org-heading", x: frame.x + 20, y: frame.y + 16, w: 120, h: 12 })).toBe(true);
          const first = plan.outside.map((n) => plan.boxes.get(n.id) as Box).sort((p, q) => p.y - q.y || p.x - q.x)[0];
          if (first) expect(inView({ id: "other-heading", x: first.x, y: Math.max(4, first.y - 22), w: 120, h: 12 })).toBe(true);
        }
      });

  it("fits a Groups view of ten groups beside the panel, or opens on the selected group", () => {
    const groups = Array.from({ length: 10 }, (_, i) => ({ id: `group:a/g${i}`, kind: "group" as const, name: `g${i}`, group: `g${i}` }));
    const view = { width: 782, height: 675 };
    const plan = layoutGroups(groups, [], (n) => n.kind === "group", 3, view, "group:a/g7");
    const at = fitView(plan, view);
    const inView = (b: Box) => b.x * at.scale >= at.left - 1 && (b.x + b.w) * at.scale <= at.left + view.width + 1 && b.y * at.scale >= at.top - 1 && (b.y + b.h) * at.scale <= at.top + view.height + 1;
    const shown = groups.filter((g) => inView(plan.boxes.get(g.id) as Box));
    if (shown.length < groups.length) expect(inView(plan.boxes.get("group:a/g7") as Box)).toBe(true);
    else expect(at.target.id).toBe("whole");
    expect(shown.length).toBe(groups.length);
  });
});

describe("the settled estate opens with the frame and the Other accounts column", () => {
  // Five connected accounts (one never read), the management account, nine
  // outside accounts, and about twenty one-side lines from the accounts to
  // them: the estate QA Visual saw once every read had settled.
  function settled() {
    const ids = ["network", "prod", "platform", "data", "sandbox"];
    const ns: TopologyNode[] = [
      ...ids.map((n) => ({
        id: `account:${n}`,
        kind: "account" as const,
        name: n,
        label: "connected" as const,
        role: n === "network" ? ("hub" as const) : ("spoke" as const),
        read: n === "sandbox" ? ("not_read" as const) : ("read" as const),
      })),
      { id: "account:mgmt", kind: "account", name: "Management", label: "same_org" },
      ...Array.from({ length: 9 }, (_, i) => ({ id: `account:o${i}`, kind: "account" as const, name: `o${i}`, label: (i < 7 ? "unknown" : "known") as "unknown" | "known" })),
    ];
    const l = (from: string, to: string, type: string, seen: "both" | "one" = "both"): TopologyLink => ({
      from: `account:${from}`, to: `account:${to}`, type, kind: "configured", count: 2, both_sides: 0, seen_from: seen,
    });
    const ls: TopologyLink[] = [
      ...["prod", "platform", "data"].flatMap((s) => ["shared_via", "attached_to_transit"].map((t) => l(s, "network", t))),
      l("platform", "prod", "alias_of"),
      l("prod", "data", "may_access"),
      l("platform", "mgmt", "trusts", "one"),
      ...["network", "prod", "platform", "data"].flatMap((s, si) => Array.from({ length: 5 }, (_, k) => l(s, `o${(si * 2 + k) % 9}`, k % 2 ? "trusts" : "targets", "one"))),
    ];
    return { nodes: ns, links: ls };
  }

  it("at 390 opens with the Organization heading and the frame's top-left in view, and every connected card", () => {
    const view = { width: 342, height: 600 };
    const { nodes: ns, links: ls } = settled();
    const plan = layoutAccounts(ns, ls, view);
    const at = fitView(plan, view);
    const frame = plan.frame as Box;
    const heading = { x: frame.x + 20, y: frame.y + 16, w: 120, h: 12 };
    expect(heading.x * at.scale - at.left).toBeGreaterThanOrEqual(0);
    expect(heading.y * at.scale - at.top).toBeGreaterThanOrEqual(0);
    expect((heading.x + heading.w) * at.scale - at.left).toBeLessThanOrEqual(view.width);
    expect((heading.y + heading.h) * at.scale - at.top).toBeLessThanOrEqual(view.height);
    // Not scrolled down past the frame's top edge.
    expect(at.top).toBeLessThanOrEqual(frame.y * at.scale);
    for (const n of ns.filter((x) => x.label === "connected")) {
      const b = plan.boxes.get(n.id) as Box;
      expect(b.x * at.scale - at.left, n.id).toBeGreaterThanOrEqual(0);
      expect((b.x + b.w) * at.scale - at.left, n.id).toBeLessThanOrEqual(view.width);
      expect(b.y * at.scale - at.top, n.id).toBeGreaterThanOrEqual(0);
      expect((b.y + b.h) * at.scale - at.top, n.id).toBeLessThanOrEqual(view.height);
    }
  });

  for (const [viewName, view] of [["1440", { width: 1166, height: 675 }], ["390", { width: 342, height: 600 }]] as const) {
    it(`at ${viewName}: every connected card is in view and nothing is cut at the view edge`, () => {
      const { nodes: ns, links: ls } = settled();
      const plan = layoutAccounts(ns, ls, view);
      const at = fitView(plan, view);
      const scaled = (b: Box) => ({ x0: b.x * at.scale - at.left, x1: (b.x + b.w) * at.scale - at.left, y0: b.y * at.scale - at.top, y1: (b.y + b.h) * at.scale - at.top });
      const inside = (b: Box) => {
        const r = scaled(b);
        return r.x0 >= -0.5 && r.x1 <= view.width + 0.5 && r.y0 >= -0.5 && r.y1 <= view.height + 0.5;
      };
      const outsideView = (b: Box) => {
        const r = scaled(b);
        return r.x1 <= 0.5 || r.x0 >= view.width - 0.5 || r.y1 <= 0.5 || r.y0 >= view.height - 0.5;
      };
      for (const n of ns.filter((x) => x.label === "connected")) expect(inside(plan.boxes.get(n.id) as Box), n.id).toBe(true);
      const frame = plan.frame as Box;
      const headings: Box[] = [{ id: "organization", x: frame.x + 20, y: frame.y + 16, w: 120, h: 12 }];
      if (plan.otherHeading) headings.push({ ...plan.otherHeading, y: plan.otherHeading.y + 4, h: 12, w: 130 });
      // The left and top edges never cut a card or heading; at 1440 no edge does.
      for (const b of [...plan.boxes.values(), ...headings]) {
        const r = scaled(b);
        const cutLeftOrTop = (r.x0 < -0.5 && r.x1 > 0.5) || (r.y0 < -0.5 && r.y1 > 0.5);
        expect(cutLeftOrTop, `${b.id} cut at the left or top edge`).toBe(false);
        if (viewName === "1440") expect(inside(b) || outsideView(b), `${b.id} cut at an edge`).toBe(true);
      }
      if (viewName === "1440") {
        for (const h of headings) expect(inside(h), `heading ${h.id}`).toBe(true);
        expect(inside(plan.boxes.get(plan.outside[0].id) as Box), "first outside card").toBe(true);
      }
      // One line from each account to the column, not one per outside account.
      const toColumn = plan.links.filter((k) => k.to.startsWith("column:"));
      expect(toColumn.length).toBeLessThanOrEqual(5);
      expect(toColumn.every((k) => /^To \d+ other accounts?$/.test(k.label) && k.title.includes(":"))).toBe(true);
    });
  }
});
