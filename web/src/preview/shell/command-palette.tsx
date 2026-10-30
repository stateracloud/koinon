// SPDX-License-Identifier: Apache-2.0

import { Search } from 'lucide-react'
import { Dialog as DialogPrimitive } from 'radix-ui'
import { Fragment, useMemo, useState, type KeyboardEvent } from 'react'
import { Badge } from '@/preview/components/button'
import { Kbd, Keys } from '@/preview/components/layout'
import { usePortal, usePreview, type PaletteAction } from '@/preview/lib/context'
import { usePreviewNavigate } from '@/preview/lib/navigation'
import { runs } from '@/preview/mock/data'
import { screens } from '@/preview/screens'
import { goTo, shortcutGroups } from '@/preview/shell/nav'

type Group = { heading: string; actions: PaletteAction[] }

const create: PaletteAction[] = [
  { label: 'New agent from a template…', keys: ['C', 'A'], href: '/preview/agents/new' },
  { label: 'Add an MCP server or skill…', keys: ['C', 'M'], href: '/preview/marketplace' },
]

function matches(a: PaletteAction, q: string): boolean {
  return !q || a.label.toLowerCase().includes(q) || (a.hint ?? '').toLowerCase().includes(q)
}

/**
 * CommandPalette is ⌘K (Nav-cmdk.dc.html): actions for the page's selection first, then Create and Go to. Typing
 * filters; a pasted run ID opens that run; any preview screen can be found by name. ↑ ↓ move, ↵ runs, esc closes.
 */
export function CommandPalette() {
  const { paletteOpen, setPaletteOpen } = usePreview()
  const container = usePortal()
  return (
    <DialogPrimitive.Root open={paletteOpen} onOpenChange={setPaletteOpen}>
      <DialogPrimitive.Portal container={container}>
        <DialogPrimitive.Overlay className="kp-scrim" />
        <DialogPrimitive.Content className="ks-overlay kp-dialog" data-size="palette" aria-describedby={undefined}>
          <DialogPrimitive.Title className="ks-sr">Command palette</DialogPrimitive.Title>
          <PaletteBody />
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  )
}

/** PaletteBody holds the query; it unmounts with the dialog, so every opening starts empty. */
function PaletteBody() {
  const { setPaletteOpen, selection } = usePreview()
  const navigate = usePreviewNavigate()
  const [query, setQuery] = useState('')
  const [active, setActive] = useState(0)

  const groups: Group[] = useMemo(() => {
    const q = query.trim().toLowerCase()
    const out: Group[] = []
    if (selection) out.push({ heading: selection.heading, actions: selection.actions.filter((a) => matches(a, q)) })
    const runHits = q.length >= 4 ? runs.filter((r) => r.id.startsWith(q.replace(/^run\s+/, ''))) : []
    if (runHits.length) out.push({ heading: 'Runs', actions: runHits.map((r) => ({ label: `Open run ${r.id}`, hint: `${r.agent} · ${r.task}`, href: `/preview/runs/${r.id}` })) })
    out.push({ heading: 'Create', actions: create.filter((a) => matches(a, q)) })
    out.push({ heading: 'Go to', actions: goTo.filter((a) => matches(a, q)) })
    if (q) out.push({ heading: 'Screens', actions: screens.filter((s) => s.title.toLowerCase().includes(q)).map((s) => ({ label: s.title, href: s.href })) })
    return out.filter((g) => g.actions.length > 0)
  }, [query, selection])

  const flat = groups.flatMap((g) => g.actions)
  const current = Math.min(active, Math.max(flat.length - 1, 0))

  function run(a: PaletteAction) {
    setPaletteOpen(false)
    if (a.onSelect) a.onSelect()
    else if (a.href) navigate(a.href)
  }

  function onKeyDown(e: KeyboardEvent<HTMLInputElement>) {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setActive((current + 1) % Math.max(flat.length, 1))
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      setActive((current - 1 + flat.length) % Math.max(flat.length, 1))
    } else if (e.key === 'Enter') {
      e.preventDefault()
      const a = flat[current]
      if (a) run(a)
    }
  }

  let index = -1
  return (
    <>
          <div className="ks-row" style={{ padding: '12px 16px', borderBottom: '1px solid var(--divider)', flexWrap: 'nowrap' }}>
            <Search className="ks-icon" aria-hidden="true" />
            <label className="ks-sr" htmlFor="kp-cmdk-q">
              Command
            </label>
            <input
              id="kp-cmdk-q"
              type="text"
              role="combobox"
              aria-expanded="true"
              aria-controls="kp-cmdk-list"
              aria-activedescendant={flat.length ? `kp-cmdk-${current}` : undefined}
              autoComplete="off"
              placeholder="Type a command, or paste a run ID or URL"
              value={query}
              onChange={(e) => {
                setQuery(e.target.value)
                setActive(0)
              }}
              onKeyDown={onKeyDown}
              style={{ flex: 1, minWidth: 0, border: 0, background: 'transparent', font: 'inherit', color: 'var(--popover-foreground)', outline: 'none', height: 28 }}
            />
            {selection && <Badge variant="outline">{selection.title}</Badge>}
          </div>
          <div id="kp-cmdk-list" role="listbox" aria-label="Commands" className="kp-cmdk-list">
            {flat.length === 0 && <p className="ks-p" style={{ padding: '8px 8px' }}>No command matches “{query}”.</p>}
            {groups.map((g, gi) => (
              <Fragment key={g.heading}>
                {gi > 0 && <div className="sb-sep" role="presentation" />}
                <div className="sb-pop-label" role="presentation">
                  {g.heading}
                </div>
                {g.actions.map((a) => {
                  index += 1
                  const i = index
                  return (
                    <div
                      key={g.heading + a.label}
                      id={`kp-cmdk-${i}`}
                      role="option"
                      aria-selected={i === current}
                      className="kp-cmdk-item"
                      onMouseMove={() => setActive(i)}
                      onClick={() => run(a)}
                    >
                      <span style={{ flex: 1 }}>{a.label}</span>
                      {a.hint && <span className="kp-hint">{a.hint}</span>}
                      {a.keys && <Keys keys={a.keys} />}
                    </div>
                  )
                })}
              </Fragment>
            ))}
          </div>
          <div className="ks-row" style={{ padding: '8px 16px', borderTop: '1px solid var(--divider)', fontSize: 12 }}>
            <Kbd>↑</Kbd>
            <Kbd>↓</Kbd> move <Kbd>↵</Kbd> run <Kbd>esc</Kbd> close
            <span style={{ marginLeft: 'auto' }}>{selection ? 'Actions for the selection come first' : 'Select a task or run to act on it'}</span>
          </div>
    </>
  )
}

/** ShortcutsSheet is the ? sheet: every keyboard shortcut, by where it works. */
export function ShortcutsSheet() {
  const { shortcutsOpen, setShortcutsOpen } = usePreview()
  const container = usePortal()
  return (
    <DialogPrimitive.Root open={shortcutsOpen} onOpenChange={setShortcutsOpen}>
      <DialogPrimitive.Portal container={container}>
        <DialogPrimitive.Overlay className="kp-scrim" />
        <DialogPrimitive.Content className="ks-overlay kp-dialog ks-stack-4" data-size="palette" style={{ padding: 24 }} aria-describedby={undefined}>
          <div className="ks-between">
            <DialogPrimitive.Title className="ks-h3">Keyboard shortcuts</DialogPrimitive.Title>
            <span style={{ fontSize: 12 }}>
              Press <Kbd>?</Kbd> anywhere
            </span>
          </div>
          <div className="ks-grid-2" style={{ gap: 20 }}>
            {shortcutGroups.map((g) => (
              <div className="ks-stack" key={g.name}>
                <h3 className="ks-h4">{g.name}</h3>
                {g.keys.map((k) => (
                  <div className="ks-between" style={{ fontSize: 13 }} key={k.what}>
                    <span>{k.what}</span>
                    <Keys keys={k.keys} />
                  </div>
                ))}
              </div>
            ))}
          </div>
          <div className="ks-row" style={{ justifyContent: 'flex-end' }}>
            <DialogPrimitive.Close className="kn-btn kn-btn-outline kn-btn-size-sm">Close</DialogPrimitive.Close>
          </div>
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  )
}
