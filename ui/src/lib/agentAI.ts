// agentAI — pure, DOM-free decision logic shared by the Enterprise AI-settings
// control (X27 item 9a) and the mode control's detect cross-wire (item 9b).
//
// Everything here is intentionally side-effect-free so it can be unit-tested in
// the node vitest env (the UI has no jsdom/testing-library). The components are
// thin shells over these helpers.

import { ApiError } from "@/lib/api";

// extractCode pulls the structured `code` discriminator out of an ApiError
// body (the server returns { error, code, remedy } on a guarded 422). Returns
// "" when the error is not an ApiError or carries no string code.
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

// extractRemedy pulls the server-authored `remedy` string out of an ApiError
// body. Returns "" when absent — callers fall back to their own copy.
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

// DETECT_AI_DISABLED_FALLBACK matches the server remedy for the detect guard
// (pkg/runtimemode), used only if the wire body omits it. Detect needs AI
// enabled AND an API key, so the fallback covers both.
export const DETECT_AI_DISABLED_FALLBACK =
  "Enable AI and set an API key in Admin → AI settings, then arm detect.";

// detectAiDisabledRemedy returns the remedy copy when a mode PUT was blocked by
// the detect AI-guard (422 with code "ai_disabled"), else null. Used by the
// mode control to surface the remedy INLINE on the detect path instead of a
// generic error toast.
export function detectAiDisabledRemedy(err: unknown): string | null {
  if (!(err instanceof ApiError) || err.status !== 422) return null;
  if (extractCode(err) !== "ai_disabled") return null;
  return extractRemedy(err) || DETECT_AI_DISABLED_FALLBACK;
}

// NO_ENCRYPTION_KEY_FALLBACK is shown when the server omits a remedy on the
// no_encryption_key denial.
export const NO_ENCRYPTION_KEY_FALLBACK =
  "The server has no master key configured (VERSUS_ENTERPRISE_SECRET_KEY is unset), so the API key cannot be stored. Set it on the server and retry.";

// noEncryptionKeyMessage returns the actionable server-not-configured message
// when a key write was refused (422 with code "no_encryption_key"), else null.
export function noEncryptionKeyMessage(err: unknown): string | null {
  if (!(err instanceof ApiError) || err.status !== 422) return null;
  if (extractCode(err) !== "no_encryption_key") return null;
  return extractRemedy(err) || NO_ENCRYPTION_KEY_FALLBACK;
}

// keySetLabel renders the masked key status for display. It only ever shows the
// last four chars the server already masked — never a full key (which the UI
// never receives).
export function keySetLabel(keySet: boolean, last4: string): string {
  if (!keySet) return "No key set";
  const tail = last4.trim();
  return tail ? `Key set ····${tail}` : "Key set";
}
