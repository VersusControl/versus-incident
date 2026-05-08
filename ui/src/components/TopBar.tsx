import { useQuery } from "@tanstack/react-query";
import { Circle } from "lucide-react";
import { api } from "@/lib/api";

// TopBar shows the current page title plus a small live status dot tied to
// the same /status endpoint the dashboard uses. The dot turns red when the
// API can't be reached so operators always know if the agent is down.
interface Props {
  title: string;
  subtitle?: string;
  actions?: React.ReactNode;
}

export function TopBar({ title, subtitle, actions }: Props) {
  const { data, isError } = useQuery({
    queryKey: ["status-pulse"],
    queryFn: api.status,
    refetchInterval: 5_000,
    retry: 0,
  });

  const ok = !isError && !!data;

  return (
    <header
      className="flex h-14 shrink-0 items-center justify-between
                 border-b border-ink-100 bg-white px-6"
    >
      <div className="flex items-baseline gap-3">
        <h1 className="text-base font-semibold text-ink-900">{title}</h1>
        {subtitle && (
          <span className="text-xs text-ink-400">{subtitle}</span>
        )}
      </div>

      <div className="flex items-center gap-3">
        {actions}
        <div className="flex items-center gap-1.5 text-2xs text-ink-400">
          <Circle
            size={8}
            className={ok ? "fill-good text-good" : "fill-bad text-bad"}
          />
          <span>{ok ? "Agent online" : "Agent unreachable"}</span>
        </div>
      </div>
    </header>
  );
}
