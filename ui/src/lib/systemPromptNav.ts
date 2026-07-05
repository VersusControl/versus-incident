// systemPromptNav.ts — the single source of truth for where the System-prompt
// page lives in the app, so the in-app nav entry (Decisions header), the route
// mount, the page's "Back" link, and the legacy redirect all agree. Keeping the
// path in one pure module makes the wiring unit-testable without rendering the
// router (the project's lib/*.ts + lib/*.test.ts pattern) and stops the page
// from drifting back into being reachable by direct URL only.

// SYSTEM_PROMPT_PARENT is the view the System-prompt page hangs off of — the
// Decisions view, whose detect-mode AI calls the constant system prompt heads.
// It is both the nav-entry home and the page's "Back" target.
export const SYSTEM_PROMPT_PARENT = "/agent/decisions";

// SYSTEM_PROMPT_PATH is the canonical in-app route for the System-prompt page,
// nested under the Decisions view so the IA reads parent → detail.
export const SYSTEM_PROMPT_PATH = `${SYSTEM_PROMPT_PARENT}/system-prompt`;

// LEGACY_SYSTEM_PROMPT_REDIRECT is the pre-redesign bookmark that must keep
// resolving to the canonical path.
export const LEGACY_SYSTEM_PROMPT_REDIRECT = "/detect/system-prompt";
