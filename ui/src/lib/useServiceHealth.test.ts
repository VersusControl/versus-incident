import { describe, expect, it } from "vitest";
import { ApiError } from "./api";
import { serviceHealthPollingInterval } from "./useServiceHealth";

describe("service health polling", () => {
  it.each([401, 403])("stops after an authorization response with status %s", (status) => {
    expect(serviceHealthPollingInterval(new ApiError(status, "private response"))).toBe(false);
  });

  it("keeps the live cadence for non-authorization outcomes", () => {
    expect(serviceHealthPollingInterval(new ApiError(503, "unavailable"))).toBe(60_000);
    expect(serviceHealthPollingInterval(null)).toBe(60_000);
  });
});