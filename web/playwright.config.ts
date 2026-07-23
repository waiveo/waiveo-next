import { defineConfig, devices } from "@playwright/test";

// The real-interaction gate (Wave 4b, Task 5). Playwright is DEV-ONLY (Apache-2.0):
// it never enters the production bundle (`vite build`) and adds no external request
// to `dist` — it drives the already-built console the feeder serves.
//
// The full Go stack (feeder + relay, and the SPA embedded in the feeder) is brought
// up and torn down by `make web-e2e`, NOT by Playwright — so there is no `webServer`
// here. The tests point at the feeder's self-signed HTTPS origin; `ignoreHTTPSErrors`
// accepts the dev leaf exactly as the Vite dev proxy's `secure:false` does. One
// worker, no retries: the suite drives ONE shared, stateful stack (create → edit →
// delete a single row), so parallelism or a silent retry would corrupt its own state.
const BASE_URL = process.env.PW_BASE_URL ?? "https://127.0.0.1:7420";

export default defineConfig({
  testDir: "./tests/e2e",
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: !!process.env.CI,
  timeout: 30_000,
  expect: { timeout: 10_000 },
  reporter: [["list"]],
  use: {
    baseURL: BASE_URL,
    ignoreHTTPSErrors: true,
    headless: true,
    // A desktop viewport (>=1024px) so the locked-left nav rail is shown (its links
    // are directly clickable, no drawer) and the data table renders its table view.
    viewport: { width: 1440, height: 900 },
    trace: "off",
    video: "off",
    screenshot: "off",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"], viewport: { width: 1440, height: 900 } } }],
});
