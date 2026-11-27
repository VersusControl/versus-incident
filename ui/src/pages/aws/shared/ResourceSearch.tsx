import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Search } from "lucide-react";
import { useRetryUnlessForbidden } from "@/lib/access";
import { api } from "@/lib/api";
import { resourceTypeWords, SEARCH_MAX, SEARCH_MIN, type SearchItem } from "@/lib/awsEstate";

// Search resources by name over the scope, from the stored reads, and open
// the chosen one in Topology's Focus view. At most 20 results; more reads
// "More than 20, refine".

function focusPath(item: Pick<SearchItem, "ref" | "account">, region?: string): string {
  const p = new URLSearchParams({ level: "focus", account: item.account, ref: item.ref });
  // Focus keeps the Region the search ran in.
  if (region) p.set("region", region);
  return `/aws/topology?${p.toString()}`;
}

export function ResourceSearch({ account, region, accountName }: { account?: string; region?: string; accountName: (id: string) => string }) {
  const navigate = useNavigate();
  const retry = useRetryUnlessForbidden();
  const [text, setText] = useState("");
  const [asked, setAsked] = useState("");
  const [open, setOpen] = useState(false);
  const search = useQuery({
    queryKey: ["aws-topology-search", asked, account ?? "", region ?? ""],
    queryFn: () => api.awsTopologySearch({ q: asked, account: account || undefined, region: region || undefined }),
    enabled: asked.length >= SEARCH_MIN,
    retry,
  });
  const choose = (item: SearchItem) => {
    setOpen(false);
    navigate(focusPath(item, region));
  };
  const items = search.data?.items ?? [];
  return (
    <div className="relative" data-resource-search>
      <form
        role="search"
        className="inline-flex items-center gap-1.5"
        onSubmit={(e) => {
          e.preventDefault();
          const q = text.trim();
          if (q.length < SEARCH_MIN) return;
          setAsked(q);
          setOpen(true);
        }}
      >
        <Search size={12} aria-hidden="true" className="text-ink-300" />
        <input
          type="search"
          className="input h-7 w-56 text-xs"
          aria-label="Search resources"
          placeholder="Search resources"
          maxLength={SEARCH_MAX}
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Escape") setOpen(false);
          }}
        />
      </form>
      {open && asked && (
        <div className="absolute right-0 z-dropdown mt-1 w-80 max-w-[calc(100vw-2rem)] rounded-card border border-ink-500/60 bg-surface-raised py-1 shadow-lg" data-search-results>
          {search.isPending ? (
            <p className="px-3 py-2 text-xs text-ink-300">Searching…</p>
          ) : search.isError ? (
            <p className="px-3 py-2 text-xs text-sev-critical">Couldn't search</p>
          ) : items.length === 0 ? (
            <p className="px-3 py-2 text-xs text-ink-300">No resource matches</p>
          ) : (
            <ul role="listbox" aria-label="Resources found">
              {items.map((item) => (
                <li key={item.ref} role="option" aria-selected={false}>
                  <button type="button" className="block w-full min-w-0 px-3 py-1.5 text-left hover:bg-ink-600/50" onClick={() => choose(item)}>
                    <span className="block truncate font-mono text-xs text-ink-50">{item.name}</span>
                    <span className="block truncate text-2xs text-ink-300">
                      {[resourceTypeWords(item.type, item.service), accountName(item.account), item.region].filter(Boolean).join(" · ")}
                    </span>
                  </button>
                </li>
              ))}
            </ul>
          )}
          {search.data?.capped && (
            <p className="border-t border-ink-500/40 px-3 py-1.5 text-2xs text-ink-300" data-search-capped>
              More than 20, refine
            </p>
          )}
        </div>
      )}
    </div>
  );
}
