import { useEffect, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { AlertCircle, Eye, EyeOff, Loader2 } from "lucide-react";
import {
  AUTH_EXPIRED_EVENT,
  ApiError,
  api,
  getSecret,
  getSsoSession,
  getSsoStatus,
  localLogin,
  resolveInitialAuth,
  signIn,
  type SSOStatus,
} from "./api";
import { classifyLocalLoginError } from "./localAdmin";
import { resolveSignInSurface } from "./signInSurface";
import { Modal } from "@/components/Modal";
import { useTheme } from "./theme";

interface Props {
  children: React.ReactNode;
}

// AuthGate decides the initial console-entry state. It admits TWO credentials:
//  - an established SSO session (the HttpOnly versus_enterprise_session cookie),
//    probed via the deployment org's whoami. After the OIDC callback redirects
//    to "/", a held session opens the app directly instead of re-prompting; or
//  - on an OSS/community binary, the X-Gateway-Secret gateway secret, verified
//    against /api/admin/config/agent (always mounted + secret-gated, so it
//    works whether or not the agent is enabled). The gateway secret is OSS-only
//    — a licensed binary never offers it; its sign-in is the built-in admin / SSO.
//    A bad/absent credential AND no session fall through to the sign-in screen.
//    Transient network errors deliberately do NOT trap the user (kept
//    behavior). Mid-session OSS secret rotation is handled by <ReauthModal>.
export function AuthGate({ children }: Props) {
  const [ready, setReady] = useState<"checking" | "needs-secret" | "ok">(
    "checking",
  );

  useEffect(() => {
    let alive = true;
    resolveInitialAuth({
      hasSecret: () => Boolean(getSecret()),
      checkSecret: () => api.getAgentConfig(),
      deploymentOrg: () => api.getSSODeployment().then((d) => d.org),
      probeSession: (org) => getSsoSession(org),
    }).then((state) => {
      if (alive) setReady(state);
    });
    return () => {
      alive = false;
    };
  }, []);

  if (ready === "checking") {
    return (
      <div
        data-testid="auth-gate-loading"
        className="flex h-full items-center justify-center gap-2 text-ink-300"
      >
        <span aria-hidden className="sk h-2 w-2 rounded-full" />
        Connecting…
      </div>
    );
  }

  if (ready === "needs-secret") {
    return (
      // No bg here — body paints surface-sunken + the accent page glow,
      // which this screen wants more than any other.
      <div className="flex h-full items-center justify-center p-4">
        <SecretForm
          standalone
          onSuccess={() => setReady("ok")}
        />
      </div>
    );
  }

  return <>{children}</>;
}

// ReauthModal — mounted once in AppShell. The gateway-secret rotation reauth
// it offers is an OSS-only concern: on an OSS/community binary, when a request
// 401s with a stored secret (rotation), it opens OVER the current page and a
// successful re-entry invalidates every query so the page recovers in place.
// On a LICENSED binary the gateway secret is not a human credential — an
// expired session must return the user to the enterprise sign-in (built-in
// admin / SSO), so the expired event bounces to "/" instead of prompting for
// the secret. Enterprise-ness is read from the pre-auth /deployment route,
// which 200s only on a licensed binary.
export function ReauthModal() {
  const [open, setOpen] = useState(false);
  const qc = useQueryClient();

  useEffect(() => {
    const onExpired = () => {
      api
        .getSSODeployment()
        .then(() => {
          // Licensed binary: no gateway-secret reauth. Drive back to the
          // enterprise sign-in (built-in admin / SSO) by re-entering AuthGate.
          window.location.assign("/");
        })
        .catch(() => {
          // OSS/community: the gateway secret was rotated — re-prompt in place.
          setOpen(true);
        });
    };
    window.addEventListener(AUTH_EXPIRED_EVENT, onExpired);
    return () => window.removeEventListener(AUTH_EXPIRED_EVENT, onExpired);
  }, []);

  if (!open) return null;

  return (
    <Modal title="Session expired" onClose={() => setOpen(false)} size="sm">
      <p className="mb-3 text-xs text-ink-300">
        The gateway secret was rejected — it may have been rotated on the
        server. Enter the current secret to continue where you left off.
      </p>
      <SecretForm
        onSuccess={() => {
          setOpen(false);
          qc.invalidateQueries();
        }}
      />
    </Modal>
  );
}

function SecretForm({
  onSuccess,
  standalone,
}: {
  onSuccess: () => void;
  standalone?: boolean;
}) {
  const [input, setInput] = useState("");
  const [show, setShow] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const { theme } = useTheme();
  // Login-screen SSO option: only the standalone screen probes whether the
  // deployment has SSO configured, and only then offers the provider button.
  const [sso, setSso] = useState<SSOStatus | null>(null);
  // enterprise is true once the license-issued deployment org resolves (the
  // /deployment route 200s only on a licensed binary; community 403s). It gates
  // the built-in default-admin login form — community never sees it (G1).
  const [enterprise, setEnterprise] = useState(false);

  useEffect(() => {
    if (!standalone) return;
    let alive = true;
    // Source the org from the license-issued deployment org, not a
    // hardcoded "default": ask the pre-auth /deployment route first, then probe
    // SSO status for that org. A community/unlicensed binary 403s /deployment;
    // any failure leaves sso null AND enterprise false, so the screen falls back
    // to the OSS-only gateway-secret path.
    api
      .getSSODeployment()
      .then((d) => {
        if (alive) setEnterprise(true);
        return getSsoStatus(d.org);
      })
      .then((s) => {
        if (alive) setSso(s);
      })
      .catch(() => {
        // No enterprise / no SSO — OSS falls back to the gateway-secret form.
      });
    return () => {
      alive = false;
    };
  }, [standalone]);

  const form = (
    <form
      onSubmit={async (e) => {
        e.preventDefault();
        setError(null);
        setBusy(true);
        try {
          await signIn(input);
          onSuccess();
        } catch (err) {
          if (err instanceof ApiError && err.status === 401) {
            setError("Credential rejected by the agent.");
          } else if (err instanceof Error) {
            setError(err.message);
          } else {
            setError("Unable to reach the agent.");
          }
        } finally {
          setBusy(false);
        }
      }}
      className={standalone ? "card w-full p-6" : undefined}
    >
      {standalone && (
        <div className="mb-5">
          <h1 className="text-base font-semibold text-ink-50">Sign in</h1>
        </div>
      )}
      <label className="field-label" htmlFor="gateway-secret">
        Gateway secret
      </label>
      {/* Eye toggle INSIDE the field — the detached "Show" button read as a
          second action competing with submit. */}
      <div className="relative mb-3">
        <input
          id="gateway-secret"
          autoFocus
          type={show ? "text" : "password"}
          autoComplete="current-password"
          value={input}
          onChange={(e) => setInput(e.target.value)}
          className="input h-10 pr-10 text-sm"
        />
        <button
          type="button"
          aria-label={show ? "Hide secret" : "Show secret"}
          aria-pressed={show}
          className="absolute right-1 top-1/2 -translate-y-1/2 rounded-control p-2 text-ink-300 hover:bg-ink-600 hover:text-ink-100"
          onClick={() => setShow((s) => !s)}
        >
          {show ? (
            <EyeOff size={14} aria-hidden />
          ) : (
            <Eye size={14} aria-hidden />
          )}
        </button>
      </div>
      {error && (
        <p
          role="alert"
          className="mb-3 flex items-start gap-1.5 text-xs text-sev-critical"
        >
          <AlertCircle size={13} className="mt-px shrink-0" aria-hidden />
          {error}
        </p>
      )}
      <button
        type="submit"
        className="btn btn-primary h-10 w-full justify-center text-sm"
        disabled={busy}
      >
        {busy ? (
          <>
            <Loader2 size={14} className="animate-spin" aria-hidden />
            Verifying…
          </>
        ) : (
          "Sign in"
        )}
      </button>
    </form>
  );

  if (!standalone) return form;

  // When the deployment has SSO configured, offer the IdP connection button(s).
  // On enterprise the built-in admin form sits above them; on OSS the
  // gateway-secret form drops below an "or" divider. The SSO flow round-trips to
  // the IdP and returns to "/" with the enterprise session cookie set. SSO is
  // offered SOLELY through named multi-IdP connections — one button per enabled
  // connection.
  const returnTo = `?return_to=${encodeURIComponent("/")}`;
  const ssoButtons: { key: string; label: string; url: string }[] =
    sso?.enabled && sso.connections
      ? sso.connections.map((c) => ({
          key: c.id,
          label: c.display_name || "SSO",
          url: `${c.login_url}${returnTo}`,
        }))
      : [];
  const hasSso = ssoButtons.length > 0;

  // The credential mix is a pure decision of {enterprise, hasSso, require_sso}.
  // The gateway-secret form is OSS-only — a licensed binary NEVER renders it,
  // even with zero SSO and no require_sso (the built-in admin form is the
  // bootstrap path). On OSS the gateway secret is the sign-in, retired only when
  // SSO enforces require_sso (a state a community binary never reaches).
  const surface = resolveSignInSurface({
    enterprise,
    hasSso,
    requireSso: Boolean(sso?.require_sso),
  });

  // Sidebar is force-dark, so we always show the light (white) logo —
  // unless the app theme is light, in which case we show the dark logo
  // so it stays visible if the sidebar ever inherits the light surface.
  const logoSrc = theme === "dark" ? "/versus-logo-light.svg" : "/versus-logo-dark.svg";

  // Standalone screen: brand block above the card — the first screen
  // anyone sees leads with the product, not a YAML path.
  return (
    <div className="w-full max-w-[380px] animate-[modal-in_200ms_ease-out]">
      <div className="mb-6 flex flex-col items-center gap-3">
        <div className="flex h-12 w-12 items-center justify-center rounded-xl border border-accent/30 bg-accent-subtle shadow-[0_0_24px_rgb(var(--accent)/0.25)]">
          <img src={logoSrc} alt="Versus" className="h-5 w-auto" />
        </div>
        <div className="text-center">
          <div className="text-sm font-semibold uppercase tracking-[0.18em] text-ink-50">
            Versus Incident
          </div>
          <div className="mt-0.5 text-2xs text-ink-300">
            Incident Console
          </div>
        </div>
      </div>
      {/* Built-in default-admin login (G1). Shown only on a licensed binary —
          community 403s /deployment so `enterprise` stays false and this never
          renders. It is the pre-SSO bootstrap path: the operator signs in
          directly as the built-in admin to configure the first SSO connection.
          SSO buttons render below it. The gateway-secret form is OSS-only and
          never appears here. */}
      {surface.showLocalAdmin && <LocalAdminLoginForm onSuccess={onSuccess} />}
      {surface.showSso && (
        <div className={surface.showLocalAdmin ? "mt-4" : undefined}>
          {surface.showLocalAdmin && (
            <div className="mb-4 flex items-center gap-3 text-2xs uppercase tracking-wider text-ink-400">
              <span className="h-px flex-1 bg-ink-600" />
              or sign in with SSO
              <span className="h-px flex-1 bg-ink-600" />
            </div>
          )}
          <div className="flex flex-col gap-2">
            {ssoButtons.map((b) => (
              <button
                key={b.key}
                type="button"
                onClick={() => {
                  window.location.href = b.url;
                }}
                className="btn btn-primary h-10 w-full justify-center text-sm"
              >
                Sign in with {b.label}
              </button>
            ))}
          </div>
        </div>
      )}
      {surface.showGatewaySecret && (
        <div className={surface.showSso ? "mt-4" : undefined}>
          {surface.showSso && (
            <div className="mb-4 flex items-center gap-3 text-2xs uppercase tracking-wider text-ink-400">
              <span className="h-px flex-1 bg-ink-600" />
              or use the gateway secret
              <span className="h-px flex-1 bg-ink-600" />
            </div>
          )}
          {form}
        </div>
      )}
    </div>
  );
}

// LocalAdminLoginForm — the built-in default-admin (non-SSO) sign-in form (G1).
// It posts username + password to POST /enterprise/api/auth/local/login with
// credentials: "same-origin" (so the Set-Cookie session is honoured) and, on
// success, opens the console as that owner session. It mirrors the server's
// no-enumeration contract: a 401 renders a GENERIC invalid-credentials message
// (never "disabled" vs "wrong password"), a 429 renders a DISTINCT lockout
// state. Hooks: data-testid local-login-form / -username / -password / -submit
// / -error / -locked.
function LocalAdminLoginForm({ onSuccess }: { onSuccess: () => void }) {
  const [username, setUsername] = useState("admin");
  const [password, setPassword] = useState("");
  const [show, setShow] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [locked, setLocked] = useState(false);
  const [busy, setBusy] = useState(false);

  return (
    <form
      data-testid="local-login-form"
      onSubmit={async (e) => {
        e.preventDefault();
        setError(null);
        setLocked(false);
        setBusy(true);
        try {
          await localLogin(username, password);
          onSuccess();
        } catch (err) {
          const state = classifyLocalLoginError(err);
          if (state.kind === "locked") setLocked(true);
          else setError(state.message);
        } finally {
          setBusy(false);
        }
      }}
      className="card w-full p-6"
    >
      <div className="mb-5">
        <h1 className="text-base font-semibold text-ink-50">Sign in</h1>
        <p className="mt-1 text-xs leading-relaxed text-ink-300">
          Use the built-in administrator account.
        </p>
      </div>
      <label className="field-label" htmlFor="local-login-username">
        Username
      </label>
      <input
        id="local-login-username"
        data-testid="local-login-username"
        type="text"
        autoComplete="username"
        autoCapitalize="none"
        spellCheck={false}
        value={username}
        onChange={(e) => setUsername(e.target.value)}
        className="input mb-3 h-10 text-sm"
      />
      <label className="field-label" htmlFor="local-login-password">
        Password
      </label>
      <div className="relative mb-3">
        <input
          id="local-login-password"
          data-testid="local-login-password"
          type={show ? "text" : "password"}
          autoComplete="current-password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          className="input h-10 pr-10 text-sm"
        />
        <button
          type="button"
          aria-label={show ? "Hide password" : "Show password"}
          aria-pressed={show}
          className="absolute right-1 top-1/2 -translate-y-1/2 rounded-control p-2 text-ink-300 hover:bg-ink-600 hover:text-ink-100"
          onClick={() => setShow((s) => !s)}
        >
          {show ? (
            <EyeOff size={14} aria-hidden />
          ) : (
            <Eye size={14} aria-hidden />
          )}
        </button>
      </div>
      {locked && (
        <p
          data-testid="local-login-locked"
          role="alert"
          className="mb-3 flex items-start gap-1.5 text-xs text-sev-warn"
        >
          <AlertCircle size={13} className="mt-px shrink-0" aria-hidden />
          Too many failed attempts. Please wait and try again.
        </p>
      )}
      {error && (
        <p
          data-testid="local-login-error"
          role="alert"
          className="mb-3 flex items-start gap-1.5 text-xs text-sev-critical"
        >
          <AlertCircle size={13} className="mt-px shrink-0" aria-hidden />
          {error}
        </p>
      )}
      <button
        type="submit"
        data-testid="local-login-submit"
        className="btn btn-primary h-10 w-full justify-center text-sm"
        disabled={busy}
      >
        {busy ? (
          <>
            <Loader2 size={14} className="animate-spin" aria-hidden />
            Signing in…
          </>
        ) : (
          "Sign in"
        )}
      </button>
    </form>
  );
}
