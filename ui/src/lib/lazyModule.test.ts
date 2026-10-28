import { describe, expect, it, vi } from "vitest";
import { isDynamicImportFailure, loadLazyModule } from "./lazyModule";

function memoryStorage() {
  const values = new Map<string, string>();
  return {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => values.set(key, value),
    removeItem: (key: string) => values.delete(key),
  };
}

describe("loadLazyModule", () => {
  it("recognizes stale dynamic-import failures", () => {
    expect(
      isDynamicImportFailure(
        new TypeError("Failed to fetch dynamically imported module: /assets/page-old.js"),
      ),
    ).toBe(true);
    expect(isDynamicImportFailure(new Error("request denied"))).toBe(false);
  });

  it("reloads once when a deployed chunk hash is stale", async () => {
    const storage = memoryStorage();
    const reload = vi.fn();
    void loadLazyModule(
      () => Promise.reject(new TypeError("Failed to fetch dynamically imported module")),
      "DetectDetailPage",
      { reload, storage },
    );
    await Promise.resolve();

    expect(reload).toHaveBeenCalledOnce();
    expect(storage.getItem("versus:lazy-reload:DetectDetailPage")).toBe("1");
  });

  it("does not loop when the chunk still fails after reloading", async () => {
    const storage = memoryStorage();
    storage.setItem("versus:lazy-reload:DetectDetailPage", "1");
    const error = new TypeError("Failed to fetch dynamically imported module");

    await expect(
      loadLazyModule(() => Promise.reject(error), "DetectDetailPage", {
        reload: vi.fn(),
        storage,
      }),
    ).rejects.toBe(error);
  });

  it("clears the reload marker after a successful import", async () => {
    const storage = memoryStorage();
    storage.setItem("versus:lazy-reload:DetectDetailPage", "1");

    await expect(
      loadLazyModule(() => Promise.resolve({ page: true }), "DetectDetailPage", {
        storage,
      }),
    ).resolves.toEqual({ page: true });
    expect(storage.getItem("versus:lazy-reload:DetectDetailPage")).toBeNull();
  });
});