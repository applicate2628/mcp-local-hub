import { defineConfig } from "@playwright/test";

// Playwright drives real Chromium against a per-test-spawned mcphub gui
// process. The global setup compiles the Go binary once before the run
// so individual tests can spawn it in ~300ms instead of re-running
// `go run` (~5s compile) on each test.
export default defineConfig({
  testDir: "./tests",
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? undefined : 1,
  reporter: process.env.CI ? "github" : [["list"], ["html", { open: "never" }]],
  globalSetup: "./global-setup.ts",
  use: {
    trace: "on-first-retry",
    screenshot: "only-on-failure",
  },
  projects: [
    {
      name: "chromium",
      use: { browserName: "chromium" },
    },
  ],
  timeout: 30_000,
  expect: { timeout: 5_000 },
});
