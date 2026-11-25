// SPDX-License-Identifier: Apache-2.0

import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, relative, resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

// ADR 0126 section 2: components use role tokens, never a raw Tailwind palette colour. oxlint has no rule for a
// class-name pattern, so this test is the lint rule: it fails on any `amber-500`, `green-600`, `red-500/15` and the
// like in the sources (src/index.css holds the only hex values).
const root = resolve(process.cwd(), 'src')
const palette = /\b(?:[a-z-]*:)*(?:bg|text|border(?:-[a-z]{1,2})?|ring(?:-offset)?|fill|stroke|from|to|via|outline|divide|decoration|shadow|accent|caret)-(?:red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose|slate|gray|zinc|neutral|stone)-\d{2,3}\b/

function sources(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const p = join(dir, name)
    if (statSync(p).isDirectory()) return name === 'gen' ? [] : sources(p)
    return /\.(tsx?|jsx?)$/.test(name) && !name.endsWith('.test.ts') && !name.endsWith('.test.tsx') ? [p] : []
  })
}

describe('raw Tailwind palette colours', () => {
  it('detects a raw class', () => {
    expect(palette.test('rounded border-amber-500/60')).toBe(true)
    expect(palette.test('dark:text-green-600')).toBe(true)
    expect(palette.test('border-l-amber-500')).toBe(true)
    expect(palette.test('ring-offset-red-500')).toBe(true)
    expect(palette.test('bg-success/15 text-warning')).toBe(false)
  })
  it('are absent outside index.css', () => {
    const hits = sources(root).filter((f) => palette.test(readFileSync(f, 'utf8'))).map((f) => relative(root, f))
    expect(hits).toEqual([])
  })
})
