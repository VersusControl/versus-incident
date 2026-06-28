import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { ApiError, api, type SSODeployment } from "@/lib/api";

// useDeploymentOrg resolves the single-tenant deployment org this enterprise
// binary serves. The org is sourced server-side from the LICENSE_KEY (X4) — it
// is NOT operator-selectable — so every admin SSO control targets it instead of
// a hardcoded "default". The pre-auth /enterprise/api/sso/deployment route is
// license-gated, so a community/unlicensed binary 403s it; the caller treats
// that (and an absent OSS route 404) as "not enterprise" and renders the locked
// upsell. Cached process-wide (the org can't change without a restart).
export function useDeploymentOrg(): UseQueryResult<SSODeployment, unknown> {
  return useQuery<SSODeployment>({
    queryKey: ["sso-deployment"],
    queryFn: () => api.getSSODeployment(),
    staleTime: Infinity,
    retry: (count, err) => {
      if (err instanceof ApiError && [401, 403, 404, 503].includes(err.status)) {
        return false;
      }
      return count < 1;
    },
  });
}
