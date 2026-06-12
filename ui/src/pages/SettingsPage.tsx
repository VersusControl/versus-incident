import { useSearchParams } from "react-router-dom";
import { TopBar } from "@/components/TopBar";
import { SegmentedControl } from "@/components/SegmentedControl";
import { IncidentsConfigPanel } from "./IncidentsConfigPage";
import { AgentConfigPanel } from "./AgentConfigPage";

// SettingsPage — the Manage-zone home for the read-only configuration
// views. Incidents and Agent config are URL-synced tabs
// (?tab=incidents|agent) so each view is deep-linkable; the legacy
// /config/incidents and /config/agent routes redirect here. Both panels
// keep their SecretBanner: secrets never reach the browser, only their
// presence is shown.
export function SettingsPage() {
  const [params] = useSearchParams();
  const tab = params.get("tab") === "agent" ? "agent" : "incidents";

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
            defaultValue="incidents"
            aria-label="Settings tabs"
            options={[
              { value: "incidents", label: "Incidents" },
              { value: "agent", label: "Agent" },
            ]}
          />
        </div>
        {tab === "incidents" ? <IncidentsConfigPanel /> : <AgentConfigPanel />}
      </main>
    </>
  );
}
