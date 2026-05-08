import { NavLink } from "react-router-dom";
import {
  Activity,
  AlertTriangle,
  EyeOff,
  LayoutDashboard,
  Layers,
  LogOut,
  Server,
  Triangle,
  type LucideIcon,
} from "lucide-react";
import clsx from "clsx";
import { clearSecret } from "@/lib/api";

const overviewItems = [
  { to: "/dashboard", label: "Dashboard", icon: LayoutDashboard },
];

const incidentItems = [
  { to: "/incidents", label: "Incidents", icon: AlertTriangle },
];

const agentItems = [
  { to: "/status",   label: "Status",   icon: Activity },
  { to: "/patterns", label: "Patterns", icon: Layers },
  { to: "/shadow",   label: "Shadow",   icon: EyeOff },
  { to: "/services", label: "Services", icon: Server },
];

// Datadog-style left rail: dark navy, slim, icon + label, accent stripe on
// the active link.
export function Sidebar() {
  return (
    <aside className="dark-scroll flex h-full w-56 shrink-0 flex-col
                      border-r border-ink-700 bg-ink-900 text-ink-200">
      <div className="flex items-center gap-2 px-4 py-4">
        <Triangle
          size={18}
          className="rotate-180 text-accent"
          fill="currentColor"
        />
        <div className="leading-tight">
          {/* <div className="text-sm font-semibold text-ink-50">Versus</div> */}
          <div className="text-2xs uppercase tracking-wider text-ink-400">
            Incident · Admin
          </div>
        </div>
      </div>

      <nav className="flex-1 px-2 py-2">
        <div className="px-2 py-2 text-2xs uppercase tracking-wider text-ink-500">
          Overview
        </div>
        {overviewItems.map(({ to, label, icon: Icon }) => (
          <SideLink key={to} to={to} label={label} Icon={Icon} />
        ))}
        <div className="mt-2 px-2 py-2 text-2xs uppercase tracking-wider text-ink-500">
          Incidents
        </div>
        {incidentItems.map(({ to, label, icon: Icon }) => (
          <SideLink key={to} to={to} label={label} Icon={Icon} />
        ))}
        <div className="mt-2 px-2 py-2 text-2xs uppercase tracking-wider text-ink-500">
          AI Agent
        </div>
        {agentItems.map(({ to, label, icon: Icon }) => (
          <SideLink key={to} to={to} label={label} Icon={Icon} />
        ))}
      </nav>

      <div className="border-t border-ink-700 px-2 py-2">
        <button
          className="flex w-full items-center gap-2 rounded-md px-3 py-2
                     text-xs text-ink-300 hover:bg-ink-800 hover:text-ink-50"
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
}: {
  to: string;
  label: string;
  Icon: LucideIcon;
}) {
  return (
    <NavLink
      to={to}
      className={({ isActive }) =>
        clsx(
          "group flex items-center gap-2 rounded-md px-3 py-2 text-xs",
          "transition-colors",
          isActive
            ? "bg-accent-subtle text-ink-50"
            : "text-ink-300 hover:bg-ink-800 hover:text-ink-50",
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
