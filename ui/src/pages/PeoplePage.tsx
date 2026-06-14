import { useSearchParams } from "react-router-dom";
import { TopBar } from "@/components/TopBar";
import { SegmentedControl } from "@/components/SegmentedControl";
import { MembersPanel } from "./MembersPage";
import { TeamsPanel } from "./TeamsPage";

// PeoplePage — the Manage-zone home for the assignment roster. Members
// and Teams are URL-synced tabs (?tab=members|teams) so each view is
// deep-linkable; the legacy /members and /teams routes redirect here.
export function PeoplePage() {
  const [params] = useSearchParams();
  const tab = params.get("tab") === "teams" ? "teams" : "members";

  return (
    <>
      <TopBar
        title="People"
        subtitle="Members and teams for incident assignment"
      />
      <main className="flex-1 overflow-auto p-6">
        <div className="mb-4">
          <SegmentedControl
            param="tab"
            defaultValue="members"
            aria-label="People tabs"
            options={[
              { value: "members", label: "Members" },
              { value: "teams", label: "Teams" },
            ]}
          />
        </div>
        {tab === "members" ? <MembersPanel /> : <TeamsPanel />}
      </main>
    </>
  );
}
