import clsx from "clsx";

interface Props {
  children: React.ReactNode;
  tone?: "default" | "good" | "warn" | "bad" | "accent";
  className?: string;
}

export function Pill({ children, tone = "default", className }: Props) {
  return (
    <span
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
  if (v === "") return <Pill>—</Pill>;
  return <Pill tone="accent">{verdict}</Pill>;
}
