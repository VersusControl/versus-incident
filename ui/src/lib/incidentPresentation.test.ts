import { describe, expect, it } from "vitest";
import {
  deliveryErrorLabel,
  hasDeliveryError,
  notifiedCardView,
  notifyStatusDisplay,
  safeIncidentHref,
} from "./incidentPresentation";

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

describe("Notified card view", () => {
  it("names each failed channel with its own reason from the joined error", () => {
    const view = notifiedCardView({
      notify_status: "partial",
      channels_notified: ["email"],
      notify_error: "slack: invalid_auth\ntelegram: chat not found",
    });
    expect(view.rows).toEqual([
      { channel: "email", outcome: "sent" },
      { channel: "slack", outcome: "failed", reason: "invalid_auth" },
      { channel: "telegram", outcome: "failed", reason: "chat not found" },
    ]);
    expect(view.empty).toBeUndefined();
    expect(view.unattributedError).toBeUndefined();
  });

  it("marks enabled channels that did not succeed as failed", () => {
    const view = notifiedCardView({ notify_status: "failed", channels_enabled: ["slack", "email"], notify_error: "slack: invalid_auth" });
    expect(view.rows).toEqual([
      { channel: "slack", outcome: "failed", reason: "invalid_auth" },
      { channel: "email", outcome: "failed" },
    ]);
  });

  it("keeps an error with no channel name visible and says the channel was not recorded", () => {
    const view = notifiedCardView({ notify_status: "failed", notify_error: "context deadline exceeded" });
    expect(view.rows).toEqual([]);
    expect(view.empty).toBe("Delivery failed; the failing channel was not recorded.");
    expect(view.unattributedError).toBe("context deadline exceeded");
  });

  it("treats enabled minus notified as the failed channels", () => {
    const view = notifiedCardView({
      notify_status: "partial",
      channels_enabled: ["slack", "email"],
      channels_notified: ["email"],
      notify_error: "slack: invalid_auth",
    });
    expect(view.rows).toEqual([
      { channel: "email", outcome: "sent" },
      { channel: "slack", outcome: "failed", reason: "invalid_auth" },
    ]);
  });

  it("reads none as nothing sent", () => {
    expect(notifyStatusDisplay({ notify_status: "none" })).toBe("nothing sent");
    expect(notifiedCardView({ notify_status: "none" }).empty).toBe("No channel was enabled at the time, so nothing was sent.");
    expect(notifiedCardView({ notify_status: "suppressed" }).empty).toBe("The notification was suppressed, so nothing was sent.");
  });

  it("never pairs a sent status with an empty channel list", () => {
    expect(notifiedCardView({ notify_status: "sent" }).empty).toBe("No channel was enabled at the time, so nothing was sent.");
    expect(notifyStatusDisplay({ notify_status: "sent" })).toBe("nothing sent");
    expect(notifyStatusDisplay({ notify_status: "sent", channels_notified: ["slack"] })).toBe("sent");
    expect(notifiedCardView({ notify_status: "pending" }).empty).toBe("Delivery has not finished yet.");
    expect(notifiedCardView({}).empty).toBe("No delivery was recorded.");
  });
});
