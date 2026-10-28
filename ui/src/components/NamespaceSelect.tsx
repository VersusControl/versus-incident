import { useEffect, useId, useLayoutEffect, useRef, useState, type CSSProperties } from "react";
import { createPortal } from "react-dom";
import { useKubernetesCluster, useKubernetesQuery } from "@/lib/useKubernetesCluster";

export function NamespaceSelect({ value, onChange, label = "Namespace", id, className = "input w-full min-w-0" }: {
  value: string; onChange: (namespace: string) => void; label?: string; id?: string; className?: string;
}) {
  const { client } = useKubernetesCluster();
  const generated = useId();
  const [search, setSearch] = useState("");
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const field = useRef<HTMLDivElement>(null);
  const panel = useRef<HTMLDivElement>(null);
  const [position, setPosition] = useState<CSSProperties>({});
  useLayoutEffect(() => {
    if (!open) return;
    const place = () => {
      const anchor = field.current?.getBoundingClientRect();
      if (!anchor) return;
      const width = Math.min(Math.max(anchor.width, 160), window.innerWidth - 16);
      const below = window.innerHeight - anchor.bottom - 12;
      const above = anchor.top - 12;
      const down = below >= Math.min(180, above);
      setPosition({
        width,
        left: Math.max(8, Math.min(anchor.left, window.innerWidth - width - 8)),
        ...(down ? { top: anchor.bottom + 4 } : { bottom: window.innerHeight - anchor.top + 4 }),
        maxHeight: Math.max(80, Math.min(280, down ? below : above)),
      });
    };
    place();
    window.addEventListener("resize", place);
    window.addEventListener("scroll", place, true);
    return () => {
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", place, true);
    };
  }, [open]);
  useEffect(() => {
    if (!open) return;
    const outside = (event: PointerEvent) => {
      if (!field.current?.contains(event.target as Node) && !panel.current?.contains(event.target as Node)) {
        setOpen(false); setSearch("");
      }
    };
    document.addEventListener("pointerdown", outside);
    return () => document.removeEventListener("pointerdown", outside);
  }, [open]);
  const namespaces = useKubernetesQuery({
    queryKey: ["kubernetes-namespaces", value],
    queryFn: () => client.kubernetesNamespaces(value || undefined),
    staleTime: 30000,
    retry: false,
    refetchInterval: (query) => query.state.data?.sync.state === "syncing" ? 1000 : 30000,
  });
  const options = ["", ...new Set(namespaces.isError ? [] : namespaces.data?.items ?? [])]
    .filter((name) => !search || (name || "All namespaces").toLowerCase().includes(search.toLowerCase()));
  const listID = `${generated}-options`;
  useEffect(() => {
    if (open) document.getElementById(`${generated}-${active}`)?.scrollIntoView?.({ block: "nearest" });
  }, [active, generated, open]);
  const choose = (namespace: string) => { onChange(namespace); setSearch(""); setOpen(false); setActive(0); };
  return <div ref={field} className="relative min-w-0" onBlur={(event) => { if (!event.currentTarget.contains(event.relatedTarget) && !panel.current?.contains(event.relatedTarget)) { setOpen(false); setSearch(""); } }}>
    <input id={id} role="combobox" aria-label={label} aria-autocomplete="list" aria-expanded={open} aria-controls={open ? listID : undefined}
      aria-activedescendant={open && options.length ? `${generated}-${Math.min(active, options.length - 1)}` : undefined}
      className={className} value={open ? search : value || "All namespaces"}
      onFocus={() => { setOpen(true); setSearch(""); }}
      onClick={() => setOpen(true)}
      onChange={(event) => { setSearch(event.target.value); setActive(0); setOpen(true); }}
      onKeyDown={(event) => {
        if (event.key === "Escape") { setOpen(false); setSearch(""); return; }
        if (event.key === "ArrowDown" || event.key === "ArrowUp") {
          event.preventDefault(); setOpen(true);
          setActive((index) => Math.max(0, Math.min(options.length - 1, index + (event.key === "ArrowDown" ? 1 : -1))));
        }
        if (event.key === "Enter") {
          event.preventDefault();
          if (open && options[active] !== undefined) choose(options[active]);
        }
      }} />
    {open && createPortal(<div ref={panel} style={position} onKeyDown={(event) => { if (event.key === "Escape") { field.current?.querySelector("input")?.focus(); setOpen(false); setSearch(""); } }}
      onBlur={(event) => { if (!panel.current?.contains(event.relatedTarget) && !field.current?.contains(event.relatedTarget)) { setOpen(false); setSearch(""); } }}
      className="fixed z-overlay overflow-auto rounded-control border border-ink-600 bg-surface-raised shadow-overlay">
      {(namespaces.isPending || namespaces.data?.sync.state === "syncing") && <p role="status" className="p-2 text-xs text-ink-400">Loading namespaces…</p>}
      {namespaces.isError && <div role="status" className="p-2 text-xs text-ink-400">Namespaces unavailable. <button type="button" className="text-link" onClick={() => void namespaces.refetch()}>Retry namespaces</button></div>}
      <ul id={listID} role="listbox" aria-label={`${label} options`} className="max-h-52 overflow-y-auto">
        {options.map((name, index) => <li key={name} id={`${generated}-${index}`} role="option" aria-selected={value === name}
          onPointerDown={(event) => event.preventDefault()} onClick={() => choose(name)}
          className={`cursor-pointer truncate px-3 py-2 text-xs ${index === active ? "bg-accent-subtle text-link" : "text-ink-200"}`}>{name || "All namespaces"}</li>)}
      </ul>
      {!options.length && !namespaces.isPending && <p className="p-2 text-xs text-ink-400">No matching namespaces</p>}
    </div>, document.body)}
  </div>;
}
