import { useEffect, useRef, useState } from "react";
import { NavLink } from "react-router-dom";
import {
  Activity,
  BellOff,
  Boxes,
  ChartNoAxesCombined,
  ChevronLeft,
  ChevronRight,
  CircleGauge,
  GitBranch,
  Flame,
  LayoutDashboard,
  Lock,
  MessageSquare,
  Route,
  ScrollText,
  Search,
  Settings,
  ShieldCheck,
  Siren,
  Sparkles,
  Target,
  Users,
  UserRoundCog,
  Wrench,
  type LucideIcon,
} from "lucide-react";
import clsx from "clsx";
import { useQuery } from "@tanstack/react-query";
import { api, ApiError } from "@/lib/api";
import { useTheme } from "@/lib/theme";
import { useDeploymentOrg } from "@/lib/useDeploymentOrg";

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
  icon: LucideIcon;
  end?: boolean;
  dim?: boolean;
  dimTitle?: string;
  locked?: boolean;
  enterpriseOnly?: boolean;
  requiresAgent?: boolean;
  // A greenlit-but-unbuilt capability: renders as a non-clickable, dimmed row
  // with an "in development" indicator instead of a NavLink. Never navigates
  // and never shows active state, independent of agent/enterprise state.
  inDev?: boolean;
}

interface AgentSideItem extends SideItem {
  zone: "Agent" | "AI";
}

// A nav zone: a job-grouped section with a representative icon (shown beside the
// header when expanded, and as the sole section marker when the rail is
// collapsed) and its items.
interface SideZone {
  title: string;
  icon: LucideIcon;
  items: SideItem[];
}

function partitionAgentItems(items: AgentSideItem[], isOSS: boolean) {
  const visible = (zone: AgentSideItem["zone"]) =>
    items.filter(
      (item) => item.zone === zone && (!isOSS || !item.enterpriseOnly),
    );

  return {
    Agent: visible("Agent"),
    AI: visible("AI"),
    Enterprise: isOSS ? items.filter((item) => item.enterpriseOnly) : [],
  };
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

  const deploymentOrg = useDeploymentOrg();
  const isOSS =
    deploymentOrg.error instanceof ApiError &&
    (deploymentOrg.error.status === 403 || deploymentOrg.error.status === 404);

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

  const respond: SideItem[] = [
    { to: "/now", label: "Now", icon: CircleGauge },
    { to: "/incidents", label: "Incidents", icon: Siren },
    { to: "/agent/chat", label: "Chat", icon: MessageSquare, requiresAgent: true },
  ];
  const agent: AgentSideItem[] = [
    { to: "/agent", label: "Overview", icon: LayoutDashboard, end: true, zone: "Agent", requiresAgent: true },
    { to: "/agent/services", label: "Services", icon: Boxes, zone: "Agent", requiresAgent: true },
    { to: "/agent/logs", label: "Logs", icon: ScrollText, zone: "Agent", requiresAgent: true },
    {
      to: "/agent/metrics",
      label: "Metrics",
      icon: ChartNoAxesCombined,
      zone: "Agent",
      requiresAgent: true,
      enterpriseOnly: true,
      locked: enterpriseLocked,
      dim: enterpriseLocked,
      dimTitle: enterpriseLocked
        ? "Enterprise feature — requires an intelligence license"
        : undefined,
    },
    {
      to: "/agent/traces",
      label: "Traces",
      icon: Route,
      zone: "Agent",
      requiresAgent: true,
      enterpriseOnly: true,
      locked: enterpriseLocked,
      dim: enterpriseLocked,
      dimTitle: enterpriseLocked
        ? "Enterprise feature — requires an intelligence license"
        : undefined,
    },
  ];
  // AI groups the agent's tools and reasoning surfaces. Enterprise and
  // runtime availability gates stay attached to their existing destinations.
  const ai: AgentSideItem[] = [
    { to: "/agent/tools", label: "Tool catalog", icon: Wrench, zone: "AI" },
    { to: "/agent/decisions", label: "Decisions", icon: GitBranch, zone: "AI", requiresAgent: true },
    { to: "/analyses", label: "Analyses", icon: Search, zone: "AI", requiresAgent: true },
    {
      to: "/agent/alert-fatigue",
      label: "Alert fatigue",
      icon: BellOff,
      zone: "AI",
      requiresAgent: true,
      enterpriseOnly: true,
      locked: enterpriseLocked,
      dim: enterpriseLocked,
      dimTitle: enterpriseLocked
      ? "Enterprise feature — requires an intelligence license"
      : undefined,
    },
    {
      to: "/agent/slo",
      label: "SLIs/SLOs",
      icon: Target,
      zone: "AI",
      requiresAgent: true,
      enterpriseOnly: true,
      locked: enterpriseLocked,
      dim: enterpriseLocked,
      dimTitle: enterpriseLocked
      ? "Enterprise feature — requires an intelligence license"
      : undefined,
    },
  ];

  const manage: SideItem[] = [
    { to: "/people", label: "People", icon: Users },
    { to: "/admin", label: "Admin", icon: UserRoundCog },
    { to: "/settings", label: "Settings", icon: Settings },
  ];

  // When the agent is disabled, lock only execution-backed destinations. The
  // Tool catalog remains readable so operators can inspect and configure it.
  const AGENT_OFF_HINT =
    "AI agent is disabled — set agent.enable to use these views";
  const applyAgentOff = (items: SideItem[]): SideItem[] =>
    agentOff
      ? items.map((it) =>
          it.requiresAgent
            ? {
                ...it,
                dim: true,
                locked: true,
                dimTitle: AGENT_OFF_HINT,
              }
            : it,
        )
      : items;

  const partitioned = partitionAgentItems([...agent, ...ai], isOSS);
  const zones: SideZone[] = [
    { title: "Respond", icon: Flame, items: applyAgentOff(respond) },
    {
      title: "Agent",
      icon: Activity,
      items: applyAgentOff(partitioned.Agent),
    },
    {
      title: "AI",
      icon: Sparkles,
      items: applyAgentOff(partitioned.AI),
    },
    ...(isOSS
      ? [
          {
            title: "Enterprise",
            icon: Lock,
            items: applyAgentOff(partitioned.Enterprise),
          },
        ]
      : []),
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
  items,
  onNavigate,
}: SideZone & {
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

// CollapsedZone renders one zone in the narrow rail. The icon links to the
// zone's primary (first navigable) item, and hovering or focusing it opens a
// flyout listing every item in the zone — without it the other items are
// unreachable until the rail is expanded. Zones with no navigable item
// (nothing but in-dev placeholders) render a non-interactive icon marker
// instead of a dead link, but still list their contents on hover.
function CollapsedZone({
  title,
  icon: Icon,
  items,
  onNavigate,
}: SideZone & {
  onNavigate?: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [pos, setPos] = useState({ top: 0, left: 0 });
  const wrapRef = useRef<HTMLDivElement>(null);
  const closeTimer = useRef<number | null>(null);
  const primary = items.find((it) => it.to && !it.inDev);

  // The rail's <nav> is overflow-y-auto, which clips an absolutely positioned
  // panel, so the flyout is fixed and placed from the icon's measured rect.
  // It starts flush against the rail: the visible gap is transparent padding
  // INSIDE the flyout, so crossing it never leaves the hover target.
  const place = () => {
    const el = wrapRef.current;
    if (!el) return;
    const r = el.getBoundingClientRect();
    const estHeight = 34 + items.length * 36;
    const top = Math.max(8, Math.min(r.top, window.innerHeight - 8 - estHeight));
    setPos({ top, left: r.right });
  };

  const cancelClose = () => {
    if (closeTimer.current !== null) {
      window.clearTimeout(closeTimer.current);
      closeTimer.current = null;
    }
  };

  const show = () => {
    cancelClose();
    place();
    setOpen(true);
  };

  // Closing is deferred so a pointer that clips a corner on its way to the
  // panel does not dismiss it mid-travel.
  const hide = () => {
    cancelClose();
    closeTimer.current = window.setTimeout(() => setOpen(false), 200);
  };

  const hideNow = () => {
    cancelClose();
    setOpen(false);
  };

  useEffect(() => cancelClose, []);

  return (
    <div
      ref={wrapRef}
      onMouseEnter={show}
      onMouseLeave={hide}
      onFocus={show}
      onBlur={(e) => {
        if (!e.currentTarget.contains(e.relatedTarget as Node | null)) hideNow();
      }}
      onKeyDown={(e) => {
        if (e.key === "Escape") hideNow();
      }}
    >
      {primary?.to ? (
        <NavLink
          to={primary.to}
          end={primary.end}
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
      ) : (
        <div
          aria-label={title}
          className="mx-auto my-0.5 flex h-9 w-9 items-center justify-center rounded-control text-ink-500"
        >
          <Icon size={18} aria-hidden />
        </div>
      )}

      {open && (
        <div
          style={{ top: pos.top, left: pos.left }}
          className="fixed z-50 pl-2"
        >
          <div
            role="group"
            aria-label={title}
            data-testid={`nav-flyout-${title.toLowerCase()}`}
            className="w-52 rounded-control border border-ink-800 bg-ink-950 p-1 shadow-xl"
          >
            <div className="flex items-center gap-2 px-3 py-1.5 text-2xs uppercase tracking-wider text-ink-300">
              <Icon size={13} aria-hidden />
              <span>{title}</span>
            </div>
            {items.map((item) => (
              <SideLink
                key={item.to ?? item.label}
                {...item}
                onNavigate={() => {
                  hideNow();
                  onNavigate?.();
                }}
              />
            ))}
          </div>
        </div>
      )}
    </div>
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
        <Icon size={16} className="w-4 shrink-0" aria-hidden />
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
              "h-4 w-0.5 shrink-0 rounded-full",
              isActive ? "bg-accent" : "bg-transparent",
            )}
          />
          <Icon size={16} className="w-4 shrink-0" aria-hidden />
          <span className="flex-1">{label}</span>
          {locked && (
            <Lock size={12} className="text-ink-500" aria-label="Enterprise" />
          )}
        </>
      )}
    </NavLink>
  );
}
