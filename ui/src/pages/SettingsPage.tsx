import { ReportSettingsControl } from "@/components/ReportSettingsControl";
import { SpikeSettingsControl } from "@/components/SpikeSettingsControl";
import { CountSettingsControl } from "@/components/CountSettingsControl";
import { ServiceHealthSettingsControl } from "@/components/ServiceHealthSettingsControl";
import { SettingsLayout } from "@/components/settings/SettingsLayout";
import { SETTINGS_LEGACY_TABS, SETTINGS_SECTIONS } from "@/components/settings/sections";
import { IncidentsConfigSection } from "./IncidentsConfigPage";
import { AgentConfigSection } from "./AgentConfigPage";

// SettingsPage (/settings) — running configuration and runtime tuning, one
// section at a time. YAML-configured sections are read-only and never show
// secret values; tuning sections save without a restart.
export function SettingsPage() {
  return (
    <SettingsLayout
      title="Settings"
      navLabel="Settings sections"
      sections={SETTINGS_SECTIONS}
      legacy={{ tabs: SETTINGS_LEGACY_TABS }}
      renderSection={renderSettingsSection}
    />
  );
}

function renderSettingsSection(id: string) {
  switch (id) {
    case "count-window":
      return <CountSettingsControl />;
    case "service-health":
      return <ServiceHealthSettingsControl />;
    case "channels":
      return <IncidentsConfigSection part="channels" />;
    case "queues":
      return <IncidentsConfigSection part="queues" />;
    case "oncall":
      return <IncidentsConfigSection part="oncall" />;
    case "agent-runtime":
      return <AgentConfigSection part="runtime" />;
    case "agent-sources":
      return <AgentConfigSection part="sources" />;
    case "agent-ai":
      return <AgentConfigSection part="ai" />;
    case "agent-patterns":
      return <AgentConfigSection part="patterns" />;
    case "spike":
      return <SpikeSettingsControl />;
    case "reports":
      return <ReportSettingsControl />;
    default:
      return <IncidentsConfigSection part="server" />;
  }
}
