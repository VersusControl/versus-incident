// @vitest-environment node
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

// The log header is proven in the browser by the visual pass; these assertions
// guard the declarations that keep it inside its box at every width, which no
// jsdom render can measure.
const css = readFileSync(new URL("./KubernetesResourceDetail.css", import.meta.url), "utf8");

function rule(selector: string) {
  const start = css.lastIndexOf(`${selector} {`);
  expect(start, `${selector} is declared`).toBeGreaterThan(-1);
  return css.slice(start, css.indexOf("}", start));
}

// A selector repeated per breakpoint is declared more than once; the layout
// contract lives in the declaration that a wide viewport resolves to.
function firstRule(selector: string) {
  const start = css.indexOf(`${selector} {`);
  expect(start, `${selector} is declared`).toBeGreaterThan(-1);
  return css.slice(start, css.indexOf("}", start));
}

// The rule that declares a given declaration, with its selector, so a later
// override can be compared on the selector a browser actually matches against.
function declaringRule(needle: string) {
  const at = css.indexOf(needle);
  expect(at, `${needle} is declared`).toBeGreaterThan(-1);
  const open = css.lastIndexOf("{", at);
  return {
    selector: css.slice(css.lastIndexOf("}", open) + 1, open).trim(),
    body: css.slice(open, css.indexOf("}", at)),
  };
}

// A class, attribute or pseudo-class outweighs any element selector, so counting
// them compares two rules that both only use classes.
const weight = (selector: string) => (selector.match(/[.:[]/g) ?? []).length;

describe("Log header layout contract", () => {
  it("keeps the compact header and the log body in one column, never overlapping", () => {
    // The compact card is a column of boxes; nothing in it is positioned out of
    // flow, so no line of output can be painted over the header.
    const viewer = firstRule(".resource-log-viewer");
    expect(viewer).toContain("display: flex");
    expect(viewer).toContain("flex-direction: column");
    expect(viewer).toContain("min-width: 0");
    expect(css).not.toMatch(/\.log-toolbar\s*\{[^}]*position: absolute/);
    expect(css).not.toMatch(/\.log-heading\s*\{[^}]*position: absolute/);
    expect(css).not.toMatch(/\.log-(toolbar|heading)[^{]*\{[^}]*(left|right|top|margin)[^;}]*:\s*-\d/);
    // The body is a fixed 128px box that scrolls inside itself, at every width.
    const body = rule('.resource-log-viewer[data-compact="true"] .log-output');
    expect(body).toContain("height: 128px");
    expect(body).toContain("flex: none");
    const output = rule(".resource-log-viewer .log-output");
    expect(output).toContain("overflow: auto");
    expect(output).toContain("min-width: 0");
    // The compact panel itself is sized by its content, so its own height can
    // never be shorter than the header plus the body.
    expect(css).not.toMatch(/resource-log-slot[^{]*\{[^}]*height: 240px/);
    expect(css).not.toMatch(/resource-log-slot[^{]*\{[^}]*height: 272px/);
  });

  // QA-209: the compact body is a fixed 128px box, and the slot around it used
  // to be the one item a cramped Overview column shrank — which pushed the body
  // and the footer outside the card at 1440 and 1024. The compact slot is sized
  // by its content at every width, so nothing can be laid out below the card.
  it("sizes the compact slot by its content so the body and footer stay in the card", () => {
    const slot = css.indexOf('.resource-log-slot[data-full="false"] {');
    expect(slot, "the compact slot is declared").toBeGreaterThan(-1);
    expect(css.slice(slot, css.indexOf("}", slot))).toContain("flex: 0 0 auto");
    const viewer = css.indexOf('.resource-log-slot[data-full="false"] > .resource-log-viewer {');
    expect(viewer, "the compact viewer is declared").toBeGreaterThan(-1);
    expect(css.slice(viewer, css.indexOf("}", viewer))).toContain("flex: 0 0 auto");
    // The full Logs tab still fills the tab it owns.
    expect(rule(".resource-log-slot:not([hidden])")).toContain("flex: 1");
  });

  it("bounds the compact container select to its share of the header", () => {
    const select = firstRule(".log-heading .log-container");
    expect(select).toContain("max-width: min(10rem, 40%)");
    expect(select).toContain("min-width: 0");
    const heading = rule(".log-heading");
    expect(heading).toContain("gap: 8px");
    expect(heading).toContain("min-width: 0");
  });

  it("gives the full header exactly two rows that begin at the same edge", () => {
    const toolbar = firstRule(".log-toolbar");
    expect(toolbar).toContain("flex-direction: column");
    // No padding, margin or offset of its own: row one and row two start where
    // the toolbar starts, and the Filter lines field's row starts there too.
    expect(toolbar).toContain("padding: 0");
    expect(toolbar).toContain("margin: 0");
    for (const row of [".log-primary", ".log-secondary"]) {
      const declared = firstRule(row);
      expect(declared).toContain("min-width: 0");
      expect(declared).toContain("gap: 8px");
    }
    // The filter field's row is the second row, and its group owns the right.
    expect(firstRule(".log-secondary")).toContain("justify-content: space-between");
    // Row one is one line by default and only wraps below the width it was
    // designed for.
    expect(firstRule(".log-primary")).toContain("flex-wrap: nowrap");
    const narrow = css.slice(css.indexOf("@media (max-width: 1023px) {"));
    expect(narrow.slice(0, narrow.indexOf("\n}\n"))).toContain(".log-primary { flex-wrap: wrap; row-gap: 8px; }");
  });

  it("expands workload filters without changing the bounded Pod filter or mobile wrapping", () => {
    expect(rule(".log-filter-group-workload")).toContain("justify-content: flex-start");
    expect(rule(".log-filter-group-workload .log-filter-wrap")).toContain("max-width: none");
    expect(firstRule(".log-filter-wrap")).toContain("flex: 1 1 12rem");
    expect(firstRule(".log-filter-wrap")).toContain("max-width: 22rem");
    expect(firstRule(".log-actions")).toContain("flex: none");
    expect(rule(".log-filter-group")).toContain("flex-wrap: wrap");
  });

  it("keeps row one to a bounded select, one segmented control and one Since group", () => {
    expect(rule(".resource-log-viewer .log-pod")).toContain("max-width: 9rem");
    expect(rule(".resource-log-viewer .log-container")).toContain("max-width: min(10rem, 40%)");
    // Current and Previous are one control with one border, so the group begins
    // where a single button would and both segments share the left edge.
    const instance = firstRule(".log-instance");
    expect(instance).toContain("display: inline-flex");
    expect(instance).toContain("border: 1px solid");
    expect(instance).not.toContain("flex-direction: column");
    const since = firstRule(".log-since");
    expect(since).toContain("white-space: nowrap");
    expect(rule(".log-since-field")).toContain("display: inline-flex");
  });

  it("removes the container chip row, the Follow toggle and the More menu", () => {
    expect(css).not.toContain(".log-container-chips");
    expect(css).not.toContain(".log-follow");
    expect(css).not.toContain(".log-additional");
    expect(css).not.toContain(".log-more");
    expect(css).not.toContain(".log-selectors");
  });

  it("lets the mobile header stack instead of painting over its neighbours", () => {
    const at = css.indexOf("@media (max-width: 639px) {\n  .resource-log-viewer { --log-control-height");
    expect(at, "the mobile log rules are declared").toBeGreaterThan(-1);
    const mobile = css.slice(at, css.indexOf("\n}\n", at));
    // Everything the reader must reach at 390 is one pointer target tall.
    expect(mobile).toContain(".resource-log-viewer select { min-height: 44px; }");
    expect(mobile).toContain(".log-instance { flex: 1 1 100%; }");
    expect(mobile).toContain(".log-instance button { flex: 1; min-height: 44px; }");
    expect(mobile).toContain(".log-load-earlier { min-height: 44px; }");
    // Every row's first control owns its line rather than being squeezed, and
    // the filter field spans the row it lands on.
    expect(mobile).toContain(".log-since { flex: 1 1 100%; min-height: 44px; }");
    expect(mobile).toContain(".log-filter-wrap { flex: 1 1 100%; max-width: none; }");
    // The icon actions wrap under the field they follow instead of spilling left.
    expect(mobile).toContain(".log-actions { margin-left: auto; }");
  });

  it("re-flows list rows on the card's width so the drawer cannot clip them", () => {
    const surface = rule(".kubernetes-list-surface");
    expect(surface).toContain("container-type: inline-size");
    const narrow = rule(".kubernetes-list-row");
    expect(narrow).toContain("grid-template-columns: minmax(0, 1fr) auto");
    const at = css.indexOf("@container (min-width: 34rem)");
    expect(at, "the wide row layout is behind a container query").toBeGreaterThan(-1);
    const wide = css.slice(at, css.indexOf("\n}\n", at));
    expect(wide).toContain(".kubernetes-list-row--workload {");
    expect(wide).toContain(".kubernetes-list-row--pods {");
    // Columns that only fit a wide card stay hidden until the card is wide.
    expect(rule(".kubernetes-list-namespace,\n.kubernetes-list-chevron,\n.kubernetes-list-metric")).toContain("display: none");
  });

  it("draws the drawer over the list instead of reserving its width", () => {
    // An open drawer used to narrow the content pane, which reflowed the list
    // under it: the tab row grew a scrollbar and the columns folded. The drawer
    // is a fixed overlay now, and the list keeps the width it was laid out for.
    expect(css).not.toContain(".kubernetes-explorer-content[data-drawer");
    expect(css).not.toContain("padding-right: min(100vw");
    // The filter row answers to the card, not the viewport.
    expect(rule(".kubernetes-list-toolbar")).toContain("grid-template-columns: minmax(0, 1fr)");
    const query = css.indexOf("@container (min-width: 34rem)");
    expect(css.slice(query, css.indexOf("\n}\n", query))).toContain(".kubernetes-list-toolbar { grid-template-columns: minmax(0, 1fr) 14rem; }");
  });

  it("pulses the live dot with opacity only and stops it for reduced motion", () => {
    const dot = declaringRule("animation: log-dot-pulse");
    expect(dot.selector).toBe('.log-status[data-log-status="is-live"] .log-dot');
    expect(dot.body).toContain("animation: log-dot-pulse");
    const frames = css.slice(css.indexOf("@keyframes log-dot-pulse"), css.indexOf("@keyframes log-dot-pulse") + 120);
    expect(frames).toContain("opacity");
    expect(frames).not.toMatch(/transform|width|height|scale/);
  });

  it("stops the Live pulse under reduced motion at the animated rule's own weight", () => {
    const animated = declaringRule("animation: log-dot-pulse");
    const start = css.indexOf("@media (prefers-reduced-motion: reduce)");
    const body = css.slice(css.indexOf("{", start) + 1, css.indexOf("\n}", start)).replace(/\/\*[\s\S]*?\*\//g, "").trim();
    expect(body, "reduced motion stops the pulse").toContain("animation: none");
    const selector = body.slice(0, body.indexOf("{")).trim();
    // A lower-weight selector in the media block never wins: the animated rule is
    // more specific, so the dot keeps pulsing for a reader who asked it not to.
    expect(weight(selector)).toBeGreaterThanOrEqual(weight(animated.selector));
  });
});
