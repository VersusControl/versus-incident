import clsx from "clsx";
import {
  accountDisplay,
  byTypeText,
  countText,
  groupRuleText,
  nodeCount,
  resourceTypeWords,
  UNGROUPED_ID,
  type TopologyLink,
  type TopologyNode,
} from "@/lib/awsEstate";
import { Canvas, Legend } from "./Canvas";
import { useMapView } from "./useMapView";
import { boxStyle, layoutGroups } from "./layout";

// The Groups level: inside one account, its groups as cards that name the
// rule that made them, Ungrouped as a card of its own (never hidden), and
// what the groups link to outside the account as pills above. Opening a
// group shows its members the same way, with the other groups as pills.

function resourcesText(n: TopologyNode): string {
  const r = nodeCount(n, "resources");
  if (r.state === "not_read") return "Resources not read";
  return `${countText(r.n, r.state)} ${r.n === 1 && r.state === "read" ? "resource" : "resources"}`;
}

/** The tier line: Act now in red, else Worth a look in grey; open issues when no tier was read. */
function TierLine({ n }: { n: TopologyNode }) {
  const act = nodeCount(n, "act_now");
  const worth = nodeCount(n, "worth_a_look");
  // With neither tier nor the total read, the card is not assessed: never drawn as clear.
  if (act.state === "not_read" && worth.state === "not_read" && nodeCount(n, "open_issues").state === "not_read") {
    return (
      <span className="text-ink-300" data-tier="not_read">
        issues not read
      </span>
    );
  }
  if ((act.n ?? 0) > 0) {
    return (
      <span className="text-sev-critical" data-tier="act_now">
        {countText(act.n, act.state)} act now
      </span>
    );
  }
  if ((worth.n ?? 0) > 0) {
    return (
      <span className="text-ink-300" data-tier="worth_a_look">
        {countText(worth.n, worth.state)} worth a look
      </span>
    );
  }
  const text = issuesText(n);
  return text ? <span className="text-ink-300">{text}</span> : null;
}

function issuesText(n: TopologyNode): string | null {
  const i = nodeCount(n, "open_issues");
  if (i.n === null || i.n === 0) return null;
  return `${countText(i.n, i.state)} open ${i.n === 1 && i.state === "read" ? "issue" : "issues"}`;
}

/** Changes in 24 h, only when the view counts them (the changes lens); left out then means not read. */
function changesText(n: TopologyNode, counted: boolean): string | null {
  const c = nodeCount(n, "changes_24h");
  if (c.state === "not_read") return counted ? "changes not read" : null;
  if (c.n === 0) return null;
  return `${countText(c.n, c.state)} changed in 24 h`;
}

function GroupCard({
  g,
  selected,
  changesCounted,
  onSelect,
  onOpen,
}: {
  g: TopologyNode;
  selected: boolean;
  changesCounted: boolean;
  onSelect: (n: TopologyNode) => void;
  onOpen: (n: TopologyNode) => void;
}) {
  const changed = changesText(g, changesCounted);
  return (
    <div
      className={clsx(
        "flex h-full w-full min-w-0 flex-col justify-center rounded-card border bg-surface-raised px-4",
        selected ? "border-accent ring-1 ring-accent" : "border-ink-500/60 hover:border-ink-400",
      )}
      data-group={g.group}
    >
      <span className="flex min-w-0 items-center gap-2">
        <button
          type="button"
          onClick={() => onSelect(g)}
          aria-pressed={selected}
          className="min-w-0 truncate text-left text-sm font-semibold text-ink-50 hover:underline focus:outline-none focus-visible:ring-2 focus-visible:ring-accent"
          title={g.name}
        >
          {g.name}
        </button>
        <span className="ml-auto max-w-[55%] shrink-0 truncate rounded border border-ink-500 px-1.5 font-mono text-2xs text-ink-200" title={`Made by ${groupRuleText(g)}`} data-rule>
          {groupRuleText(g)}
        </span>
      </span>
      <span className="mt-1 flex min-w-0 items-center gap-2 text-xs text-ink-200">
        <span className="truncate">{resourcesText(g)}</span>
        <button type="button" className="ml-auto shrink-0 text-link hover:underline" onClick={() => onOpen(g)}>
          Open ›
        </button>
      </span>
      <span className="mt-1 min-h-4 truncate text-xs">
        <TierLine n={g} />
        {changed && <span className="text-ink-300"> · {changed}</span>}
      </span>
    </div>
  );
}

function UngroupedCard({ g, total, selected, onSelect }: { g: TopologyNode; total: number | null; selected: boolean; onSelect: (n: TopologyNode) => void }) {
  const r = nodeCount(g, "resources");
  const share = total && r.n !== null ? Math.round((r.n / total) * 100) : null;
  return (
    <div
      className={clsx("flex h-full w-full min-w-0 flex-col justify-center rounded-card border bg-surface-raised px-4", selected ? "border-accent ring-1 ring-accent" : "border-ink-500/60")}
      data-group={g.group}
    >
      <span className="flex min-w-0 items-center gap-2">
        <span className="text-sm font-semibold text-ink-50">Ungrouped</span>
        {share !== null && r.state === "read" && <span className="rounded border border-ink-500 px-1.5 font-mono text-2xs text-ink-200">{share}%</span>}
        <button type="button" className="ml-auto shrink-0 text-xs text-link hover:underline" onClick={() => onSelect(g)}>
          Move resources ›
        </button>
      </span>
      <span className="mt-1 truncate text-xs text-ink-200" title={g.by_type ? byTypeText(g.by_type, 20) : undefined}>
        {resourcesText(g)}
        {g.by_type && Object.keys(g.by_type).length > 0 ? `: ${byTypeText(g.by_type)}` : ""}
      </span>
      <span className="mt-1 min-h-4 truncate text-xs">
        <TierLine n={g} />
      </span>
    </div>
  );
}

function MemberCard({ n, onFocus }: { n: TopologyNode; onFocus: (id: string) => void }) {
  const dashed = n.dangling || (n.not_read?.length ?? 0) > 0;
  return (
    <button
      type="button"
      onClick={() => onFocus(n.id)}
      className={clsx(
        "flex h-full w-full min-w-0 flex-col justify-center rounded-card border bg-surface-raised px-4 text-left hover:border-ink-400 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent",
        dashed ? "border-dashed border-ink-400 bg-transparent" : "border-ink-500/60",
      )}
      title={`${resourceTypeWords(n.type ?? "", n.service)} ${n.name}`}
      data-node={n.id}
    >
      <span className="truncate text-xs text-ink-300">{resourceTypeWords(n.type ?? "", n.service)}</span>
      <span className="mt-1 truncate font-mono text-xs text-ink-50">{n.name}</span>
      <span className="mt-1 min-h-4 truncate text-xs text-ink-300">{dashed ? `not read: ${n.not_read?.join(", ") ?? ""}` : (issuesText(n) ?? "")}</span>
    </button>
  );
}

function Pill({ n, accountName }: { n: TopologyNode; accountName: (id: string) => string }) {
  const label = n.kind === "account" ? accountName(n.account ?? n.id) : n.kind === "group" ? n.name : n.name || n.description || "";
  const suffix = n.kind === "account" ? "account" : n.kind === "group" ? "group" : n.dangling ? "not connected" : "";
  return (
    <span
      className={clsx(
        "flex h-full w-full items-center justify-center truncate rounded-full border bg-surface-raised px-3 text-xs",
        n.kind === "account" && n.label !== "connected" ? "border-dashed border-ink-400" : "border-ink-500",
        n.dangling && "border-dashed border-ink-400",
      )}
      title={n.description || label}
      data-pill={n.id}
    >
      <span className="truncate font-semibold text-ink-50">{n.kind === "account" ? accountDisplay({ id: n.account ?? n.id, name: label }) : label}</span>
      {suffix && <span className="ml-1 shrink-0 text-ink-300">{suffix}</span>}
    </span>
  );
}

export function GroupsMap({
  nodes,
  links,
  opened,
  selected,
  lenses,
  onSelect,
  onOpen,
  onFocus,
  accountName,
}: {
  /** The lenses the answer was drawn with; "changes" means change counts are expected. */
  lenses?: string[];
  nodes: TopologyNode[];
  links: TopologyLink[];
  /** The group opened, when the view shows one group's members. */
  opened: string | null;
  selected: string | null;
  onSelect: (n: TopologyNode) => void;
  onOpen: (n: TopologyNode) => void;
  onFocus: (ref: string) => void;
  accountName: (id: string) => string;
}) {
  const isCard = opened ? (n: TopologyNode) => n.kind === "resource" && n.group === opened : (n: TopologyNode) => n.kind === "group";
  const [measureRef, view] = useMapView();
  const plan = layoutGroups(nodes, links, isCard, 3, view, selected);
  const groups = nodes.filter((n) => n.kind === "group");
  // The share needs an exact total: any group not read or capped leaves it unknown.
  const resources = groups.reduce<number | null>((sum, g) => (sum === null || g.resources == null || g.resources_capped ? null : sum + g.resources), 0);
  const changesCounted = (lenses ?? []).includes("changes") || groups.some((g) => g.changes_24h != null);
  const keys = ["configured"];
  if (links.some((l) => l.kind === "may_access")) keys.push("may_access");
  if (links.some((l) => l.kind === "inferred")) keys.push("inferred");
  if (plan.links.some((l) => l.sides === "one")) keys.push("one");
  return (
    <div ref={measureRef} className="min-w-0">
      <Canvas layout={plan} label={opened ? "The group's resources and their links" : "Groups and the links between them"}>
        {plan.pills.map((p) => {
          const b = plan.boxes.get(p.id);
          return b ? (
            <div key={p.id} className="absolute" style={boxStyle(b)}>
              <Pill n={p} accountName={accountName} />
            </div>
          ) : null;
        })}
        {plan.cards.map((n) => {
          const b = plan.boxes.get(n.id);
          if (!b) return null;
          let card: React.ReactNode;
          if (n.kind === "resource") card = <MemberCard n={n} onFocus={onFocus} />;
          else if (n.group === UNGROUPED_ID) card = <UngroupedCard g={n} total={resources} selected={selected === n.id} onSelect={onSelect} />;
          else card = <GroupCard g={n} selected={selected === n.id} changesCounted={changesCounted} onSelect={onSelect} onOpen={onOpen} />;
          return (
            <div key={n.id} className="absolute" style={boxStyle(b)}>
              {card}
            </div>
          );
        })}
      </Canvas>
      <Legend keys={keys} />
    </div>
  );
}
