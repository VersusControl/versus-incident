import { useQuery } from "@tanstack/react-query";
import { api, ApiError } from "@/lib/api";

export function useServiceIntelQuery(name: string) {
  return useQuery({
    queryKey: ["service-intel", name],
    queryFn: () => api.getServiceIntel(name),
    enabled: !!name,
    retry: (count, err) => {
      if (
        err instanceof ApiError &&
        (err.status === 403 || err.status === 404)
      )
        return false;
      return count < 1;
    },
  });
}