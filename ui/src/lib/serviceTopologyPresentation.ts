import { ApiError } from "@/lib/api";

export function topologyErrorCopy(error: unknown) {
  return error instanceof ApiError && [401, 403].includes(error.status)
    ? "Service topology is unavailable for this session."
    : "Service topology couldn't be loaded.";
}