// SPDX-License-Identifier: Apache-2.0

import type { PaletteAction } from '@/preview/lib/context'

/** Space is where the person is working: an org, the platform console, or Idion, their own space. */
export type Space = 'org' | 'platform' | 'idion'

export type NavItem = {
  label: string
  /** The preview path; items without one are not designed yet and render as disabled. */
  href?: string
  /** The amber "waiting on you" count. */
  count?: number
  /** The page filters by agent (shows "scoped" while current). */
  scoped?: boolean
}

export type NavGroup = { head?: string; items: NavItem[] }

/** nav is the per-space sidebar, from Sidebar.dc.html. The labels are what pages pass as `current`. */
export const nav: Record<Space, NavGroup[]> = {
  org: [
    {
      items: [
        { label: 'Overview', href: '/preview/overview' },
        { label: 'Inbox', href: '/preview/inbox', count: 7 },
        { label: 'Runs', href: '/preview/runs', scoped: true },
        { label: 'Triggers', href: '/preview/agents/lcm-upgrader', scoped: true },
      ],
    },
    {
      head: 'Build',
      items: [
        { label: 'New agent', href: '/preview/agents/new' },
        { label: 'Marketplace', href: '/preview/marketplace' },
        { label: 'Skills', href: '/preview/skills/helm-upgrade' },
        { label: 'Templates', href: '/preview/agents/new' },
      ],
    },
    { head: 'Spend', items: [{ label: 'Spend & usage', href: '/preview/spend', scoped: true }] },
    {
      head: 'Settings',
      items: [
        { label: 'Providers', href: '/preview/providers' },
        { label: 'Prices', href: '/preview/prices' },
        { label: 'Secrets', href: '/preview/secrets', scoped: true },
        { label: 'Members' },
        { label: 'General' },
      ],
    },
  ],
  platform: [
    {
      items: [
        { label: 'Orgs', href: '/preview/platform/orgs' },
        { label: 'Agents', href: '/preview/platform/agents' },
        { label: 'Usage & cost', href: '/preview/platform/usage' },
      ],
    },
    {
      head: 'Shared with orgs',
      items: [
        { label: 'Providers', href: '/preview/platform/shared?tab=providers' },
        { label: 'Templates', href: '/preview/platform/shared?tab=templates' },
        { label: 'Marketplace', href: '/preview/platform/shared?tab=mcp' },
      ],
    },
    {
      head: 'Install',
      items: [{ label: 'Settings', href: '/preview/platform/settings' }, { label: 'Audit log' }],
    },
  ],
  idion: [
    {
      items: [{ label: 'Home', href: '/preview/idion' }, { label: 'Inbox', href: '/preview/inbox', count: 1 }, { label: 'Memory' }, { label: 'Schedules' }],
    },
    { head: 'Yours', items: [{ label: 'Connections' }, { label: 'Providers' }, { label: 'Budget' }] },
  ],
}

/** goTo is the palette's fixed navigation (Nav-cmdk.dc.html), with the G-sequences PreviewRoot binds. */
export const goTo: PaletteAction[] = [
  { label: 'Inbox', keys: ['G', 'I'], href: '/preview/inbox' },
  { label: 'Overview', keys: ['G', 'O'], href: '/preview/overview' },
  { label: 'Board', keys: ['G', 'B'], href: '/preview/overview?tab=board' },
  { label: 'Prices', keys: ['G', 'P'], href: '/preview/prices' },
  { label: 'Switch to Idion', keys: ['G', 'Y'], href: '/preview/idion' },
]

/** shortcutGroups is the ? sheet (Nav-cmdk.dc.html). */
export const shortcutGroups: { name: string; keys: { what: string; keys: string[] }[] }[] = [
  { name: 'Everywhere', keys: [{ what: 'Command palette', keys: ['⌘', 'K'] }, { what: 'This sheet', keys: ['?'] }, { what: 'Filter the list', keys: ['/'] }, { what: 'Switch space', keys: ['G', 'S'] }] },
  { name: 'Go to', keys: [{ what: 'Overview', keys: ['G', 'O'] }, { what: 'Board', keys: ['G', 'B'] }, { what: 'Inbox', keys: ['G', 'I'] }, { what: 'Prices', keys: ['G', 'P'] }] },
  {
    name: 'Inbox triage',
    keys: [{ what: 'Next, previous', keys: ['J', 'K'] }, { what: 'Accept', keys: ['A'] }, { what: 'Edit', keys: ['E'] }, { what: 'Respond', keys: ['R'] }, { what: 'Snooze', keys: ['H'] }, { what: 'Mark read', keys: ['U'] }],
  },
  { name: 'Run detail', keys: [{ what: 'Summary, Timeline, Raw', keys: ['1', '2', '3'] }, { what: 'Steer', keys: ['S'] }, { what: 'Jump to live line', keys: ['L'] }, { what: 'Copy line link', keys: ['Y'] }] },
]
