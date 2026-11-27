import { Link } from "react-router-dom";
import { AwsPage } from "../shared/AwsPage";

// AWS / Issues: the sidebar entry's home until the Issues page is built.
// Links from Overview and Topology already point here with their filters.
export function AwsIssuesPage() {
  return (
    <AwsPage title="Issues" crumbs={[{ label: "Issues" }]}>
      <section className="card p-4 text-sm text-ink-200" data-issues-pending>
        The Issues list is not available in this build yet. The issues that need attention are on the{" "}
        <Link to="/aws/overview" className="text-link hover:underline">
          Overview ›
        </Link>
      </section>
    </AwsPage>
  );
}
