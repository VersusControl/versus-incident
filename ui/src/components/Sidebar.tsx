import { NavLink } from "react-router-dom";
import {
  Activity,
  AlertTriangle,
  BellOff,
  Book,
  Brain,
  Flame,
  KeyRound,
  LineChart,
  Lock,
  ScrollText,
  Server,
  Settings,
  ShieldAlert,
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
  // Absent for in-development placeholders — those aren't wired to a route yet.
  to?: string;
  label: string;
  icon: LucideIcon;
  end?: boolean;
  dim?: boolean;
  dimTitle?: string;
  locked?: boolean;
  // A greenlit-but-unbuilt capability: renders as a non-clickable, dimmed row
  // with an "in development" indicator instead of a NavLink. Never navigates
  // and never shows active state, independent of agent/enterprise state.
  inDev?: boolean;
}

export function SidebarContent({ onNavigate }: { onNavigate?: () => void }) {
  // Agent config drives whether the Agent zone is usable. Shares the
  // ["agent-config"] cache key with TopBar/NowPage — one fetch, zero extra
  // load. enable===false means the agent is deliberately off.
  const configQ = useQuery({
    queryKey: ["agent-config"],
    queryFn: api.getAgentConfig,
    staleTime: 60_000,
    retry: 1,
  });
  const agentOff = configQ.data?.enable === false;

  // The agent status route (/api/agent/status) is only mounted when the agent
  // is enabled, so skip the poll when it's off — otherwise it 404s and the
  // runbooks hint flickers. Disabled query → data undefined → runbooks off.
  const statusQ = useQuery({
    queryKey: ["status"],
    queryFn: api.status,
    enabled: !agentOff,
    staleTime: 30_000,
    retry: 1,
  });

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
    { to: "/incidents", label: "Incidents", icon: AlertTriangle },
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
  ];
  // AI groups the agent's reasoning surfaces — the Decisions it makes and
  // the SLIs/SLOs it recommends — apart from the raw learned-catalog views
  // above. SLIs/SLOs stays enterprise-gated; Decisions is ungated. Both keep
  // their existing routes; this is purely a nav regrouping.
  const ai: SideItem[] = [
    { to: "/agent/decisions", label: "Decisions", icon: Sparkles },
    { to: "/analyses", label: "Analyses", icon: Brain },
    {
      to: "/agent/slo",
      label: "SLIs/SLOs",
      icon: Target,
      locked: enterpriseLocked,
      dim: enterpriseLocked,
      dimTitle: enterpriseLocked
        ? "Enterprise feature — requires an intelligence license"
        : undefined,
    },
    // inDev
    { label: "Alert fatigue", icon: BellOff, inDev: true },
    { label: "Secret scanning", icon: KeyRound, inDev: true },
    { label: "Fraud detection", icon: ShieldAlert, inDev: true },
  ];

  const tools: SideItem[] = [
    {
      to: "/agent/runbooks",
      label: "Runbooks",
      icon: Book,
      // Visible-with-hint instead of vanishing while status loads/fails
      // (empty-nav-state rule). The page explains the 503 case.
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

  // When the agent is disabled (agent.enable=false) every Agent view and the
  // agent-backed Runbooks tool are non-functional. Dim + lock them with a
  // clear hint (visible-with-hint) so they read as disabled instead
  // of navigating to empty/erroring pages.
  const AGENT_OFF_HINT =
    "AI agent is disabled — set agent.enable to use these views";
  const applyAgentOff = (items: SideItem[]): SideItem[] =>
    agentOff
      ? items.map((it) => ({
          ...it,
          dim: true,
          locked: true,
          dimTitle: AGENT_OFF_HINT,
        }))
      : items;

  return (
    // force-dark: the rail keeps its dark identity in BOTH themes — the
    // CSS variables are re-pinned on this subtree (see index.css).
    <div className="force-dark flex h-full flex-col bg-ink-950 text-ink-100">
      <SidebarBrand />

      <nav aria-label="Primary" className="dark-scroll flex-1 overflow-y-auto px-2 py-2">
        <Zone title="Respond" items={respond} onNavigate={onNavigate} />
        <Zone title="Agent" items={applyAgentOff(agent)} onNavigate={onNavigate} />
        <Zone title="AI" items={applyAgentOff(ai)} onNavigate={onNavigate} />
        <Zone title="Tools" items={applyAgentOff(tools)} onNavigate={onNavigate} />
        <Zone title="Manage" items={manage} onNavigate={onNavigate} />
      </nav>
    </div>
  );
}

// Desktop rail. <1024px the AppShell renders SidebarContent inside a drawer
// instead (the fixed 224px rail ate 60% of a phone viewport).
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
        <SideLink key={item.to ?? item.label} {...item} onNavigate={onNavigate} />
      ))}
    </>
  );
}

function SideLink({
  to,
  label,
  icon: Icon,
  end,
  dim,
  dimTitle,
  locked,
  inDev,
  onNavigate,
}: SideItem & { onNavigate?: () => void }) {
  // In-development placeholder: a greenlit capability with no route yet.
  // Render a non-navigable, dimmed row (a div, never a NavLink) so it can't
  // navigate or show an active state regardless of agent/enterprise flags.
  if (inDev) {
    const indevSlug = label
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-+|-+$/g, "");
    return (
      <div
        aria-disabled="true"
        title="In development — coming soon"
        data-testid={`nav-indev-${indevSlug}`}
        className="group flex min-h-9 cursor-default items-center gap-2 rounded-control px-3 py-2 text-xs text-ink-400"
      >
        <span className="h-4 w-0.5 rounded-full bg-transparent" />
        <Icon size={14} />
        <span className="flex-1">{label}</span>
        <span
          data-testid="nav-indev-indicator"
          className="rounded-full bg-ink-800 px-1.5 py-px text-2xs font-medium uppercase tracking-wider text-ink-500"
        >
          Dev
        </span>
      </div>
    );
  }

  return (
    <NavLink
      to={to as string}
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
        </>
      )}
    </NavLink>
  );
}
