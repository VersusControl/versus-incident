import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type ServiceHealthSettings } from "@/lib/api";

export const serviceHealthKeys = {
  snapshot: ["service-health"] as const,
  settings: ["service-health-settings"] as const,
};

export function useServiceHealthQuery() {
  return useQuery({
    queryKey: serviceHealthKeys.snapshot,
    queryFn: api.getServiceHealth,
    refetchInterval: 60_000,
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