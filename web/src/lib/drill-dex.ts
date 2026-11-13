// SPDX-License-Identifier: Apache-2.0

// Names the pages of the restore drill's own OIDC provider (Dex v2.46.1, web/templates), so the browser step fails
// with the page it met instead of a 60 s wait for a heading. The same rules, over the same fixture pages, are in
// test/drill/oidcflow.go (ClassifyDexPage); both read test/drill/testdata/dex. Pure functions: no Playwright, no DOM.

export type DexPageKind = 'login-form' | 'wrong-password' | 'approval' | 'chooser' | 'mfa' | 'error' | 'unknown'

export interface DexPage {
  kind: DexPageKind
  heading: string
  message: string
}

const entities: Record<string, string> = { '&amp;': '&', '&lt;': '<', '&gt;': '>', '&quot;': '"', '&#34;': '"', '&#39;': "'" }

// text strips tags until none is left (one pass leaves "<script" in "<scr<script>ipt>"), then decodes entities once,
// last, so a decoded "&lt;" cannot become a tag that is then missed. One pass decodes "&amp;lt;" to "&lt;".
const text = (markup: string): string => {
  let stripped = markup
  for (let previous = ''; previous !== stripped; ) {
    previous = stripped
    stripped = stripped.replace(/<[^>]*>/g, '')
  }
  return stripped.replace(/&(amp|lt|gt|quot|#34|#39);/g, (entity) => entities[entity] ?? entity).trim()
}

// classifyDexPage tells which Dex page an HTML body is. The order matters: the approval and the MFA pages carry
// buttons and error boxes that look like the chooser and the wrong-password page.
export function classifyDexPage(html: string): DexPage {
  const heading = text(/<h2 class="theme-heading">([\s\S]*?)<\/h2>/.exec(html)?.[1] ?? '')
  const message = text(/<\/h2>\s*<p>([\s\S]*?)<\/p>/.exec(html)?.[1] ?? '')
  const page = (kind: DexPageKind): DexPage => ({ kind, heading, message })
  if (html.includes('id="webauthn-btn"') || html.includes('id="totp"')) return page('mfa')
  if (heading === 'Grant Access') return page('approval')
  if (html.includes('id="login"') && html.includes('id="password"')) {
    return page(html.includes('id="login-error"') ? 'wrong-password' : 'login-form')
  }
  if (html.includes('theme-btn-provider')) return page('chooser')
  if (heading && message) return page('error')
  return page('unknown')
}

// dexPageError is the named error for a page the browser must not meet, or undefined for the login form.
export function dexPageError(page: DexPage, login: string): Error | undefined {
  switch (page.kind) {
    case 'login-form':
      return undefined
    case 'wrong-password':
      return new Error(`dex refused the password for ${login}`)
    case 'approval':
      return new Error('dex asks for approval: skipApprovalScreen is off, or the request forces it')
    case 'chooser':
      return new Error('dex shows a connector chooser: its config has more than one connector')
    case 'mfa':
      return new Error("dex asks for a second factor: the drill's config must not set one")
    case 'error':
      return new Error(`dex error: ${page.heading}: ${page.message}`)
    default:
      return new Error(`dex shows a page the drill does not know (heading "${page.heading}")`)
  }
}

// callbackError is the named error for an answer of Koinon's /auth/oidc/callback that is not a redirect.
export function callbackError(status: number, body: string): Error | undefined {
  if (status < 400) return undefined
  const trimmed = body.trim()
  return new Error(`koinon callback ${status}: ${trimmed.length > 200 ? `${trimmed.slice(0, 200)}...` : trimmed}`)
}
