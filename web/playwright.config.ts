// SPDX-License-Identifier: Apache-2.0

import { defineConfig, devices } from '@playwright/test'

const port = 5179

// The API is faked inside the browser (e2e/mock-api.ts), so only the Vite
// dev server runs. E1 drives the real koinon-api.
export default defineConfig({
  testDir: './e2e',
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? 'github' : 'list',
  use: { baseURL: `http://localhost:${port}`, trace: 'retain-on-failure' },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    command: `pnpm exec vite --port ${port} --strictPort`,
    url: `http://localhost:${port}`,
    reuseExistingServer: !process.env.CI,
  },
})
