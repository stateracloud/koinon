// SPDX-License-Identifier: Apache-2.0

import { ChevronsUpDown, Plus, Server } from 'lucide-react'
import { DropdownMenu } from 'radix-ui'
import { Fragment, useState, type ReactNode } from 'react'
import { SpaceMark } from '@/preview/components/identity'
import { Dialog } from '@/preview/components/overlay'
import { usePortal, usePreview } from '@/preview/lib/context'
import { usePreviewNavigate } from '@/preview/lib/navigation'
import { useSpaceMeta } from '@/preview/lib/space-meta'
import { idion, orgs, type Hue } from '@/preview/mock/data'
import type { Space } from '@/preview/shell/nav'

type SpaceEntry = {
  key: string
  name: string
  sub: string
  hue: Hue
  mark: ReactNode
  count?: number
  href: string
  orgId?: string
}

type SpaceSection = { label: string; entries: SpaceEntry[] }

function sections(): SpaceSection[] {
  return [
    { label: 'Personal', entries: [{ key: 'idion', name: idion.name, sub: idion.blurb, hue: idion.hue, mark: idion.initials, count: idion.waiting, href: '/preview/idion' }] },
    {
      label: 'Organizations',
      entries: orgs.map((o) => ({ key: o.id, name: o.name, sub: `${o.role} · ${o.agents} agents`, hue: o.hue, mark: o.initials, count: o.waiting || undefined, href: '/preview/overview', orgId: o.id })),
    },
    { label: 'Administration', entries: [{ key: 'platform', name: 'Platform', sub: 'Platform admins only', hue: 'gray', mark: <Server width={14} height={14} aria-hidden="true" />, href: '/preview/platform/orgs' }] },
  ]
}

function EntryBody({ e }: { e: SpaceEntry }) {
  return (
    <>
      <SpaceMark hue={e.hue}>{e.mark}</SpaceMark>
      <span style={{ display: 'grid', flex: 1, minWidth: 0 }}>
        <span style={{ fontWeight: 500 }}>{e.name}</span>
        <span style={{ fontSize: 12 }}>{e.sub}</span>
      </span>
      {e.count ? (
        <span className="kn-count" aria-label={`${e.count} waiting on you`}>
          {e.count}
        </span>
      ) : null}
    </>
  )
}

function usePick() {
  const { setOrgId } = usePreview()
  const navigate = usePreviewNavigate()
  return (e: { href: string; orgId?: string }) => {
    if (e.orgId) setOrgId(e.orgId)
    navigate(e.href)
  }
}

/**
 * SpaceSwitcher is the sidebar's switch button and its menu: Idion, the orgs, Platform, and join or create. Its open
 * state is the preview's (so G S opens it) unless local is set, for a page that shows several sidebars.
 */
export function SpaceSwitcher({ space, local }: { space: Space; local?: boolean }) {
  const ctx = usePreview()
  const [localOpen, setLocalOpen] = useState(false)
  const switcherOpen = local ? localOpen : ctx.switcherOpen
  const setSwitcherOpen = local ? setLocalOpen : ctx.setSwitcherOpen
  const container = usePortal()
  const meta = useSpaceMeta(space)
  const pick = usePick()
  return (
    <DropdownMenu.Root open={switcherOpen} onOpenChange={setSwitcherOpen} modal={false}>
      <DropdownMenu.Trigger className="sb-switch" aria-label={`Switch space. Current: ${meta.name}`}>
        <SpaceMark hue={meta.hue}>{meta.mark}</SpaceMark>
        <span style={{ display: 'grid', minWidth: 0, flex: 1 }}>
          <span style={{ fontWeight: 500, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{meta.name}</span>
          <span className="ks-meta">{meta.kind}</span>
        </span>
        <ChevronsUpDown className="ks-icon" aria-hidden="true" style={{ color: 'var(--muted-foreground)' }} />
      </DropdownMenu.Trigger>
      <DropdownMenu.Portal container={container}>
        <DropdownMenu.Content className="ks-overlay kp-menu" align="start" sideOffset={6} style={{ width: 300 }} aria-label="Switch space">
          {sections().map((s, i) => (
            <Fragment key={s.label}>
              {i > 0 && <DropdownMenu.Separator className="sb-sep" />}
              <DropdownMenu.Label className="sb-pop-label">{s.label}</DropdownMenu.Label>
              {s.entries.map((e) => (
                <DropdownMenu.Item key={e.key} className="sb-item" onSelect={() => pick(e)}>
                  <EntryBody e={e} />
                </DropdownMenu.Item>
              ))}
            </Fragment>
          ))}
          <DropdownMenu.Separator className="sb-sep" />
          <DropdownMenu.Item className="sb-item" onSelect={() => pick({ href: '/preview/welcome' })}>
            <Plus className="ks-icon" aria-hidden="true" style={{ margin: '0 6px' }} />
            <span>Join or create an org</span>
          </DropdownMenu.Item>
        </DropdownMenu.Content>
      </DropdownMenu.Portal>
    </DropdownMenu.Root>
  )
}

/** SpacesDialog is the phone's "Spaces" sheet: the same list as the switcher, as a dialog. */
export function SpacesDialog() {
  const { spacesDialogOpen, setSpacesDialogOpen } = usePreview()
  const pick = usePick()
  function go(e: { href: string; orgId?: string }) {
    setSpacesDialogOpen(false)
    pick(e)
  }
  return (
    <Dialog open={spacesDialogOpen} onOpenChange={setSpacesDialogOpen} title="Switch space">
      <div className="ks-stack" style={{ gap: 2 }}>
        {sections().map((s) => (
          <Fragment key={s.label}>
            <h3 className="sb-pop-label">{s.label}</h3>
            {s.entries.map((e) => (
              <button key={e.key} type="button" className="sb-item kp-cmdk-item" onClick={() => go(e)}>
                <EntryBody e={e} />
              </button>
            ))}
          </Fragment>
        ))}
        <div className="sb-sep" />
        <button type="button" className="sb-item kp-cmdk-item" onClick={() => go({ href: '/preview/welcome' })}>
          <Plus className="ks-icon" aria-hidden="true" style={{ margin: '0 6px' }} />
          <span>Join or create an org</span>
        </button>
      </div>
    </Dialog>
  )
}
