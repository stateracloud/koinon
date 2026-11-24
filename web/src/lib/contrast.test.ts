// SPDX-License-Identifier: Apache-2.0

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

const css = readFileSync(resolve(process.cwd(), 'src/index.css'), 'utf8')

// ADR 0126 section 1, point 6: every text colour on every ground it uses has a contrast of at least 4.5:1
// (WCAG 2.2 AA); non-text parts (borders, focus ring) at least 3:1. This test reads the tokens from index.css,
// so a change of a token that breaks a pair fails here.

function block(selector: string): Record<string, string> {
  const start = css.indexOf(`${selector} {`)
  if (start < 0) throw new Error(`no ${selector} block in index.css`)
  const body = css.slice(css.indexOf('{', start) + 1, css.indexOf('}', start))
  const out: Record<string, string> = {}
  for (const m of body.matchAll(/--([a-z-]+):\s*(#[0-9a-f]{6});/g)) out[m[1]] = m[2]
  return out
}

const themes = { dark: block(':root'), light: block(':root[data-theme="light"]') } as const

function luminance(hex: string): number {
  const c = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255).map((v) => (v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4))
  return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2]
}

export function contrast(a: string, b: string): number {
  const [x, y] = [luminance(a), luminance(b)]
  return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05)
}

// [text token, grounds it is used on]. `overlay` carries foreground text only: the ADR dark overlay (#555468)
// gives muted text 3.5:1, so a menu or dialog must not put muted or status text on it.
const text: [string, string[]][] = [
  ['foreground', ['background', 'card', 'raised', 'overlay']],
  ['card-foreground', ['card']],
  ['muted-foreground', ['background', 'card', 'raised']],
  ['primary', ['background', 'card', 'raised']],
  ['link', ['background', 'card', 'raised']],
  ['warning', ['background', 'card', 'raised']],
  ['success', ['background', 'card', 'raised']],
  ['danger', ['background', 'card', 'raised']],
  ['info', ['background', 'card', 'raised']],
  ['primary-foreground', ['primary']],
  ['secondary-foreground', ['secondary']],
  ['accent-foreground', ['accent']],
  ['popover-foreground', ['popover']],
  ['attention-foreground', ['attention']],
  ['destructive-foreground', ['destructive']],
]
// Non-text parts: the control border, the focus ring and the logo parts against the grounds they sit on.
const nonText: [string, string[]][] = [
  ['input', ['background', 'card']],
  ['ring', ['background', 'card']],
  ['primary', ['background', 'card']],
  ['logo-gold', ['background', 'card']],
]

describe.each(Object.entries(themes))('%s theme contrast', (_name, t) => {
  for (const [fg, grounds] of text) {
    for (const g of grounds) {
      it(`${fg} on ${g} is at least 4.5:1`, () => {
        expect(t[fg], `${fg} missing`).toBeDefined()
        expect(t[g], `${g} missing`).toBeDefined()
        expect(contrast(t[fg], t[g])).toBeGreaterThanOrEqual(4.5)
      })
    }
  }
  for (const [fg, grounds] of nonText) {
    for (const g of grounds) {
      it(`${fg} on ${g} (non-text) is at least 3:1`, () => {
        expect(contrast(t[fg], t[g])).toBeGreaterThanOrEqual(3)
      })
    }
  }
})

describe('contrast()', () => {
  it('matches the ADR figures', () => {
    expect(contrast('#EAEEFE', '#1C1B29')).toBeCloseTo(14.7, 1)
    expect(contrast('#24233C', '#DDDDF3')).toBeCloseTo(11.4, 1)
    expect(contrast('#000000', '#ffffff')).toBeCloseTo(21, 5)
  })
})
