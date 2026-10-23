// Shared capture-path helper for the Playwright suites (binding standard:
// plans/harness-run/README.md, "Task runs, state and images").
//
// Inside `plans/harness-run/run.sh exec <task> <suite> -- ...` the harness
// exports HARNESS_IMAGES_DIR / HARNESS_RESULTS_DIR. Run outside run.sh, every
// path falls back to the ignored `_adhoc` folders under plans/harness-run,
// never to /tmp, the repo root or `ui/test-results`.
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
// <workspace>/versus-incident/ui/tests/e2e/support -> <workspace>
const workspaceRoot = path.resolve(here, "..", "..", "..", "..", "..");
const harnessRoot = path.join(workspaceRoot, "plans", "harness-run");

function envOr(name: string, fallback: string): string {
  const v = (process.env[name] ?? "").trim();
  return v === "" ? fallback : path.resolve(v);
}

/** Working-capture folder: `$HARNESS_IMAGES_DIR` or `.images/_adhoc`. */
export function harnessImagesDir(): string {
  return envOr("HARNESS_IMAGES_DIR", path.join(harnessRoot, ".images", "_adhoc"));
}

/** Machine-output folder: `$HARNESS_RESULTS_DIR` or `.state/_adhoc/results`. */
export function harnessResultsDir(): string {
  return envOr("HARNESS_RESULTS_DIR", path.join(harnessRoot, ".state", "_adhoc", "results"));
}

/** Live stack state: `$HARNESS_STATE_DIR` or `.state/_runtime` (what `harness.sh up` writes). */
export function harnessRuntimeStateDir(): string {
  return envOr("HARNESS_STATE_DIR", path.join(harnessRoot, ".state", "_runtime"));
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
 * Example: capturePath("k14-workload-logs", 1440, "dark", "all-pods").
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
