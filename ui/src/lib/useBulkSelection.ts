import { useCallback, useEffect, useMemo, useState } from "react";
import {
  allState,
  selectedInView,
  withAll,
  withToggled,
  type AllState,
} from "@/lib/bulkSelect";

// useBulkSelection — the shared select-all + row-selection state for the three
// learned-signal admin pages. It holds the set of selected row keys and RESETS
// it whenever `resetKey` changes, so switching tab / filter / page never leaves
// a stale selection acting on rows the operator can no longer see (the page
// composes `resetKey` from the same signature usePagination uses PLUS the page
// number). Kept thin — all the maths lives in the pure lib/bulkSelect helpers.
export function useBulkSelection(pageKeys: readonly string[], resetKey: string) {
  const [selected, setSelected] = useState<Set<string>>(() => new Set());

  // Reset on tab / filter / page change. The whole selection clears — a bulk
  // action only ever touches what's currently on screen.
  useEffect(() => {
    setSelected(new Set());
  }, [resetKey]);

  const toggle = useCallback((key: string) => {
    setSelected((s) => withToggled(s, key));
  }, []);

  const toggleAll = useCallback(
    (checked: boolean) => {
      setSelected((s) => withAll(s, pageKeys, checked));
    },
    [pageKeys],
  );

  const clear = useCallback(() => setSelected(new Set()), []);

  const headerState: AllState = useMemo(
    () => allState(selected, pageKeys),
    [selected, pageKeys],
  );

  const selectedKeys = useMemo(
    () => selectedInView(selected, pageKeys),
    [selected, pageKeys],
  );

  return {
    // isSelected drives each row checkbox.
    isSelected: (key: string) => selected.has(key),
    // headerState is the tri-state of the header select-all checkbox.
    headerState,
    // selectedKeys are the on-screen selected keys, in visible order.
    selectedKeys,
    // count is how many are selected (drives the bulk bar's visibility + label).
    count: selectedKeys.length,
    toggle,
    toggleAll,
    clear,
  };
}
