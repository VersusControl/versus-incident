import { Link } from "react-router-dom";
import { format } from "date-fns";
import clsx from "clsx";
import { countText, nodeCount, resourceTypeWords, SEVERITY_LABEL, type NodeHealth, type TopologyLink, type TopologyNode } from "@/lib/awsEstate";

// The side panel of a resource in Focus: where it is, what was not read
// about it, its open issues and recent changes, and who read its links.

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <p className="text-xs text-ink-300">{label}</p>
      <div className="mt-0.5 text-sm text-ink-100">{children}</div>
    </div>
  );
}

function NotRead({ text }: { text: string }) {
  return (
    <span className="inline-block rounded border border-dashed border-ink-500 px-2 py-0.5 text-xs text-ink-300" data-not-read>
      {text}
    </span>
  );
}

const HEALTH_TONE: Record<string, string> = {
  critical: "text-sev-critical",
  warning: "text-sev-warn",
  ok: "text-ink-100",
  none: "text-ink-200",
  unknown: "text-ink-300",
};

function clock(t: string | undefined): string {
  if (!t) return "";
  const d = new Date(t);
  return Number.isNaN(d.getTime()) ? "" : format(d, "HH:mm");
}

/** Health in words. Not read is grey and dashed; nothing here is ever green. */
function Health({ h }: { h: NodeHealth }) {
  if (h.state === "not_read") return <NotRead text={`Not read${h.summary ? `: ${h.summary}` : ""}`} />;
  const words = h.summary || { ok: "Healthy", none: "Nothing to check", unknown: "Unknown", warning: "Warning", critical: "Critical" }[h.state];
  const since = clock(h.since);
  return (
    <span data-health={h.state}>
      <span className={HEALTH_TONE[h.state]}>{words}</span>
      {since && <span className="text-ink-300"> · since {since}</span>}
    </span>
  );
}

export function FocusPanel({
  node,
  nodes,
  links,
  accountName,
}: {
  node: TopologyNode;
  nodes: TopologyNode[];
  links: TopologyLink[];
  accountName: (id: string) => string;
}) {
  const own = links.filter((e) => e.from === node.id || e.to === node.id);
  const cross = own.filter((e) => e.seen_from);
  const oneSide = cross.some((e) => e.seen_from === "one" || e.seen_from === "mixed");
  // The accounts whose reads hold this node's links: its own, and the far
  // ends of links between accounts.
  const accountOf = new Map(nodes.map((n) => [n.id, n.account]));
  const readers = [
    ...new Set(
      [node.account, ...cross.flatMap((e) => [accountOf.get(e.from), accountOf.get(e.to)])].filter((a): a is string => Boolean(a)),
    ),
  ];
  const issues = nodeCount(node, "open_issues");
  const changes = nodeCount(node, "changes_24h");
  const where = [node.account && accountName(node.account), node.region].filter(Boolean).join(", ");
  const lastRead = node.last_read_at ? new Date(node.last_read_at) : null;
  return (
    <aside className="card min-w-0 self-start" aria-label={node.name} data-focus-panel={node.id}>
      <div className="border-b border-ink-500/40 px-4 py-3">
        <h2 className="truncate text-sm font-semibold text-ink-50" title={node.name}>
          {node.name}
        </h2>
        <p className="truncate text-xs text-ink-300">{[resourceTypeWords(node.type ?? "", node.service), node.description].filter(Boolean).join(", ")}</p>
      </div>
      <div className="space-y-4 px-4 py-4">
        {node.health && (
          <Field label="Health">
            <Health h={node.health} />
          </Field>
        )}
        {where && <Field label="Where">{where}</Field>}
        {(node.not_read?.length ?? 0) > 0 && (
          <Field label="Not read">
            <NotRead text={node.not_read?.join(", ") ?? ""} />
          </Field>
        )}
        {node.top_issue ? (
          <Field label="Open issue">
            <p data-top-issue>{node.top_issue.title}</p>
            <p className="flex items-center justify-between text-xs">
              <span>
                <span
                  className={clsx(
                    node.top_issue.severity === "critical" ? "text-sev-critical" : node.top_issue.severity === "high" ? "text-sev-high" : "text-ink-200",
                  )}
                >
                  {SEVERITY_LABEL[node.top_issue.severity]}
                </span>
                <span className="text-ink-300">, {node.top_issue.tier === "act_now" ? "act now" : "worth a look"}</span>
              </span>
              <Link to={`/aws/issues?id=${encodeURIComponent(node.top_issue.id)}`} className="text-link hover:underline">
                Open ›
              </Link>
            </p>
            {(issues.n ?? 0) > 1 && <p className="mt-0.5 text-xs text-ink-300">{countText((issues.n ?? 1) - 1, issues.state)} more open</p>}
            {node.top_issue.cause_text && (
              <div className="mt-2 rounded border-l-2 border-sev-warn bg-ink-600/30 px-3 py-2 text-xs" data-likely-cause>
                <p className="text-2xs text-ink-300">Likely cause</p>
                <p className="text-ink-100">{node.top_issue.cause_text}</p>
              </div>
            )}
          </Field>
        ) : (
          <Field label="Open issues">
            {issues.state === "not_read" ? (
              <NotRead text="Not assessed" />
            ) : (
              <span data-open-issues>{countText(issues.n, issues.state)}</span>
            )}
          </Field>
        )}
        {node.last_change ? (
          <Field label="Last change">
            <div className="rounded border-l-2 border-sev-warn bg-ink-600/30 px-3 py-2 text-xs" data-last-change>
              <p className="text-ink-100">{node.last_change.event_name}</p>
              <p className="mt-0.5 text-ink-300">
                by {node.last_change.actor}, <span className="font-mono tabular-nums">{clock(node.last_change.time)}</span>
              </p>
            </div>
          </Field>
        ) : node.last_change_known === true ? (
          <Field label="Last change">
            <span data-last-change="none">No change in the last day</span>
          </Field>
        ) : node.last_change_known === false ? (
          <Field label="Last change">
            <NotRead text="Last change not read" />
          </Field>
        ) : null}
        {changes.state !== "not_read" && (
          <Field label="Changes, 24 h">
            <span className="flex items-center justify-between">
              <span>{countText(changes.n, changes.state)}</span>
              {(changes.n ?? 0) > 0 && (
                <Link to="/aws/changes" className="text-xs text-link hover:underline">
                  Open in Changes ›
                </Link>
              )}
            </span>
          </Field>
        )}
        {readers.length > 0 && (
          <Field label="Seen from">
            <span data-seen-from>
              {readers.map(accountName).join(", ")}
              {cross.length > 0 ? (oneSide ? ", one side read" : ", both sides read") : ""}
            </span>
          </Field>
        )}
        {lastRead && !Number.isNaN(lastRead.getTime()) && <Field label="Last read">{format(lastRead, "d MMM HH:mm")}</Field>}
      </div>
    </aside>
  );
}
