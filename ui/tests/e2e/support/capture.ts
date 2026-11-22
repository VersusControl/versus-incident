// Shared harness-path and capture-path helpers for the Playwright suites.
//
// The local test harness exports HARNESS_DIR when it launches Playwright, so
// specs find its scripts and fixtures without a hardcoded location. A spec that
// needs one skips with HARNESS_DIR_MISSING when the variable is not set.
//
// When the harness wraps a recorded run it also exports HARNESS_IMAGES_DIR /
// HARNESS_RESULTS_DIR, and every capture lands in that run's folders. Under the
// harness without a recorded run, paths fall back to the harness's ignored
// `_adhoc` folders. Without the harness at all, they fall back to the ignored
// `ui/test-results` folder; never to /tmp or the repo root.
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
// <ui>/tests/e2e/support -> <ui>
const uiRoot = path.resolve(here, "..", "..", "..");
const localOutput = path.join(uiRoot, "test-results");

/** Neutral skip reason for a spec that needs the harness directory. */
export const HARNESS_DIR_MISSING = "harness directory not set";

/** The harness directory exported as HARNESS_DIR, or "" when it is not set. */
export function harnessDir(): string {
  const v = (process.env.HARNESS_DIR ?? "").trim();
  return v === "" ? "" : path.resolve(v);
}

/** A path inside the harness directory. Skip with HARNESS_DIR_MISSING first. */
export function harnessPath(...parts: string[]): string {
  const dir = harnessDir();
  if (dir === "") throw new Error(HARNESS_DIR_MISSING);
  return path.join(dir, ...parts);
}

/** Fallback below the harness directory, or below `ui/test-results` without it. */
function fallback(harnessRelative: string[], local: string): string {
  const dir = harnessDir();
  return dir === "" ? path.join(localOutput, local) : path.join(dir, ...harnessRelative);
}

function envOr(name: string, fallback: string): string {
  const v = (process.env[name] ?? "").trim();
  return v === "" ? fallback : path.resolve(v);
}

/** Working-capture folder: `$HARNESS_IMAGES_DIR` or the harness's `.images/_adhoc`. */
export function harnessImagesDir(): string {
  return envOr("HARNESS_IMAGES_DIR", fallback([".images", "_adhoc"], "images"));
}

/** Machine-output folder: `$HARNESS_RESULTS_DIR` or the harness's `.state/_adhoc/results`. */
export function harnessResultsDir(): string {
  return envOr("HARNESS_RESULTS_DIR", fallback([".state", "_adhoc", "results"], "results"));
}

/** Live stack state: `$HARNESS_STATE_DIR` or the harness's `.state/_runtime` (what `harness.sh up` writes). */
export function harnessRuntimeStateDir(): string {
  return envOr("HARNESS_STATE_DIR", fallback([".state", "_runtime"], "runtime"));
}

/** Where captures are written: `$CAPTURE_DIR`, defaulting to the images dir. */
export function captureDir(): string {
  return envOr("CAPTURE_DIR", harnessImagesDir());
}

function part(label: string, value: string | number): string {
  const s = String(value)
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
  if (s === "") throw new Error(`capturePath: ${label} must not be empty`);
  return s;
}

/**
 * `$CAPTURE_DIR/<surface>-<viewport>-<theme>-<state>.png`, creating the dir.
 * Example: capturePath("kubernetes-workload-logs", 1440, "dark", "all-pods").
 */
export function capturePath(
  surface: string,
  viewport: string | number,
  theme: string,
  state: string,
): string {
  const dir = captureDir();
  fs.mkdirSync(dir, { recursive: true });
  return path.join(
    dir,
    `${part("surface", surface)}-${part("viewport", viewport)}-${part("theme", theme)}-${part("state", state)}.png`,
  );
}
