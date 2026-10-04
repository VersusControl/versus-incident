import { describe, it, expect } from "vitest";
import { ApiError, buildAISettingsInput } from "@/lib/api";
import {
  DETECT_AI_DISABLED_FALLBACK,
  NO_ENCRYPTION_KEY_FALLBACK,
  detectAiDisabledRemedy,
  extractCode,
  extractRemedy,
  effectiveAITransportLabel,
  endpointKeyNotice,
  keySetLabel,
  noEncryptionKeyMessage,
  providerKeyNotice,
  projectAISettingsProvider,
  buildAISettingsSave,
} from "@/lib/agentAI";

// These tests pin the pure decision logic the AI-settings control and the
// mode control's detect cross-wire hang off, since the
// UI has no DOM test harness. The contracts that matter to operators:
//   1. a 422 with code "ai_disabled" surfaces the server remedy inline on the
//      detect path — never a generic error.
//   2. a 422 with code "no_encryption_key" surfaces the actionable
//      server-not-configured message.
//   3. the masked key status only ever renders last4 — never a full key.

const aiDisabled = () =>
  new ApiError(422, "AI must be enabled before entering detect mode", {
    error: "AI must be enabled before entering detect mode",
    code: "ai_disabled",
    remedy: "Enable AI in Agent → AI settings, then arm detect.",
  });

const noEncKey = () =>
  new ApiError(422, "cannot store API key: encryption key not configured", {
    error: "cannot store API key: encryption key not configured",
    code: "no_encryption_key",
    remedy: "Set VERSUS_ENTERPRISE_SECRET_KEY (base64 32 bytes) and retry.",
  });

describe("extractCode / extractRemedy", () => {
  it("reads the structured code and remedy out of an ApiError body", () => {
    const err = aiDisabled();
    expect(extractCode(err)).toBe("ai_disabled");
    expect(extractRemedy(err)).toMatch(/Enable AI/);
  });

  it("returns '' for non-ApiError or bodyless errors", () => {
    expect(extractCode(new Error("boom"))).toBe("");
    expect(extractCode(new ApiError(500, "x"))).toBe("");
    expect(extractRemedy(new Error("boom"))).toBe("");
    expect(extractRemedy("nope")).toBe("");
  });

  it("ignores a non-string code/remedy", () => {
    const err = new ApiError(422, "x", { code: 42, remedy: { a: 1 } });
    expect(extractCode(err)).toBe("");
    expect(extractRemedy(err)).toBe("");
  });
});

describe("detectAiDisabledRemedy", () => {
  it("returns the server remedy for a 422 ai_disabled", () => {
    expect(detectAiDisabledRemedy(aiDisabled())).toBe(
      "Enable AI in Agent → AI settings, then arm detect.",
    );
  });

  it("falls back to canned copy when the remedy is missing", () => {
    const err = new ApiError(422, "x", { code: "ai_disabled" });
    expect(detectAiDisabledRemedy(err)).toBe(DETECT_AI_DISABLED_FALLBACK);
  });

  it("is null for any other status, code, or error", () => {
    expect(detectAiDisabledRemedy(noEncKey())).toBeNull();
    expect(
      detectAiDisabledRemedy(new ApiError(403, "x", { code: "ai_disabled" })),
    ).toBeNull();
    expect(detectAiDisabledRemedy(new Error("x"))).toBeNull();
    expect(detectAiDisabledRemedy(null)).toBeNull();
  });
});

describe("noEncryptionKeyMessage", () => {
  it("returns the actionable server-config message for a 422 no_encryption_key", () => {
    expect(noEncryptionKeyMessage(noEncKey())).toMatch(/SECRET_KEY/);
  });

  it("falls back to canned copy when the remedy is missing", () => {
    const err = new ApiError(422, "x", { code: "no_encryption_key" });
    expect(noEncryptionKeyMessage(err)).toBe(NO_ENCRYPTION_KEY_FALLBACK);
  });

  it("is null for the ai_disabled 422 and for other errors", () => {
    expect(noEncryptionKeyMessage(aiDisabled())).toBeNull();
    expect(noEncryptionKeyMessage(new ApiError(500, "x"))).toBeNull();
    expect(noEncryptionKeyMessage(new Error("x"))).toBeNull();
  });
});

describe("keySetLabel", () => {
  it("shows the masked last4 when a key is set", () => {
    expect(keySetLabel(true, "ab12")).toBe("Key set ····ab12");
  });

  it("shows a generic 'Key set' when last4 is empty", () => {
    expect(keySetLabel(true, "")).toBe("Key set");
    expect(keySetLabel(true, "  ")).toBe("Key set");
  });

  it("shows 'No key set' when no key is configured", () => {
    expect(keySetLabel(false, "")).toBe("No key set");
    expect(keySetLabel(false, "ab12")).toBe("No key set");
  });
});

describe("providerKeyNotice", () => {
  it("is silent when the provider is unchanged", () => {
    const n = providerKeyNotice("openai", "openai", false);
    expect(n.show).toBe(false);
    expect(n.requireKey).toBe(false);
    expect(n.message).toBe("");
  });

  it("requires a new key when switching between keyed providers", () => {
    const n = providerKeyNotice("openai", "deepseek", false);
    expect(n.show).toBe(true);
    expect(n.requireKey).toBe(true);
    expect(n.tone).toBe("warn");
    expect(n.message).toMatch(/new API key/);
    expect(n.message).toMatch(/cannot be reused/);
  });

  it("requires a new key for every keyed provider authentication style", () => {
    const n = providerKeyNotice("openai", "claude", false);
    expect(n.requireKey).toBe(true);
    expect(n.tone).toBe("warn");
    expect(n.message).toMatch(/new API key/);
  });

  it("is informational (no key required) when a new key is entered with the switch", () => {
    const n = providerKeyNotice("openai", "gemini", true);
    expect(n.show).toBe(true);
    expect(n.requireKey).toBe(false);
    expect(n.tone).toBe("info");
    expect(n.message).toMatch(/key you entered/);
  });

  it("treats a blank provider as preserve", () => {
    const n = providerKeyNotice("openai", "", false);
    expect(n.show).toBe(false);
    expect(n.requireKey).toBe(false);
  });

  it("directs an active override to the explicit YAML revert action", () => {
    const n = providerKeyNotice("openai", "", false, true);
    expect(n.show).toBe(true);
    expect(n.requireKey).toBe(false);
    expect(n.tone).toBe("info");
    expect(n.message).toMatch(/Revert to YAML floor/);
    expect(n.message).toMatch(/keep its current provider/);
  });

  it("keeps a blank saved override valid when the selection remains blank", () => {
    const n = providerKeyNotice("", "", false, true);
    expect(n.show).toBe(false);
    expect(n.requireKey).toBe(false);
  });

  it("warns that ollama clears the stored runtime key without requiring one", () => {
    const n = providerKeyNotice("openai", "ollama", false);
    expect(n.show).toBe(true);
    expect(n.requireKey).toBe(false);
    expect(n.tone).toBe("warn");
    expect(n.message).toMatch(/clear the stored runtime API key/);
  });

  it("requires a key when moving from legacy blank state to a keyed provider", () => {
    const n = providerKeyNotice("", "gemini", false);
    expect(n.requireKey).toBe(true);
  });
});

describe("runtime AI endpoint settings", () => {
  const savedCustom = {
    enabled: true,
    provider: "ollama",
    base_url: "https://same.example/v1",
    base_url_inherited: true,
    key_set: false,
    source: "yaml" as const,
  };

  it.each([true, false])("preselects Custom for an effective URL with inherited=%s", (inherited) => {
    const view = { ...savedCustom, base_url_inherited: inherited };
    expect(projectAISettingsProvider(view)).toBe("custom");
    expect(projectAISettingsProvider({ provider: "gemini", base_url: "  " })).toBe("gemini");
  });

  it("defaults a blank native provider to openai", () => {
    expect(projectAISettingsProvider({ provider: "  ", base_url: "" })).toBe("openai");
  });

  it.each(["yaml", "override"] as const)("requires a fresh key for an unchanged inherited URL from %s without a runtime key", (source) => {
    const view = { ...savedCustom, source };
    const blocked = buildAISettingsSave(view, false, "custom", "  ", view.base_url);
    expect(blocked.requireKey).toBe(true);
    expect(blocked.endpointNotice).toMatchObject({ show: true, requireKey: true });
    const result = buildAISettingsSave(view, false, "custom", " fresh-key ", view.base_url);
    expect(result.requireKey).toBe(false);
    expect(result.input).toEqual({ enabled: false, provider: "openai", apiKey: "fresh-key", baseURL: view.base_url });
  });

  it.each([true, false])("preserves an unchanged custom override and usable runtime key with inherited=%s", (inherited) => {
    const view = { ...savedCustom, provider: "openai", source: "override" as const, key_set: true, base_url_inherited: inherited };
    const result = buildAISettingsSave(view, false, "custom", "  ", ` ${view.base_url} `);
    expect(result.requireKey).toBe(false);
    expect(result.customEndpointMissing).toBe(false);
    expect(result.providerNotice.show).toBe(false);
    expect(result.input).toEqual({ enabled: false, provider: "openai", apiKey: "", baseURL: savedCustom.base_url });
    expect(buildAISettingsInput(result.input.enabled, result.input.provider, result.input.apiKey, result.input.baseURL)).toEqual({
      enabled: false, provider: "openai", base_url: savedCustom.base_url,
    });
  });

  it.each(["openai", "deepseek", "qwen", "claude", "gemini"])("requires a fresh key when first selecting native %s from YAML", (provider) => {
    const view = { ...savedCustom, provider, base_url: "" };
    expect(buildAISettingsSave(view, true, provider, "  ", "").requireKey).toBe(true);
    const result = buildAISettingsSave(view, true, provider, " fresh-key ", "");
    expect(result.requireKey).toBe(false);
    expect(result.input).toEqual({ enabled: true, provider, apiKey: "fresh-key", baseURL: "" });
  });

  it("requires a fresh key when activating a native keyed route on a blank override", () => {
    const view = { ...savedCustom, source: "override" as const, provider: "", base_url: "" };
    const provider = projectAISettingsProvider(view);
    expect(buildAISettingsSave(view, true, provider, "", "").requireKey).toBe(true);
    expect(buildAISettingsSave(view, true, provider, "fresh-key", "").requireKey).toBe(false);
    expect(buildAISettingsSave(view, true, "ollama", "", "").requireKey).toBe(false);
  });

  it.each(["openai", "deepseek", "qwen", "claude", "gemini"])("guards custom to native %s and explicitly clears the YAML URL", (provider) => {
    expect(buildAISettingsSave(savedCustom, true, provider, "", savedCustom.base_url).requireKey).toBe(true);
    const result = buildAISettingsSave(savedCustom, true, provider, " fresh-key ", savedCustom.base_url);
    expect(result.requireKey).toBe(false);
    expect(result.input).toEqual({ enabled: true, provider, apiKey: "fresh-key", baseURL: "" });
    expect(buildAISettingsInput(result.input.enabled, result.input.provider, result.input.apiKey, result.input.baseURL).base_url).toBe("");
  });

  it("only allows keyless ollama when selecting its empty native URL", () => {
    const native = buildAISettingsSave(savedCustom, true, "ollama", "", savedCustom.base_url);
    expect(native.requireKey).toBe(false);
    expect(native.input).toEqual({ enabled: true, provider: "ollama", apiKey: "", baseURL: "" });
    expect(native.endpointNotice.message).toMatch(/clear the stored runtime API key/);
    const changed = buildAISettingsSave(savedCustom, true, "custom", "", "https://new.example/v1");
    expect(changed.requireKey).toBe(true);
    expect(changed.input.provider).toBe("openai");
  });

  it("requires a fresh key for a new custom destination and rejects a blank custom URL", () => {
    const native = { ...savedCustom, provider: "openai", base_url: "" };
    expect(buildAISettingsSave(native, true, "custom", "", savedCustom.base_url).requireKey).toBe(true);
    const result = buildAISettingsSave(native, true, "custom", " secret ", ` ${savedCustom.base_url} `);
    expect(result.requireKey).toBe(false);
    expect(result.input).toEqual({ enabled: true, provider: "openai", apiKey: "secret", baseURL: savedCustom.base_url });
    expect(buildAISettingsSave(native, true, "custom", "secret", "  ").customEndpointMissing).toBe(true);
  });

  it("saves native enable changes as an explicit PUT payload rather than a full reset", () => {
    const view = { ...savedCustom, provider: "openai", base_url: "", source: "override" as const, key_set: true };
    const result = buildAISettingsSave(view, false, "openai", "", "https://yaml.example/v1");
    expect(result.requireKey).toBe(false);
    expect(result.input).toEqual({ enabled: false, provider: "openai", apiKey: "", baseURL: "" });
  });

  it("omits an unchanged endpoint but preserves an explicit native endpoint", () => {
    expect(buildAISettingsInput(true, "openai", "", undefined)).toEqual({
      enabled: true,
      provider: "openai",
    });
    expect(buildAISettingsInput(true, "openai", "", "")).toEqual({
      enabled: true,
      provider: "openai",
      base_url: "",
    });
  });

  it("trims custom endpoint and provider credentials from the PUT payload", () => {
    expect(
      buildAISettingsInput(true, " openai ", " secret ", " https://llm.example/v1 "),
    ).toEqual({
      enabled: true,
      provider: "openai",
      api_key: "secret",
      base_url: "https://llm.example/v1",
    });
  });

  it("requires a fresh key when a keyed endpoint destination changes", () => {
    const notice = endpointKeyNotice(
      "https://old.example/v1",
      "https://new.example/v1",
      "openai",
      false,
    );
    expect(notice.requireKey).toBe(true);
    expect(notice.message).toMatch(/new API key/);
  });

  it("does not request a key for an unchanged endpoint", () => {
    expect(
      endpointKeyNotice("https://same.example/v1", "https://same.example/v1", "openai", false),
    ).toMatchObject({ show: false, requireKey: false });
  });

  it("labels a custom URL as OpenAI-compatible, not as the selected provider", () => {
    expect(effectiveAITransportLabel("https://llm.example/v1", "deepseek")).toBe(
      "OpenAI-compatible (custom endpoint)",
    );
    expect(effectiveAITransportLabel("", "deepseek")).toBe("deepseek");
  });
});
