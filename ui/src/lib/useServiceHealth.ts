import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError, type ServiceHealthSettings } from "@/lib/api";

export const serviceHealthKeys = {
  snapshot: ["service-health"] as const,
  topology: ["service-topology"] as const,
  settings: ["service-health-settings"] as const,
};

export function isServiceHealthAccessDenied(error: unknown) {
  return error instanceof ApiError && [401, 403].includes(error.status);
}

export function serviceHealthPollingInterval(error: unknown) {
  return isServiceHealthAccessDenied(error) ? false : 60_000;
}

export function useServiceHealthQuery() {
  return useQuery({
    queryKey: serviceHealthKeys.snapshot,
    queryFn: api.getServiceHealth,
    refetchInterval: (query) => serviceHealthPollingInterval(query.state.error),
    retry: false,
  });
}

export function useServiceTopologyQuery(enabled = true) {
  return useQuery({
    queryKey: serviceHealthKeys.topology,
    queryFn: api.getServiceTopology,
    enabled,
    refetchInterval: (query) => serviceHealthPollingInterval(query.state.error),
    retry: false,
  });
}

export function useServiceHealthSettingsQuery() {
  return useQuery({
    queryKey: serviceHealthKeys.settings,
    queryFn: api.getServiceHealthSettings,
  });
}

export function useUpdateServiceHealthSettings(options: {
  onSuccess?: (settings: ServiceHealthSettings) => void;
  onError?: (error: unknown) => void;
} = {}) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (settings: ServiceHealthSettings) => api.updateServiceHealthSettings(settings),
    onSuccess: (saved) => {
      queryClient.setQueryData(serviceHealthKeys.settings, saved);
      queryClient.invalidateQueries({ queryKey: serviceHealthKeys.snapshot });
      options.onSuccess?.(saved);
    },
    onError: options.onError,
  });
}