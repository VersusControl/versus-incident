import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

const here = path.dirname(fileURLToPath(import.meta.url));
const css = readFileSync(path.resolve(here, "../index.css"), "utf8");

describe("card text wrapping", () => {
  it("lets card columns shrink and wraps long unbroken values", () => {
    const bodyRule = css.match(/\.card-body\s*\{([^}]*)\}/)?.[1] ?? "";
    const childRule = css.match(/\.card-body\s*>\s*\*\s*\{([^}]*)\}/)?.[1] ?? "";

    expect(bodyRule).toContain("min-width: 0");
    expect(bodyRule).toContain("overflow-wrap: anywhere");
    expect(childRule).toContain("min-width: 0");
  });

  it("keeps modal and peek content inside the overlay viewport", () => {
    const bodyRule = css.match(/\.overlay-body\s*\{([^}]*)\}/)?.[1] ?? "";
    const preRule = css.match(/\.overlay-body\s+pre\s*\{([^}]*)\}/)?.[1] ?? "";

    expect(bodyRule).toContain("max-width: 100%");
    expect(bodyRule).toContain("overflow-x: hidden");
    expect(bodyRule).toContain("overflow-wrap: anywhere");
    expect(preRule).toContain("white-space: pre-wrap");
    expect(preRule).toContain("overflow-wrap: anywhere");
  });

  it("defines the product max-width scale in shared CSS", () => {
    const lg = css.match(/\.max-w-lg\s*\{([^}]*)\}/)?.[1] ?? "";
    const xl = css.match(/\.max-w-xl\s*\{([^}]*)\}/)?.[1] ?? "";
    const xxl = css.match(/\.max-w-2xl\s*\{([^}]*)\}/)?.[1] ?? "";

    expect(lg).toContain("max-width: 720px");
    expect(xl).toContain("max-width: 960px");
    expect(xxl).toContain("max-width: 1200px");
  });
});