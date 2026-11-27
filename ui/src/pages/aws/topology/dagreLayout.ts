import dagre from "@dagrejs/dagre";

// Node placement and edge routing by dagre. Each line's label is laid out
// as a box on its own line, so other lines route around it as they do
// around cards; its opaque background reads as a gap in its own line. We
// still draw everything ourselves. Cards with no line and no
// frame are not handed to dagre (it would put them all in one long row):
// they stand in a grid under the graph.

export interface DagreNode {
  id: string;
  w: number;
  h: number;
  /** Nodes with the same parent are drawn inside one frame. */
  parent?: string;
}

export interface DagreEdge {
  key: string;
  from: string;
  to: string;
  labelW: number;
  labelH: number;
  /**
   * The two ends sit in one rank (two spokes of a hub): dagre would push
   * them a rank apart, so the line stays out of its ranking and is routed
   * afterwards along a free band beside the rank. Its label is hover-only.
   */
  sameRank?: boolean;
}

export interface DagreBox {
  id: string;
  x: number;
  y: number;
  w: number;
  h: number;
}

export interface DagreResult {
  width: number;
  height: number;
  boxes: Map<string, DagreBox>;
  /** Each edge's polyline and its label's box (none for a same-rank line). */
  edges: Map<string, { points: Array<[number, number]>; label?: { x: number; y: number; w: number; h: number } }>;
  /** Each parent's frame around its children, with room for a title. */
  frames: Map<string, DagreBox>;
}

export interface DagreOptions {
  rankdir: "TB" | "LR";
  nodesep: number;
  ranksep: number;
  /** Columns of the grid that holds cards with no line. */
  looseColumns?: number;
  /** Space above a frame's first card for its title. */
  frameTitle?: number;
  /** Space above the grid of cards with no line, for its title. */
  looseTitle?: number;
}

const MARGIN = 24;
const LOOSE_GAP = 24;

/** Clear space between a label and the cards across the gap. */
const GAP = 16;

type Pt = [number, number];

/**
 * Square routes through free bands. dagre's bends sit in the right ranks,
 * but the straight runs between them can cut across a label or a card. A
 * free band is a strip across the map, between two ranks, that no card,
 * label or frame edge touches. Each run between two bends goes along its
 * own axis to a free band between them, across in the band, and on to the
 * next bend, so it meets nothing on the way. Lines that share a band get
 * their own lane in it.
 */
function routeSquare(
  raw: Pt[],
  a: DagreBox,
  b: DagreBox,
  bands: Array<[number, number]>,
  lanes: Map<number, number>,
  rankdir: "TB" | "LR",
): Pt[] {
  // Work in (rank, across) coordinates: y and x for top-down, x and y for left-right.
  const toRA = (p: Pt): Pt => (rankdir === "TB" ? [p[1], p[0]] : [p[0], p[1]]);
  const fromRA = (p: Pt): Pt => (rankdir === "TB" ? [p[1], p[0]] : [p[0], p[1]]);
  const span = (box: DagreBox) =>
    rankdir === "TB" ? { r0: box.y, r1: box.y + box.h, a0: box.x, a1: box.x + box.w } : { r0: box.x, r1: box.x + box.w, a0: box.y, a1: box.y + box.h };
  const inner = raw.slice(1, -1).map(toRA);
  const side = (box: DagreBox, toward: Pt): Pt => {
    const s2 = span(box);
    const r = toward[0] >= (s2.r0 + s2.r1) / 2 ? s2.r1 : s2.r0;
    return [r, Math.min(Math.max(toward[1], s2.a0 + 8), s2.a1 - 8)];
  };
  const pts: Pt[] = [side(a, inner[0] ?? toRA(raw[raw.length - 1])), ...inner];
  pts.push(side(b, pts[pts.length - 1]));
  const out: Pt[] = [pts[0]];
  for (let i = 1; i < pts.length; i++) {
    const p = out[out.length - 1];
    const q = pts[i];
    if (Math.abs(p[1] - q[1]) < 0.5) {
      out.push(q);
      continue;
    }
    const lo = Math.min(p[0], q[0]);
    const hi = Math.max(p[0], q[0]);
    const band = bands.filter(([b0, b1]) => b0 >= lo - 0.5 && b1 <= hi + 0.5).sort((x, y) => Math.abs((x[0] + x[1]) / 2 - (lo + hi) / 2) - Math.abs((y[0] + y[1]) / 2 - (lo + hi) / 2))[0];
    if (!band) {
      out.push(q);
      continue;
    }
    const key = band[0];
    const used = lanes.get(key) ?? 0;
    lanes.set(key, used + 1);
    const room = band[1] - band[0];
    const lane = band[0] + room / 2 + (((used % 5) - 2) * Math.min(6, room / 6));
    out.push([lane, p[1]], [lane, q[1]], q);
  }
  return out.map(fromRA);
}

/**
 * A line between two cards of one rank: out of the far side of both, along
 * the first free band past them, and back in. The band holds no card,
 * label or frame edge, so the line meets nothing on the way.
 */
function routeBeside(a: DagreBox, b: DagreBox, bands: Array<[number, number]>, lanes: Map<number, number>, rankdir: "TB" | "LR"): Pt[] {
  const fromRA = (p: Pt): Pt => (rankdir === "TB" ? [p[1], p[0]] : [p[0], p[1]]);
  const span = (box: DagreBox) =>
    rankdir === "TB" ? { r1: box.y + box.h, mid: box.x + box.w / 2 } : { r1: box.x + box.w, mid: box.y + box.h / 2 };
  const sa = span(a);
  const sb = span(b);
  const far = Math.max(sa.r1, sb.r1);
  const band = bands.find(([b0]) => b0 >= far - 0.5);
  const key = band ? band[0] : far;
  const used = lanes.get(key) ?? 0;
  lanes.set(key, used + 1);
  const lane = band ? band[0] + (band[1] - band[0]) / 2 + ((used % 5) - 2) * Math.min(6, (band[1] - band[0]) / 6) : far + 12;
  return [fromRA([sa.r1, sa.mid]), fromRA([lane, sa.mid]), fromRA([lane, sb.mid]), fromRA([sb.r1, sb.mid])];
}

/** Move runs that lie along a frame edge 8 px away from it; both ends of the run move, so it stays square. */
function offFrames(points: Pt[], frames: DagreBox[]): void {
  for (let i = 1; i < points.length; i++) {
    const p = points[i - 1];
    const q = points[i];
    for (const f of frames) {
      if (p[1] === q[1]) {
        for (const edge of [f.y, f.y + f.h]) {
          if (Math.abs(p[1] - edge) < 4) {
            const y = p[1] >= edge ? edge + 8 : edge - 8;
            p[1] = y;
            q[1] = y;
          }
        }
      } else if (p[0] === q[0]) {
        for (const edge of [f.x, f.x + f.w]) {
          if (Math.abs(p[0] - edge) < 4) {
            const x = p[0] >= edge ? edge + 8 : edge - 8;
            p[0] = x;
            q[0] = x;
          }
        }
      }
    }
  }
}

/** Strips along the rank axis that no card, label or frame edge covers. */
function freeBands(covered: Array<[number, number]>, from: number, to: number): Array<[number, number]> {
  const sorted = [...covered].sort((x, y) => x[0] - y[0]);
  const bands: Array<[number, number]> = [];
  let at = from;
  for (const [c0, c1] of sorted) {
    if (c0 - at >= 8) bands.push([at + 2, c0 - 2]);
    at = Math.max(at, c1);
  }
  if (to - at >= 8) bands.push([at + 2, to - 2]);
  return bands;
}

export function dagreLayout(nodes: DagreNode[], edges: DagreEdge[], opts: DagreOptions): DagreResult {
  const ranked = edges.filter((e) => !e.sameRank);
  const linked = new Set(edges.flatMap((e) => [e.from, e.to]));
  // A card inside a frame stays in the graph, so the frame holds it.
  const placed = nodes.filter((n) => linked.has(n.id) || n.parent);
  const loose = nodes.filter((n) => !linked.has(n.id) && !n.parent);
  const parents = [...new Set(placed.map((n) => n.parent).filter((p): p is string => Boolean(p)))];

  const g = new dagre.graphlib.Graph({ compound: parents.length > 0, multigraph: true });
  // A label sits in its own half rank, in the middle of the gap between two
  // ranks of cards: each half of the gap holds half the widest label (along
  // the rank axis) and clear space.
  const widest = Math.max(0, ...ranked.map((e) => (opts.rankdir === "LR" ? e.labelW : e.labelH)));
  const ranksep = Math.max(opts.ranksep, Math.ceil(widest / 2) + GAP);
  g.setGraph({ rankdir: opts.rankdir, nodesep: opts.nodesep, ranksep, edgesep: 24, marginx: MARGIN, marginy: MARGIN + (opts.frameTitle ?? 0) });
  g.setDefaultEdgeLabel(() => ({}));
  for (const p of parents) g.setNode(p, { paddingTop: opts.frameTitle ?? 0 });
  for (const n of placed) {
    g.setNode(n.id, { width: n.w, height: n.h });
    if (n.parent) g.setParent(n.id, n.parent);
  }
  for (const e of ranked) g.setEdge(e.from, e.to, { width: e.labelW, height: e.labelH, labelpos: "c" }, e.key);
  if (placed.length > 0) dagre.layout(g);

  const boxes = new Map<string, DagreBox>();
  for (const n of placed) {
    const v = g.node(n.id);
    boxes.set(n.id, { id: n.id, x: v.x - n.w / 2, y: v.y - n.h / 2, w: n.w, h: n.h });
  }
  const frames = new Map<string, DagreBox>();
  for (const p of parents) {
    const v = g.node(p);
    const title = opts.frameTitle ?? 0;
    frames.set(p, { id: p, x: v.x - v.width / 2, y: v.y - v.height / 2 - title, w: v.width, h: v.height + title });
  }
  // Label boxes first: they are obstacles the routes keep clear of.
  const labels = new Map<string, { x: number; y: number; w: number; h: number }>();
  for (const e of ranked) {
    const v = g.edge({ v: e.from, w: e.to, name: e.key });
    if (v) labels.set(e.key, { x: (v.x ?? 0) - e.labelW / 2, y: (v.y ?? 0) - e.labelH / 2, w: e.labelW, h: e.labelH });
  }
  const tb = opts.rankdir === "TB";
  const covered: Array<[number, number]> = [
    ...[...boxes.values(), ...labels.values()].map((b): [number, number] => (tb ? [b.y, b.y + b.h] : [b.x, b.x + b.w])),
    // A frame's edges across the rank axis: no route runs along them.
    ...[...frames.values()].flatMap((f): Array<[number, number]> =>
      tb ? [[f.y - 4, f.y + 4], [f.y + f.h - 4, f.y + f.h + 4]] : [[f.x - 4, f.x + 4], [f.x + f.w - 4, f.x + f.w + 4]],
    ),
  ];
  const graphSize = g.graph();
  const bands = freeBands(covered, 0, tb ? (graphSize.height ?? 0) : (graphSize.width ?? 0));
  const lanes = new Map<number, number>();
  const out: DagreResult["edges"] = new Map();
  for (const e of edges.filter((x) => x.sameRank)) {
    const a = boxes.get(e.from);
    const b = boxes.get(e.to);
    if (a && b) out.set(e.key, { points: routeBeside(a, b, bands, lanes, opts.rankdir) });
  }
  for (const e of ranked) {
    const v = g.edge({ v: e.from, w: e.to, name: e.key });
    const label = labels.get(e.key);
    if (!v || !label) continue;
    const raw = (v.points ?? []).map((p: { x: number; y: number }) => [p.x, p.y] as Pt);
    const a = boxes.get(e.from);
    const b = boxes.get(e.to);
    const points = a && b && raw.length >= 2 ? routeSquare(raw, a, b, bands, lanes, opts.rankdir) : raw;
    out.set(e.key, { points, label });
  }

  // A run that lands on a frame's edge is moved 8 px off it, so no line
  // reads as part of the frame.
  for (const route of out.values()) offFrames(route.points, [...frames.values()]);

  // The size of the laid-out graph, then the loose cards in a grid under it.
  const graph = placed.length > 0 ? g.graph() : { width: 0, height: 0 };
  let width = graph.width ?? 0;
  let top = placed.length > 0 ? (graph.height ?? 0) : MARGIN;
  if (loose.length > 0) {
    top += opts.looseTitle ?? 0;
    const cols = Math.max(1, opts.looseColumns ?? 3);
    const colW = Math.max(...loose.map((n) => n.w));
    const rowH = Math.max(...loose.map((n) => n.h));
    loose.forEach((n, i) => {
      boxes.set(n.id, { id: n.id, x: MARGIN + (i % cols) * (colW + LOOSE_GAP), y: top + Math.floor(i / cols) * (rowH + LOOSE_GAP), w: n.w, h: n.h });
    });
    const rows = Math.ceil(loose.length / cols);
    top += rows * (rowH + LOOSE_GAP) - LOOSE_GAP + MARGIN;
    width = Math.max(width, MARGIN * 2 + Math.min(cols, loose.length) * (colW + LOOSE_GAP) - LOOSE_GAP);
  }
  return { width, height: top, boxes, edges: out, frames };
}
