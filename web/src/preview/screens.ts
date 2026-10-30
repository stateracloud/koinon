// SPDX-License-Identifier: Apache-2.0

import type { Theme } from '@/preview/lib/context'

/** ScreenGroup is one parallel work stream; each owns its page file under src/preview/pages/. */
export type ScreenGroup = 'shell' | 'overview' | 'run' | 'agents' | 'marketplace' | 'providers' | 'platform' | 'onboarding'

export type Screen = {
  /** The mockup board's title in canvas.json. */
  title: string
  /** The mockup file(s) this screen is built from. */
  mockups: string[]
  group: ScreenGroup
  /** The preview path to open, with a query for a state (such as ?tab=board). */
  href: string
  /** The theme the mockup board shows; the index links that theme first. */
  theme?: Theme
  /** Shown in a 390 × 844 phone frame. */
  phone?: boolean
}

export const groups: { id: ScreenGroup; title: string; file: string }[] = [
  { id: 'shell', title: 'Navigation', file: 'src/preview/pages/shell-screens.tsx' },
  { id: 'onboarding', title: 'First login and Idion', file: 'src/preview/pages/onboarding.tsx' },
  { id: 'overview', title: 'Org overview and inbox', file: 'src/preview/pages/overview.tsx' },
  { id: 'run', title: 'Run detail', file: 'src/preview/pages/run.tsx' },
  { id: 'agents', title: 'New agent and the agent page', file: 'src/preview/pages/agents.tsx' },
  { id: 'marketplace', title: 'Marketplace and skills', file: 'src/preview/pages/marketplace.tsx' },
  { id: 'providers', title: 'Providers, prices, secrets and spend', file: 'src/preview/pages/providers.tsx' },
  { id: 'platform', title: 'Platform console', file: 'src/preview/pages/platform.tsx' },
]

/** screens lists every mockup board, in the canvas order, with where it lives in the preview. */
export const screens: Screen[] = [
  { group: 'shell', title: '1a · Org sidebar (shared by every page)', mockups: ['Sidebar.dc.html'], href: '/preview/sidebar' },
  { group: 'shell', title: '1b · Space switcher open, Runs with scope as a filter', mockups: ['Nav-switcher.dc.html'], href: '/preview/runs?switcher=open' },
  { group: 'shell', title: '1c · Cmd-K acting on a selection, and the ? sheet', mockups: ['Nav-cmdk.dc.html'], href: '/preview/commands' },
  { group: 'onboarding', title: '2 · First login: join, create or ask, then a spend limit', mockups: ['FirstLogin.dc.html'], href: '/preview/welcome' },
  { group: 'overview', title: '3a · Org overview, dark', mockups: ['Main.dc.html'], href: '/preview/overview', theme: 'dark' },
  { group: 'overview', title: '3b · Org overview, light (L2)', mockups: ['Overview-light.dc.html'], href: '/preview/overview', theme: 'light' },
  { group: 'overview', title: '3c · Org overview, high contrast', mockups: ['Overview-hc.dc.html'], href: '/preview/overview', theme: 'high-contrast' },
  { group: 'overview', title: '3d · Org overview, phone', mockups: ['Overview-phone.dc.html'], href: '/preview/overview', phone: true },
  { group: 'run', title: '4a · Run detail, dark', mockups: ['Run.dc.html'], href: '/preview/runs/9c41d07a', theme: 'dark' },
  { group: 'run', title: '4b · Run detail, light (L2)', mockups: ['Run-light.dc.html'], href: '/preview/runs/9c41d07a', theme: 'light' },
  { group: 'overview', title: '5 · Inbox: typed cards, triage keys, scoped plan approval', mockups: ['Inbox.dc.html'], href: '/preview/inbox' },
  { group: 'agents', title: '6a · New agent: template gallery', mockups: ['NewAgent-gallery.dc.html'], href: '/preview/agents/new' },
  { group: 'agents', title: '6b · New agent: template detail', mockups: ['NewAgent-template.dc.html'], href: '/preview/agents/new/template' },
  { group: 'agents', title: '6c · New agent: blank form', mockups: ['NewAgent-blank.dc.html'], href: '/preview/agents/new/blank' },
  { group: 'agents', title: '7a · Agent page: model, triggers, skills, versions', mockups: ['Agent.dc.html'], href: '/preview/agents/lcm-upgrader' },
  { group: 'agents', title: '7b · Instant rollback: verify and reason', mockups: ['Agent-rollback.dc.html'], href: '/preview/agents/lcm-upgrader/rollback' },
  { group: 'marketplace', title: '8a · Marketplace, dark', mockups: ['Marketplace.dc.html'], href: '/preview/marketplace', theme: 'dark' },
  { group: 'marketplace', title: '8b · Marketplace, light (L2)', mockups: ['Marketplace-light.dc.html'], href: '/preview/marketplace', theme: 'light' },
  { group: 'marketplace', title: '8c · Add skill: one line, upload, paste, tree editor', mockups: ['Skill-add.dc.html'], href: '/preview/skills/add' },
  { group: 'marketplace', title: '8d · Skill detail', mockups: ['Skill-detail.dc.html'], href: '/preview/skills/helm-upgrade' },
  { group: 'providers', title: '9a · Providers, with the test result', mockups: ['Providers.dc.html'], href: '/preview/providers' },
  { group: 'providers', title: '9b · Add provider: credential, test, discover', mockups: ['Provider-add.dc.html'], href: '/preview/providers/add' },
  { group: 'providers', title: '9c · Prices with its drawer', mockups: ['Prices.dc.html'], href: '/preview/prices' },
  { group: 'providers', title: '9d · Secrets, write-only', mockups: ['Secrets.dc.html'], href: '/preview/secrets' },
  { group: 'providers', title: '10 · Spend and usage', mockups: ['Spend.dc.html'], href: '/preview/spend' },
  { group: 'platform', title: '11a · Platform: Orgs, archive and delete', mockups: ['Platform-orgs.dc.html'], href: '/preview/platform/orgs' },
  { group: 'platform', title: '11b · Platform: Usage & cost', mockups: ['Platform-usage.dc.html'], href: '/preview/platform/usage' },
  { group: 'platform', title: '11c · Platform: Shared with orgs', mockups: ['Platform-shared.dc.html'], href: '/preview/platform/shared' },
  { group: 'platform', title: '11d · Platform: Settings', mockups: ['Platform-settings.dc.html'], href: '/preview/platform/settings' },
  { group: 'platform', title: '11e · Platform: agent inventory', mockups: ['Platform-agents.dc.html'], href: '/preview/platform/agents' },
  { group: 'onboarding', title: '12a · Idion: meet Daimon', mockups: ['Idion-firstrun.dc.html'], href: '/preview/idion/welcome' },
  { group: 'onboarding', title: '12b · Idion home', mockups: ['Idion-home.dc.html'], href: '/preview/idion' },
]
