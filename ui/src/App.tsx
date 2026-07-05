import { Suspense, lazy } from "react";
import { Navigate, Route, Routes, useParams } from "react-router-dom";
import { AuthGate } from "./lib/auth";
import {
  LEGACY_SYSTEM_PROMPT_REDIRECT,
  SYSTEM_PROMPT_PATH,
} from "./lib/systemPromptNav";
import { AppShell } from "./components/AppShell";
import { SkCard } from "./components/Skeleton";
// Hot paths (the 3am set) stay in the main chunk:
import { NowPage } from "./pages/NowPage";
import { IncidentsPage } from "./pages/IncidentsPage";
import { IncidentDetailPage } from "./pages/IncidentDetailPage";
import { AnalysesListPage } from "./pages/AnalysesListPage";
import { AgentOverviewPage } from "./pages/AgentOverviewPage";
import { PatternsPage } from "./pages/PatternsPage";
import { DecisionsPage } from "./pages/DecisionsPage";
import { ServicesPage } from "./pages/ServicesPage";

// Cold paths load on demand (route-level splitting, no extra deps) —
// keeps the main chunk inside the ≤120KB gzip budget.
const AnalysisDetailPage = lazyPage(
  () => import("./pages/AnalysisDetailPage"),
  "AnalysisDetailPage",
);
const PatternDetailPage = lazyPage(
  () => import("./pages/PatternDetailPage"),
  "PatternDetailPage",
);
const DetectDetailPage = lazyPage(
  () => import("./pages/DetectDetailPage"),
  "DetectDetailPage",
);
const ShadowDetailPage = lazyPage(
  () => import("./pages/ShadowDetailPage"),
  "ShadowDetailPage",
);
const SystemPromptPage = lazyPage(
  () => import("./pages/SystemPromptPage"),
  "SystemPromptPage",
);
const RunbooksPage = lazyPage(() => import("./pages/RunbooksPage"), "RunbooksPage");
const ServiceDetailPage = lazyPage(
  () => import("./pages/ServiceDetailPage"),
  "ServiceDetailPage",
);
const MetricsPage = lazyPage(
  () => import("./pages/LearnedSignalsView"),
  "MetricsPage",
);
const TracesPage = lazyPage(
  () => import("./pages/LearnedSignalsView"),
  "TracesPage",
);
const SLORecommendationsPage = lazyPage(
  () => import("./pages/SLORecommendationsPage"),
  "SLORecommendationsPage",
);
const PeoplePage = lazyPage(() => import("./pages/PeoplePage"), "PeoplePage");
const SettingsPage = lazyPage(() => import("./pages/SettingsPage"), "SettingsPage");
const AdminPage = lazyPage(() => import("./pages/AdminPage"), "AdminPage");

// lazyPage adapts our named-export pages to React.lazy and wraps them in a
// Suspense fallback that mirrors a loading card (no blank flash).
function lazyPage(
  loader: () => Promise<Record<string, unknown>>,
  name: string,
) {
  const L = lazy(async () => {
    const m = await loader();
    return { default: m[name] as React.ComponentType };
  });
  return function LazyRoute() {
    return (
      <Suspense
        fallback={
          <main className="flex-1 overflow-auto p-6">
            <SkCard lines={4} />
          </main>
        }
      >
        <L />
      </Suspense>
    );
  };
}

// Route map per UX_REDESIGN §3.3. Three zones (Respond / Agent / Manage) +
// a full set of legacy redirects so pre-redesign bookmarks and alert
// deep-links keep working (back-stack-integrity / deep-linking rules).
export default function App() {
  return (
    <AuthGate>
      <Routes>
        <Route element={<AppShell />}>
          <Route index element={<Navigate to="/now" replace />} />

          {/* Respond */}
          <Route path="/now" element={<NowPage />} />
          <Route path="/incidents" element={<IncidentsPage />} />
          <Route path="/incidents/:id" element={<IncidentDetailPage />} />
          <Route path="/analyses" element={<AnalysesListPage />} />
          <Route
            path="/incidents/:id/analyses/:analysisId"
            element={<AnalysisDetailPage />}
          />

          {/* Agent */}
          <Route path="/agent" element={<AgentOverviewPage />} />
          <Route path="/agent/logs" element={<PatternsPage />} />
          <Route path="/agent/logs/:id" element={<PatternDetailPage />} />
          <Route path="/agent/patterns/:id" element={<PatternDetailPage />} />
          <Route path="/agent/metrics" element={<MetricsPage />} />
          <Route path="/agent/traces" element={<TracesPage />} />
          <Route path="/agent/slo" element={<SLORecommendationsPage />} />
          <Route path="/agent/decisions" element={<DecisionsPage />} />
          <Route
            path={SYSTEM_PROMPT_PATH}
            element={<SystemPromptPage />}
          />
          <Route
            path="/agent/decisions/detect/:id"
            element={<DetectDetailPage />}
          />
          <Route
            path="/agent/decisions/shadow/:patternId"
            element={<ShadowDetailPage />}
          />
          <Route path="/agent/services" element={<ServicesPage />} />
          <Route
            path="/agent/services/:name"
            element={<ServiceDetailPage />}
          />
          <Route path="/agent/runbooks" element={<RunbooksPage />} />

          {/* Manage */}
          <Route path="/people" element={<PeoplePage />} />
          <Route path="/admin" element={<AdminPage />} />
          <Route path="/settings" element={<SettingsPage />} />

          {/* Legacy redirects (old IA → new homes) */}
          <Route path="/dashboard" element={<Navigate to="/now" replace />} />
          <Route path="/status" element={<Navigate to="/agent" replace />} />
          <Route
            path="/agent/patterns"
            element={<Navigate to="/agent/logs" replace />}
          />
          <Route
            path="/agent/baselines"
            element={<Navigate to="/agent/metrics" replace />}
          />
          <Route
            path="/patterns"
            element={<Navigate to="/agent/logs" replace />}
          />
          <Route path="/patterns/:id" element={<PatternIdRedirect />} />
          <Route
            path="/detect"
            element={<Navigate to="/agent/decisions?tab=detect" replace />}
          />
          <Route
            path={LEGACY_SYSTEM_PROMPT_REDIRECT}
            element={<Navigate to={SYSTEM_PROMPT_PATH} replace />}
          />
          <Route path="/detect/:id" element={<DetectIdRedirect />} />
          <Route
            path="/shadow"
            element={<Navigate to="/agent/decisions?tab=shadow" replace />}
          />
          <Route path="/shadow/:patternId" element={<ShadowIdRedirect />} />
          <Route
            path="/services"
            element={<Navigate to="/agent/services" replace />}
          />
          <Route
            path="/runbooks"
            element={<Navigate to="/agent/runbooks" replace />}
          />
          <Route
            path="/members"
            element={<Navigate to="/people?tab=members" replace />}
          />
          <Route
            path="/teams"
            element={<Navigate to="/people?tab=teams" replace />}
          />
          <Route
            path="/config/incidents"
            element={<Navigate to="/settings" replace />}
          />
          <Route
            path="/config/agent"
            element={<Navigate to="/settings?tab=agent" replace />}
          />
          <Route
            path="/postmortems"
            element={<Navigate to="/analyses?tab=postmortems" replace />}
          />
          <Route
            path="/incidents/:id/analyses"
            element={<IncidentAnalysesRedirect />}
          />

          <Route path="*" element={<Navigate to="/now" replace />} />
        </Route>
      </Routes>
    </AuthGate>
  );
}

function PatternIdRedirect() {
  const { id } = useParams();
  return <Navigate to={`/agent/logs/${id}`} replace />;
}

function DetectIdRedirect() {
  const { id } = useParams();
  return <Navigate to={`/agent/decisions/detect/${id}`} replace />;
}

function ShadowIdRedirect() {
  const { patternId } = useParams();
  return <Navigate to={`/agent/decisions/shadow/${patternId}`} replace />;
}

function IncidentAnalysesRedirect() {
  const { id } = useParams();
  return <Navigate to={`/analyses?incident=${id}`} replace />;
}
