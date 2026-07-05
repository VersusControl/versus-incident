import { useContext } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { LogOut, Menu, Moon, Search, Sun } from "lucide-react";
import clsx from "clsx";
import {
  api,
  clearSecret,
  getSsoSession,
  localLogout,
  ssoLogout,
} from "@/lib/api";
import { isLocalAdminSession } from "@/lib/localAdmin";
import { useOpenIncidentCount } from "@/lib/hooks";
import { formatOriginCounts } from "@/lib/incidentList";
import { useTheme } from "@/lib/theme";
import { roleLabel, isAdminRole } from "@/lib/role";
import { useEffectiveRole } from "@/lib/useEffectiveRole";
import { ShellContext } from "./shellContext";

// TopBar truth table (audit S6 — the old bar showed a red "Agent
// unreachable" alarm during its own initial load and treated "disabled"
// and "down" identically):
//   config says enable:false      → neutral gray "agent off"
//   liveness loading (first time) → dim pulse, never red
//   liveness error ×3 consecutive → red "unreachable"
//   liveness error <3             → amber "reconnecting…"
//   ok                            → mode chip; the MODE is the status
//                                   (training=info, shadow=warn, detect=ok)
interface Props {
  title: string;
  subtitle?: string;
  actions?: React.ReactNode;
}

export function TopBar({ title, subtitle, actions }: Props) {
  const shell = useContext(ShellContext);
  const navigate = useNavigate();
  const { open, originCounts } = useOpenIncidentCount();
  const { theme, toggle } = useTheme();

  const config = useQuery({
    queryKey: ["agent-config"],
    queryFn: api.getAgentConfig,
    staleTime: 60_000,
    retry: 1,
  });
  // Only poll the agent liveness endpoint when the agent is actually enabled.
  // /api/agent/status does not exist when agent.enable=false, so polling it
  // would 404 forever and the chip would show "reconnecting…" for a deployment
  // that is simply running without the agent. Gate on the config: undefined
  // (still loading) keeps polling; an explicit false stops it.
  const liveness = useQuery({
    queryKey: ["status-pulse"],
    queryFn: api.status,
    enabled: config.data?.enable !== false,
    refetchInterval: () => (document.hidden ? false : 30_000),
    retry: 1,
  });

  return (
    <header
      className="z-sticky flex h-14 shrink-0 items-center justify-between gap-3
                 border-b border-ink-600 bg-surface-sunken px-4 lg:px-6"
    >
      <div className="flex min-w-0 items-center gap-3">
        <button
          aria-label="Open navigation"
          className="rounded-control p-1.5 text-ink-300 hover:bg-ink-700 hover:text-ink-100 lg:hidden"
          onClick={() => shell?.openDrawer()}
        >
          <Menu size={18} />
        </button>
        <div className="flex min-w-0 items-baseline gap-3">
          <h1 className="truncate text-base font-semibold text-ink-50">{title}</h1>
          {subtitle && (
            <span className="hidden truncate text-xs text-ink-300 sm:inline">
              {subtitle}
            </span>
          )}
        </div>
      </div>

      <div className="flex shrink-0 items-center gap-3">
        {actions}
        <TopBarIdentity />
        <SignOutButton />
        <button
          aria-label={
            theme === "dark" ? "Switch to light theme" : "Switch to dark theme"
          }
          title={
            theme === "dark" ? "Switch to light theme" : "Switch to dark theme"
          }
          className="rounded-control p-1.5 text-ink-400 hover:bg-ink-700 hover:text-ink-100"
          onClick={toggle}
        >
          {theme === "dark" ? <Sun size={15} /> : <Moon size={15} />}
        </button>
        <button
          aria-label="Search (press /)"
          title="Search — press /"
          className="hidden rounded-control p-1.5 text-ink-400 hover:bg-ink-700 hover:text-ink-100 sm:block"
          onClick={() => {
            const el =
              document.querySelector<HTMLElement>("[data-page-search]");
            if (el) {
              el.focus();
            } else {
              // Same fallback as the "/" shortcut (§2.4): pages without a
              // search land on Incidents instead of a silent no-op.
              navigate("/incidents");
              window.setTimeout(() => {
                document
                  .querySelector<HTMLElement>("[data-page-search]")
                  ?.focus();
              }, 80);
            }
          }}
        >
          <Search size={15} />
        </button>
        {open > 0 && (
          <Link
            to="/incidents?status=open"
            title={`${open} open — AI-detected vs webhook/alerts`}
            className="rounded-full bg-sev-critical/15 px-2 py-0.5 text-2xs font-semibold tabular-nums text-sev-critical hover:bg-sev-critical/25"
          >
            {formatOriginCounts(originCounts)}
          </Link>
        )}
        <AgentChip
          configLoading={config.isLoading}
          agentEnabled={config.data?.enable}
          mode={config.data?.mode}
          livenessLoading={liveness.isLoading}
          livenessError={liveness.isError}
          failures={liveness.failureCount}
        />
      </div>
    </header>
  );
}

function AgentChip({
  configLoading,
  agentEnabled,
  mode,
  livenessLoading,
  livenessError,
  failures,
}: {
  configLoading: boolean;
  agentEnabled?: boolean;
  mode?: string;
  livenessLoading: boolean;
  livenessError: boolean;
  failures: number;
}) {
  let cls = "border-ink-500 bg-ink-700 text-ink-300";
  let dot = "bg-ink-400";
  let label = "agent";

  if (agentEnabled === false) {
    // Config says the agent is off — this is a deliberate state, not an
    // error. A 404 from the (unmounted) status endpoint must NOT surface as
    // "reconnecting…"/"unreachable", so this check comes first.
    label = "agent off";
  } else if (livenessLoading || configLoading) {
    label = "connecting…";
    dot = "motion-safe:animate-pulse bg-ink-300";
  } else if (livenessError) {
    if (failures >= 3) {
      label = "unreachable";
      cls = "border-sev-critical/40 bg-sev-critical/15 text-sev-critical";
      dot = "bg-sev-critical";
    } else {
      label = "reconnecting…";
      cls = "border-sev-warn/40 bg-sev-warn/15 text-sev-warn";
      dot = "motion-safe:animate-pulse bg-sev-warn";
    }
  } else if (mode) {
    label = mode;
    if (mode === "detect") {
      cls = "border-sev-ok/40 bg-sev-ok/15 text-sev-ok";
      dot = "bg-sev-ok";
    } else if (mode === "shadow") {
      cls = "border-sev-warn/40 bg-sev-warn/15 text-sev-warn";
      dot = "bg-sev-warn";
    } else {
      cls = "border-sev-info/40 bg-sev-info/15 text-sev-info";
      dot = "bg-sev-info";
    }
  }

  return (
    <Link
      to="/agent"
      className={clsx(
        "inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-2xs font-medium",
        cls,
      )}
    >
      <span aria-hidden className={clsx("h-1.5 w-1.5 rounded-full", dot)} />
      {label}
    </Link>
  );
}

// SignOutButton relocates the former sidebar sign-out into the top bar, next
// to TopBarIdentity. It reuses the EXACT revoke-then-clear handler the sidebar
// used: revoke any established enterprise session (HttpOnly cookie) before
// clearing local state — clearing the secret alone leaves the cookie valid and
// AuthGate would re-admit on reload. The deployment-org probe 403s on a
// community/OSS binary (no SSO) and is simply skipped, so a gateway-secret-only
// operator just clears the secret. A BUILT-IN default-admin (local) session is
// revoked via the local-admin logout route; an SSO session via the SSO route
// (G4). Both revoke the same shared session cookie, so the SSO route is a safe
// fallback when the session kind can't be determined.
function SignOutButton() {
  return (
    <button
      data-testid="sign-out"
      aria-label="Sign out"
      title="Sign out"
      className="inline-flex items-center gap-1.5 rounded-full border border-ink-600
                 px-2 py-0.5 text-2xs font-medium text-ink-300 hover:bg-ink-700
                 hover:text-ink-100"
      onClick={async () => {
        try {
          const dep = await api.getSSODeployment();
          let local = false;
          try {
            local = isLocalAdminSession(await getSsoSession(dep.org));
          } catch {
            // No live session / cannot determine — fall through to SSO logout.
          }
          if (local) {
            await localLogout();
          } else {
            await ssoLogout(dep.org);
          }
        } catch {
          // No enterprise / no SSO session — nothing to revoke.
        }
        clearSecret();
        window.location.reload();
      }}
    >
      <LogOut size={13} aria-hidden />
      <span className="hidden sm:inline">Sign out</span>
    </button>
  );
}

// TopBarIdentity mirrors the sidebar's SignedInIdentity in the shared page
// header: WHO the operator is signed in as and their effective RBAC role. It
// renders ONLY when a live SSO / built-in-admin session resolves (an enterprise
// binary, signed in). On a community/OSS binary or a gateway-secret-only
// operator the whoami 403s, so nothing renders — no layout shift, no error. It
// reuses the SAME data path (useEffectiveRole → getSsoSession) and role helpers
// (roleLabel / isAdminRole) as the sidebar so the two can never disagree.
function TopBarIdentity() {
  const access = useEffectiveRole();
  if (access.loading || !access.hasSession) {
    return null;
  }
  const sess = access.session.data;
  if (!sess) {
    return null;
  }
  const identity = sess.email?.trim() || sess.subject?.trim() || "Signed in";
  const admin = isAdminRole(access.role);
  return (
    <div className="hidden min-w-0 items-center gap-2 sm:flex" title={identity}>
      <span
        data-testid="topbar-identity"
        className="max-w-[16rem] truncate text-xs text-ink-300"
      >
        {identity}
      </span>
      <span
        data-testid="topbar-role"
        className={clsx(
          "inline-flex items-center rounded-full px-1.5 py-0.5 text-2xs font-medium uppercase tracking-wide",
          admin ? "bg-accent-subtle text-accent" : "bg-ink-700 text-ink-200",
        )}
      >
        {roleLabel(access.role)}
      </span>
    </div>
  );
}
