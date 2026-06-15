// test-setup — global vitest setup, wired via `setupFiles` in vitest.config.ts.
//
// happy-dom 20.9.0 (pinned in this repo) exposes `globalThis.localStorage`
// as a BARE object with no getItem/setItem/removeItem/clear/length/key, so
// any test (or component code) that touches localStorage throws a TypeError.
// Real browsers always provide a spec-compliant Storage, so this is a
// test-environment gap only. Installing the Map-backed shim from
// ./lib/test-local-storage here — once, globally — gives EVERY test a
// working localStorage without each test file having to remember to call
// installMemoryLocalStorage() itself.
//
// The shim is re-installed and reset before every test so state never
// leaks between tests. installMemoryLocalStorage() is idempotent (it uses
// Object.defineProperty with configurable: true), so test files that still
// call it at module scope keep working unchanged — they simply replace this
// global instance with their own, which the per-test reset below then
// leaves untouched in those files (they own their own reset()).
import { beforeEach } from "vitest";
import { installMemoryLocalStorage } from "./lib/test-local-storage";

let ls = installMemoryLocalStorage();

beforeEach(() => {
  // Re-install a fresh Map-backed Storage before each test so the global
  // localStorage is always present and empty at the start of every test,
  // regardless of what a prior test wrote.
  ls = installMemoryLocalStorage();
  ls.reset();
});
