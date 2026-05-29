import { NavLink } from "react-router-dom";
import {
  Activity,
  AlertTriangle,
  Brain,
  EyeOff,
  FileText,
  LayoutDashboard,
  Layers,
  LogOut,
  Server,
  Settings,
  Sliders,
  Sparkles,
  Triangle,
  User,
  Users,
  type LucideIcon,
} from "lucide-react";
import clsx from "clsx";
import { clearSecret } from "@/lib/api";

interface SideItem {
  to: string;
  label: string;
  icon: LucideIcon;
  disabled?: boolean;
}

const overviewItems = [
  { to: "/dashboard", label: "Dashboard", icon: LayoutDashboard },
];

const incidentItems: SideItem[] = [
  { to: "/incidents", label: "Incidents", icon: AlertTriangle },
  { to: "/analyses", label: "Analyses", icon: Brain },
  { to: "/postmortems", label: "Post Mortem", icon: FileText, disabled: true },
];

const peopleItems = [
  { to: "/members", label: "Members", icon: User },
  { to: "/teams",   label: "Teams",   icon: Users },
];

const agentItems = [
  { to: "/status",   label: "Status",   icon: Activity },
  { to: "/patterns", label: "Patterns", icon: Layers },
  { to: "/shadow",   label: "Shadow",   icon: EyeOff },
  { to: "/detect",   label: "Detect",   icon: Sparkles },
  { to: "/services", label: "Services", icon: Server },
];

const configItems = [
  { to: "/config/incidents", label: "Incidents Config", icon: Sliders },
  { to: "/config/agent",     label: "Agent Config",     icon: Settings },
];

// Datadog-style left rail: dark navy, slim, icon + label, accent stripe on
// the active link.
export function Sidebar() {
  return (
    <aside className="dark-scroll flex h-full w-56 shrink-0 flex-col overflow-y-auto
                      border-r border-ink-800 bg-ink-950 text-ink-100">
      <div className="flex items-center gap-2 px-4 py-4">
        <Triangle
          size={18}
          className="rotate-180 text-accent"
          fill="currentColor"
        />
        <div className="leading-tight">
          {/* <div className="text-sm font-semibold text-ink-50">Versus</div> */}
          <div className="text-2xs uppercase tracking-wider text-ink-200">
            Incident · Admin
          </div>
        </div>
      </div>

      <nav className="flex-1 px-2 py-2">
        <div className="px-2 py-2 text-2xs uppercase tracking-wider text-ink-300">
          Overview
        </div>
        {overviewItems.map(({ to, label, icon: Icon }) => (
          <SideLink key={to} to={to} label={label} Icon={Icon} />
        ))}
        <div className="mt-2 px-2 py-2 text-2xs uppercase tracking-wider text-ink-300">
          Incidents
        </div>
        {incidentItems.map(({ to, label, icon: Icon, disabled }) => (
          <SideLink key={to} to={to} label={label} Icon={Icon} disabled={disabled} />
        ))}
        <div className="mt-2 px-2 py-2 text-2xs uppercase tracking-wider text-ink-300">
          People
        </div>
        {peopleItems.map(({ to, label, icon: Icon }) => (
          <SideLink key={to} to={to} label={label} Icon={Icon} />
        ))}
        <div className="mt-2 px-2 py-2 text-2xs uppercase tracking-wider text-ink-300">
          AI Agent
        </div>
        {agentItems.map(({ to, label, icon: Icon }) => (
          <SideLink key={to} to={to} label={label} Icon={Icon} />
        ))}
        <div className="mt-2 px-2 py-2 text-2xs uppercase tracking-wider text-ink-300">
          Configuration
        </div>
        {configItems.map(({ to, label, icon: Icon }) => (
          <SideLink key={to} to={to} label={label} Icon={Icon} />
        ))}
      </nav>

      <div className="border-t border-ink-800 px-2 py-2">
        <button
          className="flex w-full items-center gap-2 rounded-md px-3 py-2
                     text-xs text-ink-100 hover:bg-ink-800 hover:text-ink-50"
          onClick={() => {
            clearSecret();
            window.location.reload();
          }}
        >
          <LogOut size={14} />
          Sign out
        </button>
      </div>
    </aside>
  );
}

function SideLink({
  to,
  label,
  Icon,
  disabled,
}: {
  to: string;
  label: string;
  Icon: LucideIcon;
  disabled?: boolean;
}) {
  if (disabled) {
    return (
      <span
        className="group flex cursor-not-allowed items-center gap-2 rounded-md px-3 py-2 text-xs text-ink-500"
        title="Coming soon"
      >
        <span className="h-4 w-0.5 rounded-full bg-transparent" />
        <Icon size={14} />
        <span>{label}</span>
      </span>
    );
  }

  return (
    <NavLink
      to={to}
      className={({ isActive }) =>
        clsx(
          "group flex items-center gap-2 rounded-md px-3 py-2 text-xs",
          "transition-colors",
          isActive
            ? "bg-accent-subtle text-ink-50"
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
          <span>{label}</span>
        </>
      )}
    </NavLink>
  );
}
