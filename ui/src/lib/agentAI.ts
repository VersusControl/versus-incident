// agentAI — pure, DOM-free decision logic shared by the Enterprise AI-settings
// control and the mode control's detect cross-wire.
//
// Everything here is intentionally side-effect-free so it can be unit-tested in
// the node vitest env (the UI has no jsdom/testing-library). The components are
// thin shells over these helpers.

import { ApiError, type AISettingsView } from "@/lib/api";

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

// ProviderKeyNotice is the pure verdict for the AI-settings provider `<select>`.
// It tells the control whether a provider change is staged and whether the
// operator must supply the selected provider's key before Save can proceed.
export interface ProviderKeyNotice {
  // show — a provider change is staged (the selection differs from the saved
  // provider); render the inline notice.
  show: boolean;
  // requireKey — a keyed provider changed but no new key was entered. The
  // control must block Save; the server independently enforces the invariant.
  requireKey: boolean;
  // tone — "warn" when a key is owed or a stored key will be cleared.
  tone: "warn" | "info";
  message: string;
}

// providerKeyNotice computes the staged-provider-change verdict from the saved
// provider, the currently-selected provider, and whether a new key was entered.
// It is the single source of truth for the inline notice and Save disabled
// state, kept DOM-free so it can be unit-tested in the node vitest env.
export function providerKeyNotice(
  savedProvider: string,
  selectedProvider: string,
  keyEntered: boolean,
  onOverride = false,
): ProviderKeyNotice {
  const sel = selectedProvider.trim();
  if (sel === "" && onOverride && savedProvider.trim() !== "") {
    return {
      show: true,
      requireKey: false,
      tone: "info",
      message:
        "To use the provider from YAML, use Revert to YAML floor below. Saving this override will keep its current provider.",
    };
  }
  // A blank provider remains an explicit preserve operation at the API
  // boundary for enable-only writes and legacy clients.
  const changed = sel !== "" && sel !== savedProvider.trim();
  if (!changed) {
    return { show: false, requireKey: false, tone: "info", message: "" };
  }
  if (sel === "ollama") {
    return {
      show: true,
      requireKey: false,
      tone: "warn",
      message:
        "Saving will switch the model provider to ollama and clear the stored runtime API key.",
    };
  }
  if (keyEntered) {
    return {
      show: true,
      requireKey: false,
      tone: "info",
      message: `Saving will switch the model provider to ${sel} with the key you entered.`,
    };
  }
  return {
    show: true,
    requireKey: true,
    tone: "warn",
    message: `Enter a new API key to switch providers.`,
  };
}

export interface EndpointKeyNotice {
  show: boolean;
  requireKey: boolean;
  message: string;
}

export function endpointKeyNotice(
  savedBaseURL: string,
  selectedBaseURL: string,
  selectedProvider: string,
  keyEntered: boolean,
): EndpointKeyNotice {
  const saved = savedBaseURL.trim();
  const selected = selectedBaseURL.trim();
  if (saved === selected) {
    return { show: false, requireKey: false, message: "" };
  }

  const keyRequired = selected !== "" || selectedProvider.trim() !== "ollama";
  if (!keyRequired) {
    return {
      show: true,
      requireKey: false,
      message: "Saving will use the native ollama endpoint and clear the stored runtime API key.",
    };
  }
  if (keyEntered) {
    return {
      show: true,
      requireKey: false,
      message: selected
        ? "Saving will change the endpoint using the new API key you entered."
        : "Saving will switch to the native endpoint using the new API key you entered.",
    };
  }
  return {
    show: true,
    requireKey: true,
    message: "Enter a new API key when changing endpoint destinations.",
  };
}

export function effectiveAITransportLabel(baseURL: string, provider: string): string {
  if (baseURL.trim()) return "OpenAI-compatible (custom endpoint)";
  return provider.trim() || "config default";
}

export function projectAISettingsProvider(
  view: Pick<AISettingsView, "provider" | "base_url">,
): string {
  return view.base_url.trim() ? "custom" : view.provider.trim() || "openai";
}

export function buildAISettingsSave(
  view: Pick<AISettingsView, "provider" | "base_url" | "source" | "key_set">,
  enabled: boolean,
  selectedProvider: string,
  keyInput: string,
  baseURL: string,
) {
  const custom = selectedProvider === "custom";
  const provider = custom ? "openai" : selectedProvider;
  const selectedBaseURL = custom ? baseURL.trim() : "";
  const apiKey = keyInput.trim();
  const providerNotice = providerKeyNotice(
    view.base_url.trim() ? "openai" : view.source === "override" ? view.provider : "",
    provider,
    apiKey.length > 0,
    view.source === "override",
  );
  const endpointNotice: EndpointKeyNotice = custom && !view.key_set && !apiKey
    ? {
        show: true,
        requireKey: true,
        message: "Enter a new API key to save a custom endpoint.",
      }
    : endpointKeyNotice(
        view.base_url,
        selectedBaseURL,
        provider,
        apiKey.length > 0,
      );
  return {
    input: { enabled, provider, apiKey, baseURL: selectedBaseURL },
    providerNotice,
    endpointNotice,
    requireKey: providerNotice.requireKey || endpointNotice.requireKey,
    customEndpointMissing: custom && !selectedBaseURL,
  };
}
