// SPDX-License-Identifier: Apache-2.0

import { Logo } from '@/preview/components/identity'
import { Kbd } from '@/preview/components/layout'
import { usePreview } from '@/preview/lib/context'
import { PreviewLink } from '@/preview/lib/link'
import { nav, type Space } from '@/preview/shell/nav'
import { SpaceSwitcher } from '@/preview/shell/spaces'

/**
 * Sidebar is the per-space navigation (Sidebar.dc.html): the logo, the space switcher, the space's groups with the
 * current page as a card, and the ⌘K and ? hints. current is the nav label of the page, such as "Overview".
 * localSwitcher gives this sidebar its own switcher state (for pages that show several sidebars).
 */
export function Sidebar({ space, current, localSwitcher }: { space: Space; current?: string; localSwitcher?: boolean }) {
  const { setPaletteOpen, setShortcutsOpen } = usePreview()
  return (
    <nav className="sb" aria-label="Main">
      <div style={{ padding: '0 8px' }}>
        <PreviewLink href={space === 'idion' ? '/preview/idion' : space === 'platform' ? '/preview/platform/orgs' : '/preview/overview'} className="ks-a" aria-label="Home">
          <Logo product={space === 'idion' ? 'idion' : 'koinon'} size={22} />
        </PreviewLink>
      </div>
      <SpaceSwitcher space={space} local={localSwitcher} />
      {nav[space].map((g, gi) => (
        <div className="sb-nav" key={g.head ?? gi}>
          {g.head && <div className="sb-head">{g.head}</div>}
          {g.items.map((item) => {
            const cur = item.label === current
            const body = (
              <>
                <span style={{ flex: 1 }}>{item.label}</span>
                {item.count ? (
                  <span className="kn-count" aria-label={`${item.count} waiting on you`}>
                    {item.count}
                  </span>
                ) : null}
                {item.scoped && cur && (
                  <span className="ks-meta" title="Filters by agent">
                    scoped
                  </span>
                )}
              </>
            )
            if (!item.href) {
              return (
                <span key={item.label} className="sb-a" aria-disabled="true" title="Not in the v2 mockups yet">
                  {body}
                </span>
              )
            }
            return (
              <PreviewLink key={item.label} className="sb-a" href={item.href} aria-current={cur ? 'page' : undefined}>
                {body}
              </PreviewLink>
            )
          })}
        </div>
      ))}
      <div className="sb-foot">
        <button type="button" className="ks-row" style={{ gap: 4, background: 'transparent', border: 0, color: 'inherit', font: 'inherit' }} onClick={() => setPaletteOpen(true)}>
          <Kbd>⌘K</Kbd> Commands
        </button>
        <button type="button" className="ks-row" style={{ gap: 4, marginLeft: 'auto', background: 'transparent', border: 0, color: 'inherit', font: 'inherit' }} onClick={() => setShortcutsOpen(true)}>
          <Kbd>?</Kbd> Shortcuts
        </button>
      </div>
    </nav>
  )
}
