// SPDX-License-Identifier: Apache-2.0

import type { ReactNode } from 'react'

// Setup-guide bodies use a small Markdown subset: paragraphs, "- " lists,
// **bold** and `code`. They are rendered as React elements, never as HTML.

function inline(text: string): ReactNode[] {
  const out: ReactNode[] = []
  const re = /(\*\*[^*]+\*\*|`[^`]+`)/g
  let last = 0
  let i = 0
  for (const m of text.matchAll(re)) {
    if (m.index > last) out.push(text.slice(last, m.index))
    const tok = m[0]
    out.push(tok.startsWith('**') ? <strong key={i++}>{tok.slice(2, -2)}</strong> : <code key={i++} className="bg-muted rounded px-1 text-xs">{tok.slice(1, -1)}</code>)
    last = m.index + tok.length
  }
  if (last < text.length) out.push(text.slice(last))
  return out
}

/** GuideText renders a setup-guide step body. */
export function GuideText({ text }: { text: string }) {
  const blocks = text.trim().split(/\n\s*\n/)
  return (
    <div className="grid gap-2 text-sm">
      {blocks.map((b, bi) => {
        const lines = b.split('\n').map((l) => l.trim())
        if (lines.every((l) => l.startsWith('- '))) {
          return (
            <ul key={bi} className="list-disc pl-5">
              {lines.map((l, li) => (
                <li key={li}>{inline(l.slice(2))}</li>
              ))}
            </ul>
          )
        }
        return <p key={bi}>{inline(lines.join(' '))}</p>
      })}
    </div>
  )
}
