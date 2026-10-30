// SPDX-License-Identifier: Apache-2.0

// The org overview (Main.dc.html and its light, high-contrast and phone variants): the waiting strip, the Agents and
// Board tabs, and the concierge's reserved space. The theme and the phone layout come from the shell.

import { Plus } from 'lucide-react'
import { useMemo, useState } from 'react'
import { useRouterState } from '@tanstack/react-router'
import {
  agents,
  inboxItems,
  notices,
  orgSpendToday,
  tasks,
  usd,
  waitingOnYou,
  waitingTotal,
  type Agent,
  type Task,
} from '@/preview/mock/data'
import { boardColumns, boardViews, teamOf, viewChips, waitingToInbox, type BoardView } from '@/preview/overview/data'
import {
  AgeChip,
  AgentAvatar,
  Badge,
  Button,
  ButtonLink,
  FilterBar,
  Meter,
  NoticeBanner,
  PageHeader,
  Pill,
  PreviewLink,
  PreviewShell,
  RunStatus,
  StateDot,
  StatusBadge,
  Tabs,
  TabPanel,
  Textarea,
  type FilterChip,
} from '@/preview/ui'

type Tab = 'agents' | 'board'

function useTabParam(): Tab {
  const search = useRouterState({ select: (s) => s.location.searchStr })
  return new URLSearchParams(search).get('tab') === 'board' ? 'board' : 'agents'
}

export function OverviewScreen() {
  const param = useTabParam()
  const [tab, setTab] = useState<Tab>(param)
  // G B (or any link with ?tab=) changes the query while this page stays mounted.
  const [seen, setSeen] = useState(param)
  if (seen !== param) {
    setSeen(param)
    setTab(param)
  }

  const agentCount = agents.length
  return (
    <PreviewShell space="org" current="Overview" crumbs={[{ label: 'Overview' }]} banner={<NoticeBanner notices={notices} />}>
      <PageHeader
        title="Overview"
        description={
          <span className="ks-num">
            {agentCount} agents · {orgSpendToday.running} running · <span className="ks-strong" style={{ color: 'var(--foreground)' }}>{usd(orgSpendToday.spent)}</span> spent today of the org&apos;s{' '}
            <span className="ks-strong" style={{ color: 'var(--foreground)' }}>{usd(orgSpendToday.limit)}</span> daily limit
          </span>
        }
        actions={
          <ButtonLink href="/preview/agents/new">
            <Plus aria-hidden="true" />
            New agent
          </ButtonLink>
        }
      />

      <WaitingStrip />

      <Tabs
        label="Overview views"
        idPrefix="ov"
        value={tab}
        onChange={setTab}
        tabs={[
          { id: 'agents', label: 'Agents', count: agentCount },
          { id: 'board', label: 'Board', count: tasks.length },
        ]}
      />
      <TabPanel idPrefix="ov" tab={tab}>
        {tab === 'agents' ? <AgentsTab /> : <BoardTab />}
      </TabPanel>

      <Concierge />
    </PreviewShell>
  )
}

function WaitingStrip() {
  return (
    <section aria-labelledby="waiting-h" className="ks-stack-3">
      <div className="ks-row" style={{ alignItems: 'baseline' }}>
        <h2 id="waiting-h" className="ks-h3">
          Waiting on you
        </h2>
        <span className="ks-meta ks-num">
          {waitingOnYou.length} of {waitingTotal} · oldest first · ages turn orange at 1 d and 3 d, red at 7 d
        </span>
        <PreviewLink className="ks-link" href="/preview/inbox" style={{ marginLeft: 'auto' }}>
          Open inbox
        </PreviewLink>
      </div>
      <ul className="ks-ul" style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: 8 }}>
        {waitingOnYou.map((w, i) => (
          <li key={w.title} className="ks-wait">
            <div className="ks-between">
              <span className="ks-label">{w.kind}</span>
              <AgeChip hours={w.hoursWaiting} />
            </div>
            <PreviewLink className="ks-a" href={`/preview/inbox?item=${waitingToInbox[i] ?? inboxItems[i]?.id}`} style={{ fontWeight: 500 }}>
              {w.title}
            </PreviewLink>
            <span className="ks-row" style={{ gap: 6 }}>
              <AgentAvatar name={w.agent} decorative />
              <span className="ks-meta">{w.agent}</span>
            </span>
          </li>
        ))}
      </ul>
    </section>
  )
}

// ---------------------------------------------------------------------------------------------------------------
// Agents tab

function AgentsTab() {
  return (
    <div className="ks-stack-4" style={{ gap: 24 }}>
      <OrgChart />
      <section aria-labelledby="agents-h" className="ks-stack-3">
        <h2 id="agents-h" className="ks-h3">
          Agents
        </h2>
        <div className="ks-grid">
          {agents.map((a) => (
            <AgentCard key={a.id} agent={a} />
          ))}
        </div>
      </section>
    </div>
  )
}

function ChartNode({ id, name, note, tone }: { id: string; name: string; note: string; tone?: 'info' | 'err' }) {
  return (
    <PreviewLink className="ks-node" href={`/preview/agents/${id}`}>
      <AgentAvatar name={name} decorative />
      <span style={{ display: 'grid' }}>
        <span style={{ fontWeight: 500 }}>{name}</span>
        <span className={tone ? `ks-meta ks-${tone}` : 'ks-meta'}>{note}</span>
      </span>
    </PreviewLink>
  )
}

function OrgChart() {
  return (
    <section aria-labelledby="chart-h" className="ks-panel ks-stack-3">
      <div className="ks-row" style={{ alignItems: 'baseline' }}>
        <h2 id="chart-h" className="ks-h4">
          Org chart
        </h2>
        <span className="ks-meta">Agents only. Each line is a &ldquo;Reports to&rdquo;; agents that report to no agent sit under the org.</span>
      </div>
      {/* The chart is wider than a phone; it scrolls sideways and is reachable by keyboard. */}
      <div style={{ overflowX: 'auto', padding: '4px 0 8px' }} tabIndex={0} role="region" aria-label="Org chart">
        <ul className="ks-tree" style={{ minWidth: 900 }}>
          <li>
            <div className="ks-node ks-node-org">
              <span className="sb-mark kn-avatar" style={{ background: 'var(--blue-200)', color: 'var(--blue-1000)', boxShadow: 'inset 0 0 0 1px var(--blue-600)' }} aria-hidden="true">
                PE
              </span>
              <span style={{ display: 'grid' }}>
                <span className="ks-strong">Platform Engineering</span>
                <span className="ks-meta">Org · {agents.length} agents</span>
              </span>
            </div>
            <ul>
              <li>
                <span className="ks-team">FinOps</span>
                <ChartNode id="finops-analyst" name="FinOps Analyst" note="Running now" tone="info" />
              </li>
              <li>
                <span className="ks-team">Research</span>
                <ChartNode id="researcher" name="Researcher" note="Last run 26 min ago" />
                <ul>
                  <li>
                    <ChartNode id="docs-gardener" name="Docs Gardener" note="Draft" />
                  </li>
                </ul>
              </li>
              <li>
                <span className="ks-team">Upgrades</span>
                <ChartNode id="lcm-upgrader" name="LCM Upgrader" note="Running now · 2 sub-runs" tone="info" />
                <ul>
                  <li>
                    <ChartNode id="release-notes-scout" name="Release Notes Scout" note="Unhealthy" tone="err" />
                  </li>
                </ul>
              </li>
            </ul>
          </li>
        </ul>
      </div>
    </section>
  )
}

function AgentCard({ agent: a }: { agent: Agent }) {
  const percent = Math.round((a.spendToday / a.limitToday) * 100)
  const unhealthy = a.state === 'unhealthy'
  return (
    <article className="ks-panel ks-stack-3">
      <div style={{ display: 'flex', alignItems: 'flex-start', gap: 10 }}>
        <AgentAvatar name={a.name} size="lg" decorative />
        <div style={{ display: 'grid', flex: 1, minWidth: 0 }}>
          <PreviewLink className="ks-a" href={`/preview/agents/${a.id}`} style={{ fontWeight: 600, fontSize: 15 }}>
            {a.name}
          </PreviewLink>
          <span className="ks-meta">
            {a.team} · reports to {a.reports}
          </span>
        </div>
        <StatusBadge kind="agent" state={a.state} />
      </div>
      <div className="ks-row ks-meta">
        <span className="ks-mono" style={{ color: 'var(--foreground)' }}>
          {a.model}
        </span>
        <span>· {a.modelFrom}</span>
      </div>
      {a.running && a.runs ? (
        <PreviewLink className="ks-row ks-a" href={`/preview/runs/${a.runs[0].id}`} style={{ gap: 6 }}>
          <StateDot state="working" />
          <RunStatus runs={a.runs} />
        </PreviewLink>
      ) : (
        <p className="ks-p ks-muted">{a.lastRun}</p>
      )}
      <div className="ks-stack" style={{ gap: 6 }}>
        <div className="ks-between">
          <span className="ks-meta">Today&apos;s spend</span>
          <span className="ks-num" style={{ fontSize: 12, lineHeight: '16px' }}>
            <span className="ks-strong">{usd(a.spendToday)}</span> <span className="ks-muted">of {usd(a.limitToday)} · {percent}%</span>
          </span>
        </div>
        <Meter used={a.spendToday} limit={a.limitToday} label={`${percent}% of today’s limit used`} />
      </div>
      <dl className="ks-dl">
        <div>
          <dt>Tokens today</dt>
          <dd>{a.tokensToday}</dd>
        </div>
        <div>
          <dt>Cache hit rate</dt>
          <dd>{a.cacheHit}</dd>
        </div>
        <div>
          <dt>Waiting on you</dt>
          <dd className={a.waiting ? 'ks-warn' : 'ks-muted'}>{a.waiting ? `${a.waiting} waiting · oldest ${a.oldestWaiting}` : 'Nothing'}</dd>
        </div>
        <div>
          <dt>Health</dt>
          <dd className={unhealthy ? 'ks-err' : undefined}>{a.health}</dd>
        </div>
        <div style={{ gridColumn: '1 / -1' }}>
          <dt>Next schedule</dt>
          <dd>{a.nextSchedule}</dd>
        </div>
      </dl>
    </article>
  )
}

// ---------------------------------------------------------------------------------------------------------------
// Board tab

type Filters = { state: string[]; team: string[]; text: string }

/** The mockup opens on its saved view's chips with all 12 tasks shown; the first change makes the filter real. */
const mockupFilters: Filters = { ...viewChips.upgrades, text: '' }

function matches(t: Task, f: Filters): boolean {
  if (f.state.length && !f.state.includes(t.state)) return false
  if (f.team.length && !f.team.includes(teamOf(t))) return false
  for (const raw of f.text.toLowerCase().split(/\s+/).filter(Boolean)) {
    const m = /^([a-z]+):(>?)(.*)$/.exec(raw)
    if (!m) {
      if (!`${t.title} ${t.agent} ${t.source}`.toLowerCase().includes(raw)) return false
      continue
    }
    const [, key, gt, val] = m
    if (!val) continue
    if (key === 'agent' && !t.agent.toLowerCase().includes(val)) return false
    else if (key === 'source' && t.source !== val) return false
    else if (key === 'state' && t.state !== val) return false
    else if (key === 'age') {
      const days = parseFloat(val)
      if (Number.isNaN(days) || (gt ? (t.hoursWaiting ?? 0) <= days * 24 : (t.hoursWaiting ?? 0) < days * 24)) return false
    }
  }
  return true
}

function BoardTab() {
  const [filters, setFilters] = useState<Filters>(mockupFilters)
  const [applied, setApplied] = useState(false)
  const [view, setView] = useState<BoardView | null>('upgrades')
  const [saved, setSaved] = useState(false)

  function change(next: Filters, nextView: BoardView | null) {
    setFilters(next)
    setView(nextView)
    setApplied(true)
    setSaved(false)
  }

  const chips: FilterChip[] = [
    ...(filters.state.length ? [{ key: 'state', value: filters.state.join(','), onRemove: () => change({ ...filters, state: [] }, null) }] : []),
    ...(filters.team.length ? [{ key: 'team', value: filters.team.join(','), onRemove: () => change({ ...filters, team: [] }, null) }] : []),
  ]

  const shown = useMemo(() => (applied ? tasks.filter((t) => matches(t, filters)) : tasks), [applied, filters])
  const viewLabel = boardViews.find((v) => v.id === view)?.label

  return (
    <div className="ks-stack-3">
      <div className="ks-row">
        <FilterBar
          id="board-q"
          label="Filter tasks"
          chips={chips}
          placeholder="Type a filter: agent: source: age:>3d"
          value={filters.text}
          onChange={(text) => change({ ...filters, text }, view)}
        />
        <div className="ks-views" aria-label="Saved views">
          <span className="ks-meta">Views</span>
          {boardViews.map((v) => (
            <Pill key={v.id} pressed={view === v.id} onClick={() => (view === v.id ? change({ state: [], team: [], text: filters.text }, null) : change({ ...viewChips[v.id], text: filters.text }, v.id))}>
              {v.label}
            </Pill>
          ))}
          <Button variant="ghost" size="xs" onClick={() => setSaved(true)}>
            Save view
          </Button>
        </div>
      </div>
      <p className="ks-p ks-meta" role="status">
        {saved
          ? 'View saved for the org (preview only; nothing is stored).'
          : applied
            ? `Showing ${shown.length} of ${tasks.length} tasks${viewLabel ? `, view “${viewLabel}”` : ''}. Views are stored on the server and shared with the org.`
            : `Showing all ${tasks.length} tasks; the saved view “${viewLabel}” filters this list. Views are stored on the server and shared with the org.`}
      </p>
      <div className="ks-board">
        {boardColumns.map((c) => {
          const col = shown.filter((t) => t.state === c.key)
          return (
            <section key={c.key} className="ks-column" aria-label={c.label}>
              <h3>
                <StateDot state={c.key} />
                {c.label}
                <span className="ks-n">{col.length}</span>
              </h3>
              {col.length === 0 && <p className="ks-p ks-meta">Nothing here.</p>}
              <ul className="ks-ul ks-stack">
                {col.map((t) => (
                  <TaskTile key={t.title} task={t} label={c.label} />
                ))}
              </ul>
            </section>
          )
        })}
      </div>
    </div>
  )
}

function TaskTile({ task: t, label }: { task: Task; label: string }) {
  const h = t.hoursWaiting
  const band = h === undefined ? undefined : h >= 168 ? '7d' : h >= 72 ? '3d' : h >= 24 ? '1d' : 'new'
  const failedSub = t.subs?.some((s) => s.state === 'failed')
  return (
    <li className="ks-tile" data-st={t.state} data-age={band}>
      <div className="ks-between" style={{ minHeight: 20 }}>
        <span className="ks-st-label">
          <StateDot state={t.state} />
          {label}
        </span>
        {h !== undefined && <AgeChip hours={h} />}
      </div>
      <PreviewLink className="ks-a" href={t.state === 'blocked' ? '/preview/inbox' : '/preview/runs/9c41d07a2e'} style={{ fontWeight: 500 }}>
        {t.title}
      </PreviewLink>
      <span className="ks-row" style={{ gap: 6 }}>
        <AgentAvatar name={t.agent} decorative />
        <span className="ks-meta">
          {t.agent} · {t.source}
        </span>
      </span>
      {t.subs && (
        <div className="ks-subs">
          {t.subs.map((s) => (
            <span key={s.name} className="ks-sub">
              <StateDot state={s.state} />
              {s.name}
            </span>
          ))}
        </div>
      )}
      {failedSub && <span className="ks-meta ks-err">A sub-run failed; check before trusting the result.</span>}
    </li>
  )
}

function Concierge() {
  return (
    <section aria-labelledby="concierge-h" className="ks-panel ks-stack-3">
      <div className="ks-row">
        <h2 id="concierge-h" className="ks-h4">
          Concierge
        </h2>
        <Badge variant="outline">Coming in v0.3</Badge>
      </div>
      <p className="ks-p ks-muted">The concierge will set up agents with you, explain a cost or find a run. This space is kept for it.</p>
      <div className="ks-row" style={{ alignItems: 'flex-end' }}>
        <label htmlFor="concierge-box" className="ks-sr">
          Message the concierge
        </label>
        <Textarea id="concierge-box" disabled placeholder="Ask the concierge…" style={{ flex: '1 1 320px' }} />
        <Button disabled>Send</Button>
      </div>
    </section>
  )
}
