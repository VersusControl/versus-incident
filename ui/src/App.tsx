import { Navigate, Route, Routes } from "react-router-dom";
import { AuthGate } from "./lib/auth";
import { AppShell } from "./components/AppShell";
import { DashboardPage } from "./pages/DashboardPage";
import { StatusPage } from "./pages/StatusPage";
import { PatternsPage } from "./pages/PatternsPage";
import { PatternDetailPage } from "./pages/PatternDetailPage";
import { ShadowPage } from "./pages/ShadowPage";
import { ShadowDetailPage } from "./pages/ShadowDetailPage";
import { ServicesPage } from "./pages/ServicesPage";
import { IncidentsPage } from "./pages/IncidentsPage";
import { IncidentDetailPage } from "./pages/IncidentDetailPage";

export default function App() {
  return (
    <AuthGate>
      <Routes>
        <Route element={<AppShell />}>
          <Route index element={<Navigate to="/dashboard" replace />} />
          <Route path="/dashboard" element={<DashboardPage />} />
          <Route path="/incidents" element={<IncidentsPage />} />
          <Route path="/incidents/:id" element={<IncidentDetailPage />} />
          <Route path="/status" element={<StatusPage />} />
          <Route path="/patterns" element={<PatternsPage />} />
          <Route path="/patterns/:id" element={<PatternDetailPage />} />
          <Route path="/shadow" element={<ShadowPage />} />
          <Route path="/shadow/:patternId" element={<ShadowDetailPage />} />
          <Route path="/services" element={<ServicesPage />} />
          <Route path="*" element={<Navigate to="/dashboard" replace />} />
        </Route>
      </Routes>
    </AuthGate>
  );
}
