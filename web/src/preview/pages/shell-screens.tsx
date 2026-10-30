// SPDX-License-Identifier: Apache-2.0

// Group "shell": the navigation screens (Sidebar.dc.html, Nav-switcher.dc.html, Nav-cmdk.dc.html) and the phone frame.

import { useEffect, useMemo, useState } from 'react'
import { useRouterState } from '@tanstack/react-router'
import { agents, runDot, runs, type Run } from '@/preview/mock/data'
import { Sidebar } from '@/preview/shell/sidebar'
import {
  AgeChip,
  AgentAvatar,
  Button,
  FilterBar,
  Kbd,
  NativeSelect,
  PageHeader,
  Panel,
  Pill,
  PreviewLink,
  PreviewShell,
  Segmented,
  StateDot,
  Table,
  ThemeSwitch,
  usePaletteSelection,
  usePreview,
  type PaletteSelection,
} from '@/preview/ui'

/** SidebarScreen is mockup 1a: the sidebar of each space, side by side. */
export function SidebarScreen() {
  const spaces = [
    { space: 'org' as const, current: 'Overview', title: 'Org' },
    { space: 'platform' as const, current: 'Orgs', title: 'Platform' },
    { space: 'idion' as const, current: 'Home', title: 'Idion' },
  ]
  return (
    <div className="kp-index">
      <PageHeader
        title="Sidebar"
        description="The same sidebar on every page, one per space. Open a switcher to change space; the current page is a card."
        actions={
          <PreviewLink className="ks-link" href="/preview">
            Every screen
          </PreviewLink>
        }
      />
      <div className="ks-row" style={{ alignItems: 'stretch', gap: 24 }}>
        {spaces.map((s) => (
          <section key={s.space} aria-label={`${s.title} sidebar`} className="ks-stack">
            <h2 className="ks-label">{s.title}</h2>
            <div style={{ width: 240, height: 760, border: '1px solid var(--border)', borderRadius: 'var(--radius-lg)', overflow: 'hidden' }}>
              <Sidebar space={s.space} current={s.current} localSwitcher />
            </div>
          </section>
        ))}
      </div>
    </div>
  )
}

function runSelection(r: Run): PaletteSelection {
  const agent = agents.find((a) => a.name === r.agent)
  return {
    key: r.id,
    title: `Run ${r.id}`,
    heading: 'For the selected run',
    actions: [
      { label: 'Open the run', hint: r.id, keys: ['O'], href: `/preview/runs/${r.id}` },
      { label: 'Steer the run…', hint: 'about $0.02 a steer', keys: ['S'], href: `/preview/runs/${r.id}?steer=1` },
      { label: `Pause ${r.agent}`, hint: 'stops new runs', keys: ['P'], href: `/preview/agents/${agent?.id ?? 'lcm-upgrader'}` },
      { label: 'Open its spend, filtered', keys: ['$'], href: '/preview/spend' },
    ],
  }
}

/**
 * RunsPage is mockup 1b: one Runs page for the whole org, with the scope as a filter (pick an agent; the page and
 * its URL stay the same). Click a row to select it; ⌘K then acts on that run. ?switcher=open opens the switcher.
 */
export function RunsPage() {
  const [scope, setScope] = useState<'org' | 'agent'>('agent')
  const [agentName, setAgentName] = useState('LCM Upgrader')
  const [since, setSince] = useState(true)
  const [view, setView] = useState<'' | 'failed' | 'expensive'>('')
  const [query, setQuery] = useState('')
  const [selected, setSelected] = useState<string | null>(null)

  const rows = useMemo(() => {
    let out = scope === 'agent' ? runs.filter((r) => r.agent === agentName) : runs
    if (view === 'failed') out = out.filter((r) => r.state === 'failed')
    if (view === 'expensive') out = [...out].sort((a, b) => parseFloat(b.cost.slice(1)) - parseFloat(a.cost.slice(1)))
    const q = query.trim().toLowerCase()
    if (q) out = out.filter((r) => `${r.id} ${r.task} ${r.state} ${r.source} ${r.agent}`.toLowerCase().includes(q.replace(/^[a-z]+:/, '')))
    return out
  }, [scope, agentName, view, query])

  const total = rows.reduce((s, r) => s + parseFloat(r.cost.slice(1)), 0)
  const agentCount = new Set(rows.map((r) => r.agent)).size
  const summary =
    scope === 'agent'
      ? `${rows.length} runs of ${agentName} in the last 7 days · $${total.toFixed(2)}`
      : `${rows.length} runs in the last 7 days · $${total.toFixed(2)} · ${agentCount} agents`

  const sel = rows.find((r) => r.id === selected) ?? null
  usePaletteSelection(sel ? runSelection(sel) : null)

  const chips = [
    ...(scope === 'agent' ? [{ key: 'agent', value: agentName.toLowerCase().replace(/\s+/g, '-'), onRemove: () => setScope('org') }] : []),
    ...(since ? [{ key: 'since', value: '7d', onRemove: () => setSince(false) }] : []),
  ]

  return (
    <PreviewShell space="org" current="Runs" crumbs={scope === 'agent' ? [{ label: 'Runs' }, { label: agentName }] : [{ label: 'Runs' }]}>
      <PageHeader title="Runs" description="One page for the whole org. Pick an agent to filter it; the page and its URL stay the same." />
      <Panel className="ks-stack-3">
        <div className="ks-row">
          <span className="ks-label">Scope</span>
          <Segmented
            label="Scope"
            value={scope}
            onChange={setScope}
            options={[
              { id: 'org', label: 'Whole org' },
              { id: 'agent', label: 'One agent' },
            ]}
          />
          {scope === 'agent' && (
            <>
              <label className="ks-sr" htmlFor="scope-agent">
                Agent
              </label>
              <NativeSelect id="scope-agent" style={{ width: 220 }} value={agentName} onChange={(e) => setAgentName(e.target.value)}>
                {agents.map((a) => (
                  <option key={a.id}>{a.name}</option>
                ))}
              </NativeSelect>
            </>
          )}
          <span className="ks-meta" style={{ marginLeft: 'auto' }}>
            The same filter works on Triggers, Spend &amp; usage and Secrets.
          </span>
        </div>
        <div className="ks-row">
          <FilterBar id="runs-q" label="Filter runs" chips={chips} placeholder="state: source: cost:>1" value={query} onChange={setQuery} />
          <div className="ks-views">
            <span className="ks-meta">Views</span>
            <Pill pressed={view === 'failed'} onClick={() => setView(view === 'failed' ? '' : 'failed')}>
              Failed this week
            </Pill>
            <Pill pressed={view === 'expensive'} onClick={() => setView(view === 'expensive' ? '' : 'expensive')}>
              Most expensive
            </Pill>
          </div>
        </div>
      </Panel>
      <Table label="Runs">
        <thead>
          <tr>
            <th scope="col">Run</th>
            <th scope="col">Agent</th>
            <th scope="col">Task</th>
            <th scope="col">State</th>
            <th scope="col">Source</th>
            <th scope="col">Started</th>
            <th scope="col" className="ks-r">
              Duration
            </th>
            <th scope="col" className="ks-r">
              Tokens
            </th>
            <th scope="col" className="ks-r">
              Cost
            </th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.id} aria-selected={r.id === selected} onClick={() => setSelected(r.id)} onFocus={() => setSelected(r.id)}>
              <td>
                <PreviewLink className="ks-a ks-mono" href={`/preview/runs/${r.id}`}>
                  {r.id}
                </PreviewLink>
                {r.parent && <span className="ks-meta"> · sub-run of {r.parent}</span>}
              </td>
              <td>
                <span className="ks-row" style={{ gap: 6, flexWrap: 'nowrap' }}>
                  <AgentAvatar name={r.agent} decorative />
                  {r.agent}
                </span>
              </td>
              <td>{r.task}</td>
              <td>
                <span className="ks-row" style={{ gap: 6, flexWrap: 'nowrap' }}>
                  <StateDot state={runDot[r.state]} />
                  {r.state}
                </span>
              </td>
              <td className="ks-muted">{r.source}</td>
              <td className="ks-muted ks-num">{r.started}</td>
              <td className="ks-r">{r.duration}</td>
              <td className="ks-r">{r.tokens}</td>
              <td className="ks-r">{r.cost}</td>
            </tr>
          ))}
          {rows.length === 0 && (
            <tr>
              <td colSpan={9} className="ks-muted">
                No runs match this filter.
              </td>
            </tr>
          )}
        </tbody>
      </Table>
      <p className="ks-p ks-meta ks-num">
        {summary}
        {sel ? (
          <>
            {' '}
            · Run <span className="ks-mono">{sel.id}</span> selected: press <Kbd>⌘K</Kbd> to act on it.
          </>
        ) : (
          ' · Select a row, then press ⌘K to act on it.'
        )}
      </p>
    </PreviewShell>
  )
}

type DemoTile = { id: string; title: string; agent: string; state: 'blocked' | 'working'; days?: number; band?: '1d' | '3d' | '7d' }

const demoTiles: DemoTile[] = [
  { id: 'merge', title: 'Merge ingress-nginx 1.14 PR', agent: 'LCM Upgrader', state: 'blocked', days: 9, band: '7d' },
  { id: 'gpu', title: 'Which cost center owns the idle GPU nodes?', agent: 'FinOps Analyst', state: 'blocked', days: 4, band: '3d' },
  { id: 'cert', title: 'Upgrade cert-manager to 1.19', agent: 'LCM Upgrader', state: 'working' },
]

function tileSelection(t: DemoTile): PaletteSelection {
  if (t.id === 'merge') {
    return {
      key: t.id,
      title: t.title,
      heading: 'For the selected task',
      actions: [
        { label: 'Review and approve the merge', hint: 'opens the approval', keys: ['A'], href: '/preview/inbox' },
        { label: 'Open the run', hint: 'a71c0e44', keys: ['O'], href: '/preview/runs/a71c0e44' },
        { label: 'Steer the run…', hint: 'about $0.02 a steer', keys: ['S'], href: '/preview/runs/a71c0e44?steer=1' },
        { label: 'Snooze until tomorrow', keys: ['H'] },
        { label: 'Pause LCM Upgrader', hint: 'stops new runs', keys: ['P'], href: '/preview/agents/lcm-upgrader' },
        { label: 'Open its spend, filtered', keys: ['$'], href: '/preview/spend' },
      ],
    }
  }
  const agent = agents.find((a) => a.name === t.agent)
  return {
    key: t.id,
    title: t.title,
    heading: 'For the selected task',
    actions: [
      t.state === 'blocked' ? { label: 'Answer in the inbox', keys: ['R'], href: '/preview/inbox' } : { label: 'Open the run', hint: '9c41d07a', keys: ['O'], href: '/preview/runs/9c41d07a' },
      { label: 'Snooze until tomorrow', keys: ['H'] },
      { label: `Pause ${t.agent}`, hint: 'stops new runs', keys: ['P'], href: `/preview/agents/${agent?.id ?? 'lcm-upgrader'}` },
      { label: 'Open its spend, filtered', keys: ['$'], href: '/preview/spend' },
    ],
  }
}

/**
 * CommandsPage is mockup 1c: a board with a selected task and ⌘K acting on it (the palette opens on arrival), and
 * the ? sheet. Click a tile to change the selection.
 */
export function CommandsPage() {
  const { setPaletteOpen, setShortcutsOpen } = usePreview()
  const [selected, setSelected] = useState('merge')
  const tile = demoTiles.find((t) => t.id === selected) ?? demoTiles[0]
  usePaletteSelection(tileSelection(tile))
  useEffect(() => {
    setPaletteOpen(true)
  }, [setPaletteOpen])

  const columns: { key: 'blocked' | 'working'; label: string; count: number }[] = [
    { key: 'blocked', label: 'Blocked on you', count: 4 },
    { key: 'working', label: 'Working', count: 2 },
  ]
  return (
    <PreviewShell space="org" current="Overview" crumbs={[{ label: 'Board' }]}>
      <PageHeader
        title="Board"
        actions={
          <>
            <Button variant="outline" size="sm" onClick={() => setPaletteOpen(true)}>
              <Kbd>⌘K</Kbd> Palette
            </Button>
            <Button variant="outline" size="sm" onClick={() => setShortcutsOpen(true)}>
              <Kbd>?</Kbd> Shortcuts
            </Button>
          </>
        }
      />
      <p className="ks-p ks-muted">
        Select a task, then press <Kbd>⌘K</Kbd>: the actions for the selection come first.
      </p>
      <div className="ks-board">
        {columns.map((c) => (
          <section key={c.key} className="ks-column" aria-label={c.label}>
            <h3>
              <StateDot state={c.key} />
              {c.label}
              <span className="ks-n">{c.count}</span>
            </h3>
            {demoTiles
              .filter((t) => t.state === c.key)
              .map((t) => (
                <button
                  key={t.id}
                  type="button"
                  className="ks-tile"
                  data-st={t.state}
                  data-age={t.band}
                  aria-selected={t.id === selected}
                  aria-pressed={t.id === selected}
                  onClick={() => setSelected(t.id)}
                  style={{ textAlign: 'left', font: 'inherit', color: 'inherit', cursor: 'pointer' }}
                >
                  <span className="ks-between">
                    <span className="ks-st-label">
                      <StateDot state={t.state} />
                      {c.label}
                    </span>
                    {t.days !== undefined && <AgeChip days={t.days} />}
                  </span>
                  <span style={{ fontWeight: 500 }}>{t.title}</span>
                  <span className="ks-meta">
                    {t.agent}
                    {t.id === selected && ' · selected'}
                  </span>
                </button>
              ))}
          </section>
        ))}
      </div>
    </PreviewShell>
  )
}

/** PhoneFramePage shows a preview path in a 390 × 844 phone frame: /preview/phone?path=/preview/overview. */
export function PhoneFramePage() {
  const searchStr = useRouterState({ select: (s) => s.location.searchStr })
  const { theme } = usePreview()
  const raw = new URLSearchParams(searchStr).get('path') ?? '/preview/overview'
  const path = raw.startsWith('/preview') ? raw : '/preview/overview'
  const src = `${path}${path.includes('?') ? '&' : '?'}theme=${theme}`
  return (
    <div className="kp-index" style={{ justifyItems: 'center' }}>
      <div className="ks-row" style={{ width: 390 }}>
        <h1 className="ks-h3">Phone, 390 × 844</h1>
        <PreviewLink className="ks-link" href="/preview" style={{ marginLeft: 'auto' }}>
          Every screen
        </PreviewLink>
      </div>
      <ThemeSwitch id="kp-phone-theme" />
      <div className="kp-phone">
        <iframe title={`Phone view of ${path}`} src={src} />
      </div>
      <p className="ks-p ks-meta">
        Showing <span className="ks-mono">{path}</span>. The frame follows the theme switch.
      </p>
    </div>
  )
}
