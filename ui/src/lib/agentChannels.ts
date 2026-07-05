// agentChannels — pure, DOM-free decision logic shared by the Enterprise
// runtime notification-channel settings control (Item 2, the runtimeai
// sibling). Everything here is side-effect-free so it can be unit-tested in the
// node vitest env (the UI has no jsdom/testing-library); the component is a thin
// shell over these helpers.

import { ApiError } from "@/lib/api";
import type { ChannelSettingsView, ChannelFieldSchema } from "@/lib/api";

// CHANNELS is the closed, ordered set of notification channels the override
// addresses. It mirrors the enterprise runtimechannels.Channels() registry and
// the OSS config.AlertConfig channel fields one-for-one; the server re-validates
// on WRITE and rejects an unknown channel with 400, so this is the UI's first
// line of defence, not the authority.
export const CHANNELS = [
  "slack",
  "telegram",
  "viber",
  "email",
  "msteams",
  "lark",
] as const;

export type ChannelName = (typeof CHANNELS)[number];

// CHANNEL_LABELS maps the wire channel key to its display label.
export const CHANNEL_LABELS: Record<string, string> = {
  slack: "Slack",
  telegram: "Telegram",
  viber: "Viber",
  email: "Email",
  msteams: "Microsoft Teams",
  lark: "Lark",
};

// FieldSpec is the UI-side field descriptor: the wire name, a display label,
// whether it is a write-only secret (rendered masked), and whether it is a
// boolean toggle. It mirrors the server's per-channel field schema
// (runtimechannels specs).
export interface FieldSpec {
  name: string;
  label: string;
  secret: boolean;
  bool: boolean;
}

// CHANNEL_SCHEMA is the single, data-driven field schema that drives all six
// channel cards (one table, not six components). Secret fields are write-only:
// the input is masked, a blank submission preserves the stored value. It
// mirrors the enterprise runtimechannels field specs one-for-one.
export const CHANNEL_SCHEMA: Record<string, FieldSpec[]> = {
  slack: [
    { name: "token", label: "Bot token", secret: true, bool: false },
    { name: "channel_id", label: "Channel ID", secret: false, bool: false },
    { name: "template_path", label: "Template path", secret: false, bool: false },
  ],
  telegram: [
    { name: "bot_token", label: "Bot token", secret: true, bool: false },
    { name: "chat_id", label: "Chat ID", secret: false, bool: false },
    { name: "template_path", label: "Template path", secret: false, bool: false },
    { name: "use_proxy", label: "Use proxy", secret: false, bool: true },
  ],
  viber: [
    { name: "bot_token", label: "Bot token", secret: true, bool: false },
    { name: "api_type", label: "API type (bot | channel)", secret: false, bool: false },
    { name: "user_id", label: "User ID", secret: false, bool: false },
    { name: "channel_id", label: "Channel ID", secret: false, bool: false },
    { name: "template_path", label: "Template path", secret: false, bool: false },
    { name: "use_proxy", label: "Use proxy", secret: false, bool: true },
  ],
  email: [
    { name: "smtp_host", label: "SMTP host", secret: false, bool: false },
    { name: "smtp_port", label: "SMTP port", secret: false, bool: false },
    { name: "username", label: "SMTP username", secret: true, bool: false },
    { name: "password", label: "SMTP password", secret: true, bool: false },
    { name: "to", label: "To", secret: false, bool: false },
    { name: "subject", label: "Subject", secret: false, bool: false },
    { name: "template_path", label: "Template path", secret: false, bool: false },
  ],
  msteams: [
    { name: "power_automate_url", label: "Power Automate URL", secret: true, bool: false },
    { name: "template_path", label: "Template path", secret: false, bool: false },
  ],
  lark: [
    { name: "webhook_url", label: "Webhook URL", secret: true, bool: false },
    { name: "template_path", label: "Template path", secret: false, bool: false },
    { name: "use_proxy", label: "Use proxy", secret: false, bool: true },
  ],
};

// channelLabel renders a channel's display label (falls back to the key).
export function channelLabel(channel: string): string {
  return CHANNEL_LABELS[channel] ?? channel;
}

// fieldSetLabel renders the masked status of a secret field. It only ever shows
// the server-masked hint (last-4 for tokens, scheme+host for URLs) — never a
// raw secret (the UI never receives one).
export function fieldSetLabel(set: boolean, hint: string): string {
  if (!set) return "Not set";
  const h = hint.trim();
  return h ? `Set (${h})` : "Set";
}

// NO_ENCRYPTION_KEY_FALLBACK is shown when the server omits a remedy on the
// no_encryption_key denial for a channel secret write.
export const NO_ENCRYPTION_KEY_FALLBACK =
  "The server has no master key configured, so the channel secret cannot be stored. Set VERSUS_ENTERPRISE_SECRET_KEY on the server and retry.";

// extractCode pulls the structured `code` discriminator out of an ApiError body.
export function extractCode(err: unknown): string {
  if (
    err instanceof ApiError &&
    err.body &&
    typeof err.body === "object" &&
    "code" in err.body
  ) {
    const code = (err.body as { code: unknown }).code;
    return typeof code === "string" ? code : "";
  }
  return "";
}

// extractRemedy pulls the server-authored `remedy` string out of an ApiError body.
export function extractRemedy(err: unknown): string {
  if (
    err instanceof ApiError &&
    err.body &&
    typeof err.body === "object" &&
    "remedy" in err.body
  ) {
    const remedy = (err.body as { remedy: unknown }).remedy;
    return typeof remedy === "string" ? remedy : "";
  }
  return "";
}

// noEncryptionKeyMessage returns the actionable server-not-configured message
// when a channel secret write was refused (422 no_encryption_key), else null.
export function noEncryptionKeyMessage(err: unknown): string | null {
  if (!(err instanceof ApiError) || err.status !== 422) return null;
  if (extractCode(err) !== "no_encryption_key") return null;
  return extractRemedy(err) || NO_ENCRYPTION_KEY_FALLBACK;
}

// buildChannelPut composes the PUT body for one channel from the operator's
// staged form values. It is the single source of truth for the write contract:
//   - a BLANK secret field is OMITTED so the server preserves the stored value
//     (write-only: a secret the UI never received is never sent back empty),
//   - a non-blank secret is sent as-is (rotated),
//   - non-secret fields are always sent (they round-trip from the masked view),
//   - bool fields are sent as real JSON booleans.
// The staged secret values are held transiently by the caller and cleared after
// the PUT — never persisted client-side.
export function buildChannelPut(
  channel: string,
  enable: boolean,
  values: Record<string, string>,
): { enable: boolean; fields: Record<string, string | boolean> } {
  const schema = CHANNEL_SCHEMA[channel] ?? [];
  const fields: Record<string, string | boolean> = {};
  for (const f of schema) {
    const raw = values[f.name] ?? "";
    if (f.secret) {
      const v = raw.trim();
      if (v) fields[f.name] = v; // blank omitted → preserve stored secret
      continue;
    }
    if (f.bool) {
      fields[f.name] = raw === "true" || raw === "on" || raw === "1";
      continue;
    }
    fields[f.name] = raw;
  }
  return { enable, fields };
}

// initialChannelValues seeds a channel's form from its masked view: non-secret
// fields pre-fill from their echoed value; secret fields ALWAYS start blank
// (write-only — the UI never receives the secret, and a blank submission
// preserves it).
export function initialChannelValues(
  channel: string,
  view: ChannelSettingsView | undefined,
): Record<string, string> {
  const schema = CHANNEL_SCHEMA[channel] ?? [];
  const out: Record<string, string> = {};
  const fields = view?.fields ?? {};
  for (const f of schema) {
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

// isSecretField reports whether a channel's field is a write-only secret, from
// the server-provided schema when available, else the static UI schema.
export function isSecretField(
  channel: string,
  field: string,
  schema?: ChannelFieldSchema,
): boolean {
  if (schema && field in schema) return schema[field].secret;
  return (CHANNEL_SCHEMA[channel] ?? []).some((f) => f.name === field && f.secret);
}
