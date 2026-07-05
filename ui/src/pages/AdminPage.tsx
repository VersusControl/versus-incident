import { TopBar } from "@/components/TopBar";
import { AgentModeControl } from "@/components/AgentModeControl";
import { AgentAISettingsControl } from "@/components/AgentAISettingsControl";
import { AgentChannelsSettingsControl } from "@/components/AgentChannelsSettingsControl";
import { AgentSSOConnectionsControl } from "@/components/AgentSSOConnectionsControl";
import { AdminMembersControl } from "@/components/AdminMembersControl";

// AdminPage (/admin) — the Manage-zone home for Enterprise configuration. It
// consolidates the privileged controls so operators have one obvious place to
// manage runtime mode, AI settings, SSO / login access, and members + roles.
//
// Every control is authorized by the caller's RBAC role carried by their SSO
// session (runtime:manage / sso:manage / roles:manage), NOT a static admin
// token. Each control reads its own state and gates itself: OSS / community
// builds render the locked Enterprise upsell, a signed-out operator is asked to
// sign in, a viewer/responder sees a read-only notice, and only an admin/owner
// gets the live control — so the page is safe to surface unconditionally.
export function AdminPage() {
  return (
    <>
      <TopBar
        title="Admin"
        subtitle="Enterprise configuration — runtime mode, AI, SSO access, and members"
      />
      <main className="flex-1 overflow-auto p-6">
        {/* Runtime mode control (Enterprise; RBAC runtime:manage). Reads its own
            state so OSS / community renders the locked upsell and no control. */}
        <AgentModeControl />

        {/* AI settings control (Enterprise; RBAC runtime:manage). The mode
            control's detect guard links here when AI is off. */}
        <AgentAISettingsControl />

        {/* Notification-channel settings control (Enterprise; RBAC
            runtime:manage). Per-channel runtime creds + enable override; masked
            write-only secrets, save takes effect without restart. Locked upsell
            on OSS / community. */}
        <AgentChannelsSettingsControl />

        {/* SSO / identity providers (Enterprise; RBAC sso:manage, per-org). The
            single canonical SSO panel: a Keycloak-style list of Google /
            Microsoft Entra / OIDC providers (one sign-in button each) plus the
            login-enforcement policy (require SSO / MFA). Locked upsell on OSS /
            community; the org is sourced from the license, not operator-picked. */}
        <AgentSSOConnectionsControl />

        {/* Members & roles (Enterprise; RBAC roles:manage, per-org). Lists the
            people who sign in via SSO with their effective role, lets an admin
            assign roles, and manages the deployment's default admin user. */}
        <AdminMembersControl />
      </main>
    </>
  );
}

export default AdminPage;
