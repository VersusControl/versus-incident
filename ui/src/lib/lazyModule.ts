type ReloadStorage = Pick<Storage, "getItem" | "setItem" | "removeItem">;

interface LoadLazyModuleOptions {
  reload?: () => void;
  storage?: ReloadStorage | null;
}

export function isDynamicImportFailure(error: unknown): boolean {
  const message = error instanceof Error ? error.message : String(error);
  return /failed to fetch dynamically imported module|importing a module script failed|error loading dynamically imported module|loading chunk|chunkloaderror/i.test(
    message,
  );
}

export async function loadLazyModule<T>(
  loader: () => Promise<T>,
  name: string,
  options: LoadLazyModuleOptions = {},
): Promise<T> {
  const marker = `versus:lazy-reload:${name}`;
  let storage = options.storage;
  if (storage === undefined) {
    try {
      storage = window.sessionStorage;
    } catch {
      storage = null;
    }
  }

  try {
    const module = await loader();
    try {
      storage?.removeItem(marker);
    } catch {
      // Storage cleanup is best-effort; the imported page is already usable.
    }
    return module;
  } catch (error) {
    if (!isDynamicImportFailure(error) || !storage) throw error;

    try {
      if (storage.getItem(marker) === "1") throw error;
      storage.setItem(marker, "1");
    } catch {
      throw error;
    }

    (options.reload ?? (() => window.location.reload()))();
    return new Promise<T>(() => {});
  }
}