import { useSearchParams } from "react-router-dom";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import clsx from "clsx";
import { SkCard } from "@/components/Skeleton";
import { useRetryUnlessForbidden } from "@/lib/access";
import { api, ApiError } from "@/lib/api";
import {
  accountDisplay,
  countText,
  DEFAULT_LENSES,
  fmtNumber,
  groupRuleText,
  nodeCount,
  notReadBadge,
  resourceTypeWords,
  TOPOLOGY_LENSES,
  type TopologyLens,
  type TopologyLevel,
  type TopologyNode,
  type TopologyQuery,
  type TopologyView,
} from "@/lib/awsEstate";
import { AwsOff, AwsPage, NoAccounts, PageError, ScopeBar } from "../shared/AwsPage";
import { ResourceSearch } from "../shared/ResourceSearch";
import { useAwsAccounts } from "../shared/useAwsAccounts";
import { AccountsMap } from "./AccountsMap";
import { FocusMap } from "./FocusMap";
import { FocusPanel } from "./FocusPanel";
import { GroupPanel } from "./GroupPanel";
import { GroupsMap } from "./GroupsMap";

// AWS / Topology: never the whole estate. Accounts first (the server skips
// it for one account), then the groups inside one account or one group's
// resources, then one resource and what is up to three hops from it. A
// view holds at most 300 nodes; beyond that the page shows the server's
// narrower-scope hint and draws nothing. Topology has no summary card, so
// it says nothing about coverage.

function parseLenses(v: string | null): TopologyLens[] {
  if (v === null) return DEFAULT_LENSES;
  const ids = new Set(TOPOLOGY_LENSES.map((l) => l.id));
  const out = v.split(",").filter((x): x is TopologyLens => ids.has(x as TopologyLens));
  return out.length > 0 ? out : DEFAULT_LENSES;
}

function parseHops(v: string | null): 1 | 2 | 3 {
  return v === "1" ? 1 : v === "3" ? 3 : 2;
}

function parseLevel(v: string | null, ref: string): TopologyLevel | undefined {
  if (v === "accounts" || v === "groups" || v === "focus") return v;
  return ref ? "focus" : undefined;
}

/** Phone width: the map is wider than the screen, so the page opens in the List view. */
function isPhoneWidth(): boolean {
  return typeof window !== "undefined" && typeof window.matchMedia === "function" && window.matchMedia("(max-width: 639px)").matches;
}

function useTopologyParams() {
  const [params, setParams] = useSearchParams();
  const view = params.get("view");
  const ref = params.get("ref") ?? "";
  const set = (changes: Record<string, string | null>) => {
    const next = new URLSearchParams(params);
    for (const [k, v] of Object.entries(changes)) {
      if (v === null || v === "") next.delete(k);
      else next.set(k, v);
    }
    setParams(next, { replace: true });
  };
  return {
    level: parseLevel(params.get("level"), ref),
    account: params.get("account") ?? "",
    region: params.get("region") ?? "",
    group: params.get("group") ?? "",
    ref,
    hops: parseHops(params.get("hops")),
    lenses: parseLenses(params.get("lens")),
    // An explicit choice wins; with none, a phone opens the List view.
    list: view === "list" || (view === null && isPhoneWidth()),
    selected: params.get("sel"),
    set,
  };
}

function LensBar({ lenses, onChange }: { lenses: TopologyLens[]; onChange: (l: TopologyLens[]) => void }) {
  return (
    <span className="inline-flex flex-wrap items-center gap-1.5" data-lenses>
      <span aria-hidden="true">Lens</span>
      {TOPOLOGY_LENSES.map((l) => {
        const on = lenses.includes(l.id);
        return (
          <button
            key={l.id}
            type="button"
            aria-pressed={on}
            // At least one lens stays on, so the map always says which links it draws.
            disabled={on && lenses.length === 1}
            onClick={() => onChange(on ? lenses.filter((x) => x !== l.id) : [...lenses, l.id])}
            className={clsx(
              "min-h-7 rounded-control border px-3 text-xs font-medium focus:outline-none focus-visible:ring-2 focus-visible:ring-accent",
              on ? "border-accent bg-accent-subtle text-ink-50" : "border-ink-500 text-ink-300 hover:text-ink-100",
            )}
          >
            {l.label}
          </button>
        );
      })}
    </span>
  );
}

function MapListSwitch({ list, onChange }: { list: boolean; onChange: (list: boolean) => void }) {
  return (
    <div role="group" aria-label="View" className="inline-flex rounded-control border border-ink-500 bg-surface-raised p-0.5">
      {(["Map", "List"] as const).map((v) => {
        const on = (v === "List") === list;
        return (
          <button
            key={v}
            type="button"
            aria-pressed={on}
            onClick={() => onChange(v === "List")}
            className={clsx("min-h-6 rounded px-2.5 text-xs font-medium", on ? "bg-accent-subtle text-ink-50" : "text-ink-300 hover:text-ink-100")}
          >
            {v}
          </button>
        );
      })}
    </div>
  );
}

/** Too many nodes for one view: the server's hint, never a cut graph. */
function TooMany({ view, onHops }: { view: TopologyView; onHops: (h: 1 | 2 | 3) => void }) {
  const hint = view.hint;
  const fit = view.level === "focus" && hint?.hops && hint.hops >= 1 && hint.hops <= 3 ? (hint.hops as 1 | 2 | 3) : null;
  return (
    <section className="card p-4" data-too-many role="status">
      <p className="text-sm text-ink-50">
        {view.node_count && view.node_count > 0
          ? `More than ${fmtNumber(Math.max(view.node_count, view.max_nodes ?? 0))} to draw at once`
          : "Too many to draw at once"}
      </p>
      {hint?.text && <p className="mt-1 text-xs text-ink-300">{hint.text}</p>}
      {fit !== null && (
        <button type="button" className="btn mt-3" onClick={() => onHops(fit)}>
          Show {fit} {fit === 1 ? "hop" : "hops"}
        </button>
      )}
    </section>
  );
}

function Empty({ text, notRead = false }: { text: string; notRead?: boolean }) {
  return (
    <section className={clsx("card p-4 text-sm", notRead ? "border-dashed text-ink-300" : "text-ink-200")} data-empty data-not-read={notRead || undefined}>
      {text}
    </section>
  );
}

function ListTable({ head, rows }: { head: string[]; rows: Array<{ key: string; cells: React.ReactNode[]; onClick?: () => void; dashed?: boolean }> }) {
  return (
    <div className="overflow-x-auto" data-list>
      <table className="w-full min-w-[32rem] text-sm">
        <thead>
          <tr className="text-2xs uppercase tracking-wider text-ink-300">
            {head.map((h, i) => (
              <th key={h} className={clsx("px-4 py-2 font-medium", i === 0 ? "text-left" : "text-right")}>
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.key} className={clsx("border-t border-ink-500/40", r.onClick && "cursor-pointer hover:bg-ink-600/30", r.dashed && "text-ink-300")} onClick={r.onClick}>
              {r.cells.map((c, i) => (
                <td key={i} className={clsx("max-w-[18rem] truncate px-4 py-2", i === 0 ? "text-left text-ink-50" : "text-right font-mono text-xs tabular-nums text-ink-100")}>
                  {c}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** The account an ARN names: arn:aws:service:region:account:… ; empty when it names none. */
function arnAccount(ref: string): string {
  const account = ref.split(":")[4] ?? "";
  return /^[0-9]{12}$/.test(account) ? account : "";
}

function countCell(n: TopologyNode, field: "resources" | "open_issues"): string {
  const c = nodeCount(n, field);
  return c.state === "not_read" ? "not read" : countText(c.n, c.state);
}

export function AwsTopologyPage() {
  const p = useTopologyParams();
  const { set } = p;
  const known = useAwsAccounts();
  const retry = useRetryUnlessForbidden();
  // Groups and Focus take exactly one account; the server refuses Groups
  // over two or more. A link without one opens the account the resource's
  // ARN names, or the first account.
  const needsOne = p.level === "groups" || p.level === "focus";
  const fallback = needsOne && !p.account ? arnAccount(p.ref) || (known.accounts[0]?.account_id ?? "") : "";
  const waiting = needsOne && !p.account && !fallback && !known.loaded && !known.error;
  // No account named and the account list failed: say so, with a retry, never load forever.
  const noAccountToRead = needsOne && !p.account && !fallback && known.error !== null;
  const query: TopologyQuery = {
    level: p.level,
    account: p.level === "accounts" ? undefined : p.account || fallback || undefined,
    region: p.region || undefined,
    group: p.level === "groups" ? p.group || undefined : undefined,
    ref: p.level === "focus" ? p.ref || undefined : undefined,
    hops: p.level === "focus" ? p.hops : undefined,
    lens: p.lenses,
  };
  const needsRef = p.level === "focus" && !p.ref;
  const topology = useQuery({
    queryKey: ["aws-topology", query],
    queryFn: () => api.awsTopology(query),
    enabled: !needsRef && !waiting && !noAccountToRead,
    retry,
    placeholderData: keepPreviousData,
  });
  const view = topology.data;
  const names = new Map<string, string>();
  for (const a of known.accounts) names.set(a.account_id, a.display_name || a.account_id);
  for (const n of view?.nodes ?? []) if (n.kind === "account" && n.account && n.name && n.name !== n.account) names.set(n.account, n.name);
  const accountName = (id: string) => names.get(id) ?? accountDisplay({ id });
  const account = view?.account || p.account;
  const level = view?.level ?? p.level ?? "accounts";
  const openedGroup = level === "groups" ? view?.group || "" : "";
  const openedNode = openedGroup ? view?.nodes.find((n) => n.kind === "group" && n.group === openedGroup) : undefined;

  const crumbs: Array<{ label: string; to?: string }> = [{ label: "Topology", to: level === "accounts" ? undefined : "/aws/topology?level=accounts" }];
  if (level !== "accounts" && account) crumbs.push({ label: accountName(account), to: openedGroup || level === "focus" ? `/aws/topology?level=groups&account=${account}` : undefined });
  if (openedGroup) crumbs.push({ label: openedNode?.name ?? openedGroup });
  if (level === "focus") crumbs.push({ label: view?.nodes.find((n) => n.id === view.ref)?.name ?? "Resource" });

  const focusOn = (ref: string) => {
    const owner = view?.nodes.find((n) => n.id === ref)?.account || account || arnAccount(ref);
    set({ level: "focus", ref, account: owner || null, sel: null, group: null });
  };
  let body: React.ReactNode;
  let panel: React.ReactNode = null;
  if (needsRef) {
    body = <Empty text="Open a resource from a group or an issue to see what it connects to." />;
  } else if (noAccountToRead) {
    body = <PageError error={known.error} onRetry={known.retry} retrying={known.retrying} what="accounts" />;
  } else if (topology.isPending || waiting) {
    body = <SkCard lines={8} />;
  } else if (topology.isError) {
    body =
      topology.error instanceof ApiError && topology.error.status === 404 ? (
        <Empty text="Not in the stored reads. It may have been deleted, or its account removed." />
      ) : (
        <PageError error={topology.error} onRetry={() => void topology.refetch()} retrying={topology.isFetching} what="Topology" />
      );
  } else if (!view?.enabled) {
    body = <AwsOff />;
  } else if (view.too_many) {
    body = <TooMany view={view} onHops={(h) => set({ hops: h === 2 ? null : String(h) })} />;
  } else if (view.level === "accounts") {
    const connected = view.nodes.filter((n) => n.kind === "account" && n.label === "connected");
    if (connected.length === 0) body = <NoAccounts />;
    else if (p.list)
      body = (
        <section className="card min-w-0">
          <ListTable
            head={["Account", "Resources", "Open issues"]}
            rows={view.nodes
              .filter((n) => n.kind === "account")
              .map((n) => ({
                key: n.id,
                dashed: n.label !== "connected" || n.read === "not_read",
                onClick: n.label === "connected" ? () => set({ level: "groups", account: n.account ?? "", sel: null }) : undefined,
                cells: [
                  accountDisplay({ id: n.account ?? n.id, name: n.name }),
                  n.label === "connected" ? countCell(n, "resources") : "not connected",
                  n.label === "connected" ? countCell(n, "open_issues") : "",
                ],
              }))}
          />
        </section>
      );
    else
      body = (
        <section className="card min-w-0" data-level="accounts">
          <AccountsMap nodes={view.nodes} links={view.links} canWrite={known.canWrite} onOpen={(id) => set({ level: "groups", account: id, sel: null, group: null })} />
        </section>
      );
  } else if (view.level === "groups") {
    const notRead = view.nodes.length === 1 && view.nodes[0].kind === "account" && view.nodes[0].read === "not_read" ? view.nodes[0] : null;
    const groups = view.nodes.filter((n) => n.kind === "group");
    const selected = !openedGroup ? (groups.find((g) => g.id === p.selected) ?? null) : null;
    if (notRead) body = <Empty text={notReadBadge(notRead.not_read_reason)} notRead />;
    else if (view.nodes.length === 0) body = <Empty text={openedGroup ? "No resources in this group" : "No resources in this account"} />;
    else {
      const cards = openedGroup ? view.nodes.filter((n) => n.kind === "resource" && n.group === openedGroup) : groups;
      body = p.list ? (
        <section className="card min-w-0">
          <ListTable
            head={openedGroup ? ["Resource", "Type", "Open issues"] : ["Group", "Made by", "Resources", "Open issues"]}
            rows={cards.map((n) => ({
              key: n.id,
              dashed: (n.not_read?.length ?? 0) > 0,
              onClick: openedGroup ? () => focusOn(n.id) : () => set({ sel: n.id }),
              cells: openedGroup
                ? [n.name, resourceTypeWords(n.type ?? "", n.service), countCell(n, "open_issues")]
                : [n.name, groupRuleText(n), countCell(n, "resources"), countCell(n, "open_issues")],
            }))}
          />
        </section>
      ) : (
        <section className="card min-w-0" data-level="groups">
          <GroupsMap
            nodes={view.nodes}
            links={view.links}
            opened={openedGroup || null}
            lenses={view.lenses}
            selected={p.selected}
            onSelect={(n) => set({ sel: n.id })}
            onOpen={(n) => set({ group: n.group ?? null, sel: null, account: account || null, level: "groups" })}
            onFocus={focusOn}
            accountName={accountName}
          />
        </section>
      );
      if (selected && account) panel = <GroupPanel key={selected.id} account={account} region={p.region || undefined} group={selected} groups={groups} canWrite={known.canWrite} />;
    }
  } else if (view.level === "focus") {
    const ref = view.ref || p.ref;
    if (view.nodes.length === 0) body = <Empty text="This resource is not in the stored reads" />;
    else {
      const selectedId = p.selected ?? ref;
      const node = view.nodes.find((n) => n.id === selectedId) ?? view.nodes.find((n) => n.id === ref);
      body = p.list ? (
        <section className="card min-w-0">
          <ListTable
            head={["Resource", "Type", "Account"]}
            rows={view.nodes.map((n) => ({
              key: n.id,
              dashed: n.dangling || n.kind === "other",
              onClick: n.dangling || n.kind === "other" ? undefined : () => set({ sel: n.id }),
              cells: [n.name || n.description || "", resourceTypeWords(n.type ?? n.kind, n.service), n.account ? accountName(n.account) : "not connected"],
            }))}
          />
        </section>
      ) : (
        <section className="card min-w-0" data-level="focus">
          <FocusMap
            nodes={view.nodes}
            links={view.links}
            refId={ref}
            selected={selectedId}
            onSelect={(id) => set({ sel: id })}
            hops={p.hops}
            onHops={(h) => set({ hops: h === 2 ? null : String(h) })}
            accountName={accountName}
          />
        </section>
      );
      if (node && !node.dangling && node.kind !== "other") panel = <FocusPanel node={node} nodes={view.nodes} links={view.links} accountName={accountName} />;
    }
  } else {
    body = <Empty text="Nothing to show" />;
  }

  return (
    <AwsPage title="Topology" crumbs={crumbs}>
      <ScopeBar
        accounts={known.accounts.map((a) => ({ id: a.account_id, name: a.display_name || a.account_id }))}
        regions={view?.regions ?? known.regions}
        account={level === "accounts" ? "" : account}
        region={p.region}
        onRegion={(v) => set({ region: v })}
        onAccount={(v) => set(v ? { level: "groups", account: v, sel: null, ref: null, group: null } : { level: "accounts", account: null, sel: null, ref: null, group: null })}
      >
        <LensBar lenses={p.lenses} onChange={(l) => set({ lens: l.join(",") === DEFAULT_LENSES.join(",") ? null : l.join(",") })} />
        <MapListSwitch list={p.list} onChange={(l) => set({ view: l ? "list" : isPhoneWidth() ? "map" : null })} />
        <ResourceSearch account={level === "accounts" ? undefined : account} region={p.region} accountName={accountName} />
      </ScopeBar>
      <div className={clsx("grid min-w-0 gap-4", panel && "xl:grid-cols-[minmax(0,1fr)_23rem]")}>
        <div className="min-w-0">{body}</div>
        {panel}
      </div>
    </AwsPage>
  );
}
