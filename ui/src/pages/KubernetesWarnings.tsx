import { type ReactNode } from "react";
import { CoverageMarker } from "@/components/CoverageMarker";
type KubernetesWarning = { id: string; content: ReactNode };

export function KubernetesWarnings({ warnings, scope }: { warnings: KubernetesWarning[]; scope: string }) {
  const unique = [...new Map(warnings.map((warning) => [warning.id, warning])).values()];
  return <CoverageMarker key={scope} reasons={unique.map((warning) => warning.content)} />;
}