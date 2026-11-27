import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import clsx from "clsx";
import { useToast } from "@/components/toastContext";
import { api, ApiError } from "@/lib/api";
import { accountDisplay, countText, fmtNumber, nodeCount, notReadBadge, ROLE_LABEL, type TopologyLink, type TopologyNode } from "@/lib/awsEstate";
import { Canvas, Legend } from "./Canvas";
import { useMapView } from "./useMapView";
import { boxStyle, layoutAccounts } from "./layout";

// The Accounts level: one card per connected account inside the
// organization's frame, accounts of the organization we cannot read on its
// last row, and every other account we have evidence of in a column on the
// right. Links are summarised by kind with a count.

function resourcesLine(a: TopologyNode) {
  if (a.read === "not_read") {
    return (
      <span className="text-ink-300" data-not-read>
        {notReadBadge(a.not_read_reason)}
      </span>
    );
  }
  const r = nodeCount(a, "resources");
  const issues = nodeCount(a, "open_issues");
  const actNow = nodeCount(a, "act_now");
  const state = r.state === "read" && a.read === "partial" ? "capped" : r.state;
  // A lower bound reads "more than 900 resources, not all read" and wraps, never cut.
  const n = r.state === "not_read" ? "resources not read" : state === "capped" && (r.n ?? 0) > 0 ? `more than ${fmtNumber(r.n)} resources, not all read` : `${countText(r.n, state)} resources`;
  // Red only when an Act-now issue is open; other open issues stay neutral.
  const urgent = (actNow.n ?? 0) > 0;
  return (
    <>
      {n}
      {a.read === "partial" && a.not_read_reason && <span className="text-ink-300"> · {notReadBadge(a.not_read_reason)}</span>}
      {issues.state === "not_read" && (
        <span className="text-ink-300" data-tier="not_read">
          , issues not read
        </span>
      )}
      {issues.n !== null && issues.n > 0 && (
        <span className={urgent ? "text-sev-critical" : "text-ink-300"} data-tier={urgent ? "act_now" : "worth_a_look"}>
          , {countText(issues.n, issues.state)} open {issues.n === 1 && issues.state === "read" ? "issue" : "issues"}
        </span>
      )}
    </>
  );
}

function ConnectedCard({ a, onOpen }: { a: TopologyNode; onOpen: (account: string) => void }) {
  const notRead = a.read === "not_read";
  return (
    <button
      type="button"
      onClick={() => onOpen(a.account ?? a.id)}
      className={clsx(
        "flex h-full w-full min-w-0 flex-col justify-center rounded-card border bg-surface-raised px-4 text-left hover:border-accent focus:outline-none focus-visible:ring-2 focus-visible:ring-accent",
        notRead ? "border-dashed border-ink-400 bg-transparent" : "border-ink-500/60",
      )}
      title={`${a.name} (${a.account ?? a.id})`}
      data-account-card={a.account ?? a.id}
      data-state={a.read ?? "read"}
    >
      <span className="flex min-w-0 items-center gap-2">
        <span className="truncate text-sm font-semibold text-ink-50">{accountDisplay({ id: a.account ?? a.id, name: a.name })}</span>
        {a.role && <span className="pill shrink-0">{ROLE_LABEL[a.role]}</span>}
      </span>
      {(a.role_detail || a.description) && (
        <span className="mt-1 line-clamp-2 text-xs text-ink-200" title={a.role_detail || a.description}>
          {a.role_detail || a.description}
        </span>
      )}
      <span className="mt-1 line-clamp-2 text-xs text-ink-300" data-resources-line>
        {resourcesLine(a)}
      </span>
    </button>
  );
}

function SameOrgCard({ a }: { a: TopologyNode }) {
  return (
    <div className="flex h-full w-full min-w-0 flex-col justify-center rounded-card border border-dashed border-ink-400 px-4" data-account-card={a.account} data-label="same_org">
      <span className="flex min-w-0 items-center gap-2">
        <span className="truncate text-sm font-semibold text-ink-50">{a.name && a.name !== a.account ? a.name : "Management"}</span>
        <span className="shrink-0 font-mono text-xs text-ink-300">{`${(a.account ?? "").slice(0, 4)}-…`}</span>
        <Link to="/aws/settings" className="ml-auto shrink-0 text-xs text-link hover:underline">
          Connect ›
        </Link>
      </span>
      <span className="mt-1 truncate text-xs text-ink-300">Same organization, not connected</span>
    </div>
  );
}

function LabelForm({ id, onDone }: { id: string; onDone: () => void }) {
  const [name, setName] = useState("");
  const qc = useQueryClient();
  const toast = useToast();
  const save = useMutation({
    mutationFn: () => api.saveAwsAccountLabel(id, { label: "known", name: name.trim() }),
    onSuccess: () => {
      toast.push({ tone: "ok", title: "Account labelled" });
      void qc.invalidateQueries({ queryKey: ["aws-topology"] });
      onDone();
    },
    onError: (e) => toast.push({ tone: "error", title: e instanceof ApiError ? e.message : "Couldn't save the label" }),
  });
  return (
    <form
      className="mt-1.5 flex items-center gap-1.5"
      onSubmit={(e) => {
        e.preventDefault();
        if (name.trim()) save.mutate();
      }}
    >
      <input
        className="input h-7 min-w-0 flex-1 text-xs"
        aria-label={`Name for account ${id}`}
        placeholder="Name, e.g. our log archive"
        maxLength={128}
        value={name}
        onChange={(e) => setName(e.target.value)}
        autoFocus
      />
      <button type="submit" className="btn btn-primary-strong px-2 py-1" disabled={!name.trim() || save.isPending}>
        Save
      </button>
      <button type="button" className="btn px-2 py-1" onClick={onDone}>
        Cancel
      </button>
    </form>
  );
}

function OtherCard({ a, canWrite }: { a: TopologyNode; canWrite: boolean }) {
  const [labelling, setLabelling] = useState(false);
  const unknown = a.label === "unknown";
  const id = a.account ?? a.id;
  return (
    <div className="flex h-full w-full min-w-0 flex-col justify-center rounded-card border border-dashed border-ink-400 px-4" data-account-card={id} data-label={a.label}>
      <span className="flex min-w-0 items-center gap-2">
        <span className="truncate text-sm font-semibold text-ink-50" title={id}>
          {accountDisplay({ id, name: a.name })}
        </span>
        <span className="pill ml-auto shrink-0">{unknown ? "Unknown" : "Known"}</span>
      </span>
      <span className="mt-1 truncate text-xs text-ink-300">{a.description || (unknown ? "Not labelled" : "Labelled by you")}</span>
      {unknown &&
        (labelling ? (
          <LabelForm id={id} onDone={() => setLabelling(false)} />
        ) : (
          <button
            type="button"
            className="btn mt-1.5 self-start border-accent px-2 py-1 text-ink-50"
            disabled={!canWrite}
            title={canWrite ? undefined : "Your role can't label accounts"}
            onClick={() => setLabelling(true)}
          >
            Label this account
          </button>
        ))}
    </div>
  );
}

function FarCard({ n }: { n: TopologyNode }) {
  const far = n.description || n.name;
  return (
    <div className="flex h-full w-full min-w-0 flex-col justify-center rounded-card border border-dashed border-ink-400 px-4" data-far={n.id}>
      <span className="truncate text-sm font-semibold text-ink-50">{n.type === "organization" ? "Organization" : "Not connected"}</span>
      <span className="mt-1 truncate text-xs text-ink-300" title={far}>
        {far}
      </span>
    </div>
  );
}

export function AccountsMap({
  nodes,
  links,
  canWrite,
  onOpen,
}: {
  nodes: TopologyNode[];
  links: TopologyLink[];
  canWrite: boolean;
  onOpen: (account: string) => void;
}) {
  const [measureRef, view] = useMapView();
  const plan = layoutAccounts(nodes, links, view);
  // The Other accounts heading sits at the top of its column, beside the frame.
  const heading = plan.otherHeading;
  const keys = ["both", "one", "not_connected"];
  if (links.some((l) => l.kind === "may_access")) keys.splice(1, 0, "may_access");
  return (
    <div ref={measureRef} className="min-w-0">
      <Canvas layout={plan} label="Accounts and the links between them">
        {plan.frame && (
          <p className="absolute text-2xs font-medium uppercase tracking-widest text-ink-300" style={{ left: plan.frame.x + 20, top: plan.frame.y + 16 }} data-heading="organization">
            {plan.inside.some((n) => n.label === "same_org") ? "Organization" : "Connected accounts"}
          </p>
        )}
        {heading && (
          <p className="absolute text-2xs font-medium uppercase tracking-widest text-ink-300" style={{ left: heading.x, top: heading.y + 4 }} data-heading="other-accounts">
            Other accounts
          </p>
        )}
        {[...plan.inside, ...plan.outside].map((n) => {
          const b = plan.boxes.get(n.id);
          if (!b) return null;
          let card: React.ReactNode;
          if (n.kind === "account" && n.label === "connected") card = <ConnectedCard a={n} onOpen={onOpen} />;
          else if (n.kind === "account" && n.label === "same_org") card = <SameOrgCard a={n} />;
          else if (n.kind === "account") card = <OtherCard a={n} canWrite={canWrite} />;
          else card = <FarCard n={n} />;
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
