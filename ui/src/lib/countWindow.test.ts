import { describe, it, expect } from "vitest";
import {
  COUNT_WINDOW_LABELS,
  DEFAULT_COUNT_WINDOW,
  countWindowLabel,
} from "./countWindow";

describe("countWindowLabel", () => {
  it("names every window the server accepts", () => {
    expect(Object.keys(COUNT_WINDOW_LABELS)).toEqual([
      "24h",
      "7d",
      "30d",
      "90d",
      "all",
    ]);
  });

  it("renders both the settings and top-bar forms", () => {
    expect(countWindowLabel("30d")).toBe("Last 30 days");
    expect(countWindowLabel("30d", "short")).toBe("last 30d");
  });

  // The top bar renders before the setting has loaded. Falling back to the
  // server's default keeps the label honest instead of flashing a window the
  // counts were never computed over.
  it("falls back to the default while loading or on an unknown value", () => {
    const want = COUNT_WINDOW_LABELS[DEFAULT_COUNT_WINDOW];
    expect(countWindowLabel(undefined)).toBe(want.long);
    expect(countWindowLabel("6mo", "short")).toBe(want.short);
  });
});
