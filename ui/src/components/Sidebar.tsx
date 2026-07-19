import { useState } from "react";
import { NavLink } from "react-router-dom";
import {
  Activity,
  ChevronLeft,
  ChevronRight,
  Flame,
  Lock,
  ShieldCheck,
  Sparkles,
  Wrench,
  type LucideIcon,
} from "lucide-react";
import clsx from "clsx";
import { useQuery } from "@tanstack/react-query";
import { api, ApiError } from "@/lib/api";
import { useTheme } from "@/lib/theme";

const COLLAPSE_KEY = "versus.sidebar.collapsed";

// Theme-aware sidebar brand — uses the same SVG logo approach as the
// management platform TopNav Brand component.
function SidebarBrand({ collapsed }: { collapsed?: boolean }) {
  const { theme } = useTheme();
  // Sidebar is force-dark, so we always show the light (white) logo —
  // unless the app theme is light, in which case we show the dark logo
  // so it stays visible if the sidebar ever inherits the light surface.
  const logoSrc = theme === "dark" ? "/versus-logo-light.svg" : "/versus-logo-light.svg";

  return (
    <div
      className={clsx(
        "flex items-center gap-2 py-4",
        collapsed ? "justify-center px-2" : "px-4",
      )}
    >
      <img src={logoSrc} alt="Versus" className="h-5 w-auto" />
      {!collapsed && (
        <div className="text-2xs uppercase tracking-wider text-ink-200">
          Versus Incident
        </div>
      )}
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
  end?: boolean;
  dim?: boolean;
  dimTitle?: string;
  locked?: boolean;
  // A greenlit-but-unbuilt capability: renders as a non-clickable, dimmed row
  // with an "in development" indicator instead of a NavLink. Never navigates
  // and never shows active state, independent of agent/enterprise state.
  inDev?: boolean;
}

// A nav zone: a job-grouped section with a representative icon (shown beside the
// header when expanded, and as the sole section marker when the rail is
// collapsed) and its items.
interface SideZone {
  title: string;
  icon: LucideIcon;
  items: SideItem[];
}

export function SidebarContent({
  onNavigate,
  collapsed = false,
  onToggleCollapse,
}: {
  onNavigate?: () => void;
  collapsed?: boolean;
  // When provided, the desktop collapse/expand toggle is rendered. The mobile
  // drawer omits it (it passes only onNavigate), so the drawer stays unchanged.
  onToggleCollapse?: () => void;
}) {
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
    { to: "/now", label: "Now" },
    { to: "/incidents", label: "Incidents" },
  ];
  const agent: SideItem[] = [
    { to: "/agent", label: "Overview", end: true },
    { to: "/agent/services", label: "Services" },
    { to: "/agent/logs", label: "Logs" },
    {
      to: "/agent/metrics",
      label: "Metrics",
      locked: enterpriseLocked,
      dim: enterpriseLocked,
      dimTitle: enterpriseLocked
        ? "Enterprise feature — requires an intelligence license"
        : undefined,
    },
    {
      to: "/agent/traces",
      label: "Traces",
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
    { to: "/agent/decisions", label: "Decisions" },
    { to: "/analyses", label: "Analyses" },
    {
      to: "/agent/slo",
      label: "SLIs/SLOs",
      locked: enterpriseLocked,
      dim: enterpriseLocked,
      dimTitle: enterpriseLocked
        ? "Enterprise feature — requires an intelligence license"
        : undefined,
    },
    // inDev
    { label: "Alert fatigue", inDev: true },
    { label: "Secret scanning", inDev: true },
    { label: "Fraud detection", inDev: true },
  ];

  const tools: SideItem[] = [
    {
      to: "/agent/runbooks",
      label: "Runbooks",
      // Visible-with-hint instead of vanishing while status loads/fails
      // (empty-nav-state rule). The page explains the 503 case.
      dim: !runbooksAvailable,
      dimTitle: runbooksAvailable
        ? undefined
        : "Requires the AI subsystem (agent.ai.enable) and a storage backend — open for details",
    },
  ];
  const manage: SideItem[] = [
    { to: "/people", label: "People" },
    { to: "/admin", label: "Admin" },
    { to: "/settings", label: "Settings" },
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

  const zones: SideZone[] = [
    { title: "Respond", icon: Flame, items: respond },
    { title: "Agent", icon: Activity, items: applyAgentOff(agent) },
    { title: "AI", icon: Sparkles, items: applyAgentOff(ai) },
    { title: "Tools", icon: Wrench, items: applyAgentOff(tools) },
    { title: "Manage", icon: ShieldCheck, items: manage },
  ];

  return (
    // force-dark: the rail keeps its dark identity in BOTH themes — the
    // CSS variables are re-pinned on this subtree (see index.css).
    <div className="force-dark flex h-full flex-col bg-ink-950 text-ink-100">
      <SidebarBrand collapsed={collapsed} />

      <nav
        aria-label="Primary"
        className="dark-scroll flex-1 overflow-y-auto px-2 py-2"
      >
        {zones.map((zone) =>
          collapsed ? (
            <CollapsedZone key={zone.title} {...zone} onNavigate={onNavigate} />
          ) : (
            <Zone key={zone.title} {...zone} onNavigate={onNavigate} />
          ),
        )}
      </nav>

      {onToggleCollapse && (
        <div className="border-t border-ink-800 p-2">
          <button
            type="button"
            onClick={onToggleCollapse}
            aria-expanded={!collapsed}
            aria-label={collapsed ? "Expand sidebar" : "Collapse sidebar"}
            title={collapsed ? "Expand sidebar" : "Collapse sidebar"}
            className={clsx(
              "flex min-h-9 w-full items-center gap-2 rounded-control py-2 text-xs text-ink-300 transition-colors hover:bg-ink-800 hover:text-ink-100",
              collapsed ? "justify-center px-0" : "px-3",
            )}
          >
            {collapsed ? (
              <ChevronRight size={16} aria-hidden />
            ) : (
              <>
                <ChevronLeft size={16} aria-hidden />
                <span className="flex-1 text-left">Collapse</span>
              </>
            )}
          </button>
        </div>
      )}
    </div>
  );
}

// Desktop rail. <1024px the AppShell renders SidebarContent inside a drawer
// instead (the fixed 224px rail ate 60% of a phone viewport). The rail can be
// collapsed to a narrow icon-only strip; the choice persists across reloads.
export function Sidebar() {
  const [collapsed, setCollapsed] = useState<boolean>(() => {
    try {
      return window.localStorage.getItem(COLLAPSE_KEY) === "1";
    } catch {
      return false;
    }
  });

  const toggle = () =>
    setCollapsed((prev) => {
      const next = !prev;
      try {
        window.localStorage.setItem(COLLAPSE_KEY, next ? "1" : "0");
      } catch {
        // localStorage unavailable (private mode / SSR) — the toggle still
        // works for the session, it just won't persist.
      }
      return next;
    });

  return (
    <aside
      className={clsx(
        "hidden h-full shrink-0 border-r border-ink-800 transition-[width] duration-150 lg:block",
        collapsed ? "w-14" : "w-56",
      )}
    >
      <SidebarContent collapsed={collapsed} onToggleCollapse={toggle} />
    </aside>
  );
}

function Zone({
  title,
  icon: Icon,
  items,
  onNavigate,
}: SideZone & {
  onNavigate?: () => void;
}) {
  return (
    <>
      <div className="mt-2 flex items-center gap-2 px-2 py-2 text-2xs uppercase tracking-wider text-ink-300 first:mt-0">
        <Icon size={13} aria-hidden />
        <span>{title}</span>
      </div>
      {items.map((item) => (
        <SideLink key={item.to ?? item.label} {...item} onNavigate={onNavigate} />
      ))}
    </>
  );
}

// CollapsedZone renders one zone as a single group-icon link in the narrow
// rail: it points at the zone's primary (first navigable) item and exposes the
// zone name via the tooltip, so the collapsed rail stays usable with icons
// alone. Zones with no navigable item (nothing but in-dev placeholders) render
// a non-interactive icon marker instead of a dead link.
function CollapsedZone({
  title,
  icon: Icon,
  items,
  onNavigate,
}: SideZone & {
  onNavigate?: () => void;
}) {
  const primary = items.find((it) => it.to && !it.inDev);

  if (!primary?.to) {
    return (
      <div
        title={title}
        aria-label={title}
        className="mx-auto my-0.5 flex h-9 w-9 items-center justify-center rounded-control text-ink-500"
      >
        <Icon size={18} aria-hidden />
      </div>
    );
  }

  return (
    <NavLink
      to={primary.to}
      end={primary.end}
      title={title}
      aria-label={title}
      onClick={onNavigate}
      className={({ isActive }) =>
        clsx(
          "mx-auto my-0.5 flex h-9 w-9 items-center justify-center rounded-control transition-colors",
          isActive
            ? "bg-accent-subtle text-ink-50"
            : "text-ink-200 hover:bg-ink-800 hover:text-ink-50",
        )
      }
    >
      <Icon size={18} aria-hidden />
    </NavLink>
  );
}

function SideLink({
  to,
  label,
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
          <span className="flex-1">{label}</span>
          {locked && (
            <Lock size={12} className="text-ink-500" aria-label="Enterprise" />
          )}
        </>
      )}
    </NavLink>
  );
}
