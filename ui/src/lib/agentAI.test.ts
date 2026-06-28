import { describe, it, expect } from "vitest";
import { ApiError } from "@/lib/api";
import {
  DETECT_AI_DISABLED_FALLBACK,
  NO_ENCRYPTION_KEY_FALLBACK,
  detectAiDisabledRemedy,
  extractCode,
  extractRemedy,
  isHeaderAuthProvider,
  keySetLabel,
  noEncryptionKeyMessage,
  providerKeyNotice,
} from "@/lib/agentAI";

// These tests pin the pure decision logic the AI-settings control (X27 item
// 9a) and the mode control's detect cross-wire (item 9b) hang off, since the
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

describe("isHeaderAuthProvider", () => {
  it("flags claude/gemini as header-auth (per-org key falls back to YAML)", () => {
    expect(isHeaderAuthProvider("claude")).toBe(true);
    expect(isHeaderAuthProvider("gemini")).toBe(true);
    expect(isHeaderAuthProvider(" gemini ")).toBe(true);
  });

  it("treats the Bearer providers as NOT header-auth", () => {
    expect(isHeaderAuthProvider("openai")).toBe(false);
    expect(isHeaderAuthProvider("deepseek")).toBe(false);
    expect(isHeaderAuthProvider("qwen")).toBe(false);
    expect(isHeaderAuthProvider("")).toBe(false);
  });
});

describe("providerKeyNotice (B35 — provider-switch key prompt)", () => {
  it("is silent when the provider is unchanged", () => {
    const n = providerKeyNotice("openai", "openai", false);
    expect(n.show).toBe(false);
    expect(n.requireKey).toBe(false);
    expect(n.message).toBe("");
  });

  it("warns + requires a key when switching provider with NO new key (Bearer)", () => {
    const n = providerKeyNotice("openai", "deepseek", false);
    expect(n.show).toBe(true);
    expect(n.requireKey).toBe(true);
    expect(n.tone).toBe("warn");
    expect(n.message).toMatch(/deepseek/);
    expect(n.message).toMatch(/existing per-org key will be reused/);
  });

  it("warns about the YAML fallback when switching to a header-auth provider with no key", () => {
    const n = providerKeyNotice("openai", "claude", false);
    expect(n.requireKey).toBe(true);
    expect(n.tone).toBe("warn");
    expect(n.message).toMatch(/authenticates by header/);
    expect(n.message).toMatch(/YAML-configured key/);
  });

  it("is informational (no key required) when a new key is entered with the switch", () => {
    const n = providerKeyNotice("openai", "gemini", true);
    expect(n.show).toBe(true);
    expect(n.requireKey).toBe(false);
    expect(n.tone).toBe("info");
    expect(n.message).toMatch(/key you entered/);
  });

  it("treats picking 'config default' (blank) as a change off a saved provider", () => {
    const n = providerKeyNotice("openai", "", false);
    expect(n.show).toBe(true);
    expect(n.requireKey).toBe(true);
    expect(n.message).toMatch(/config default/);
  });

  it("does not fire when both saved and selected are blank (config default)", () => {
    const n = providerKeyNotice("", "", false);
    expect(n.show).toBe(false);
  });
});
