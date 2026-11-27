import { useQuery } from "@tanstack/react-query";
import { useRetryUnlessForbidden } from "@/lib/access";
import { api, type AwsAccount } from "@/lib/api";

/** The configured accounts, their names and Regions, and whether the caller may write. */
export function useAwsAccounts() {
  const retry = useRetryUnlessForbidden();
  const settings = useQuery({
    queryKey: ["aws-settings"],
    queryFn: () => api.awsSettings(),
    retry,
    staleTime: 30_000,
  });
  const accounts: AwsAccount[] = settings.data?.enabled ? settings.data.accounts : [];
  const byId = new Map(accounts.map((a) => [a.account_id, a]));
  const name = (id: string) => byId.get(id)?.display_name || id;
  const regions = [...new Set(accounts.flatMap((a) => a.regions.selected))].sort();
  return {
    accounts,
    name,
    regions,
    canWrite: settings.data?.capabilities.can_write === true,
    loaded: settings.isSuccess,
    /** The account list could not be read. */
    error: settings.isError ? settings.error : null,
    retry: () => void settings.refetch(),
    retrying: settings.isFetching,
  };
}

