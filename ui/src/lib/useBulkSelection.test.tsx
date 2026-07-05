// @vitest-environment jsdom
import { describe, it, expect, afterEach } from "vitest";
import { renderHook, act, cleanup } from "@testing-library/react";
import { useBulkSelection } from "@/lib/useBulkSelection";

// Behavioural tests for the shared selection hook. These pin the two contracts
// that can only be observed through the hook's state transitions:
//   • select-all toggles every visible key and the header tri-state tracks it;
//   • the selection RESETS whenever the resetKey changes (tab / filter / page),
//     so a bulk action never acts on rows the operator can no longer see.
afterEach(cleanup);

describe("useBulkSelection", () => {
  it("select-all selects every visible key and reports 'all'", () => {
    const keys = ["a", "b", "c"];
    const { result } = renderHook(() => useBulkSelection(keys, "k1"));

    expect(result.current.headerState).toBe("none");
    act(() => result.current.toggleAll(true));
    expect(result.current.count).toBe(3);
    expect(result.current.headerState).toBe("all");
    expect(result.current.selectedKeys).toEqual(["a", "b", "c"]);

    act(() => result.current.toggleAll(false));
    expect(result.current.count).toBe(0);
    expect(result.current.headerState).toBe("none");
  });

  it("toggling one row moves the header to the indeterminate 'some' state", () => {
    const keys = ["a", "b", "c"];
    const { result } = renderHook(() => useBulkSelection(keys, "k1"));
    act(() => result.current.toggle("a"));
    expect(result.current.isSelected("a")).toBe(true);
    expect(result.current.headerState).toBe("some");
    expect(result.current.count).toBe(1);
  });

  it("resets the selection when the resetKey changes (tab / filter / PAGE change)", () => {
    const keys = ["a", "b", "c"];
    const { result, rerender } = renderHook(
      ({ resetKey }) => useBulkSelection(keys, resetKey),
      { initialProps: { resetKey: "all|active|q|1" } },
    );
    act(() => result.current.toggleAll(true));
    expect(result.current.count).toBe(3);

    // Page 1 → page 2 (or any filter/tab change) clears the selection.
    rerender({ resetKey: "all|active|q|2" });
    expect(result.current.count).toBe(0);
    expect(result.current.headerState).toBe("none");
  });

  it("clear() empties the selection", () => {
    const keys = ["a", "b"];
    const { result } = renderHook(() => useBulkSelection(keys, "k1"));
    act(() => result.current.toggleAll(true));
    expect(result.current.count).toBe(2);
    act(() => result.current.clear());
    expect(result.current.count).toBe(0);
  });
});
