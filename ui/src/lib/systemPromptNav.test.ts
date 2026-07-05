import { describe, expect, it } from "vitest";

import {
  LEGACY_SYSTEM_PROMPT_REDIRECT,
  SYSTEM_PROMPT_PARENT,
  SYSTEM_PROMPT_PATH,
} from "./systemPromptNav";

// The System-prompt page was reachable only by direct URL after its Decisions
// button was removed (B50). It is now surfaced by an in-app nav entry on the
// Decisions header. These assert the wiring the entry, the route mount, the
// "Back" link, and the legacy redirect all share, so they can never drift apart
// and re-orphan the page.
describe("systemPromptNav", () => {
  it("nests the page under the Decisions view so the IA reads parent → detail", () => {
    expect(SYSTEM_PROMPT_PATH).toBe("/agent/decisions/system-prompt");
    expect(SYSTEM_PROMPT_PARENT).toBe("/agent/decisions");
    expect(SYSTEM_PROMPT_PATH.startsWith(`${SYSTEM_PROMPT_PARENT}/`)).toBe(true);
  });

  it("keeps the pre-redesign bookmark distinct from the canonical path", () => {
    expect(LEGACY_SYSTEM_PROMPT_REDIRECT).toBe("/detect/system-prompt");
    expect(LEGACY_SYSTEM_PROMPT_REDIRECT).not.toBe(SYSTEM_PROMPT_PATH);
  });
});
