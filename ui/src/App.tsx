import { Navigate, Route, Routes } from "react-router-dom";
import { AuthGate } from "./lib/auth";
import { AppShell } from "./components/AppShell";
import { DashboardPage } from "./pages/DashboardPage";
import { StatusPage } from "./pages/StatusPage";
import { PatternsPage } from "./pages/PatternsPage";
import { PatternDetailPage } from "./pages/PatternDetailPage";
import { ShadowPage } from "./pages/ShadowPage";
import { ShadowDetailPage } from "./pages/ShadowDetailPage";
import { DetectPage } from "./pages/DetectPage";
import { DetectDetailPage } from "./pages/DetectDetailPage";
import { SystemPromptPage } from "./pages/SystemPromptPage";
import { ServicesPage } from "./pages/ServicesPage";
import { IncidentsPage } from "./pages/IncidentsPage";
import { IncidentDetailPage } from "./pages/IncidentDetailPage";
import { AnalysesPage } from "./pages/AnalysesPage";
import { AnalysesListPage } from "./pages/AnalysesListPage";
import { AnalysisDetailPage } from "./pages/AnalysisDetailPage";
import { IncidentsConfigPage } from "./pages/IncidentsConfigPage";
import { AgentConfigPage } from "./pages/AgentConfigPage";
import { MembersPage } from "./pages/MembersPage";
import { TeamsPage } from "./pages/TeamsPage";

export default function App() {
  return (
    <AuthGate>
      <Routes>
        <Route element={<AppShell />}>
          <Route index element={<Navigate to="/dashboard" replace />} />
          <Route path="/dashboard" element={<DashboardPage />} />
          <Route path="/incidents" element={<IncidentsPage />} />
          <Route path="/analyses" element={<AnalysesListPage />} />
          <Route path="/incidents/:id" element={<IncidentDetailPage />} />
          <Route path="/incidents/:id/analyses" element={<AnalysesPage />} />
          <Route
            path="/incidents/:id/analyses/:analysisId"
            element={<AnalysisDetailPage />}
          />
          <Route path="/status" element={<StatusPage />} />
          <Route path="/patterns" element={<PatternsPage />} />
          <Route path="/patterns/:id" element={<PatternDetailPage />} />
          <Route path="/shadow" element={<ShadowPage />} />
          <Route path="/shadow/:patternId" element={<ShadowDetailPage />} />
          <Route path="/detect" element={<DetectPage />} />
          <Route path="/detect/system-prompt" element={<SystemPromptPage />} />
          <Route path="/detect/:id" element={<DetectDetailPage />} />
          <Route path="/services" element={<ServicesPage />} />
          <Route path="/members" element={<MembersPage />} />
          <Route path="/teams" element={<TeamsPage />} />
          <Route path="/config/incidents" element={<IncidentsConfigPage />} />
          <Route path="/config/agent" element={<AgentConfigPage />} />
          <Route path="*" element={<Navigate to="/dashboard" replace />} />
        </Route>
      </Routes>
    </AuthGate>
  );
}
