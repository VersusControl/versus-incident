import { describe, expect, it } from "vitest";

// Action approval and direct proposals run only from the Versus DevOps Agent
// CLI; the server refuses browser sessions on these routes. The web app must
// not ship a client for them, so a refused request can never be one click away.
const sources = import.meta.glob(["../**/*.{ts,tsx}", "!../**/*.test.{ts,tsx}"], {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>;

const actRoutes = [
  /\/approvals\/[^"'`\s]*\/approve/,
  /approvals\/\$\{[^}]+\}\/approve/,
  /\/agent\/proposals/,
  /["'`]\/proposals["'`]/,
];

describe("web build has no action approval or proposal client", () => {
  it("scans the production sources", () => {
    expect(Object.keys(sources).length).toBeGreaterThan(50);
    expect(Object.keys(sources)).toContain("../pages/ChatPage.tsx");
  });

  it("never references the approve or direct-proposal routes", () => {
    const offenders = Object.entries(sources).flatMap(([file, text]) =>
      actRoutes.filter((route) => route.test(text)).map((route) => `${file} ${route}`),
    );
    expect(offenders).toEqual([]);
  });

  it("detects a route reference when one is present", () => {
    const sample = 'request(`/api/v1/agent/approvals/${id}/approve`, { method: "POST" })';
    expect(actRoutes.some((route) => route.test(sample))).toBe(true);
    expect(actRoutes.some((route) => route.test('request("/api/v1/agent/proposals")'))).toBe(true);
  });
});

// A browser request carrying the gateway secret as a header is a credential the
// server accepts for approving actions. The web app authenticates with its
// HttpOnly session cookie, so the header may appear in exactly one place: the
// sign-in exchange that trades the secret for that cookie. Whole-line comments
// are skipped; any other mention of the header name counts, so a new caller
// fails here even if it builds the request some other way.
const gatewayHeader = /x-gateway-secret/gi;

function headerMentions(text: string): number {
  return text
    .split("\n")
    .filter((line) => !/^\s*(\/\/|\/\*|\*)/.test(line))
    .reduce((count, line) => count + (line.match(gatewayHeader)?.length ?? 0), 0);
}

function signInExchange(text: string): string {
  return text.match(/export async function signIn\([\s\S]*?\n\}\n/)?.[0] ?? "";
}

describe("web build sends the gateway secret only in the sign-in exchange", () => {
  it("never attaches the gateway-secret header outside sign-in", () => {
    const offenders = Object.entries(sources).flatMap(([file, text]) => {
      const allowed = file === "./api.ts" ? headerMentions(signInExchange(text)) : 0;
      const extra = headerMentions(text) - allowed;
      return extra > 0 ? [`${file} (${extra})`] : [];
    });
    expect(offenders).toEqual([]);
  });

  it("keeps the one sign-in exchange to a single header use", () => {
    const exchange = signInExchange(sources["./api.ts"]);
    expect(exchange).toContain("/api/auth/gateway-session");
    expect(headerMentions(exchange)).toBe(1);
  });

  it("detects a new header use when one is present", () => {
    expect(headerMentions('headers.set("X-Gateway-Secret", value);')).toBe(1);
    expect(headerMentions("const h = { 'x-gateway-secret': token };")).toBe(1);
    expect(headerMentions("// X-Gateway-Secret is never sent")).toBe(0);
  });
});
