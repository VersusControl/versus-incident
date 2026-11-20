// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { format } from "date-fns";
import { fmtCellCount, gridBuckets, gridLayout, type ChangesBucket, type ChangesView } from "@/lib/awsChanges";
import { ActivityGrid } from "./ActivityGrid";
import { columnPct } from "./gridColumns";
import { usualView } from "./changesFixtures";

// The activity grid sizes its columns from the server's bucket step and the
// bucket count, so a 7 day window always fits the card. jsdom has no layout,
// so the fit is asserted on the widths the grid sets on its table and cells.

const H = 3_600_000;
const SINCE = "2026-11-13T14:27:00.000Z";
const UNTIL = new Date(Date.parse(SINCE) + 168 * H).toISOString();

/** The grid's room for the table at each desktop width (card inner width, measured in the browser). */
const GRID_WIDTH_PX = { 1280: 1006, 1440: 1166 } as const;

function bucket(start: string, over: Partial<ChangesBucket> = {}): ChangesBucket {
  return { start, pipeline: 0, person: 0, aws_service: 0, unknown: 0, errors: 0, state: "complete", ...over };
}

function sevenDays(stepHours: number, fill: (i: number) => Partial<ChangesBucket> = () => ({})): ChangesBucket[] {
  const out: ChangesBucket[] = [];
  for (let i = 0; i < 168 / stepHours; i++) out.push(bucket(new Date(Date.parse(SINCE) + i * stepHours * H).toISOString(), fill(i)));
  return out;
}

function weekView(b: ChangesBucket[], bucketSeconds?: number): ChangesView {
  return usualView({
    window: { since: SINCE, until: UNTIL, settled_until: UNTIL },
    buckets: b,
    alarms: [],
    suspect: { status: "none", change_id: null, alarm_id: null, searched: null },
    ...(bucketSeconds ? { bucket_seconds: bucketSeconds } : {}),
  });
}

function table(): HTMLElement {
  return document.querySelector("[role='table']") as HTMLElement;
}

function personCells(): HTMLElement[] {
  return [...document.querySelectorAll("[data-row='person'] [role='cell']")] as HTMLElement[];
}

afterEach(cleanup);

describe("ActivityGrid — long windows", () => {
  it("7 d with 28 six-hour buckets: 28 columns that fit the card at 1280 and 1440, day labels with 6 h minor ticks, and the 6 h span in each hover", () => {
    const b = sevenDays(6, (i) => (i === 27 ? { person: 627 } : {}));
    render(<ActivityGrid view={weekView(b, 21_600)} />);

    expect(table().dataset.columns).toBe("28");
    expect(table().dataset.stepSeconds).toBe("21600");
    expect(table().getAttribute("aria-label")).toBe("Changes per 6 hours by actor");
    const cells = personCells();
    expect(cells).toHaveLength(28);

    const tableMin = parseFloat(table().style.minWidth);
    const cellMin = parseFloat(cells[0].style.minWidth);
    expect(tableMin).toBeLessThanOrEqual(GRID_WIDTH_PX[1280]);
    expect(tableMin).toBeLessThanOrEqual(GRID_WIDTH_PX[1440]);
    expect(144 + 28 * cellMin).toBeLessThanOrEqual(tableMin);
    expect(document.querySelector("[class*='min-w-[1.5rem]']")).toBeNull();

    const axis = document.querySelector("[data-day-axis]") as HTMLElement;
    expect(axis.querySelectorAll("[data-minor-tick]")).toHaveLength(28);
    const days = [...axis.querySelectorAll("[data-day-label]")].map((e) => e.textContent);
    expect(days.length).toBeGreaterThanOrEqual(6);
    for (const d of days) expect(d).toMatch(/^[A-Z][a-z]{2} \d{1,2}$/);
    const firstMidnight = new Date(Date.parse(SINCE));
    firstMidnight.setHours(24, 0, 0, 0);
    expect(days[0]).toBe(format(firstMidnight, "MMM d"));

    // The last column holds the changes; its hover names the whole 6 h span.
    const last = cells[27];
    expect(last.textContent).toBe("627");
    const start = new Date(Date.parse(UNTIL) - 6 * H);
    expect(last.title).toContain(format(start, "HH:mm"));
    expect(last.title).toContain(format(new Date(UNTIL), "HH:mm"));
    expect(last.title).toContain("–");
    expect(last.getAttribute("aria-label")).toContain("–");
  });

  it("an older server's 168 hourly buckets for 7 d are grouped into 28 six-hour columns: counts summed, worst state kept", () => {
    const hourly = sevenDays(1, (i) => {
      if (i < 6) return { pipeline: 2, errors: 1 }; // column 0: all complete
      if (i < 12) return i === 7 ? { state: "gap" } : { person: 3 }; // column 1: one hour unread
      if (i < 18) return { state: "gap", gap_accounts: ["111"] }; // column 2: nothing read
      if (i >= 162) return i === 167 ? { state: "arriving", aws_service: 1 } : { aws_service: 1 }; // last: unsettled
      return {};
    });
    const view = weekView(hourly);
    const { buckets, stepSeconds } = gridBuckets(view);
    expect(stepSeconds).toBe(21_600);
    expect(buckets).toHaveLength(28);
    expect(buckets[0]).toMatchObject({ start: SINCE, pipeline: 12, errors: 6, state: "complete" });
    expect(buckets[1]).toMatchObject({ person: 15, state: "partial" });
    expect(buckets[2]).toMatchObject({ state: "gap", gap_accounts: ["111"] });
    expect(buckets[27]).toMatchObject({ aws_service: 6, state: "arriving" });

    render(<ActivityGrid view={view} />);
    expect(table().dataset.columns).toBe("28");
    expect(personCells().map((c) => c.dataset.state).slice(0, 3)).toEqual(["complete", "partial", "gap"]);
    expect(document.querySelectorAll("[data-day-axis] [data-minor-tick]")).toHaveLength(28);
  });

  it("no overflow at any column count: the table's floor stays inside the card at 1280 and 1440", () => {
    for (const n of [1, 12, 24, 28, 48, 168, 400]) {
      const { tableMinPx, cellMinPx } = gridLayout(n);
      expect(tableMinPx).toBeLessThanOrEqual(GRID_WIDTH_PX[1280]);
      expect(tableMinPx).toBeLessThanOrEqual(GRID_WIDTH_PX[1440]);
      expect(tableMinPx).toBeGreaterThanOrEqual(144 + n * cellMinPx);
    }
    // The 24 h view keeps its usual 24 px columns and clock axis.
    expect(gridLayout(24).cellMinPx).toBe(24);
    render(<ActivityGrid view={usualView()} />);
    expect(table().dataset.columns).toBe("24");
    expect(table().getAttribute("aria-label")).toBe("Changes per hour by actor");
    expect(document.querySelector("[data-day-axis]")).toBeNull();
  });

  it("7 d at 1280: a 5-character count such as 3,750 shows short and fits its column with 2 px each side; the exact count is in the label and hover", () => {
    // Advance widths of Inter at 11 px (text-2xs, tabular figures), from the
    // browser: "3,750" measures 31.3 px. jsdom has no layout, so the fit is
    // checked against these widths.
    const glyph = (c: string): number => ({ ",": 2.9, ".": 2.9, k: 5.7, M: 9.7, B: 7.6 })[c] ?? 7.2;
    const textPx = (t: string): number => [...t].reduce((w, c) => w + glyph(c), 0);
    // The card's 1280 grid room, less its px-4 padding and the row labels, over 28 columns.
    const columnPx = (GRID_WIDTH_PX[1280] - 32 - 144) / 28;
    const room = columnPx - 1 - 2 * 2; // right border, 2 px padding each side

    expect(textPx("3,750")).toBeGreaterThan(room);
    for (const n of [999, 1000, 3750, 9949, 9950, 12_345, 99_499, 99_500, 412_000, 949_999, 3_200_000, 98_000_000, 250_000_000]) {
      expect(textPx(fmtCellCount(n)), `${n} → ${fmtCellCount(n)}`).toBeLessThanOrEqual(room);
    }

    const b = sevenDays(6, (i) => (i === 3 ? { person: 3750 } : i === 4 ? { person: 12_345 } : i === 5 ? { person: 627 } : {}));
    render(<ActivityGrid view={weekView(b, 21_600)} />);
    const cells = personCells();
    expect(cells[3].textContent).toBe("3.8k");
    expect(cells[3].getAttribute("aria-label")).toContain(": 3,750");
    expect(cells[3].title).toContain(": 3,750");
    expect(cells[4].textContent).toBe("12k");
    expect(cells[4].getAttribute("aria-label")).toContain(": 12,345");
    expect(cells[5].textContent).toBe("627");
    for (const c of cells) expect(c.className).toMatch(/\bpx-0\.5\b/);
  });
});

/** A mix of every cell kind: empty, unread, partly read, short and four-digit counts, and an unsettled last column. */
function mixedBuckets(n: number, stepHours: number): ChangesBucket[] {
  const out: ChangesBucket[] = [];
  for (let i = 0; i < n; i++) {
    const start = new Date(Date.parse(SINCE) + i * stepHours * H).toISOString();
    const kind = i % 5;
    const over: Partial<ChangesBucket> =
      i === n - 1
        ? { state: "arriving", person: 46, pipeline: 358 }
        : kind === 1
          ? { state: "gap" }
          : kind === 2
            ? { person: 4054, pipeline: 2901, errors: 7 }
            : kind === 3
              ? { state: "partial", person: 12, gap_accounts: ["111"] }
              : {};
    out.push(bucket(start, over));
  }
  return out;
}

function mixedView(n: number, stepHours: number, over: Partial<ChangesView> = {}): ChangesView {
  const until = new Date(Date.parse(SINCE) + n * stepHours * H).toISOString();
  return usualView({
    window: { since: SINCE, until, settled_until: until },
    buckets: mixedBuckets(n, stepHours),
    alarms: [],
    gaps: [],
    suspect: { status: "none", change_id: null, alarm_id: null, searched: null },
    bucket_seconds: stepHours * 3600,
    ...over,
  });
}

/** The x of an overlay or label, in percent of the column grid. */
function leftPct(el: Element): number {
  return parseFloat((el as HTMLElement).style.left);
}

describe("ActivityGrid — one column grid for cells and overlays", () => {
  for (const [n, stepHours] of [
    [28, 6],
    [24, 1],
  ] as const) {
    it(`${n} columns: every column has the same box, with or without a count, padding and borders inside it`, () => {
      render(<ActivityGrid view={mixedView(n, stepHours)} />);
      expect(table().dataset.columns).toBe(String(n));
      const share = `${100 / n}%`;
      const slots = [
        ...document.querySelectorAll("[role='cell']"),
        ...document.querySelectorAll("[data-minor-tick]"),
        ...(n === 24 ? [...document.querySelectorAll("[role='table'] > div[aria-hidden] > div.flex > div")] : []),
      ] as HTMLElement[];
      // Four rows of cells (no unknown actors here) and one axis slot per column.
      expect(slots.length).toBe(5 * n);
      const boxes = new Set(slots.map((c) => `${c.style.flexGrow}|${c.style.flexShrink}|${c.style.flexBasis}|${c.style.minWidth}|${c.style.boxSizing}`));
      expect([...boxes]).toEqual([`0|0|${share}|${gridLayout(n).cellMinPx}px|border-box`]);
      for (const c of slots) expect(c.className).not.toMatch(/\bflex-1\b/);
      // The cells with counts are the ones that carry padding and borders; they keep the same box.
      const cells = [...document.querySelectorAll("[role='cell']")] as HTMLElement[];
      const counted = cells.filter((c) => c.textContent !== "");
      expect(counted.length).toBeGreaterThan(0);
      for (const c of counted) {
        expect(c.className).toMatch(/\bpx-0\.5\b/);
        expect(c.className).toMatch(/\bbox-border\b/);
      }
      expect(cells.some((c) => c.textContent === "4.1k")).toBe(true);
      expect(cells.some((c) => c.textContent === "" && c.dataset.state === "gap")).toBe(true);
    });
  }

  it("bands, alarm lines, day labels and ticks sit at index × column width on the 7 d grid", () => {
    const n = 28;
    const col = 100 / n;
    const start = (i: number) => new Date(Date.parse(SINCE) + i * 6 * H).toISOString();
    const view = mixedView(n, 6, {
      gaps: [{ account: "111", region: "eu-west-1", from: start(3), to: start(5), reason: "throttled" }],
      alarms: [{ id: "a1", name: "checkout 5xx", account: "111", region: "eu-west-1", resource_arns: [], start: start(10), start_known: true, end: null }],
    });
    render(<ActivityGrid view={view} />);

    const band = document.querySelector("[data-band='throttled']") as HTMLElement;
    expect(leftPct(band)).toBeCloseTo(3 * col, 6);
    expect(parseFloat(band.style.width)).toBeCloseTo(2 * col, 6);
    expect(leftPct(document.querySelector("[data-alarm-line='a1']") as Element)).toBeCloseTo(10 * col, 6);
    // A time inside a column lands at its column's start plus the fraction of the column it is into.
    const { buckets } = gridBuckets(view);
    const ends = buckets.map((_, i) => buckets[i + 1]?.start ?? view.window.until);
    expect(columnPct(buckets, ends, new Date(Date.parse(start(7)) + 1.5 * H).toISOString())).toBeCloseTo((7 + 0.25) * col, 6);

    const labels = [...document.querySelectorAll("[data-day-label]")];
    expect(labels.length).toBeGreaterThanOrEqual(6);
    const midnight = new Date(Date.parse(SINCE));
    midnight.setHours(24, 0, 0, 0);
    for (const [k, el] of labels.entries()) {
      const t = new Date(midnight);
      t.setDate(midnight.getDate() + k);
      const index = (t.getTime() - Date.parse(SINCE)) / (6 * H);
      expect(leftPct(el), el.textContent ?? "").toBeCloseTo(index * col, 6);
    }
    // One tick per column, each starting at its column's edge: the same box as the cells.
    const ticks = [...document.querySelectorAll("[data-minor-tick]")] as HTMLElement[];
    expect(ticks).toHaveLength(n);
    for (const t of ticks) expect(t.style.flexBasis).toBe(`${col}%`);
  });

  it("a gap that ends partway through a column draws its band beneath that column's count, which sits on a card-colour chip; the band keeps the gap's exact time position", () => {
    const n = 24;
    const col = 100 / n;
    const start = (i: number) => new Date(Date.parse(SINCE) + i * H).toISOString();
    // Column 6 is unread; the gap ends 0.4 h into column 7, which has counts.
    const to = new Date(Date.parse(start(7)) + 0.4 * H).toISOString();
    const view = mixedView(n, 1, {
      gaps: [{ account: "111", region: "eu-west-1", from: start(6), to, reason: "throttled" }],
      alarms: [{ id: "a1", name: "checkout 5xx", account: "111", region: "eu-west-1", resource_arns: [], start: to, start_known: true, end: null }],
    });
    render(<ActivityGrid view={view} />);

    const band = document.querySelector("[data-band='throttled']") as HTMLElement;
    expect(leftPct(band)).toBeCloseTo(6 * col, 6);
    expect(parseFloat(band.style.width)).toBeCloseTo(1.4 * col, 6);
    // The band stays on the default layer; the count is lifted above it.
    expect(band.className).not.toMatch(/(^|\s)z-/);

    const count = personCells()[7].querySelector("[data-count]") as HTMLElement;
    expect(count.textContent).toBe("4.1k");
    expect(count.className).toMatch(/\bz-\[1\]/);
    expect(count.className).toMatch(/\bbg-surface\b/);
    expect(count.className).toMatch(/\brelative\b/);
    // Same stacking parent: the count's cell and the band both sit in the row group, which sets no z-index.
    const group = band.closest("[role='rowgroup']") as HTMLElement;
    expect(group.contains(count)).toBe(true);
    expect(group.className).not.toMatch(/(^|\s)z-/);

    // A count no band reaches keeps the heat colour behind it, with no chip.
    const clear = personCells()[2].querySelector("[data-count]") as HTMLElement;
    expect(clear.className).not.toMatch(/\bbg-surface\b/);
    expect(clear.className).not.toMatch(/\bz-\[/);

    // The alarm line stays above the counts.
    expect((document.querySelector("[data-alarm-line='a1']") as HTMLElement).className).toMatch(/\bz-\[2\]/);
  });

  it("a gap shorter than 1 px keeps the minimum band width, and the count in the next column that the widened band runs into sits on a chip", () => {
    const n = 28;
    const col = 100 / n;
    const start = (i: number) => new Date(Date.parse(SINCE) + i * 6 * H).toISOString();
    // A 2-minute gap ending 1 minute before column 7, which has counts.
    const from = new Date(Date.parse(start(7)) - 3 * 60 * 1000).toISOString();
    const to = new Date(Date.parse(start(7)) - 60 * 1000).toISOString();
    render(<ActivityGrid view={mixedView(n, 6, { gaps: [{ account: "111", region: "eu-west-1", from, to, reason: "too_busy" }] })} />);

    const band = document.querySelector("[data-band='too_busy']") as HTMLElement;
    expect(leftPct(band)).toBeCloseTo((7 - 3 / 360) * col, 6);
    expect(parseFloat(band.style.width)).toBeCloseTo(0.5, 6);
    // The drawn band reaches past column 7's start.
    expect(leftPct(band) + parseFloat(band.style.width)).toBeGreaterThan(7 * col);

    const count = personCells()[7].querySelector("[data-count]") as HTMLElement;
    expect(count.textContent).toBe("4.1k");
    expect(count.className).toMatch(/\bz-\[1\]/);
    expect(count.className).toMatch(/\bbg-surface\b/);
    // A column the band does not reach keeps no chip.
    const clear = personCells()[12].querySelector("[data-count]") as HTMLElement;
    expect(clear.className).not.toMatch(/\bbg-surface\b/);
  });

  it("the arriving outline is its own layer, starts at the unsettled column's edge, and never covers that column's count", () => {
    for (const [n, stepHours] of [
      [28, 6],
      [24, 1],
    ] as const) {
      const until = Date.parse(SINCE) + n * stepHours * H;
      // Settled partway into the last column, as with real CloudTrail delivery lag.
      const settled = new Date(until - 0.4 * stepHours * H).toISOString();
      const view = mixedView(n, stepHours, { window: { since: SINCE, until: new Date(until).toISOString(), settled_until: settled } });
      render(<ActivityGrid view={view} />);

      const outline = document.querySelector("[data-arriving-outline]") as HTMLElement;
      expect(outline.closest("[role='cell']")).toBeNull();
      expect(outline.style.right).toBe("0px");
      expect(leftPct(outline)).toBeCloseTo(((n - 1) / n) * 100, 6);
      expect(outline.className).toMatch(/\bborder-2\b/);

      // Geometry at 1280: the outline's 2 px start border against the count's box
      // inside the last column (1 px dashed border and 2 px padding before the text).
      const gridPx = GRID_WIDTH_PX[1280] - 32 - 144;
      const columnPx = gridPx / n;
      const outlineStart = (leftPct(outline) / 100) * gridPx;
      const outlineInnerEdge = outlineStart + 2;
      const last = personCells()[n - 1];
      expect(last.textContent).toBe("46");
      expect(last.className).toMatch(/\bborder\b/);
      expect(last.className).toMatch(/\bpx-0\.5\b/);
      const countBoxStart = (n - 1) * columnPx + 1 + 2;
      const countBoxEnd = n * columnPx - 1 - 2;
      expect(outlineInnerEdge).toBeLessThanOrEqual(countBoxStart);
      // The end border is the grid's right edge, also outside the count's box.
      expect(gridPx - 2).toBeGreaterThanOrEqual(countBoxEnd);
      cleanup();
    }
  });
});
