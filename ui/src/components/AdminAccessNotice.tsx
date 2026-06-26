import { LogIn, ShieldAlert } from "lucide-react";

// AdminAccessNotice is the shared read-only hint the enterprise admin controls
// render when the caller may not manage them. Privileged management rides the
// SSO session's RBAC role, so the only remedies are (a) sign in via SSO, or
// (b) be granted the admin role by an owner.
//
//   reason="sign-in" — no live SSO session (the gateway-secret/data-plane
//                       operator). Management requires an org sign-in.
//   reason="role"    — signed in, but the role is read-only (viewer/responder).
export function AdminAccessNotice({ reason }: { reason: "sign-in" | "role" }) {
  if (reason === "sign-in") {
    return (
      <div
        data-testid="admin-access-notice"
        data-reason="sign-in"
        className="flex items-start gap-3 text-xs text-ink-300"
      >
        <LogIn size={16} className="mt-0.5 shrink-0 text-accent" aria-hidden />
        <div>
          <p className="font-medium text-ink-100">Sign in to manage</p>
          <p className="mt-0.5">
            Managing this requires signing in with your organization account
            (single sign-on). Privileges are carried by your role.
          </p>
        </div>
      </div>
    );
  }
  return (
    <div
      data-testid="admin-access-notice"
      data-reason="role"
      className="flex items-start gap-3 text-xs text-ink-300"
    >
      <ShieldAlert size={16} className="mt-0.5 shrink-0 text-sev-warn" aria-hidden />
      <div>
        <p className="font-medium text-ink-100">Requires the admin role</p>
        <p className="mt-0.5">
          Your account has read-only access here. An owner or admin can grant
          you the admin role from the Members panel.
        </p>
      </div>
    </div>
  );
}
