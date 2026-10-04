import { describe, it, expect } from "vitest";
import {
  GROUP_EXPAND_STEP,
  GROUP_PREVIEW_ROWS,
  groupByService,
  groupViews,
  nextGroupLimit,
} from "./serviceGroups";

type Row = { id: number; service: string };
const svc = (r: Row) => r.service;

const rows: Row[] = [
  { id: 1, service: "web" },
  { id: 2, service: "api" },
  { id: 3, service: "web" },
  { id: 4, service: "web" },
  { id: 5, service: "api" },
  { id: 6, service: "db" },
];

describe("groupByService", () => {
  it("orders groups by their first row so the active sort is kept", () => {
    const groups = groupByService(rows, svc);
    expect(groups.map((g) => g.service)).toEqual(["web", "api", "db"]);
    expect(groups[0].rows.map((r) => r.id)).toEqual([1, 3, 4]);
  });
});

describe("groupViews", () => {
  it("previews the first rows of each group by default", () => {
    const [web, api, db] = groupViews(groupByService(rows, svc), {});
    expect(web.rows.map((r) => r.id)).toEqual([1, 3]);
    expect(web).toMatchObject({ total: 3, hidden: 1, expanded: false });
    expect(api).toMatchObject({ total: 2, hidden: 0, expanded: false });
    expect(db).toMatchObject({ total: 1, hidden: 0, expanded: false });
  });

  it("shows more rows once a group's limit is raised", () => {
    const [web] = groupViews(groupByService(rows, svc), { web: 100 });
    expect(web).toMatchObject({ total: 3, hidden: 0, expanded: true });
  });
});

describe("nextGroupLimit", () => {
  it("expands the preview to one step, then adds a step at a time", () => {
    expect(nextGroupLimit(GROUP_PREVIEW_ROWS, 500)).toBe(GROUP_EXPAND_STEP);
    expect(nextGroupLimit(GROUP_EXPAND_STEP, 500)).toBe(2 * GROUP_EXPAND_STEP);
  });

  it("never exceeds the group size", () => {
    expect(nextGroupLimit(GROUP_PREVIEW_ROWS, 7)).toBe(7);
    expect(nextGroupLimit(GROUP_EXPAND_STEP, 150)).toBe(150);
  });
});
