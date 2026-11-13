// SPDX-License-Identifier: Apache-2.0

import { defineConfig, devices } from '@playwright/test'
import { proxyFromEnv } from './src/lib/playwright-proxy.ts'

// The browser sign-in of the restore drill on the lab profile (ADR 0109, #518). It drives the web UI that the
// restored koinon-api itself serves (the shipped build, no dev server), so KOINON_DRILL_WEB_URL is the
// address of that API: the drill suite port-forwards it and passes the address in. Run by test/drill, not by
// hand; to try it against any install: KOINON_DRILL_WEB_URL=... KOINON_DRILL_WEB_PASSWORD=... pnpm test:e2e-drill
const url = process.env.KOINON_DRILL_WEB_URL
if (!url) throw new Error('KOINON_DRILL_WEB_URL is not set; the drill suite sets it (test/drill, profile lab)')

// Behind an egress proxy: HTTPS_PROXY/https_proxy and NO_PROXY/no_proxy (docs/dev/ci.md). Playwright ignores
// them itself. The credentials stay in use.proxy; they are not logged (list reporter, no trace).
const proxy = proxyFromEnv(process.env)

// The OIDC spec signs in through the drill's own Dex, which has a cluster name only (KOINON_DRILL_IDP_HOST,
// dex.<ns>.svc.cluster.local:5556). The drill host reaches it through a port-forward (KOINON_DRILL_IDP_ADDR,
// 127.0.0.1:<port>), so Chromium maps the name to it. The rule names the host, the replacement carries the port. The
// proxy bypasses the name too (bypassFromEnv).
const idpHost = process.env.KOINON_DRILL_IDP_HOST?.replace(/:\d+$/, '')
const idpAddr = process.env.KOINON_DRILL_IDP_ADDR
const args = idpHost && idpAddr ? [`--host-resolver-rules=MAP ${idpHost} ${idpAddr}`] : []

export default defineConfig({
  testDir: './e2e-drill',
  outputDir: process.env.PLAYWRIGHT_OUTPUT_DIR ?? 'test-results-drill',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 2 * 60_000,
  expect: { timeout: 30_000 },
  forbidOnly: true,
  // The list reporter only: a JSON report would carry step titles that include fill() values.
  reporter: [['list']],
  // No trace and no video: both record fill() text, and the password is typed into this page.
  use: {
    baseURL: url,
    trace: 'off',
    video: 'off',
    screenshot: 'only-on-failure',
    ...(proxy ? { proxy } : {}),
    ...(args.length ? { launchOptions: { args } } : {}),
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
})
