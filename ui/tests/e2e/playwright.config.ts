import { defineConfig, devices } from "@playwright/test";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import * as dotenv from "dotenv";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

// Load tests/e2e/.env (copied from .env.example) so the base URL and the
// gateway secret come from env, never the repo. CI can inject the same vars
// directly. Nothing here is secret-by-default — the OSS gateway secret is a
// local single-admin credential, and the .env file is gitignored.
dotenv.config({ path: path.resolve(__dirname, ".env") });

// Robustness lesson carried over from the management-platform harness: a bare
// `?? default` does NOT fall back when the var is present-but-empty. Treat an
// empty/whitespace E2E_BASE_URL as unset so a stray `E2E_BASE_URL=` line can't
// produce an empty baseURL (every page.goto would then resolve against "").
function nonEmpty(value: string | undefined, fallback: string): string {
  const v = (value ?? "").trim();
  return v === "" ? fallback : v;
}

// The default targets a locally run binary on :8080. `plans/harness-run/harness.sh e2e ui`
// sets E2E_BASE_URL and credentials for the active harness app (OSS or Enterprise).
const baseURL = nonEmpty(process.env.E2E_BASE_URL, "http://localhost:8080");
const headful = (process.env.E2E_HEADFUL ?? "").toLowerCase() === "true";

// This config drives a REAL running instance like an operator, unless a spec
// intercepts the API itself. It does NOT start a server — bring one up first:
//   • plans/harness-run/harness.sh up oss            (or an enterprise-* scenario)
//   • or locally: build ui/dist, then `go run ./cmd` from versus-incident/
// Review-first: read the spec + README before running against any instance.
export default defineConfig({
  testDir: ".",
  testMatch: "**/*.spec.ts",
  // Local admin surfaces settle quickly, but a cold instance + first API round
  // trip can be slow; give each case room without being generous to a hang.
  timeout: 60_000,
  expect: { timeout: 15_000 },
  // Serial: the intake + report specs mutate a single shared, persisted runtime
  // setting and reload to prove persistence — they must not race each other.
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [["list"], ["html", { open: "never" }]],
  use: {
    baseURL,
    headless: !headful,
    trace: "on-first-retry",
    screenshot: "only-on-failure",
    video: "on-first-retry",
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
