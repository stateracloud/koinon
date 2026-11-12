// SPDX-License-Identifier: Apache-2.0

import { defineConfig, devices } from '@playwright/test'

// The browser sign-in of the restore drill on the lab profile (ADR 0109, #518). It drives the web UI that the
// restored koinon-api itself serves (the shipped build, no dev server), so KOINON_DRILL_WEB_URL is the
// address of that API: the drill suite port-forwards it and passes the address in. Run by test/drill, not by
// hand; to try it against any install: KOINON_DRILL_WEB_URL=... KOINON_DRILL_WEB_PASSWORD=... pnpm test:e2e-drill
const url = process.env.KOINON_DRILL_WEB_URL
if (!url) throw new Error('KOINON_DRILL_WEB_URL is not set; the drill suite sets it (test/drill, profile lab)')

export default defineConfig({
  testDir: './e2e-drill',
  outputDir: process.env.PLAYWRIGHT_OUTPUT_DIR ?? 'test-results-drill',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 2 * 60_000,
  expect: { timeout: 30_000 },
  forbidOnly: true,
  reporter: [['list'], ['json', { outputFile: process.env.PLAYWRIGHT_JSON_REPORT ?? 'test-results-drill/report.json' }]],
  // No trace and no video: both record fill() text, and the password is typed into this page.
  use: { baseURL: url, trace: 'off', video: 'off', screenshot: 'only-on-failure' },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
})
