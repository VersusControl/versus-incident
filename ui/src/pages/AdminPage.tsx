import { AgentModeControl } from "@/components/AgentModeControl";
import { AgentAISettingsControl } from "@/components/AgentAISettingsControl";
import { AgentChannelsSettingsControl } from "@/components/AgentChannelsSettingsControl";
import { AlertFatigueSettingsControl } from "@/components/AlertFatigueSettingsControl";
import { AgentSSOConnectionsControl } from "@/components/AgentSSOConnectionsControl";
import { AdminMembersControl } from "@/components/AdminMembersControl";
import { SettingsLayout } from "@/components/settings/SettingsLayout";
import { ADMIN_LEGACY_HASHES, ADMIN_SECTIONS } from "@/components/settings/sections";
import { useEffectiveRole } from "@/lib/useEffectiveRole";

// AdminPage (/admin) — Enterprise administration, one section at a time.
// Every control gates itself on the caller's RBAC role, so the page is safe to
// show to any operator: community builds see the locked upsell per section.
export function AdminPage() {
  const access = useEffectiveRole();
  const community = !access.loading && !access.enterprise;

  return (
    <SettingsLayout
      title="Admin"
      navLabel="Admin sections"
      sections={ADMIN_SECTIONS}
      legacy={{ hashes: ADMIN_LEGACY_HASHES }}
      lockedEnterprise={community}
      headerActions={
        access.loading ? null : (
          <span className={community ? "pill" : "pill pill-accent"}>
            {community ? "Community" : "Enterprise"}
          </span>
        )
      }
      renderSection={renderAdminSection}
    />
  );
}

function renderAdminSection(id: string) {
  switch (id) {
    case "ai":
      return <AgentAISettingsControl />;
    case "channels":
      return <AgentChannelsSettingsControl />;
    case "alert-fatigue":
      return <AlertFatigueSettingsControl />;
    case "sso":
      return <AgentSSOConnectionsControl />;
    case "members":
      return <AdminMembersControl />;
    default:
      return <AgentModeControl />;
  }
}

export default AdminPage;
