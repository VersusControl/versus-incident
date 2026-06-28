import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  AlertCircle,
  AlertTriangle,
  Eye,
  EyeOff,
  Loader2,
  Lock,
  Pencil,
  Plus,
  Trash2,
} from "lucide-react";
import {
  ApiError,
  api,
  type SSOConnectionInput,
  type SSOConnectionType,
  type SSOConnectionView,
  type SSOConnectionsEnvelope,
  type SSOPolicyEnvelope,
  type SSOPolicyInput,
} from "@/lib/api";
import { noEncryptionKeyMessage } from "@/lib/agentAI";
import { classifySsoError, formatList, parseList, type SsoErrorState } from "@/lib/ssoConfig";
import { useDeploymentOrg } from "@/lib/useDeploymentOrg";
import { useEffectiveRole } from "@/lib/useEffectiveRole";
import { adminGateState } from "@/lib/role";
import { AdminAccessNotice } from "@/components/AdminAccessNotice";
import { EnterpriseLockedBody } from "@/components/EnterpriseLocked";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { useToast } from "@/components/toastContext";

// AgentSSOConnectionsControl — the operator surface for the Enterprise
// multi-IdP "connections" (the Keycloak-style identity-provider list). An org
// may register SEVERAL named connections — Google, Azure AD (Microsoft Entra),
// or a generic OIDC issuer — each rendered as one "Sign in with …" button on
// the login screen. It reads /enterprise/api/sso/:org/connections and lets an
// admin add / edit / delete a connection and seal its client secret.
//
// This is the single, canonical SSO/identity panel: it also owns the X4-T4
// login-enforcement policy (require_sso / require_mfa) as an "Enforcement"
// subsection, so the admin page renders ONE SSO section.
//
// Every request rides the SSO session cookie and is authorized by the caller's
// RBAC role (sso:manage, held by admin/owner). The
// panel is gated on the caller's effective role (useEffectiveRole):
//   not enterprise        → locked Enterprise upsell (no panel)
//   no SSO session         → "sign in to manage" notice
//   viewer / responder     → read-only "requires the admin role" notice
//   admin / owner          → the live connections + enforcement panel
//
// The client secret is NEVER persisted to browser storage — it lives in a
// transient field, is sent on the single PUT (omitted when blank, preserving
// the existing seal), then cleared. The server returns only the masked last4.

const TYPE_LABELS: Record<SSOConnectionType, string> = {
  google: "Google",
  azure: "Microsoft Entra (Azure AD)",
  oidc: "Generic OIDC",
};

export function AgentSSOConnectionsControl() {
  const qc = useQueryClient();
  const toast = useToast();
  // The org is sourced from the LICENSE_KEY (X4) via the pre-auth /deployment
  // route, not a hardcoded "default". It is not operator-selectable — this
  // panel targets the deployment's own org.
  const dep = useDeploymentOrg();
  const org = dep.data?.org ?? "";
  // The caller's effective RBAC role (from the SSO session whoami) gates the
  // whole panel — managing IdPs requires sso:manage (admin/owner).
  const access = useEffectiveRole();
  const gate = adminGateState({
    loading: access.loading,
    enterprise: access.enterprise,
    hasSession: access.hasSession,
    isAdmin: access.isAdmin,
  });
  const [editing, setEditing] = useState<string | null>(null); // conn id | "__new__" | null

  const conns = useQuery<SSOConnectionsEnvelope>({
    queryKey: ["sso-connections", org],
    queryFn: () => api.listSSOConnections(org),
    // Only an admin issues the privileged GET (fail closed for viewers).
    enabled: !!org && gate === "admin",
    retry: (count, err) => {
      if (err instanceof ApiError && [401, 403, 404, 503].includes(err.status)) {
        return false;
      }
      return count < 1;
    },
  });

  // The /deployment probe is itself license-gated: a community/unlicensed
  // binary 403s it (and an OSS build has no route → 404). Treat that as "not
  // enterprise" and render the locked upsell, same as a connections 403.
  const depLocked =
    dep.error instanceof ApiError && [403, 404].includes(dep.error.status);
  const state: SsoErrorState | null = conns.error ? classifySsoError(conns.error) : null;

  if (depLocked || gate === "locked" || state === "locked") {
    return (
      <ConnShell>
        <EnterpriseLockedBody title="Multi-identity SSO">
          Register several identity providers — Google, Microsoft Entra, or any
          OIDC issuer — each a one-click sign-in button on the login screen.
        </EnterpriseLockedBody>
      </ConnShell>
    );
  }

  // Still resolving which org this single-tenant binary serves, or the session
  // role.
  if (dep.isPending || gate === "loading") {
    return (
      <ConnShell>
        <div className="mt-4">
          <LoadingRow />
        </div>
      </ConnShell>
    );
  }

  // The deployment-org probe failed for a non-locked reason — surface it.
  if (dep.error || !org) {
    return (
      <ConnShell>
        <div className="mt-4">
          <ErrorRow error={dep.error} retry={() => dep.refetch()} />
        </div>
      </ConnShell>
    );
  }

  // Role gate: enterprise binary, but the caller may not manage SSO config.
  if (gate === "sign-in") {
    return (
      <ConnShell>
        <div className="mt-4">
          <AdminAccessNotice reason="sign-in" />
        </div>
      </ConnShell>
    );
  }
  if (gate === "read-only") {
    return (
      <ConnShell>
        <div className="mt-4">
          <AdminAccessNotice reason="role" />
        </div>
      </ConnShell>
    );
  }

  const invalidate = () => qc.invalidateQueries({ queryKey: ["sso-connections", org] });

  const list = conns.data?.connections ?? [];
  // The server refuses require_sso without an enabled IdP, so the Enforcement
  // subsection only appears once at least one connection is enabled.
  const hasEnabledConnection = list.some((c) => c.enabled);

  return (
    <ConnShell>
      <div className="mt-4">
        {state === "forbidden" ? (
          <AdminAccessNotice reason="role" />
        ) : conns.isPending ? (
          <LoadingRow />
        ) : state === "error" || !conns.data ? (
          <ErrorRow error={conns.error} retry={() => conns.refetch()} />
        ) : (
          <div className="flex flex-col gap-3">
            {list.length === 0 && editing === null && (
              <p className="text-xs text-ink-300">
                No identity providers yet. Add Google, Microsoft Entra, or a generic
                OIDC issuer to offer single sign-on.
              </p>
            )}

            {list.map((c) =>
              editing === c.id ? (
                <ConnectionEditor
                  key={c.id}
                  org={org}
                  view={c}
                  toast={toast}
                  onDone={() => {
                    setEditing(null);
                    invalidate();
                  }}
                  onCancel={() => setEditing(null)}
                />
              ) : (
                <ConnectionRow
                  key={c.id}
                  org={org}
                  conn={c}
                  toast={toast}
                  disabled={editing !== null}
                  onEdit={() => setEditing(c.id)}
                  onChanged={invalidate}
                />
              ),
            )}

            {editing === "__new__" ? (
              <ConnectionEditor
                org={org}
                view={null}
                toast={toast}
                onDone={() => {
                  setEditing(null);
                  invalidate();
                }}
                onCancel={() => setEditing(null)}
              />
            ) : (
              <button
                type="button"
                disabled={editing !== null}
                onClick={() => setEditing("__new__")}
                className="btn w-fit"
              >
                <Plus size={14} aria-hidden className="mr-1" />
                Add identity provider
              </button>
            )}
          </div>
        )}
      </div>

      {/* Enforcement (X4-T4): require_sso / require_mfa. Absorbed from the
          retired single-IdP form so this is the ONE place SSO is configured.
          Only shown once an IdP is enabled — the server refuses require_sso
          without an enabled IdP. */}
      {conns.data && hasEnabledConnection && (
        <div className="mt-4">
          <SSOEnforcementControl org={org} toast={toast} disabled={false} />
        </div>
      )}
    </ConnShell>
  );
}

// ConnectionRow — one configured connection: its label, type, enabled badge and
// resolved issuer, with edit / delete actions.
function ConnectionRow({
  org,
  conn,
  toast,
  disabled,
  onEdit,
  onChanged,
}: {
  org: string;
  conn: SSOConnectionView;
  toast: ReturnType<typeof useToast>;
  disabled: boolean;
  onEdit: () => void;
  onChanged: () => void;
}) {
  const [confirmDel, setConfirmDel] = useState(false);
  const del = useMutation({
    mutationFn: () => api.deleteSSOConnection(org, conn.id),
    onSuccess: () => {
      toast.push({ title: `Removed “${conn.display_name}”`, tone: "ok" });
      setConfirmDel(false);
      onChanged();
    },
    onError: (err) => {
      toast.push({
        title: "Couldn't remove connection",
        description: err instanceof Error ? err.message : String(err),
        tone: "error",
      });
    },
  });

  return (
    <div className="flex items-center justify-between gap-3 rounded-control border border-ink-600/60 bg-surface-sunken/40 p-3">
      <div className="min-w-0">
        <div className="flex items-center gap-2">
          <span className="truncate text-sm font-medium text-ink-100">
            {conn.display_name}
          </span>
          <span
            className={
              conn.enabled
                ? "rounded-full bg-sev-ok/15 px-2 py-0.5 text-2xs font-medium text-sev-ok"
                : "rounded-full bg-ink-600/60 px-2 py-0.5 text-2xs font-medium text-ink-300"
            }
          >
            {conn.enabled ? "Enabled" : "Disabled"}
          </span>
        </div>
        <div className="mt-0.5 truncate text-2xs text-ink-400">
          {TYPE_LABELS[conn.type]} · {conn.issuer}
        </div>
      </div>
      <div className="flex shrink-0 items-center gap-1">
        <button
          type="button"
          disabled={disabled}
          onClick={onEdit}
          className="rounded-control p-1.5 text-ink-300 hover:bg-ink-600 hover:text-ink-100 disabled:opacity-50"
          aria-label={`Edit ${conn.display_name}`}
        >
          <Pencil size={14} aria-hidden />
        </button>
        <button
          type="button"
          disabled={disabled}
          onClick={() => setConfirmDel(true)}
          className="rounded-control p-1.5 text-ink-300 hover:bg-sev-critical/15 hover:text-sev-critical disabled:opacity-50"
          aria-label={`Remove ${conn.display_name}`}
        >
          <Trash2 size={14} aria-hidden />
        </button>
      </div>

      {confirmDel && (
        <ConfirmDialog
          title="Remove this identity provider?"
          tone="danger"
          confirmLabel="Remove"
          busy={del.isPending}
          error={del.error instanceof Error ? del.error : null}
          message={
            <>
              This removes{" "}
              <span className="font-mono text-ink-100">{conn.display_name}</span>{" "}
              and its sign-in button. Users with other providers are unaffected.
            </>
          }
          onConfirm={() => del.mutate()}
          onClose={() => {
            if (!del.isPending) setConfirmDel(false);
          }}
        />
      )}
    </div>
  );
}

// ConnectionEditor — the add / edit form for one connection. `view` null means
// "add". The type picker drives which issuer field shows: google derives its
// issuer, azure takes a directory (tenant) id, oidc takes an explicit issuer.
function ConnectionEditor({
  org,
  view,
  toast,
  onDone,
  onCancel,
}: {
  org: string;
  view: SSOConnectionView | null;
  toast: ReturnType<typeof useToast>;
  onDone: () => void;
  onCancel: () => void;
}) {
  const isNew = view === null;

  const [id, setId] = useState(view?.id ?? "");
  const [type, setType] = useState<SSOConnectionType>(view?.type ?? "google");
  const [displayName, setDisplayName] = useState(view?.display_name ?? "");
  const [enabled, setEnabled] = useState(view?.enabled ?? true);
  const [clientId, setClientId] = useState(view?.client_id ?? "");
  const [secretInput, setSecretInput] = useState("");
  const [showSecret, setShowSecret] = useState(false);
  const [redirectUrl, setRedirectUrl] = useState(
    view?.redirect_url ??
      `${window.location.origin}/enterprise/api/sso/${org}/callback`,
  );
  const [azureTenant, setAzureTenant] = useState(view?.azure_tenant ?? "");
  const [issuer, setIssuer] = useState(view?.issuer ?? "");
  const [scopes, setScopes] = useState(formatList(view?.scopes));
  const [domains, setDomains] = useState(formatList(view?.allowed_domains ?? []));

  // Re-sync when the authoritative view changes (edit of a different row).
  useEffect(() => {
    if (!view) return;
    setId(view.id);
    setType(view.type);
    setDisplayName(view.display_name);
    setEnabled(view.enabled);
    setClientId(view.client_id);
    setSecretInput("");
    setShowSecret(false);
    setRedirectUrl(view.redirect_url);
    setAzureTenant(view.azure_tenant ?? "");
    setIssuer(view.type === "oidc" ? view.issuer : "");
    setScopes(formatList(view.scopes));
    setDomains(formatList(view.allowed_domains));
  }, [view]);

  const save = useMutation({
    mutationFn: () => {
      const slug = (isNew ? id : view!.id).trim().toLowerCase();
      const input: SSOConnectionInput = {
        type,
        display_name: displayName.trim(),
        enabled,
        client_id: clientId.trim(),
        client_secret: secretInput.trim() || undefined,
        redirect_url: redirectUrl.trim(),
        scopes: parseList(scopes),
        allowed_domains: parseList(domains),
        azure_tenant: type === "azure" ? azureTenant.trim() : undefined,
        issuer: type === "oidc" ? issuer.trim() : undefined,
      };
      return api.setSSOConnection(org, slug, input);
    },
    onSuccess: () => {
      toast.push({ title: "Identity provider saved", tone: "ok" });
      setSecretInput("");
      onDone();
    },
    onError: (err) => {
      if (noEncryptionKeyMessage(err)) {
        setSecretInput("");
        return;
      }
      toast.push({
        title: "Couldn't save identity provider",
        description: err instanceof Error ? err.message : String(err),
        tone: "error",
      });
    },
  });

  const busy = save.isPending;
  const noKeyMsg = noEncryptionKeyMessage(save.error);
  const parsedDomains = parseList(domains);
  const rejectAll = parsedDomains.length === 0;
  const idInvalid = isNew && id.trim() !== "" && !/^[a-z0-9][a-z0-9_-]{0,63}$/.test(id.trim().toLowerCase());

  return (
    <div className="rounded-control border border-link/40 bg-surface-sunken/40 p-4">
      <div className="mb-3 flex items-center justify-between">
        <h3 className="text-sm font-semibold text-ink-100">
          {isNew ? "Add identity provider" : `Edit ${view!.display_name}`}
        </h3>
      </div>

      <div className="flex flex-col gap-4">
        {/* Provider type */}
        <Field label="Provider" htmlFor="conn-type">
          <select
            id="conn-type"
            value={type}
            disabled={busy || !isNew}
            onChange={(e) => setType(e.target.value as SSOConnectionType)}
            className="input h-9 text-sm"
          >
            <option value="google">Google</option>
            <option value="azure">Microsoft Entra (Azure AD)</option>
            <option value="oidc">Generic OIDC (issuer URL)</option>
          </select>
        </Field>

        {/* Connection id (slug) — fixed after creation */}
        {isNew ? (
          <Field
            label="Connection ID"
            htmlFor="conn-id"
            hint="lowercase slug · used in the login URL"
          >
            <input
              id="conn-id"
              type="text"
              autoComplete="off"
              placeholder="google"
              value={id}
              disabled={busy}
              onChange={(e) => setId(e.target.value)}
              className="input h-9 text-sm"
            />
          </Field>
        ) : null}
        {idInvalid && (
          <p className="-mt-2 text-2xs text-sev-warn">
            ID must be 1–64 chars of lowercase letters, digits, “-” or “_”.
          </p>
        )}

        {/* Display name */}
        <Field
          label="Display name"
          htmlFor="conn-name"
          hint="shown on the “Sign in with …” button"
        >
          <input
            id="conn-name"
            type="text"
            autoComplete="off"
            placeholder={TYPE_LABELS[type]}
            value={displayName}
            disabled={busy}
            onChange={(e) => setDisplayName(e.target.value)}
            className="input h-9 text-sm"
          />
        </Field>

        {/* Azure tenant (azure only) */}
        {type === "azure" && (
          <Field
            label="Directory (tenant) ID"
            htmlFor="conn-tenant"
            hint="blank ⇒ the multi-tenant “common” authority"
          >
            <input
              id="conn-tenant"
              type="text"
              autoComplete="off"
              placeholder="00000000-0000-0000-0000-000000000000"
              value={azureTenant}
              disabled={busy}
              onChange={(e) => setAzureTenant(e.target.value)}
              className="input h-9 text-sm"
            />
          </Field>
        )}

        {/* Issuer (oidc only) */}
        {type === "oidc" && (
          <Field label="Issuer (IdP discovery URL)" htmlFor="conn-issuer">
            <input
              id="conn-issuer"
              type="url"
              autoComplete="off"
              inputMode="url"
              placeholder="https://accounts.example.com"
              value={issuer}
              disabled={busy}
              onChange={(e) => setIssuer(e.target.value)}
              className="input h-9 text-sm"
            />
          </Field>
        )}

        {/* Derived-issuer hint for google/azure */}
        {type !== "oidc" && (
          <p className="-mt-2 text-2xs text-ink-400">
            Issuer is derived automatically
            {type === "google"
              ? " (accounts.google.com)."
              : " from your tenant."}{" "}
            Register the client ID & secret with the provider.
          </p>
        )}

        {/* Client ID */}
        <Field label="Client ID" htmlFor="conn-client-id">
          <input
            id="conn-client-id"
            type="text"
            autoComplete="off"
            value={clientId}
            disabled={busy}
            onChange={(e) => setClientId(e.target.value)}
            className="input h-9 text-sm"
          />
        </Field>

        {/* Client secret (transient) */}
        <div>
          <label className="field-label" htmlFor="conn-secret">
            Client secret{" "}
            <span className="font-normal text-ink-400">
              {view?.client_secret_set
                ? "(leave blank to keep the current secret)"
                : ""}
            </span>
          </label>
          <div className="relative max-w-sm">
            <input
              id="conn-secret"
              type={showSecret ? "text" : "password"}
              autoComplete="off"
              placeholder={
                view?.client_secret_set ? "•••• stored — blank keeps it" : "client secret"
              }
              value={secretInput}
              disabled={busy}
              onChange={(e) => setSecretInput(e.target.value)}
              className="input h-9 pr-9 text-sm"
            />
            <button
              type="button"
              aria-label={showSecret ? "Hide client secret" : "Show client secret"}
              aria-pressed={showSecret}
              className="absolute right-1 top-1/2 -translate-y-1/2 rounded-control p-1.5 text-ink-300 hover:bg-ink-600 hover:text-ink-100"
              onClick={() => setShowSecret((s) => !s)}
            >
              {showSecret ? <EyeOff size={13} aria-hidden /> : <Eye size={13} aria-hidden />}
            </button>
          </div>
        </div>

        {/* Redirect URL */}
        <Field label="Redirect URL (callback)" htmlFor="conn-redirect">
          <input
            id="conn-redirect"
            type="url"
            autoComplete="off"
            inputMode="url"
            value={redirectUrl}
            disabled={busy}
            onChange={(e) => setRedirectUrl(e.target.value)}
            className="input h-9 text-sm"
          />
        </Field>

        {/* Scopes */}
        <Field
          label="Scopes"
          htmlFor="conn-scopes"
          hint="comma or space separated · openid is always implied"
        >
          <input
            id="conn-scopes"
            type="text"
            autoComplete="off"
            placeholder="email, profile"
            value={scopes}
            disabled={busy}
            onChange={(e) => setScopes(e.target.value)}
            className="input h-9 text-sm"
          />
        </Field>

        {/* Allowed domains */}
        <Field
          label="Allowed email domains"
          htmlFor="conn-domains"
          hint="comma or space separated"
        >
          <input
            id="conn-domains"
            type="text"
            autoComplete="off"
            placeholder="acme.com, example.org"
            value={domains}
            disabled={busy}
            onChange={(e) => setDomains(e.target.value)}
            className="input h-9 text-sm"
          />
        </Field>

        {/* Enable toggle */}
        <label className="flex w-fit cursor-pointer items-center gap-2 text-xs text-ink-200">
          <input
            type="checkbox"
            checked={enabled}
            disabled={busy}
            onChange={(e) => setEnabled(e.target.checked)}
            className="h-4 w-4 accent-link"
          />
          Enabled (show this provider on the login screen)
        </label>

        {/* Reject-all warning */}
        {rejectAll && (
          <div
            role="alert"
            className="flex items-start gap-2 rounded-control border border-sev-warn/40 bg-sev-warn/10 p-3 text-xs"
          >
            <AlertTriangle size={14} className="mt-0.5 shrink-0 text-sev-warn" aria-hidden />
            <div>
              <p className="font-medium text-ink-100">
                No allowed domains — every login will be rejected
              </p>
              <p className="mt-0.5 text-ink-300">
                The allow-list is empty, so the provider fails closed. Add at least
                one domain to admit those users.
              </p>
            </div>
          </div>
        )}

        {/* no_encryption_key (422) */}
        {noKeyMsg && (
          <div
            role="alert"
            className="flex items-start gap-2 rounded-control border border-sev-warn/40 bg-sev-warn/10 p-3 text-xs"
          >
            <AlertCircle size={14} className="mt-0.5 shrink-0 text-sev-warn" aria-hidden />
            <div>
              <p className="font-medium text-ink-100">Server can't store the client secret</p>
              <p className="mt-0.5 text-ink-300">{noKeyMsg}</p>
            </div>
          </div>
        )}

        {/* Actions */}
        <div className="flex items-center gap-2">
          <button
            type="button"
            disabled={busy || idInvalid || (isNew && id.trim() === "")}
            onClick={() => save.mutate()}
            className="btn btn-primary"
          >
            {save.isPending ? "Saving…" : isNew ? "Add provider" : "Save"}
          </button>
          <button type="button" disabled={busy} onClick={onCancel} className="btn">
            Cancel
          </button>
        </div>

        <p className="text-2xs text-ink-400">
          The client secret is held only for this save and is never stored in your
          browser. Leave it blank to update the other fields without re-entering it.
        </p>
      </div>
    </div>
  );
}

// LoadingRow — the "reading identity providers…" spinner row.
function LoadingRow() {
  return (
    <div className="flex items-center gap-2 text-xs text-ink-400">
      <Loader2 size={14} className="animate-spin" />
      Reading identity providers…
    </div>
  );
}

function ErrorRow({ error, retry }: { error: unknown; retry: () => void }) {
  return (
    <div className="flex items-center justify-between gap-3 text-xs">
      <span className="flex items-center gap-1.5 text-sev-critical">
        <AlertCircle size={13} />
        {error instanceof Error ? error.message : "Couldn't read identity providers."}
      </span>
      <button className="btn" onClick={retry}>
        Retry
      </button>
    </div>
  );
}

// Field — a labelled input row with an optional hint.
function Field({
  label,
  htmlFor,
  hint,
  children,
}: {
  label: string;
  htmlFor: string;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <div>
      <label className="field-label" htmlFor={htmlFor}>
        {label}
        {hint && <span className="ml-1 font-normal text-ink-400">· {hint}</span>}
      </label>
      <div className="max-w-sm">{children}</div>
    </div>
  );
}

// SSOEnforcementControl — the X4-T4 per-org human-access enforcement toggle,
// absorbed from the retired single-IdP settings form so the multi-IdP panel is
// the single SSO surface. "Enforce SSO" requires human users to sign in through
// a configured IdP; the built-in default admin stays available as a break-glass
// account, and the gateway secret is OSS machine/data-plane only (never a human
// login on a licensed binary). With SSO enforced, "require MFA" is the LIVE
// X4-T4 multi-factor gate — it rejects any SSO login the IdP did not report as
// multi-factor across the privileged surfaces. It reads/writes
// /enterprise/api/sso/:org/policy over the SSO session cookie, authorized by the
// caller's RBAC role (sso:manage), the same as the connections list. Mounted
// only when at least one IdP is enabled, so the server's `sso_not_configured`
// guard can never trip from here.
function SSOEnforcementControl({
  org,
  toast,
  disabled,
}: {
  org: string;
  toast: ReturnType<typeof useToast>;
  disabled: boolean;
}) {
  const qc = useQueryClient();
  const pol = useQuery<SSOPolicyEnvelope>({
    queryKey: ["sso-policy", org],
    queryFn: () => api.getSSOPolicy(org),
    retry: (count, err) => {
      if (err instanceof ApiError && [401, 403, 404, 503].includes(err.status)) {
        return false;
      }
      return count < 1;
    },
  });

  const requireSSO = pol.data?.policy.require_sso ?? false;
  const requireMFA = pol.data?.policy.require_mfa ?? false;

  const save = useMutation({
    mutationFn: (input: SSOPolicyInput) => api.setSSOPolicy(org, input),
    onSuccess: (data) => {
      qc.setQueryData(["sso-policy", org], data);
      toast.push({
        title: data.policy.require_sso
          ? "SSO enforced for human sign-in"
          : "SSO enforcement turned off",
        tone: "ok",
      });
    },
    onError: (err) => {
      toast.push({
        title: "Couldn't update the login policy",
        description: err instanceof Error ? err.message : String(err),
        tone: "error",
      });
    },
  });

  const busy = disabled || pol.isFetching || save.isPending;

  return (
    <div className="rounded-control border border-ink-600/60 bg-surface-sunken/40 p-3">
      <div className="mb-2 flex items-center gap-1.5 text-2xs uppercase tracking-wider text-ink-400">
        <Lock size={12} aria-hidden />
        Login enforcement
      </div>

      <label className="flex cursor-pointer items-start gap-2 text-xs text-ink-200">
        <input
          type="checkbox"
          checked={requireSSO}
          disabled={busy}
          onChange={(e) =>
            save.mutate({
              require_sso: e.target.checked,
              // dropping require_sso clears require_mfa (it has no meaning alone)
              require_mfa: e.target.checked ? requireMFA : false,
            })
          }
          className="mt-0.5 h-4 w-4 accent-link"
        />
        <span>
          <span className="font-medium text-ink-100">
            Enforce single sign-on (SSO)
          </span>
          <span className="mt-0.5 block text-2xs text-ink-400">
            When on, human users must sign in through a configured IdP. The
            built-in default admin stays available as a break-glass account.
          </span>
        </span>
      </label>

      {requireSSO && (
        <label className="ml-6 mt-2 flex cursor-pointer items-start gap-2 text-xs text-ink-200">
          <input
            type="checkbox"
            checked={requireMFA}
            disabled={busy}
            onChange={(e) =>
              save.mutate({ require_sso: true, require_mfa: e.target.checked })
            }
            className="mt-0.5 h-4 w-4 accent-link"
          />
          <span>
            <span className="font-medium text-ink-100">
              Also require multi-factor sign-in (MFA)
            </span>
            <span className="mt-0.5 block text-2xs text-ink-400">
              Only IdP logins reported as multi-factor (via the{" "}
              <span className="font-mono text-ink-300">amr</span> claim) are
              accepted.
            </span>
          </span>
        </label>
      )}

      {save.isPending && (
        <p className="mt-2 inline-flex items-center gap-1.5 text-2xs text-ink-400">
          <Loader2 size={11} className="animate-spin" aria-hidden />
          Saving…
        </p>
      )}
    </div>
  );
}

// ConnShell — the card chrome every state renders inside.
function ConnShell({ children }: { children: React.ReactNode }) {
  return (
    <div className="card mb-4">
      <div className="card-header">
        <h2 className="card-title">Identity providers</h2>
        <span className="text-2xs text-ink-400">Enterprise control</span>
      </div>
      <div className="card-body">{children}</div>
    </div>
  );
}
