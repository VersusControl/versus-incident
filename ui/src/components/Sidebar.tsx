import { NavLink } from "react-router-dom";
import {
  Activity,
  AlertTriangle,
  Book,
  Brain,
  Flame,
  LineChart,
  Lock,
  ScrollText,
  Server,
  Settings,
  ShieldCheck,
  Sparkles,
  Target,
  Users,
  Waypoints,
  type LucideIcon,
} from "lucide-react";
import clsx from "clsx";
import { useQuery } from "@tanstack/react-query";
import { api, ApiError } from "@/lib/api";
import { useOpenIncidentCount } from "@/lib/hooks";
import { useTheme } from "@/lib/theme";

// Theme-aware sidebar brand — uses the same SVG logo approach as the
// management platform TopNav Brand component.
function SidebarBrand() {
  const { theme } = useTheme();
  // Sidebar is force-dark, so we always show the light (white) logo —
  // unless the app theme is light, in which case we show the dark logo
  // so it stays visible if the sidebar ever inherits the light surface.
  const logoSrc = theme === "dark" ? "/versus-logo-light.svg" : "/versus-logo-light.svg";

  return (
    <div className="flex items-center gap-2 px-4 py-4">
      <img src={logoSrc} alt="Versus" className="h-5 w-auto" />
      <div className="text-2xs uppercase tracking-wider text-ink-200">
        Versus Incident
      </div>
    </div>
  );
}

// Three zones organized by the user's job, not the backend's modules
// (UX_REDESIGN §2.1): RESPOND is the 3am zone and always comes first;
// AGENT is calm curation; MANAGE is admin.
interface SideItem {
  to: string;
  label: string;
  icon: LucideIcon;
  end?: boolean;
  badge?: number;
  dim?: boolean;
  dimTitle?: string;
  locked?: boolean;
}

export function SidebarContent({ onNavigate }: { onNavigate?: () => void }) {
  const statusQ = useQuery({
    queryKey: ["status"],
    queryFn: api.status,
    staleTime: 30_000,
    retry: 1,
  });
  const { open } = useOpenIncidentCount();

  // Probe the enterprise baselines endpoint once to determine if Metrics/Traces
  // are available. A 403 (no intelligence license) or 404 (OSS binary — route
  // absent) means locked; any other error or success means available.
  const baselinesProbe = useQuery({
    queryKey: ["baselines-probe"],
    queryFn: async () => {
      try {
        await api.listBaselines({ type: "metric" });
        return true;
      } catch (e) {
        if (e instanceof ApiError && (e.status === 403 || e.status === 404)) {
          return false;
        }
        // Network error / 500 — assume available (the page itself handles errors)
        return true;
      }
    },
    staleTime: 5 * 60_000, // re-probe every 5 minutes at most
    retry: 1,
  });
  const enterpriseLocked = baselinesProbe.data === false;

  const runbooksAvailable = statusQ.data?.runbooks_available ?? false;

  const respond: SideItem[] = [
    { to: "/now", label: "Now", icon: Flame },
    { to: "/incidents", label: "Incidents", icon: AlertTriangle, badge: open },
    { to: "/analyses", label: "Analyses", icon: Brain },
  ];
  const agent: SideItem[] = [
    { to: "/agent", label: "Overview", icon: Activity, end: true },
    { to: "/agent/services", label: "Services", icon: Server },
    { to: "/agent/logs", label: "Logs", icon: ScrollText },
    {
      to: "/agent/metrics",
      label: "Metrics",
      icon: LineChart,
      locked: enterpriseLocked,
      dim: enterpriseLocked,
      dimTitle: enterpriseLocked
        ? "Enterprise feature — requires an intelligence license"
        : undefined,
    },
    {
      to: "/agent/traces",
      label: "Traces",
      icon: Waypoints,
      locked: enterpriseLocked,
      dim: enterpriseLocked,
      dimTitle: enterpriseLocked
        ? "Enterprise feature — requires an intelligence license"
        : undefined,
    },
    {
      to: "/agent/slo",
      label: "SLOs",
      icon: Target,
      locked: enterpriseLocked,
      dim: enterpriseLocked,
      dimTitle: enterpriseLocked
        ? "Enterprise feature — requires an intelligence license"
        : undefined,
    },
    { to: "/agent/decisions", label: "Decisions", icon: Sparkles },
  ];
  const tools: SideItem[] = [
    {
      to: "/agent/runbooks",
      label: "Runbooks",
      icon: Book,
      // Visible-with-hint instead of vanishing while status loads/fails
      // (audit I4 / empty-nav-state rule). The page explains the 503 case.
      dim: !runbooksAvailable,
      dimTitle: runbooksAvailable
        ? undefined
        : "Requires the AI subsystem (agent.ai.enable) and a storage backend — open for details",
    },
  ];
  const manage: SideItem[] = [
    { to: "/people", label: "People", icon: Users },
    { to: "/admin", label: "Admin", icon: ShieldCheck },
    { to: "/settings", label: "Settings", icon: Settings },
  ];

  return (
    // force-dark: the rail keeps its dark identity in BOTH themes — the
    // CSS variables are re-pinned on this subtree (see index.css).
    <div className="force-dark flex h-full flex-col bg-ink-950 text-ink-100">
      <SidebarBrand />

      <nav aria-label="Primary" className="dark-scroll flex-1 overflow-y-auto px-2 py-2">
        <Zone title="Respond" items={respond} onNavigate={onNavigate} />
        <Zone title="Agent" items={agent} onNavigate={onNavigate} />
        <Zone title="Tools" items={tools} onNavigate={onNavigate} />
        <Zone title="Manage" items={manage} onNavigate={onNavigate} />
      </nav>
    </div>
  );
}

// Desktop rail. <1024px the AppShell renders SidebarContent inside a drawer
// instead (audit A4: the fixed 224px rail ate 60% of a phone viewport).
export function Sidebar() {
  return (
    <aside className="hidden h-full w-56 shrink-0 border-r border-ink-800 lg:block">
      <SidebarContent />
    </aside>
  );
}

function Zone({
  title,
  items,
  onNavigate,
}: {
  title: string;
  items: SideItem[];
  onNavigate?: () => void;
}) {
  return (
    <>
      <div className="mt-2 px-2 py-2 text-2xs uppercase tracking-wider text-ink-300 first:mt-0">
        {title}
      </div>
      {items.map((item) => (
        <SideLink key={item.to} {...item} onNavigate={onNavigate} />
      ))}
    </>
  );
}

function SideLink({
  to,
  label,
  icon: Icon,
  end,
  badge,
  dim,
  dimTitle,
  locked,
  onNavigate,
}: SideItem & { onNavigate?: () => void }) {
  return (
    <NavLink
      to={to}
      end={end}
      title={dimTitle}
      onClick={onNavigate}
      className={({ isActive }) =>
        clsx(
          "group flex min-h-9 items-center gap-2 rounded-control px-3 py-2 text-xs transition-colors",
          isActive
            ? "bg-accent-subtle text-ink-50"
            : dim
              ? "text-ink-400 hover:bg-ink-800 hover:text-ink-200"
              : "text-ink-100 hover:bg-ink-800 hover:text-ink-50",
        )
      }
    >
      {({ isActive }) => (
        <>
          <span
            className={clsx(
              "h-4 w-0.5 rounded-full",
              isActive ? "bg-accent" : "bg-transparent",
            )}
          />
          <Icon size={14} />
          <span className="flex-1">{label}</span>
          {locked && (
            <Lock size={12} className="text-ink-500" aria-label="Enterprise" />
          )}
          {badge !== undefined && badge > 0 && (
            <span className="rounded-full bg-sev-critical/20 px-1.5 text-2xs tabular-nums text-sev-critical">
              {badge}
            </span>
          )}
        </>
      )}
    </NavLink>
  );
}
