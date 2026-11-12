// SPDX-License-Identifier: Apache-2.0

import { defineConfig, devices } from '@playwright/test'

const port = 5179

// The API is faked inside the browser (e2e/mock-api.ts), so only a static web server runs. E1 drives
// the real koinon-api. By default that is the Vite dev server (`pnpm test:e2e`). With
// KOINON_PW_PREVIEW=1 (CI) it serves the production build instead: run `pnpm exec vite build` first,
// and the build happens once for all workers. Specs share no state (every page mocks its own routes),
// so they run on 2 workers in CI.
const preview = !!process.env.KOINON_PW_PREVIEW
export default defineConfig({
  testDir: './e2e',
  forbidOnly: !!process.env.CI,
  // No automatic retry (ADR 0111, rule T1): a failed spec fails the run.
  retries: 0,
  workers: process.env.CI ? 2 : undefined,
  reporter: process.env.CI ? 'github' : 'list',
  use: { baseURL: `http://localhost:${port}`, trace: 'retain-on-failure' },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    command: `pnpm exec vite ${preview ? 'preview' : ''} --port ${port} --strictPort`,
    url: `http://localhost:${port}`,
    reuseExistingServer: !process.env.CI,
  },
})
