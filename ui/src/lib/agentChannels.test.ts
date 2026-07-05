import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import { ApiError } from "@/lib/api";
import type { ChannelSettingsView } from "@/lib/api";
import {
  CHANNELS,
  CHANNEL_SCHEMA,
  buildChannelPut,
  channelLabel,
  fieldSetLabel,
  initialChannelValues,
  isSecretField,
  noEncryptionKeyMessage,
} from "@/lib/agentChannels";

// These tests pin the pure decision logic the channels-settings control (Item
// 2, the runtimeai sibling) hangs off, since the UI has no DOM test harness.
// The operator-facing contracts that matter:
//   1. secrets are WRITE-ONLY: a masked view never carries a raw value, a blank
//      secret is OMITTED from the PUT so the server preserves the stored one.
//   2. non-secret fields round-trip from the masked hint.
//   3. a 422 no_encryption_key surfaces the actionable server-config message.
//   4. gating admin-visible / viewer-hidden / community-absent is driven by the
//      shared adminGateState + the control's own render (asserted against the
//      component source, mirroring catalogReset.test.ts).

const here = path.dirname(fileURLToPath(import.meta.url)); // src/lib
const read = (rel: string) => readFileSync(path.resolve(here, rel), "utf8");

describe("CHANNEL_SCHEMA", () => {
  it("covers all six channels with matching secret fields", () => {
    expect([...CHANNELS].sort()).toEqual(
      ["email", "lark", "msteams", "slack", "telegram", "viber"].sort(),
    );
    // Every channel has a schema, and the secret-bearing fields match the design.
    const secretOf = (c: string) =>
      (CHANNEL_SCHEMA[c] ?? []).filter((f) => f.secret).map((f) => f.name).sort();
    expect(secretOf("slack")).toEqual(["token"]);
    expect(secretOf("telegram")).toEqual(["bot_token"]);
    expect(secretOf("viber")).toEqual(["bot_token"]);
    expect(secretOf("email")).toEqual(["password", "username"]);
    expect(secretOf("msteams")).toEqual(["power_automate_url"]);
    expect(secretOf("lark")).toEqual(["webhook_url"]);
  });
});

describe("buildChannelPut", () => {
  it("omits a blank secret so the server preserves the stored value", () => {
    const body = buildChannelPut("slack", true, {
      token: "   ",
      channel_id: "C123",
      template_path: "t.tmpl",
    });
    expect(body.enable).toBe(true);
    expect("token" in body.fields).toBe(false); // blank secret omitted
    expect(body.fields.channel_id).toBe("C123");
    expect(body.fields.template_path).toBe("t.tmpl");
  });

  it("sends a rotated (non-blank) secret", () => {
    const body = buildChannelPut("slack", true, {
      token: "xoxb-new",
      channel_id: "C123",
      template_path: "t.tmpl",
    });
    expect(body.fields.token).toBe("xoxb-new");
  });

  it("sends bool fields as real JSON booleans", () => {
    const on = buildChannelPut("telegram", true, {
      bot_token: "",
      chat_id: "-100",
      template_path: "t.tmpl",
      use_proxy: "true",
    });
    expect(on.fields.use_proxy).toBe(true);
    const off = buildChannelPut("telegram", true, {
      chat_id: "-100",
      template_path: "t.tmpl",
      use_proxy: "false",
    });
    expect(off.fields.use_proxy).toBe(false);
  });
});

describe("initialChannelValues", () => {
  const view: ChannelSettingsView = {
    enabled: true,
    configured: true,
    source: "override",
    yaml_enabled: false,
    fields: {
      token: { set: true, hint: "…x9f2" },
      channel_id: { set: true, hint: "C0123" },
      template_path: { set: true, hint: "slack.tmpl" },
    },
  };

  it("pre-fills non-secret fields from the echoed hint but never seeds a secret", () => {
    const vals = initialChannelValues("slack", view);
    expect(vals.channel_id).toBe("C0123");
    expect(vals.template_path).toBe("slack.tmpl");
    // The secret input ALWAYS starts blank — the UI never receives the value.
    expect(vals.token).toBe("");
  });

  it("defaults to empty when there is no override", () => {
    const vals = initialChannelValues("slack", undefined);
    expect(vals.token).toBe("");
    expect(vals.channel_id).toBe("");
  });
});

describe("fieldSetLabel", () => {
  it("shows the masked hint when set, only ever the hint (never a raw secret)", () => {
    expect(fieldSetLabel(true, "…x9f2")).toBe("Set (…x9f2)");
    expect(fieldSetLabel(true, "")).toBe("Set");
    expect(fieldSetLabel(false, "…x9f2")).toBe("Not set");
  });
});

describe("isSecretField", () => {
  it("classifies secret vs non-secret fields from the static schema", () => {
    expect(isSecretField("slack", "token")).toBe(true);
    expect(isSecretField("slack", "channel_id")).toBe(false);
    expect(isSecretField("email", "password")).toBe(true);
    expect(isSecretField("msteams", "power_automate_url")).toBe(true);
  });
});

describe("noEncryptionKeyMessage", () => {
  it("returns the actionable message for a 422 no_encryption_key", () => {
    const err = new ApiError(422, "cannot store channel secret", {
      code: "no_encryption_key",
      remedy: "Set VERSUS_ENTERPRISE_SECRET_KEY and retry.",
    });
    expect(noEncryptionKeyMessage(err)).toMatch(/SECRET_KEY/);
  });

  it("is null for other statuses / codes / errors", () => {
    expect(noEncryptionKeyMessage(new ApiError(400, "x", { code: "invalid_config" }))).toBeNull();
    expect(noEncryptionKeyMessage(new Error("boom"))).toBeNull();
  });
});

describe("channelLabel", () => {
  it("renders display labels and falls back to the key", () => {
    expect(channelLabel("msteams")).toBe("Microsoft Teams");
    expect(channelLabel("slack")).toBe("Slack");
    expect(channelLabel("unknown")).toBe("unknown");
  });
});

// Source-level guards for the API client contract + the component gating, in
// the style of catalogReset.test.ts (the UI has no DOM harness).
describe("api client: channel-settings endpoints", () => {
  const apiSrc = read("./api.ts");

  it("exposes getChannelSettings as GET the masked channel-settings endpoint", () => {
    expect(apiSrc.includes("getChannelSettings")).toBe(true);
    expect(apiSrc.includes("/enterprise/api/agent/channel-settings")).toBe(true);
  });

  it("exposes setChannelSettings as a per-channel PUT", () => {
    expect(/setChannelSettings[\s\S]{0,200}method:\s*"PUT"/.test(apiSrc)).toBe(true);
  });

  it("exposes clearChannelSettings as a per-channel DELETE", () => {
    expect(/clearChannelSettings[\s\S]{0,200}method:\s*"DELETE"/.test(apiSrc)).toBe(true);
  });

  it("exposes testChannel as a per-channel POST test", () => {
    expect(/testChannel[\s\S]{0,200}\/test[\s\S]{0,80}method:\s*"POST"/.test(apiSrc)).toBe(true);
  });
});

describe("AgentChannelsSettingsControl: gating + write-only secrets", () => {
  const cmp = read("../components/AgentChannelsSettingsControl.tsx");

  it("gates on the shared adminGateState (community locked / viewer read-only / no-session sign-in)", () => {
    expect(cmp.includes("adminGateState")).toBe(true);
    expect(cmp.includes("useEffectiveRole")).toBe(true);
    expect(cmp.includes("LockedBody")).toBe(true);
    expect(cmp.includes('reason="sign-in"')).toBe(true);
    expect(cmp.includes('reason="role"')).toBe(true);
  });

  it("only issues the privileged GET for an admin (enabled: gate === admin)", () => {
    expect(/enabled:\s*gate === "admin"/.test(cmp)).toBe(true);
  });

  it("renders secret inputs masked (type password) and clears the transient secret on save", () => {
    expect(cmp.includes('type={f.secret && !showSecret[f.name] ? "password" : "text"}')).toBe(true);
    expect(/if \(f\.secret\) next\[f\.name\] = "";/.test(cmp)).toBe(true);
  });

  it("routes writes through buildChannelPut (blank secret preserved) and never localStorage", () => {
    expect(cmp.includes("buildChannelPut")).toBe(true);
    expect(cmp.includes("localStorage")).toBe(false);
  });
});
