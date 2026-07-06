import { useSearchParams } from "react-router-dom";
import { TopBar } from "@/components/TopBar";
import { SegmentedControl } from "@/components/SegmentedControl";
import { ReportSettingsControl } from "@/components/ReportSettingsControl";
import { SpikeSettingsControl } from "@/components/SpikeSettingsControl";
import { IncidentsConfigPanel } from "./IncidentsConfigPage";
import { AgentConfigPanel } from "./AgentConfigPage";

// SettingsPage — the Manage-zone home for the configuration views, grouped by
// intent into URL-synced tabs (?tab=alerting|agent|tuning) so each view is
// deep-linkable:
//   • Alerting  — how the agent alerts: the incident delivery / on-call config.
//   • Agent     — the AI runtime configuration.
//   • Detection & reports — the two editable runtime knobs: the spike-detector
//     baseline mode and the periodic incident report.
// The legacy /config/incidents route redirects to the default (Alerting) tab
// and /config/agent to ?tab=agent. Every panel keeps its SecretBanner: secrets
// never reach the browser, only their presence is shown.
export function SettingsPage() {
  const [params] = useSearchParams();
  const raw = params.get("tab");
  const tab =
    raw === "agent" ? "agent" : raw === "tuning" ? "tuning" : "alerting";

  return (
    <>
      <TopBar
        title="Settings"
        subtitle="Read-only view of the running configuration"
      />
      <main className="flex-1 overflow-auto p-6">
        <div className="mb-4">
          <SegmentedControl
            param="tab"
            defaultValue="alerting"
            aria-label="Settings tabs"
            options={[
              { value: "alerting", label: "Alerting" },
              { value: "agent", label: "Agent" },
              { value: "tuning", label: "Detection & reports" },
            ]}
          />
        </div>
        {tab === "alerting" ? (
          <div className="space-y-4">
            <IncidentsConfigPanel />
          </div>
        ) : tab === "agent" ? (
          <div className="space-y-4">
            <AgentConfigPanel />
          </div>
        ) : (
          <div className="space-y-4">
            <SpikeSettingsControl />
            <ReportSettingsControl />
          </div>
        )}
      </main>
    </>
  );
}
