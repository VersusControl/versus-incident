import type { IncidentSummary } from "./api";

export function hasDeliveryError(incident: Pick<IncidentSummary, "notify_status" | "notify_error">): boolean {
  return incident.notify_status === "failed" || incident.notify_status === "partial" || !!incident.notify_error;
}

export function deliveryErrorLabel(status?: string): string {
  return status === "partial" ? "Partial delivery" : "Notify failed";
}

export function safeIncidentHref(value: string): string | undefined {
  if (value.startsWith("/") && !value.startsWith("//") && !/[\\\s]/.test(value)) return value;
  try {
    const url = new URL(value);
    if ((url.protocol === "https:" || url.protocol === "http:") && !url.username && !url.password) return url.href;
  } catch {
    // Malformed payload links are displayed as facts, never navigated.
  }
  return undefined;
}

// Channel ids the alert fan-out names in its joined error ("slack: reason").
const KNOWN_CHANNELS = new Set(["slack", "telegram", "viber", "email", "msteams", "lark"]);

export interface NotifiedChannelRow {
  channel: string;
  outcome: "sent" | "failed" | "pending";
  reason?: string;
}

export interface NotifiedCardView {
  rows: NotifiedChannelRow[];
  // empty is the sentence shown when no channel row can be drawn. It always
  // agrees with notify_status: a "sent" incident with no channel says nothing
  // was sent, a failed one says the channel was not recorded.
  empty?: string;
  // unattributedError holds the delivery error lines no channel could be
  // read from, shown verbatim so the reason is never lost.
  unattributedError?: string;
}

type NotifyFields = Pick<IncidentSummary, "notify_status" | "notify_error" | "channels_notified" | "channels_enabled">;

// parseChannelErrors splits the fan-out's joined error (one "<channel>: <reason>"
// line per failed channel) into per-channel reasons. Lines that do not start
// with a known channel id are returned as leftovers.
export function parseChannelErrors(error?: string): { byChannel: Map<string, string>; rest: string[] } {
  const byChannel = new Map<string, string>();
  const rest: string[] = [];
  for (const line of (error ?? "").split("\n")) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    const match = /^([a-z]+):\s*(.*)$/.exec(trimmed);
    if (match && KNOWN_CHANNELS.has(match[1]) && !byChannel.has(match[1])) {
      byChannel.set(match[1], match[2] || "failed");
    } else {
      rest.push(trimmed);
    }
  }
  return { byChannel, rest };
}

// notifiedCardView derives the incident's Notified card from the delivery
// fields. Channels that succeeded come from channels_notified; the channels
// tried and failed are channels_enabled minus channels_notified, with each
// reason read from notify_error. Records from servers that omit
// channels_enabled fall back to the channel names in notify_error.
export function notifiedCardView(incident: NotifyFields): NotifiedCardView {
  const status = incident.notify_status ?? "";
  const succeeded = incident.channels_notified ?? [];
  const { byChannel, rest } = parseChannelErrors(incident.notify_error);
  const rows: NotifiedChannelRow[] = [];
  const seen = new Set<string>();
  const add = (row: NotifiedChannelRow) => {
    if (seen.has(row.channel)) return;
    seen.add(row.channel);
    rows.push(row);
  };
  for (const channel of succeeded) add({ channel, outcome: "sent" });
  for (const [channel, reason] of byChannel) add({ channel, outcome: "failed", reason });
  if (status === "failed" || status === "partial" || status === "pending") {
    for (const channel of incident.channels_enabled ?? []) {
      add({ channel, outcome: status === "pending" ? "pending" : "failed" });
    }
  }

  const unattributedError = rest.length > 0 ? rest.join("\n") : undefined;
  if (rows.length > 0) return { rows, unattributedError };

  let empty: string;
  switch (status) {
    case "none":
    case "sent":
      empty = "No channel was enabled at the time, so nothing was sent.";
      break;
    case "suppressed":
      empty = "The notification was suppressed, so nothing was sent.";
      break;
    case "grouped":
      empty = "Grouped with an earlier alert, so no separate notification was sent.";
      break;
    case "failed":
    case "partial":
      empty = "Delivery failed; the failing channel was not recorded.";
      break;
    case "pending":
      empty = "Delivery has not finished yet.";
      break;
    case "diverted":
      empty = "Delivered by an interceptor, not by a channel.";
      break;
    case "":
      empty = "No delivery was recorded.";
      break;
    default:
      empty = `Delivery status: ${status}.`;
  }
  return { rows, empty, unattributedError };
}

// notifyStatusDisplay is the overall Notified state. "none" (no channel was
// enabled) reads "nothing sent", as does a "sent" status from an older server
// with no channel that received the alert, so no surface claims a delivery the
// card cannot name.
export function notifyStatusDisplay(incident: NotifyFields): string {
  const status = incident.notify_status ?? "";
  if (status === "none") return "nothing sent";
  if (status === "sent" && (incident.channels_notified ?? []).length === 0) return "nothing sent";
  return status;
}
