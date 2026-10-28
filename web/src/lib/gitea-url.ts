// SPDX-License-Identifier: Apache-2.0

const subPathSegment = /^[A-Za-z0-9._~-]+$/
const maxSubPathLength = 100

/**
 * giteaBaseUrlError mirrors the API (connectors.Type.Locate for a self-hosted connector): an https
 * URL with a host, an optional port and an optional sub-path of plain segments, for a Gitea served
 * under one (https://example.com/gitea). Required: Gitea has no default host.
 */
export function giteaBaseUrlError(raw: string, required = true): string | undefined {
  const v = raw.trim()
  if (!v) return required ? 'Server URL is required.' : undefined
  let u: URL
  try {
    u = new URL(v)
  } catch {
    return 'Server URL must be an https URL such as https://git.example.com.'
  }
  if (u.protocol !== 'https:') return 'Server URL must start with https://.'
  if (u.username || u.password || u.search || u.hash || /[?#]/.test(v)) {
    return 'Server URL must be only a host, an optional port and an optional path.'
  }
  const path = u.pathname.replace(/\/$/, '')
  if (path) {
    if (path.length > maxSubPathLength) return `The path of the server URL is longer than ${maxSubPathLength} characters.`
    const bad = path
      .slice(1)
      .split('/')
      .find((seg) => seg === '.' || seg === '..' || !subPathSegment.test(seg))
    // new URL resolves "." and ".." itself, so only characters can still be wrong here.
    if (bad !== undefined) return 'The path of the server URL may use letters, digits, ".", "_", "~" and "-" only.'
  }
  return undefined
}
