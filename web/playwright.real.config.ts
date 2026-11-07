// SPDX-License-Identifier: Apache-2.0

import { defineConfig, devices } from '@playwright/test'

const port = 5180

// Browser e2e against a REAL Koinon install (old issue 50, plan items W1 and E1): the Vite dev
// server proxies to koinon-api on KOINON_API_URL, which runs with the reconciler, agentd,
// Postgres, OpenShell and the fake model server on kind. Nothing is mocked in the browser.
// Run it with `make test-e2e-web` (test/e2e/run.sh with SUITE=web); the mocked specs in e2e/
// keep running through playwright.config.ts.
if (!process.env.KOINON_API_URL) throw new Error('KOINON_API_URL is not set; run `make test-e2e-web`')

export default defineConfig({
  testDir: './e2e-real',
  outputDir: process.env.PLAYWRIGHT_OUTPUT_DIR ?? 'test-results',
  // One ordered scenario on one install: it changes the bootstrap password, so no retries.
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 15 * 60_000,
  expect: { timeout: 30_000 },
  forbidOnly: !!process.env.CI,
  reporter: process.env.CI ? 'github' : 'list',
  use: { baseURL: `http://localhost:${port}`, // Traces record fill() text (passwords, the fake app's key) and CI uploads test output.
    trace: process.env.CI ? 'off' : 'retain-on-failure', screenshot: 'only-on-failure' },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    command: `pnpm exec vite --port ${port} --strictPort`,
    url: `http://localhost:${port}`,
    reuseExistingServer: false,
    env: { KOINON_API_URL: process.env.KOINON_API_URL },
  },
})
