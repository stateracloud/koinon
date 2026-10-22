// SPDX-License-Identifier: Apache-2.0

// The login endpoints are plain HTTP, not RPCs, because they set and clear
// the httpOnly session cookie (ADR 0022). They are same-origin requests, so
// the API's CrossOriginProtection lets them through.

export type LoginMethods = { password: boolean; oidc: boolean }

export class LoginError extends Error {
  readonly status: number
  constructor(message: string, status: number) {
    super(message)
    this.name = 'LoginError'
    this.status = status
  }
}

async function errorMessage(res: Response, fallback: string): Promise<string> {
  try {
    const body: unknown = await res.json()
    if (body && typeof body === 'object' && 'error' in body && typeof body.error === 'string') {
      return body.error
    }
  } catch {
    // not JSON
  }
  return fallback
}

/** fetchLoginMethods reports which login methods the API has enabled. */
export async function fetchLoginMethods(): Promise<LoginMethods> {
  const res = await fetch('/auth/methods', { headers: { Accept: 'application/json' } })
  if (!res.ok) throw new LoginError(await errorMessage(res, 'could not load login options'), res.status)
  const body = (await res.json()) as Partial<LoginMethods>
  return { password: body.password === true, oidc: body.oidc === true }
}

/** login signs the bootstrap admin in with a password. */
export async function login(username: string, password: string): Promise<{ passwordChangeRequired: boolean }> {
  const res = await fetch('/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify({ username, password }),
  })
  if (!res.ok) throw new LoginError(await errorMessage(res, `login failed (${res.status})`), res.status)
  const body = (await res.json()) as { passwordChangeRequired?: boolean }
  return { passwordChangeRequired: body.passwordChangeRequired === true }
}

/** logout ends the session; the API clears the cookie even on failure. */
export async function logout(): Promise<void> {
  const res = await fetch('/auth/logout', { method: 'POST' })
  if (!res.ok && res.status !== 401) throw new LoginError('logout failed', res.status)
}

/** oidcStartURL begins an SSO login that returns to returnTo afterwards. */
export function oidcStartURL(returnTo: string | undefined): string {
  const safe = safeReturnPath(returnTo)
  return safe === '/' ? '/auth/oidc/start' : `/auth/oidc/start?return_to=${encodeURIComponent(safe)}`
}

/** safeReturnPath keeps redirects on this origin: an absolute path or "/". */
export function safeReturnPath(p: string | undefined): string {
  if (!p || !p.startsWith('/') || p.startsWith('//') || p.startsWith('/\\') || p.startsWith('/login')) return '/'
  return p
}
