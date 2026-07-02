import clsx from "clsx";
import { Bot } from "lucide-react";

interface Props {
  children: React.ReactNode;
  tone?: "default" | "good" | "warn" | "bad" | "accent";
  className?: string;
  title?: string;
}

export function Pill({ children, tone = "default", className, title }: Props) {
  return (
    <span
      title={title}
      className={clsx(
        "pill",
        tone === "good" && "pill-good",
        tone === "warn" && "pill-warn",
        tone === "bad" && "pill-bad",
        tone === "accent" && "pill-accent",
        className,
      )}
    >
      {children}
    </span>
  );
}

// VerdictPill maps the agent's verdict strings to a tone.
export function VerdictPill({ verdict }: { verdict: string }) {
  const v = (verdict || "").toLowerCase();
  if (v === "spike") return <Pill tone="bad">spike</Pill>;
  if (v === "known") return <Pill tone="good">known</Pill>;
  if (v === "unknown") return <Pill tone="warn">unknown</Pill>;
  // Empty verdict = a pattern the agent is still figuring out. Label it
  // "learning" (matching the column help) instead of a bare "—".
  if (v === "") return <Pill>learning</Pill>;
  return <Pill tone="accent">{verdict}</Pill>;
}

// SourceBadge surfaces where an incident came from so operators can tell
// AI-originated incidents apart from external ones at a glance.
// Agent-originated incidents carry a "agent:<source>:<service>" Source —
// these render an "AI" badge with a bot icon and the full provenance in
// the tooltip. External transports (http/sns/sqs/...) render a plain
// neutral pill with the transport name. An empty source renders nothing.
export function SourceBadge({ source }: { source?: string }) {
  const s = (source || "").trim();
  if (!s) return null;
  if (s === "agent" || s.startsWith("agent:")) {
    return (
      <Pill tone="accent" className="inline-flex items-center gap-1">
        <Bot size={11} />
        <span title={s}>AI</span>
      </Pill>
    );
  }
  return <Pill title={s}>{s}</Pill>;
}

