// SPDX-License-Identifier: Apache-2.0

import { groups, screens, type Screen } from '@/preview/screens'
import { Badge, Kbd, Logo, PreviewLink, ThemeSwitch } from '@/preview/ui'

function withTheme(href: string, theme: string): string {
  return `${href}${href.includes('?') ? '&' : '?'}theme=${theme}`
}

function ScreenLinks({ s }: { s: Screen }) {
  if (s.phone) {
    return (
      <PreviewLink className="kn-btn kn-btn-outline kn-btn-size-xs" href={`/preview/phone?path=${encodeURIComponent(s.href)}`}>
        Phone frame
      </PreviewLink>
    )
  }
  const first = s.theme ?? 'dark'
  const order = [first, ...(['dark', 'light', 'high-contrast'] as const).filter((t) => t !== first)]
  const label = { dark: 'Dark', light: 'Light', 'high-contrast': 'High contrast' }
  return (
    <div className="kp-index-links">
      {order.map((t, i) => (
        <PreviewLink key={t} className={`kn-btn kn-btn-size-xs ${i === 0 ? 'kn-btn-default' : 'kn-btn-outline'}`} href={withTheme(s.href, t)} aria-label={`${s.title}, ${label[t]}`}>
          {label[t]}
        </PreviewLink>
      ))}
    </div>
  )
}

/** PreviewIndexPage lists every screen of the v2 mockups, by group, with links in each theme. */
export function PreviewIndexPage() {
  return (
    <main className="kp-index">
      <div className="ks-head">
        <div className="ks-stack" style={{ gap: 8 }}>
          <Logo size={28} />
          <h1 className="ks-h1">UI v2 preview</h1>
          <p className="ks-p ks-muted" style={{ maxWidth: '70ch' }}>
            The approved v2 mockups as real screens, with mock data and no API: the visuals and the flow. Every screen sits in the
            same shell. Press <Kbd>⌘K</Kbd> for the command palette and <Kbd>?</Kbd> for the shortcuts.
          </p>
        </div>
        <ThemeSwitch id="kp-index-theme" />
      </div>
      {groups.map((g) => {
        const list = screens.filter((s) => s.group === g.id)
        return (
          <section key={g.id} aria-labelledby={`grp-${g.id}`} className="ks-stack-3">
            <div className="ks-row" style={{ alignItems: 'baseline' }}>
              <h2 id={`grp-${g.id}`} className="ks-h3">
                {g.title}
              </h2>
              <span className="ks-meta ks-mono">{g.file}</span>
            </div>
            <ul className="ks-ul kp-index-grid">
              {list.map((s) => (
                <li key={s.title} className="ks-panel ks-stack-3">
                  <div className="ks-stack" style={{ gap: 4 }}>
                    <PreviewLink className="ks-a" href={s.phone ? `/preview/phone?path=${encodeURIComponent(s.href)}` : s.href} style={{ fontWeight: 600 }}>
                      {s.title}
                    </PreviewLink>
                    <span className="ks-meta ks-mono">{s.href}</span>
                  </div>
                  <div className="ks-between">
                    <ScreenLinks s={s} />
                    <Badge variant="outline">{s.mockups.join(', ')}</Badge>
                  </div>
                </li>
              ))}
            </ul>
          </section>
        )
      })}
    </main>
  )
}
