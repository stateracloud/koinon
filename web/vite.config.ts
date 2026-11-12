// SPDX-License-Identifier: Apache-2.0
/// <reference types="vitest/config" />
import { fileURLToPath } from 'node:url'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// The dev server proxies the API so the SPA and the API share one origin, as
// in production where koinon-api serves the built SPA. Start the API on
// KOINON_API_URL (default http://localhost:8080). changeOrigin stays false:
// the API's CSRF check compares Origin with Host, and both stay the dev
// server's.
const api = process.env.KOINON_API_URL ?? 'http://localhost:8080'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  server: {
    proxy: {
      '/auth/': { target: api },
      '/healthz': { target: api },
      // Connect RPCs: /koinon.<pkg>.v1alpha1.<Service>/<Method>
      '^/koinon\\.': { target: api },
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test-setup.ts'],
    include: ['src/**/*.test.{ts,tsx}'],
    // Files share one module registry and one jsdom per worker (issue 41): PR and
    // local runs are faster. The nightly workflow (web-isolated.yml) sets
    // VITEST_ISOLATE=true to run each file in a fresh environment, which catches
    // module state that one file leaks into the next.
    isolate: process.env.VITEST_ISOLATE === 'true',
  },
})
