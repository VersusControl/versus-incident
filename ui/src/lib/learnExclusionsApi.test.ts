import { describe, it, expect, afterEach, vi } from "vitest";
import { api } from "@/lib/api";

// Wire-level contract for the BULK service exclusion write. The bulk bar must
// send INTENT (the selected names + the direction) and nothing else: a body
// carrying the resulting policy let a stale page revert a concurrent change,
// and because that body also carried the metric + log-pattern grains, a
// services bulk action could wipe a colleague's metric/log-pattern exclusions.
// The server merges against the stored policy under a per-org lock instead.

function stubFetch(payload: unknown) {
  const spy = vi.fn().mockResolvedValue(
    new Response(JSON.stringify(payload), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    }),
  );
  vi.stubGlobal("fetch", spy);
  return spy;
}

function sentBody(spy: ReturnType<typeof stubFetch>): unknown {
  const init = spy.mock.calls[0][1] as RequestInit;
  return JSON.parse(String(init.body));
}

afterEach(() => vi.unstubAllGlobals());

describe("api.setServiceLearnExclusions (bulk service intent)", () => {
  it("POSTs only the service names and the exclude flag to the batch route", async () => {
    const spy = stubFetch({
      services: ["checkout", "payments"],
      metrics: ["go_*"],
      includes: [],
      log_patterns: ["log:checkout:abc123"],
    });

    await api.setServiceLearnExclusions(["checkout", "payments"], true);

    expect(spy.mock.calls[0][0]).toBe(
      "/enterprise/api/agent/learn-exclusions/services/batch",
    );
    expect((spy.mock.calls[0][1] as RequestInit).method).toBe("POST");
    // Exactly two keys — no `metrics`, no `log_patterns`, no `services`
    // snapshot beyond the selection itself.
    expect(sentBody(spy)).toEqual({
      services: ["checkout", "payments"],
      exclude: true,
    });
  });

  it("carries exclude:false for a bulk resume", async () => {
    const spy = stubFetch({ services: [], metrics: [], log_patterns: [] });
    await api.setServiceLearnExclusions(["checkout"], false);
    expect(sentBody(spy)).toEqual({ services: ["checkout"], exclude: false });
  });

  it("maps the response across the log_patterns ⇄ patterns seam", async () => {
    stubFetch({
      services: ["checkout"],
      metrics: ["up"],
      includes: [],
      log_patterns: ["log:checkout:abc123"],
    });
    await expect(
      api.setServiceLearnExclusions(["checkout"], true),
    ).resolves.toEqual({
      services: ["checkout"],
      metrics: ["up"],
      patterns: ["log:checkout:abc123"],
    });
  });

  it("defaults every grain to an empty list on a sparse response", async () => {
    stubFetch({});
    await expect(
      api.setServiceLearnExclusions(["checkout"], false),
    ).resolves.toEqual({ services: [], metrics: [], patterns: [] });
  });
});
