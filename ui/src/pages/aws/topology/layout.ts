import type { RelationKind, TopologyLink, TopologyNode } from "@/lib/awsEstate";
import { accountDisplay, linkLabel, linkSides, UNGROUPED_ID } from "@/lib/awsEstate";
import { dagreLayout, type DagreNode } from "./dagreLayout";

// The three Topology levels laid out by dagre: it places the cards and
// routes the lines, with each line's label as a box of its own, so no line
// crosses a card or a label. The map draws boxes as HTML and lines as one
// SVG under them, as the Kubernetes view does. The same answer always
// draws the same picture.

export interface Box {
  id: string;
  x: number;
  y: number;
  w: number;
  h: number;
}

/** A box's place on the canvas, as an inline style. */
export function boxStyle(b: Box): { left: number; top: number; width: number; height: number } {
  return { left: b.x, top: b.y, width: b.w, height: b.h };
}

export interface Link {
  key: string;
  from: string;
  to: string;
  /** The words drawn beside the line; `title` holds every kind. */
  label: string;
  title: string;
  kind: RelationKind;
  sides: Sides;
  /** The polyline, in canvas pixels. */
  points: Array<[number, number]>;
  /** The label's box, on the line; `labelLines` are its lines, top to bottom. */
  labelAt?: { x: number; y: number; w: number; h: number };
  labelLines: string[];
}

export interface Layout {
  width: number;
  height: number;
  boxes: Map<string, Box>;
  links: Link[];
  /** A frame drawn around the organization's accounts. */
  frame?: Box;
  /** What to fit in view on open when the whole map does not fit. */
  fit?: Box;
  /** Where to centre when even `fit` does not fit at the smallest scale. */
  focus?: Box;
  /**
   * Boxes that must stay in view when the target is larger than the view,
   * in priority order: the first (a heading) wins when they cannot all fit.
   */
  keep?: Box[];
}

/** The map's viewport, when known: layouts pick the direction that fits it best. */
export interface View {
  width: number;
  height: number;
}

/** The smallest scale a fitted map is drawn at: below it text stops being readable. */
export const MIN_SCALE = 0.6;
/** Clear space kept round a box that must stay in view. */
const KEEP_MARGIN = 8;
const FIT_PAD = 24;

/** The scale at which `box` fits `view`, at most 1 and at least the minimum. */
export function fitScale(box: { w: number; h: number }, view: View): number {
  const s = Math.min(1, (view.width - 2 * FIT_PAD) / box.w, (view.height - 2 * FIT_PAD) / box.h);
  return Math.max(MIN_SCALE, s);
}

/** The bounding box of boxes. */
export function bounds(boxes: Box[], id = "bounds"): Box | undefined {
  if (boxes.length === 0) return undefined;
  const x0 = Math.min(...boxes.map((b) => b.x));
  const y0 = Math.min(...boxes.map((b) => b.y));
  const x1 = Math.max(...boxes.map((b) => b.x + b.w));
  const y1 = Math.max(...boxes.map((b) => b.y + b.h));
  return { id, x: x0, y: y0, w: x1 - x0, h: y1 - y0 };
}

/**
 * How to open a map in `view`: the whole map when it fits at the minimum
 * scale or larger, else the layout's `fit` box, else the minimum scale
 * centred on its `focus`. Returns the scale and the scroll offset (in
 * scaled pixels) that put the chosen box in the middle of the view.
 */
export function fitView(layout: Pick<Layout, "width" | "height" | "fit" | "focus" | "keep">, view: View): { scale: number; left: number; top: number; target: Box } {
  const whole: Box = { id: "whole", x: 0, y: 0, w: layout.width, h: layout.height };
  const fits = (b: Box) => Math.min((view.width - 2 * FIT_PAD) / b.w, (view.height - 2 * FIT_PAD) / b.h) >= MIN_SCALE;
  const target = fits(whole) ? whole : layout.fit && fits(layout.fit) ? layout.fit : (layout.focus ?? layout.fit ?? whole);
  const scale = fitScale(target, view);
  // Centre the target with a margin on every side; one wider or taller than
  // the view starts at its left or top edge with the margin, so its
  // headings and first cards are never cut.
  const place = (start: number, size: number, viewSize: number) =>
    size * scale + 2 * FIT_PAD <= viewSize ? (start + size / 2) * scale - viewSize / 2 : start * scale - FIT_PAD;
  // When the whole map was too big, keep the named boxes in view as well:
  // the scroll stays inside the range that shows each with a margin, the
  // first box (the heading) winning when the range is empty.
  const keep = target.id === "whole" ? [] : (layout.keep ?? []);
  const within = (want: number, axis: "x" | "y", viewSize: number) => {
    let lo = -Infinity;
    let hi = Infinity;
    for (const b of keep) {
      const start = axis === "x" ? b.x : b.y;
      const size = axis === "x" ? b.w : b.h;
      const nextLo = (start + size) * scale - viewSize + KEEP_MARGIN;
      const nextHi = start * scale - KEEP_MARGIN;
      if (Math.max(lo, nextLo) > Math.min(hi, nextHi)) break;
      lo = Math.max(lo, nextLo);
      hi = Math.min(hi, nextHi);
    }
    return Math.min(Math.max(want, lo), hi);
  };
  const left = clampScroll(within(place(target.x, target.w, view.width), "x", view.width), layout.width * scale - view.width);
  const top = clampScroll(within(place(target.y, target.h, view.height), "y", view.height), layout.height * scale - view.height);
  return { scale, left, top, target };
}



/** Line style: how a link is known and who saw it. One side wins over kind. */
export type Sides = "both" | "one";

export function linkStyle(kind: RelationKind, sides: Sides): { tone: "plain" | "warn"; dash?: string; name: string } {
  if (sides === "one") return { tone: "warn", dash: "6 4", name: "seen from one side" };
  if (kind === "may_access") return { tone: "plain", dash: "2 4", name: "may access" };
  if (kind === "inferred") return { tone: "plain", dash: "6 4", name: "inferred" };
  return { tone: "plain", name: "configured" };
}

/** SVG path data for a polyline. */
export function pathData(points: Array<[number, number]>): string {
  return points.map(([x, y], i) => `${i === 0 ? "M" : "L"} ${x} ${y}`).join(" ");
}

export const LABEL_H = 18;

/** A label's lines: two kinds of link stack, so labels stay narrow. */
export function labelLines(text: string): string[] {
  return text.split(" · ");
}

/** A label's width in canvas pixels at 12 px. */
export function labelWidth(text: string): number {
  return Math.ceil(text.length * 6.8) + 14;
}

type Bundle = Omit<Link, "points" | "labelAt" | "labelLines"> & { sameRank?: boolean };

/**
 * Links joining the same two cards become one line that names its kinds,
 * so lines never stack: "Resource share · Transit gateway", or "5 kinds of
 * link" with every kind in the hover title.
 */
function bundle(links: TopologyLink[], present: Set<string>, style: "summary" | "detail", typeOf: (id: string) => string): Bundle[] {
  const groups = new Map<string, TopologyLink[]>();
  for (const l of links) {
    if (l.from === l.to || !present.has(l.from) || !present.has(l.to)) continue;
    const key = [l.from, l.to].sort().join("\u0000");
    groups.set(key, [...(groups.get(key) ?? []), l]);
  }
  return [...groups].map(([key, group]) => {
    const words = group.map((l) => linkLabel(l, style, typeOf(l.from)));
    const kinds = new Set(group.map((l) => l.kind));
    return {
      key,
      from: group[0].from,
      to: group[0].to,
      label: words.length <= 2 ? words.join(" · ") : `${words.length} kinds of link`,
      title: words.join(", "),
      kind: kinds.size === 1 ? group[0].kind : "configured",
      sides: group.some((l) => linkSides(l) === "one") ? "one" : "both",
    };
  });
}

/** Lay out cards and bundles with dagre and turn the result into a Layout. */
function lay(
  cards: DagreNode[],
  bundles: Bundle[],
  opts: Parameters<typeof dagreLayout>[2],
  /** Draw labels on the lines; without, they stay in each line's hover title (phone width). */
  withLabels = true,
): Layout & { frames: Map<string, Box> } {
  const r = dagreLayout(
    cards,
    bundles.map((b) => {
      const lines = labelLines(b.label);
      return withLabels
        ? { key: b.key, from: b.from, to: b.to, labelW: Math.max(...lines.map(labelWidth)), labelH: LABEL_H * lines.length, sameRank: b.sameRank }
        : { key: b.key, from: b.from, to: b.to, labelW: 1, labelH: 1, sameRank: b.sameRank };
    }),
    opts,
  );
  const links: Link[] = bundles.map((b) => {
    const e = r.edges.get(b.key);
    return { ...b, points: e?.points ?? [], labelLines: labelLines(b.label), labelAt: e?.label && withLabels ? { x: e.label.x, y: e.label.y, w: e.label.w, h: e.label.h } : undefined };
  });
  return { width: r.width, height: r.height, boxes: r.boxes, links, frames: r.frames };
}

// ---------- Accounts ----------

export const ACCOUNT_W = 232;
export const ACCOUNT_H = 120;
const OTHER_W = 288;
/** Card widths below this map width: narrower, so the accounts fit a phone. */
const PHONE = 640;
const ACCOUNT_W_PHONE = 150;
const OTHER_W_PHONE = 200;
const OTHER_H = 64;
const ORG = "frame:organization";
/** The Other accounts column: one card to dagre, its accounts drawn inside it. */
const COLUMN = "column:other-accounts";
const COLUMN_TITLE = 28;
const COLUMN_GAP = 16;

export interface AccountsPlan extends Layout {
  /** Accounts inside the organization frame: connected, or of the same organization. */
  inside: TopologyNode[];
  /** Other accounts and what the reads cannot see, in the column beside the frame. */
  outside: TopologyNode[];
  /** Where the Other accounts heading sits, over the column. */
  otherHeading?: Box;
}

/**
 * Connected accounts and those of the same organization sit inside the
 * organization's frame; other accounts and far ends outside it. Lines run
 * left to right, so the accounts we read stand left of what they reach.
 */
export function layoutAccounts(nodes: TopologyNode[], links: TopologyLink[], view?: View): AccountsPlan {
  const isInside = (n: TopologyNode) => n.kind === "account" && (n.label === "connected" || n.label === "same_org");
  const inside = nodes.filter(isInside);
  const insideIds = new Set(inside.map((n) => n.id));
  // At phone width the cards are narrower, so four accounts fit across at a readable scale.
  const compact = Boolean(view && view.width > 0 && view.width < PHONE);
  const aw = compact ? ACCOUNT_W_PHONE : ACCOUNT_W;
  const ow = compact ? OTHER_W_PHONE : OTHER_W;
  const size = (n: TopologyNode) =>
    n.label === "connected" ? { w: aw, h: ACCOUNT_H } : n.label === "same_org" ? { w: aw + 28, h: 72 } : { w: ow, h: n.label === "unknown" ? OTHER_H + 32 : OTHER_H };

  // Every other account stands in one Other accounts column beside the
  // frame, linked accounts first. dagre places the column as one card.
  const reach = new Map<string, number>();
  for (const l of links) for (const end of [l.from, l.to]) if (!insideIds.has(end)) reach.set(end, (reach.get(end) ?? 0) + 1);
  const outside = nodes.filter((n) => !isInside(n)).sort((p, q) => (reach.get(q.id) ?? 0) - (reach.get(p.id) ?? 0));
  const cellH = Math.max(0, ...outside.map((n) => size(n).h));
  const columnBox = (cols: number) => {
    const rows = Math.ceil(outside.length / cols);
    return { w: cols * ow + (cols - 1) * COLUMN_GAP, h: COLUMN_TITLE + rows * cellH + Math.max(0, rows - 1) * COLUMN_GAP };
  };

  // Lines between the organization's accounts stay as they are. An account's
  // lines to other accounts merge into one line to the column, named by how
  // many accounts it reaches; the hover lists each account and its links.
  const present = new Set([...insideIds]);
  // The lines carry no arrows, so for placement they point away from the
  // hub: the hub takes the first rank and its spokes the next, as in mock
  // 24, instead of a long chain across the frame.
  const degree = new Map<string, number>();
  for (const l of links) if (insideIds.has(l.from) && insideIds.has(l.to)) for (const e of [l.from, l.to]) degree.set(e, (degree.get(e) ?? 0) + 1);
  const hub = inside.find((n) => n.role === "hub")?.id ?? [...degree].sort((p, q) => q[1] - p[1])[0]?.[0];
  const depth = new Map<string, number>();
  if (hub) {
    depth.set(hub, 0);
    const queue = [hub];
    while (queue.length > 0) {
      const at = queue.shift() as string;
      for (const l of links) {
        const next = l.from === at ? l.to : l.to === at ? l.from : null;
        if (next && insideIds.has(next) && !depth.has(next)) {
          depth.set(next, (depth.get(at) ?? 0) + 1);
          queue.push(next);
        }
      }
    }
  }
  const own = bundle(links, present, "summary", () => "").map((b) => {
    const df = depth.get(b.from) ?? 99;
    const dt = depth.get(b.to) ?? 99;
    if (df === dt) return { ...b, sameRank: true };
    return df > dt ? { ...b, from: b.to, to: b.from } : b;
  });
  const outward = new Map<string, TopologyLink[]>();
  for (const l of links) {
    const fromIn = insideIds.has(l.from);
    const toIn = insideIds.has(l.to);
    if (fromIn === toIn) continue;
    const mine = fromIn ? l.from : l.to;
    outward.set(mine, [...(outward.get(mine) ?? []), l]);
  }
  const nameOf = new Map(nodes.map((n) => [n.id, n.kind === "account" ? accountDisplay({ id: n.account ?? n.id, name: n.name }) : n.description || n.name]));
  const merged: Bundle[] = [...outward].map(([mine, group]) => {
    const far = [...new Set(group.map((l) => (l.from === mine ? l.to : l.from)))];
    const kinds = new Set(group.map((l) => l.kind));
    const detail = far.map((id) => {
      const words = group.filter((l) => l.from === id || l.to === id).map((l) => linkLabel(l, "summary"));
      return `${nameOf.get(id) ?? id}: ${words.join(", ")}`;
    });
    return {
      key: `${mine}\u0000${COLUMN}`,
      from: mine,
      to: COLUMN,
      label: far.length === 1 ? `To 1 other account` : `To ${far.length} other accounts`,
      title: detail.join("; "),
      kind: kinds.size === 1 ? group[0].kind : "configured",
      sides: group.some((l) => linkSides(l) === "one") ? "one" : "both",
    };
  });

  const make = (rankdir: "LR" | "TB", cols: number): AccountsPlan => {
    const cards: DagreNode[] = [
      ...inside.map((n) => ({ id: n.id, ...size(n), parent: ORG })),
      ...(outside.length > 0 ? [{ id: COLUMN, ...columnBox(cols) }] : []),
    ];
    const plan = lay(cards, [...own, ...merged], { rankdir, nodesep: compact ? 20 : 28, ranksep: compact ? 40 : 48, looseColumns: 1, frameTitle: 32, looseTitle: 0 }, !compact);
    const frame = plan.frames.get(ORG);
    const column = plan.boxes.get(COLUMN);
    plan.boxes.delete(COLUMN);
    // The column's cards, under its heading, in reading order.
    if (column) {
      outside.forEach((n, i) => {
        const sz = size(n);
        plan.boxes.set(n.id, {
          id: n.id,
          x: column.x + (i % cols) * (ow + COLUMN_GAP),
          y: column.y + COLUMN_TITLE + Math.floor(i / cols) * (cellH + COLUMN_GAP),
          w: sz.w,
          h: sz.h,
        });
      });
    }
    const connected = inside.filter((n) => n.label === "connected").map((n) => plan.boxes.get(n.id)).filter((b): b is Box => Boolean(b));
    const heading = column ? { id: "heading:other", x: column.x, y: column.y, w: Math.min(column.w, 160), h: COLUMN_TITLE } : undefined;
    const firstCard = outside[0] ? plan.boxes.get(outside[0].id) : undefined;
    // Fit the frame, the Other accounts heading and its first card, so both headings are in view.
    const fit = bounds([frame, heading, firstCard].filter((b): b is Box => Boolean(b)), "fit");
    // The Organization heading first, then the connected accounts: what must stay in view.
    const orgHeading = frame ? { id: "heading:organization", x: frame.x + 20, y: frame.y + 16, w: 120, h: 12 } : undefined;
    const focus = bounds(connected, "connected");
    const keep = [orgHeading, focus].filter((b): b is Box => Boolean(b));
    return { ...plan, frame, inside, outside, fit, focus, keep, otherHeading: heading };
  };
  if (!view || view.width <= 0 || view.height <= 0) return make("LR", outside.length <= 5 ? 1 : 2);
  // With the view known, try both directions and one to three columns of
  // other accounts. Keep one where every connected account fits at a
  // readable scale, then the one whose fit box (frame, heading, first
  // card) opens largest; left to right and fewer columns win a tie.
  const scaleOf = (b: Box | undefined) => (b ? Math.min((view.width - 2 * FIT_PAD) / b.w, (view.height - 2 * FIT_PAD) / b.h) : 0);
  const options = (["LR", "TB"] as const).flatMap((r) => (compact ? [1] : [1, 2, 3]).filter((c) => c === 1 || outside.length > c).map((c) => make(r, c)));
  const score = (p: AccountsPlan) => [scaleOf(p.focus) >= MIN_SCALE ? 1 : 0, Math.min(1, scaleOf(p.fit)), scaleOf(p.focus)];
  return options.reduce((best, p) => {
    const [a1, a2, a3] = score(best);
    const [b1, b2, b3] = score(p);
    return b1 > a1 || (b1 === a1 && (b2 > a2 * 1.02 || (Math.abs(b2 - a2) <= a2 * 0.02 && b3 > a3 * 1.02))) ? p : best;
  });
}

/**
 * With the viewport known, lay out both ways and keep the one whose fit box
 * opens at the larger scale; left to right wins a tie, as in the mock-ups.
 */
function pick<T extends Layout>(make: (rankdir: "LR" | "TB") => T, view?: View, prefer: "LR" | "TB" = "LR"): T {
  const first = make(prefer);
  if (!view || view.width <= 0 || view.height <= 0) return first;
  const second = make(prefer === "LR" ? "TB" : "LR");
  const score = (p: T) => {
    const box = p.fit ?? { w: p.width, h: p.height };
    return Math.min((view.width - 2 * FIT_PAD) / box.w, (view.height - 2 * FIT_PAD) / box.h);
  };
  return score(second) > score(first) * 1.05 ? second : first;
}

// ---------- Groups, and the members of one group ----------

export const GROUP_W = 232;
export const GROUP_H = 88;
const PILL_H = 28;
const PILL_W = 160;

export interface GroupsPlan extends Layout {
  /** The account's groups, or the opened group's members. */
  cards: TopologyNode[];
  /** Ends outside them (other accounts, other groups, what cannot be seen), as pills. */
  pills: TopologyNode[];
}

/** Groups and the pills they link to, top to bottom; groups with no line in a three-column grid under them. */
export function layoutGroups(
  nodes: TopologyNode[],
  links: TopologyLink[],
  isCard: (n: TopologyNode) => boolean,
  columns = 3,
  view?: View,
  selected?: string | null,
): GroupsPlan {
  const cards = nodes.filter(isCard);
  const pills = nodes.filter((n) => !isCard(n));
  const sizeOf = (n: TopologyNode) =>
    !isCard(n) ? { w: PILL_W, h: PILL_H } : n.kind === "group" && n.group === UNGROUPED_ID ? { w: GROUP_W * 2, h: GROUP_H } : { w: GROUP_W, h: GROUP_H };
  const present = new Set(nodes.map((n) => n.id));
  const bundles = bundle(links, present, "summary", () => "");
  const make = (rankdir: "LR" | "TB"): GroupsPlan => {
    const plan = lay(
      [...pills, ...cards].map((n) => ({ id: n.id, ...sizeOf(n) })),
      bundles,
      { rankdir, nodesep: 32, ranksep: 56, looseColumns: columns },
    );
    const chosen = selected ? plan.boxes.get(selected) : undefined;
    return { ...plan, cards, pills, focus: chosen ?? bounds(cards.map((n) => plan.boxes.get(n.id)).filter((b): b is Box => Boolean(b)).slice(0, 1)) };
  };
  return pick(make, view, "TB");
}

// ---------- Focus ----------

export const NODE_W = 212;
export const NODE_H = 60;

/**
 * What points at the resource above it, what it points at below: the
 * picture reads top-down like a request. `depth` is each node's distance
 * from the resource, negative above.
 */
export function layoutFocus(nodes: TopologyNode[], links: TopologyLink[], ref: string): Layout & { depth: Map<string, number> } {
  const ids = new Set(nodes.map((n) => n.id));
  const depth = new Map<string, number>();
  const queue: string[] = [];
  if (ids.has(ref)) {
    depth.set(ref, 0);
    queue.push(ref);
  }
  while (queue.length > 0) {
    const id = queue.shift() as string;
    const d = depth.get(id) ?? 0;
    for (const e of links) {
      if (e.from === id && ids.has(e.to) && !depth.has(e.to)) {
        depth.set(e.to, d + 1);
        queue.push(e.to);
      } else if (e.to === id && ids.has(e.from) && !depth.has(e.from)) {
        depth.set(e.from, d - 1);
        queue.push(e.from);
      }
    }
  }
  const types = new Map(nodes.map((n) => [n.id, n.type ?? ""]));
  const plan = lay(
    nodes.map((n) => ({ id: n.id, w: NODE_W, h: NODE_H })),
    bundle(links, ids, "detail", (id) => types.get(id) ?? ""),
    { rankdir: "TB", nodesep: 40, ranksep: 56, looseColumns: 3 },
  );
  // Focus opens centred on the resource itself.
  return { ...plan, depth, focus: plan.boxes.get(ref) };
}

/** Where a box should sit in view: its centre in the middle of the scroll area. */
export function centredScroll(
  box: { x: number; y: number; w: number; h: number },
  view: { width: number; height: number },
  content: { width: number; height: number },
): { left: number; top: number } {
  const left = clampScroll(box.x + box.w / 2 - view.width / 2, content.width - view.width);
  const top = clampScroll(box.y + box.h / 2 - view.height / 2, content.height - view.height);
  return { left, top };
}

function clampScroll(v: number, max: number): number {
  return Math.round(Math.max(0, Math.min(v, Math.max(0, max))));
}
