import { Outlet } from "react-router-dom";
import { Sidebar } from "./Sidebar";

// AppShell is the persistent two-column layout (Datadog-style): dark sidebar
// on the left, a flex content column on the right. Pages render inside the
// <Outlet/> and are responsible for their own TopBar/main scroll.
export function AppShell() {
  return (
    <div className="flex h-full overflow-hidden">
      <Sidebar />
      <div className="flex min-w-0 flex-1 flex-col overflow-hidden">
        <Outlet />
      </div>
    </div>
  );
}
