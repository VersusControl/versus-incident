import { describe, it, expect } from "vitest";
import { ApiError } from "@/lib/api";
import {
  DETECT_AI_DISABLED_FALLBACK,
  NO_ENCRYPTION_KEY_FALLBACK,
  detectAiDisabledRemedy,
  extractCode,
  extractRemedy,
  keySetLabel,
  noEncryptionKeyMessage,
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
