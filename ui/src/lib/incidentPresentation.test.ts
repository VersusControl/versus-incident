import { describe, expect, it } from "vitest";
import { deliveryErrorLabel, hasDeliveryError, safeIncidentHref } from "./incidentPresentation";

describe("incident delivery presentation", () => {
  it.each(["partial", "failed"])("discloses %s delivery even without a recorded reason", (notify_status) => {
    expect(hasDeliveryError({ notify_status })).toBe(true);
  });

  it("keeps a recorded reason visible regardless of overall status", () => {
    expect(hasDeliveryError({ notify_status: "sent", notify_error: "channel unavailable" })).toBe(true);
    expect(hasDeliveryError({ notify_status: "sent" })).toBe(false);
    expect(hasDeliveryError({ notify_status: "pending" })).toBe(false);
    expect(deliveryErrorLabel("partial")).toBe("Partial delivery");
    expect(deliveryErrorLabel("failed")).toBe("Notify failed");
  });
});

describe("incident navigation", () => {
  it.each(["/agent/kubernetes?view=timeline&cluster=east", "https://console.example.com/agent/chat?issue=pod.crash_loop", "http://localhost:8080/agent/kubernetes"])("preserves safe destination %s", (href) => {
    expect(safeIncidentHref(href)).toBe(href);
  });

  it.each(["", "not a URL", "//evil.example", "/\\evil.example", "javascript:alert(1)", "data:text/html,bad", "https://user:password@example.com"])("rejects unsafe destination %s", (href) => {
    expect(safeIncidentHref(href)).toBeUndefined();
  });
});
