import { useCallback, useMemo } from "react";
import { useSearchParams } from "react-router-dom";
import {
  activeFilterCount,
  readFacetSelection,
  selectionSignature,
  withFacetValues,
  withoutFacets,
  type FacetDef,
} from "@/lib/listFilters";

// useFacetFilters keeps a list page's facet selection in the URL so a filtered
// view is shareable and survives back-navigation. `facets` must be stable.
export function useFacetFilters<T>(facets: readonly FacetDef<T>[]) {
  const [params, setParams] = useSearchParams();
  const selection = useMemo(
    () => readFacetSelection(params, facets),
    [params, facets],
  );

  const setValues = useCallback(
    (id: string, values: readonly string[]) =>
      setParams((prev) => withFacetValues(prev, id, values), { replace: true }),
    [setParams],
  );

  const toggle = useCallback(
    (id: string, value: string) => {
      const current = selection[id] ?? [];
      setValues(
        id,
        current.includes(value)
          ? current.filter((v) => v !== value)
          : [...current, value],
      );
    },
    [selection, setValues],
  );

  const clear = useCallback(
    () => setParams((prev) => withoutFacets(prev, facets), { replace: true }),
    [setParams, facets],
  );

  return {
    selection,
    activeCount: activeFilterCount(selection),
    signature: selectionSignature(selection),
    toggle,
    clear,
  };
}
