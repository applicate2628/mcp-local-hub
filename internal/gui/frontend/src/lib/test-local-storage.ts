// test-local-storage — a minimal in-memory Storage shim for unit tests.
//
// The happy-dom version pinned in this repo (20.9.0) exposes
// `globalThis.localStorage` as a BARE object with no getItem/setItem/
// removeItem/clear methods, so any test that drives localStorage directly
// (or any component code that calls it) gets a TypeError. Real browsers
// always provide a spec-compliant Storage, so this is a test-environment
// gap only. installMemoryLocalStorage() replaces the global with a tiny
// Map-backed Storage so tests can exercise the real persistence path.
//
// Returns a reset() the caller can run in beforeEach/afterEach to clear
// state between tests without relying on Storage.clear() being present.
export function installMemoryLocalStorage(): { reset: () => void } {
  const store = new Map<string, string>();
  const mock: Storage = {
    get length() {
      return store.size;
    },
    clear() {
      store.clear();
    },
    getItem(key: string): string | null {
      return store.has(key) ? (store.get(key) as string) : null;
    },
    key(index: number): string | null {
      return Array.from(store.keys())[index] ?? null;
    },
    removeItem(key: string): void {
      store.delete(key);
    },
    setItem(key: string, value: string): void {
      store.set(key, String(value));
    },
  };
  Object.defineProperty(globalThis, "localStorage", {
    value: mock,
    configurable: true,
    writable: true,
  });
  return { reset: () => store.clear() };
}
