import clsx from "clsx";
import { countText, nodeCount, resourceTypeWords, type TopologyLink, type TopologyNode } from "@/lib/awsEstate";
import { Canvas, Legend } from "./Canvas";
import { boxStyle, layoutFocus } from "./layout";

// The Focus level: one resource in the middle and what is up to three hops
// away, across groups and accounts. The line style says how a link is
// known; what the reads cannot see is dashed.

function NodeCard({
  n,
  root,
  selected,
  rootAccount,
  accountName,
  onSelect,
  cause,
}: {
  cause: boolean;
  n: TopologyNode;
  root: boolean;
  selected: boolean;
  rootAccount: string | undefined;
  accountName: (id: string) => string;
  onSelect: (id: string) => void;
}) {
  const type = n.kind === "account" ? "Account" : resourceTypeWords(n.type ?? "", n.service);
  if (n.dangling || n.kind === "other") {
    return (
      <div className="flex h-full w-full min-w-0 flex-col justify-center rounded-card border border-dashed border-ink-400 px-3" data-node={n.id} data-dangling title={n.description}>
        <span className="truncate text-xs text-ink-100">{type || n.name}</span>
        <span className="truncate text-xs text-ink-300">{n.description || "not connected"}</span>
      </div>
    );
  }
  const notRead = (n.not_read?.length ?? 0) > 0 || n.health?.state === "not_read";
  const place = root
    ? [n.account && accountName(n.account), n.region].filter(Boolean).join(", ")
    : n.account && n.account !== rootAccount
      ? accountName(n.account)
      : "";
  const issues = nodeCount(n, "open_issues");
  const changed = nodeCount(n, "changes_24h");
  // A dot only for a problem: critical health or an Act-now issue in red, a warning in amber.
  const problem =
    n.health?.state === "critical" || n.top_issue?.tier === "act_now"
      ? "bg-sev-critical"
      : n.health?.state === "warning" || (issues.n ?? 0) > 0
        ? "bg-sev-warn"
        : null;
  return (
    <button
      type="button"
      onClick={() => onSelect(n.id)}
      aria-pressed={selected}
      title={`${type} ${n.name}`}
      className={clsx(
        "flex h-full w-full min-w-0 flex-col justify-center rounded-card border bg-surface-raised px-3 text-left focus:outline-none focus-visible:ring-2 focus-visible:ring-accent",
        root || selected ? "border-accent ring-1 ring-accent" : notRead ? "border-dashed border-ink-400 bg-transparent" : "border-ink-500/60 hover:border-ink-400",
        (cause || (changed.n ?? 0) > 0) && "border-l-4 border-l-sev-warn",
      )}
      data-node={n.id}
      data-likely-cause={cause || undefined}
      data-root={root || undefined}
      data-changed={(changed.n ?? 0) > 0 || undefined}
    >
      <span className="flex min-w-0 items-center gap-2">
        <span className="truncate text-xs text-ink-300">{type}</span>
        {cause && <span className="ml-auto shrink-0 rounded-full border border-sev-warn/60 bg-sev-warn/15 px-2 text-2xs text-sev-warn">Likely cause</span>}
        {!cause && place && (
          <span className="ml-auto max-w-[50%] shrink-0 truncate rounded-full border border-ink-500 px-2 text-2xs text-ink-100" title={place}>
            {place}
          </span>
        )}
      </span>
      <span className="mt-1 flex min-w-0 items-center gap-2">
        <span className={clsx("truncate font-mono", root ? "text-sm font-semibold text-ink-50" : "text-xs text-ink-50")}>{n.name}</span>
        {problem && <span className={clsx("h-2 w-2 shrink-0 rounded-full", problem)} aria-label={`${countText(issues.n, issues.state)} open issues`} data-problem />}
        {notRead && <span className="ml-auto shrink-0 text-2xs text-ink-300">not read</span>}
      </span>
    </button>
  );
}

export function FocusMap({
  nodes,
  links,
  refId,
  selected,
  onSelect,
  hops,
  onHops,
  accountName,
}: {
  nodes: TopologyNode[];
  links: TopologyLink[];
  refId: string;
  selected: string | null;
  onSelect: (id: string) => void;
  hops: number;
  onHops: (h: 1 | 2 | 3) => void;
  accountName: (id: string) => string;
}) {
  const plan = layoutFocus(nodes, links, refId);
  const root = nodes.find((n) => n.id === refId);
  const kinds = new Set(links.map((e) => e.kind));
  const keys = ["configured"];
  if (kinds.has("may_access")) keys.push("may_access");
  if (kinds.has("inferred")) keys.push("inferred");
  if (plan.links.some((e) => e.sides === "one")) keys.push("one");
  if (nodes.some((n) => n.dangling || n.kind === "other")) keys.push("not_connected");
  const hidden = nodes.reduce((sum, n) => sum + (n.hidden ?? 0), 0);
  // The nodes a likely cause changed, named by the issues on the map.
  const causes = new Set(nodes.map((n) => n.top_issue?.cause_ref).filter((r): r is string => Boolean(r)));
  return (
    <>
      <div className="flex flex-wrap items-center justify-end gap-x-4 gap-y-2 px-6 pt-4 text-xs text-ink-300">
        {hidden > 0 && (
          <span data-hidden title="Each resource shows its nearest neighbours first">
            {hidden === 1 ? "1 more resource not drawn" : `${hidden} more resources not drawn`}
          </span>
        )}
        <span className="inline-flex items-center gap-2">
          <span aria-hidden="true">Hops</span>
          <div role="group" aria-label="Hops" className="inline-flex rounded-control border border-ink-500 bg-surface-raised p-0.5">
            {([1, 2, 3] as const).map((h) => (
              <button
                key={h}
                type="button"
                aria-pressed={hops === h}
                onClick={() => onHops(h)}
                className={clsx("min-h-6 rounded px-2.5 text-xs font-medium", hops === h ? "bg-accent-subtle text-ink-50" : "text-ink-300 hover:text-ink-100")}
              >
                {h}
              </button>
            ))}
          </div>
        </span>
      </div>
      <Canvas layout={plan} label={`What is up to ${hops} hops from ${root?.name ?? "the resource"}`}>
        {nodes.map((n) => {
          const b = plan.boxes.get(n.id);
          if (!b) return null;
          return (
            <div key={n.id} className="absolute" style={boxStyle(b)}>
              <NodeCard n={n} cause={causes.has(n.id)} root={n.id === refId} selected={selected === n.id} rootAccount={root?.account} accountName={accountName} onSelect={onSelect} />
            </div>
          );
        })}
      </Canvas>
      <Legend keys={keys} />
    </>
  );
}
