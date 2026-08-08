// alertFatigueChannel — pure, DOM-free schema + write-contract helpers for the
// custom fatigue-channel form. Everything here is side-effect-free so it can be
// unit-tested in the node vitest env; the form is a thin shell over these.
//
// The field schema mirrors the enterprise alertfatigue.customChannelSpecs
// one-for-one (channel.go). The server re-validates on WRITE and rejects an
// unknown type / missing required field / over-long value with 400, so this is
// the UI's first line of defence, not the authority.

import type {
  AlertFatigueCustomChannel,
  AlertFatigueCustomChannelPut,
} from "@/lib/api";

// CUSTOM_CHANNEL_TYPES is the closed, ordered set of channel kinds a custom
// fatigue channel may be (mirrors the CHECK constraint on
// vs_alert_fatigue_channel and the OSS provider factory).
export const CUSTOM_CHANNEL_TYPES = [
  "slack",
  "telegram",
  "email",
  "msteams",
  "lark",
  "viber",
] as const;

export type CustomChannelType = (typeof CUSTOM_CHANNEL_TYPES)[number];

// CUSTOM_CHANNEL_LABELS maps the wire channel key to its display label.
export const CUSTOM_CHANNEL_LABELS: Record<string, string> = {
  slack: "Slack",
  telegram: "Telegram",
  email: "Email",
  msteams: "Microsoft Teams",
  lark: "Lark",
  viber: "Viber",
};

// CustomChannelFieldSpec is the UI-side field descriptor: the wire name, a
// display label, whether it is a write-only secret (rendered masked, only
// re-sent when the operator types a new value), whether it is required, and
// whether it is a boolean toggle.
export interface CustomChannelFieldSpec {
  name: string;
  label: string;
  secret: boolean;
  required: boolean;
  bool?: boolean;
}

// CUSTOM_CHANNEL_SPECS is the single source of truth for each channel type's
// field schema. It mirrors alertfatigue.customChannelSpecs field-for-field.
export const CUSTOM_CHANNEL_SPECS: Record<string, CustomChannelFieldSpec[]> = {
  slack: [
    { name: "token", label: "Bot token", secret: true, required: true },
    { name: "channel_id", label: "Channel ID", secret: false, required: true },
  ],
  telegram: [
    { name: "bot_token", label: "Bot token", secret: true, required: true },
    { name: "chat_id", label: "Chat ID", secret: false, required: true },
    { name: "use_proxy", label: "Use proxy", secret: false, required: false, bool: true },
  ],
  email: [
    { name: "smtp_host", label: "SMTP host", secret: false, required: true },
    { name: "smtp_port", label: "SMTP port", secret: false, required: false },
    { name: "username", label: "SMTP username", secret: true, required: true },
    { name: "password", label: "SMTP password", secret: true, required: true },
    { name: "to", label: "To", secret: false, required: true },
    { name: "subject", label: "Subject", secret: false, required: false },
  ],
  msteams: [
    {
      name: "power_automate_url",
      label: "Power Automate URL",
      secret: true,
      required: true,
    },
  ],
  lark: [
    { name: "webhook_url", label: "Webhook URL", secret: true, required: true },
    { name: "use_proxy", label: "Use proxy", secret: false, required: false, bool: true },
  ],
  viber: [
    { name: "bot_token", label: "Bot token", secret: true, required: true },
    { name: "api_type", label: "API type (bot | channel)", secret: false, required: false },
    { name: "user_id", label: "User ID", secret: false, required: false },
    { name: "channel_id", label: "Channel ID", secret: false, required: false },
    { name: "use_proxy", label: "Use proxy", secret: false, required: false, bool: true },
  ],
};

// customChannelLabel renders a channel's display label (falls back to the key).
export function customChannelLabel(channel: string): string {
  return CUSTOM_CHANNEL_LABELS[channel] ?? channel;
}

// customFieldSetLabel renders the masked status of a secret field. It only ever
// shows the server-masked hint (last-4 for tokens, scheme+host for URLs) —
// never a raw secret (the UI never receives one).
export function customFieldSetLabel(set: boolean, hint: string): string {
  if (!set) return "Not set";
  const h = hint.trim();
  return h ? `Set (${h})` : "Set";
}

// initialCustomChannelValues seeds a channel's form from its masked view:
// non-secret fields pre-fill from their echoed hint; secret fields ALWAYS start
// blank (write-only — the UI never receives the secret, and a blank submission
// preserves it). When the view is for a different channel type (or none), every
// field starts blank/off.
export function initialCustomChannelValues(
  channelType: string,
  view: AlertFatigueCustomChannel | undefined,
): Record<string, string> {
  const spec = CUSTOM_CHANNEL_SPECS[channelType] ?? [];
  const sameType =
    !!view?.configured && view.channel_type === channelType;
  const fields = sameType ? view?.fields ?? {} : {};
  const out: Record<string, string> = {};
  for (const f of spec) {
    if (f.secret) {
      out[f.name] = "";
      continue;
    }
    const mf = fields[f.name];
    if (f.bool) {
      out[f.name] = mf?.hint === "true" ? "true" : "false";
    } else {
      out[f.name] = mf?.hint ?? "";
    }
  }
  return out;
}

// buildCustomChannelPut composes the PUT body for the custom fatigue channel
// from the operator's staged form values. It is the single source of truth for
// the write contract:
//   - a BLANK secret field is OMITTED so the server preserves the stored value
//     (write-only: a secret the UI never received is never sent back empty),
//   - a non-blank secret is sent as-is (rotated),
//   - non-secret text fields are sent when non-blank (they round-trip from the
//     masked hint),
//   - bool fields are always sent as the string "true"/"false" (the server
//     config map is string-typed).
// Staged secret values are held transiently by the caller and cleared after the
// PUT — never persisted client-side.
export function buildCustomChannelPut(
  channelType: string,
  enabled: boolean,
  values: Record<string, string>,
): AlertFatigueCustomChannelPut {
  const spec = CUSTOM_CHANNEL_SPECS[channelType] ?? [];
  const config: Record<string, string> = {};
  for (const f of spec) {
    const raw = values[f.name] ?? "";
    if (f.secret) {
      const v = raw.trim();
      if (v) config[f.name] = v; // blank omitted → preserve stored secret
      continue;
    }
    if (f.bool) {
      config[f.name] =
        raw === "true" || raw === "on" || raw === "1" ? "true" : "false";
      continue;
    }
    const v = raw.trim();
    if (v) config[f.name] = v;
  }
  return { channel_type: channelType, enabled, config };
}

// canSaveCustomChannel reports whether every required field is satisfiable: a
// required non-secret field must be filled; a required secret field must be
// filled UNLESS it is already stored for this same channel type (a blank
// submission then preserves it). Mirrors the server's ErrMissingField rule so
// the UI can disable Save before a doomed request.
export function canSaveCustomChannel(
  channelType: string,
  values: Record<string, string>,
  view: AlertFatigueCustomChannel | undefined,
): boolean {
  const spec = CUSTOM_CHANNEL_SPECS[channelType] ?? [];
  const sameType = !!view?.configured && view.channel_type === channelType;
  for (const f of spec) {
    if (!f.required) continue;
    if ((values[f.name] ?? "").trim()) continue;
    if (f.secret && sameType && view?.fields?.[f.name]?.set) continue;
    return false;
  }
  return true;
}
