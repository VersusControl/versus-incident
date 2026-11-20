import { ActiveFilterChips, FilterPanel } from "@/components/FilterPanel";
import { serviceLabel, type ChangesService } from "@/lib/awsChanges";
import type { FacetChip, FacetView } from "@/lib/listFilters";

// The Changes page's Services filter panel and chips; the selection and its
// saving live in useServiceFilter.

const FACET_ID = "service";

/**
 * Every service with changes in the window, plus any chosen one with none, so
 * it stays removable; most changes first. The panel lists the checked ones
 * above the rest.
 */
function serviceOptions(services: readonly ChangesService[], selected: readonly string[]): FacetView["options"] {
  const options = services.map((s) => ({ value: s.service, label: serviceLabel(services, s.service), count: s.count }));
  for (const v of selected) {
    if (!options.some((o) => o.value === v)) options.push({ value: v, label: serviceLabel(services, v), count: 0 });
  }
  return options.sort((a, b) => b.count - a.count);
}

/** The "Services" button and its checkbox panel, reusing the Logs list page's filter. */
export function ServiceFilterPanel({
  services,
  selected,
  onChange,
}: {
  services: readonly ChangesService[];
  selected: readonly string[];
  onChange: (next: string[]) => void;
}) {
  const options = serviceOptions(services, selected);
  const facet: FacetView = { id: FACET_ID, label: "Services", options, selected };
  const allValues = options.map((o) => o.value);
  const allChosen = allValues.length > 0 && allValues.every((v) => selected.includes(v));
  return (
    <span className="inline-flex items-center" data-services-filter>
      <FilterPanel
        label="Services"
        alwaysSearchable
        pinSelected
        facets={[facet]}
        activeCount={selected.length}
        onToggle={(_id, value) => onChange(selected.includes(value) ? selected.filter((v) => v !== value) : [...selected, value])}
        onClear={() => onChange([])}
        controls={
          <div className="flex justify-end">
            <button type="button" className="btn px-2 py-1" disabled={allChosen} onClick={() => onChange(allValues)}>
              Select all
            </button>
          </div>
        }
      />
    </span>
  );
}

/** The chosen services as removable chips under the scope bar. */
export function ServiceFilterChips({
  services,
  selected,
  onChange,
}: {
  services: readonly ChangesService[];
  selected: readonly string[];
  onChange: (next: string[]) => void;
}) {
  const chips: FacetChip[] = selected.map((value) => ({ facetId: FACET_ID, value, label: `Service: ${serviceLabel(services, value)}` }));
  return (
    <ActiveFilterChips
      chips={chips}
      onRemove={(_id, value) => onChange(selected.filter((v) => v !== value))}
      onClear={() => onChange([])}
    />
  );
}
