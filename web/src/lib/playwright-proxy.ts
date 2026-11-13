// SPDX-License-Identifier: Apache-2.0

// Maps the usual proxy environment onto Playwright's `use.proxy`. Playwright ignores HTTPS_PROXY on its
// own, so a drill behind an egress proxy would fail. Node only (used by playwright.drill.config.ts).

export interface PlaywrightProxy {
  server: string
  username?: string
  password?: string
  bypass: string
}

type Env = Record<string, string | undefined>

const pick = (env: Env, upper: string, lower: string): string => (env[upper] || env[lower] || '').trim()

// proxyFromEnv returns undefined when no proxy is set. The server carries no credentials: they go in
// username and password, URL-decoded. A malformed URL throws an error that never echoes the value, since it
// may hold a password.
export function proxyFromEnv(env: Env): PlaywrightProxy | undefined {
  const raw = pick(env, 'HTTPS_PROXY', 'https_proxy')
  if (!raw) return undefined
  let u: URL
  try {
    u = new URL(raw.includes('://') ? raw : `http://${raw}`)
  } catch {
    throw new Error('HTTPS_PROXY is not a valid URL (value not shown: it may hold credentials)')
  }
  const proxy: PlaywrightProxy = { server: `${u.protocol}//${u.host}`, bypass: bypassFromEnv(env) }
  if (u.username) proxy.username = decodeURIComponent(u.username)
  if (u.password) proxy.password = decodeURIComponent(u.password)
  return proxy
}

// NO_PROXY entries first, then the loopback names the drill's port-forward needs.
export function bypassFromEnv(env: Env): string {
  const entries = pick(env, 'NO_PROXY', 'no_proxy')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
  for (const h of ['127.0.0.1', 'localhost']) if (!entries.includes(h)) entries.push(h)
  return entries.join(',')
}
