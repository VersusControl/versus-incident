import { describe, expect, it } from "vitest";
import { healthDataLabel } from "./serviceHealthPresentation";

describe("healthDataLabel", () => {
  it("translates compound backend labels into readable source names", () => {
    expect(healthDataLabel("Patterns + incidents")).toBe("Log patterns and incident records");
    expect(healthDataLabel("Logs + patterns + incidents")).toBe("Logs and incident records");
    expect(healthDataLabel("Internal data only")).toBe("Service records");
  });
  it("does not expose unknown backend labels", () => {
    expect(healthDataLabel("provider_raw + detail")).toBe("Service records");
  });
});