// SPDX-License-Identifier: Apache-2.0

import { useState, type FormEvent } from 'react'
import { Button, Dialog, DialogClose, Field, Input, Meter, NativeSelect, PageHeader, Panel, PreviewLink, PreviewShell, Segmented, TypeToConfirm } from '@/preview/ui'
import { agents, budgets as seedBudgets, spendByDay, spendProjectedPerDay, spendRatios, usd, type Budget } from '@/preview/mock/data'
import { expensiveRuns, ledgerSeries, spendKpis, systemChecks } from './data'

type Scope = 'org' | 'agent'
const MAX = 30
const UPGRADE_DAYS = new Set([12, 26, 5, 19])
const SERIES_CLASS = { a: 'ks-seg-a', b: 'ks-seg-b', c: 'ks-seg-c', d: 'ks-seg-d' } as const
const ORG = 'Platform Engineering'

function pctOf(b: Budget) {
  return Math.round((b.used / b.limit) * 100)
}

/** Per-day heights (percent of the chart) for each series, as in the mockup's script. */
function dayBars(i: number) {
  const proj = i >= 28
  const v = proj ? spendProjectedPerDay : spendByDay[i]
  const tot = (v / MAX) * 100
  const a = proj ? tot : tot * (UPGRADE_DAYS.has(i) ? 0.62 : 0.35)
  const b = proj ? 0 : tot * 0.28
  const c = proj ? 0 : tot * 0.2
  const d = proj ? 0 : Math.max(0, tot - a - b - c)
  return { proj, v, bars: { a, b, c, d } }
}

export function SpendScreen() {
  const [scope, setScope] = useState<Scope>('org')
  const [agentId, setAgentId] = useState(agents[1]?.id ?? 'lcm-upgrader')
  const [list, setList] = useState<Budget[]>(seedBudgets)
  const [pause, setPause] = useState(true)
  const [paused, setPaused] = useState(4)
  const [resume, setResume] = useState(false)
  const [addBudget, setAddBudget] = useState(false)
  const [typed, setTyped] = useState('')
  const [raiseTo, setRaiseTo] = useState('$35.00')
  const [status, setStatus] = useState('')

  const seriesFor = scope === 'org' ? null : seriesOfAgent(agentId)

  function exportCsv() {
    const lines = ['date,usd,projected', ...Array.from({ length: 31 }, (_, i) => {
      const d = dayBars(i)
      return `2026-10-${String(i + 1).padStart(2, '0')},${d.v.toFixed(2)},${d.proj}`
    })]
    const url = URL.createObjectURL(new Blob([lines.join('\n') + '\n'], { type: 'text/csv' }))
    const a = document.createElement('a')
    a.href = url
    a.download = 'spend-2026-10.csv'
    a.click()
    URL.revokeObjectURL(url)
    setStatus('Exported spend-2026-10.csv')
  }

  function doResume() {
    const n = Number(raiseTo.replace(/[^0-9.]/g, ''))
    if (n > 0) setList((l) => l.map((b) => (b.layer === 'Org' ? { ...b, limit: n } : b)))
    setPaused(0)
    setResume(false)
    setTyped('')
    setStatus(`Resumed paused agents; today's limit is ${raiseTo}`)
  }

  return (
    <PreviewShell space="org" current="Spend & usage" crumbs={[{ label: 'Spend & usage' }]}>
      <PageHeader
        title="Spend & usage"
        description="October 2026 · costs include cache reads and writes at catalog prices"
        actions={
          <>
            <Segmented<Scope>
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
                <label className="ks-sr" htmlFor="sp-agent">Agent</label>
                <NativeSelect id="sp-agent" value={agentId} onChange={(e) => setAgentId(e.target.value)} style={{ width: 'auto' }}>
                  {agents.map((a) => (
                    <option key={a.id} value={a.id}>{a.name}</option>
                  ))}
                </NativeSelect>
              </>
            )}
            <Button variant="outline" size="sm" onClick={exportCsv}>Export CSV</Button>
          </>
        }
      />
      <p className="ks-sr" role="status">{status}</p>

      <div className="ks-grid-4">
        {spendKpis.map((k) => (
          <Panel key={k.label} className="ks-stack" style={{ gap: 2 }}>
            <span className="ks-meta">{k.label}</span>
            <span className="ks-h2 ks-num">{k.value}</span>
            <span className="ks-meta">{k.note}</span>
          </Panel>
        ))}
      </div>

      <section className="ks-panel ks-stack-4" aria-labelledby="bud-h">
        <div className="ks-between">
          <div className="ks-stack" style={{ gap: 2 }}>
            <h2 id="bud-h" className="ks-h3">Budgets, today</h2>
            <span className="ks-meta">A call must fit every layer; the tightest one stops it. Alerts go to owners at 50, 75 and 100%.</span>
          </div>
          <Button variant="outline" size="sm" onClick={() => setAddBudget(true)}>Add budget</Button>
        </div>
        {list.map((b) => (
          <div key={b.layer + b.name} style={{ display: 'grid', gridTemplateColumns: 'minmax(200px, 260px) 1fr 160px', gap: 16, alignItems: 'center' }}>
            <div className="ks-stack" style={{ gap: 0 }}>
              <span className="ks-row" style={{ gap: 6 }}>
                <span className="ks-label">{b.layer}</span>
                {b.inherited && <span className="kn-badge kn-badge-secondary">Default</span>}
              </span>
              <span style={{ fontWeight: 500 }}>{b.name}</span>
              <span className="ks-meta">{b.note}</span>
            </div>
            <div className="ks-stack" style={{ gap: 2 }}>
              <Meter used={b.used} limit={b.limit} large label={`${b.layer} ${b.name}: ${pctOf(b)}% used`} />
              <div className="ks-ticks" aria-hidden="true">
                <span style={{ left: '50%' }}>50%</span>
                <span style={{ left: '75%' }}>75%</span>
                <span style={{ left: '100%', transform: 'translateX(-100%)' }}>100%</span>
              </div>
            </div>
            <div className="ks-num" style={{ textAlign: 'right' }}>
              <span className="ks-strong">{usd(b.used)}</span> <span className="ks-muted">of {usd(b.limit)}</span>
            </div>
          </div>
        ))}
        <div className="ks-divider" />
        <div className="ks-between">
          <label className="ks-row" style={{ flexWrap: 'nowrap', alignItems: 'flex-start' }}>
            <input type="checkbox" checked={pause} onChange={(e) => setPause(e.target.checked)} style={{ marginTop: 3 }} />
            <span className="ks-stack" style={{ gap: 2 }}>
              <span style={{ fontWeight: 600 }}>Pause agents at the limit</span>
              <span className="ks-meta">A hard stop at the gateway. Paused agents stay paused until an owner resumes them.</span>
            </span>
          </label>
          <div className="ks-row">
            <span className="ks-meta">{paused > 0 ? 'Last pause: 24 Oct 23:10, org limit' : 'No agents are paused'}</span>
            <Button variant="outline" size="sm" disabled={paused === 0} onClick={() => setResume(true)}>
              Resume paused agents
            </Button>
          </div>
        </div>
      </section>

      <section className="ks-panel ks-stack-3" aria-labelledby="ch-h">
        <div className="ks-between">
          <div className="ks-stack" style={{ gap: 2 }}>
            <h2 id="ch-h" className="ks-h3">Daily spend by agent</h2>
            <span className="ks-meta">Each bar opens the most expensive runs of that day. Outlined bars are the projection.</span>
          </div>
          <div className="ks-row ks-meta">
            {ledgerSeries.map((s) => (
              <span key={s.id} className="ks-row" style={{ gap: 4, opacity: seriesFor && seriesFor !== s.id ? 0.4 : 1 }}>
                <span className={`ks-swatch ${SERIES_CLASS[s.id]}`} />
                {s.name}
              </span>
            ))}
          </div>
        </div>
        <div className="ks-chart" role="img" aria-label="Daily spend for October: highest on 13 and 27 Oct, upgrade Mondays">
          {Array.from({ length: 31 }, (_, i) => {
            const { proj, v, bars } = dayBars(i)
            const label = `${i + 1} Oct: ${usd(v)}${proj ? ' projected' : ''}`
            return (
              <PreviewLink key={i} href="/preview/runs" title={label} aria-label={label}>
                <span className={proj ? 'ks-seg-proj' : 'ks-seg-a'} style={{ height: `${seriesFor && seriesFor !== 'a' && !proj ? 0 : bars.a}%` }} />
                <span className="ks-seg-b" style={{ height: `${seriesFor && seriesFor !== 'b' ? 0 : bars.b}%` }} />
                <span className="ks-seg-c" style={{ height: `${seriesFor && seriesFor !== 'c' ? 0 : bars.c}%` }} />
                <span className="ks-seg-d" style={{ height: `${seriesFor && seriesFor !== 'd' ? 0 : bars.d}%` }} />
              </PreviewLink>
            )
          })}
        </div>
        <div className="ks-between ks-meta ks-num">
          <span>1 Oct</span>
          <span>15 Oct</span>
          <span>31 Oct</span>
        </div>
      </section>

      <div className="ks-grid-2">
        <section className="ks-panel ks-stack-3" aria-labelledby="ra-h">
          <h2 id="ra-h" className="ks-h3">Ratios</h2>
          {spendRatios.map((r) => (
            <div key={r.name} className="ks-stack" style={{ gap: 4 }}>
              <div className="ks-between">
                <span style={{ fontWeight: 500 }}>{r.name}</span>
                <span className="ks-meta ks-num">{r.text}</span>
              </div>
              <div role="img" aria-label={`${r.name}: ${r.text}`} style={{ display: 'flex', height: 10, borderRadius: 'var(--radius-full)', overflow: 'hidden', boxShadow: 'inset 0 0 0 1px var(--divider)' }}>
                <span style={{ width: `${r.parts[0]}%`, background: 'var(--gray-800)' }} />
                <span style={{ width: `${r.parts[1]}%`, background: 'var(--gray-400)' }} />
                <span style={{ width: `${r.parts[2]}%`, background: 'var(--red-700)' }} />
              </div>
            </div>
          ))}
        </section>
        <section className="ks-panel ks-stack-3" aria-labelledby="sc-h">
          <h2 id="sc-h" className="ks-h3">System checks</h2>
          <ul className="ks-ul ks-stack" style={{ fontSize: 13 }}>
            {systemChecks.map((c) => (
              <li key={c.text} className="ks-row" style={{ flexWrap: 'nowrap' }}>
                <span className={c.level === 'Pass' ? 'ks-ok' : c.level === 'Warn' ? 'ks-warn' : 'ks-err'}>{c.level}</span>
                <span>
                  {c.text}
                  {'fix' in c && (
                    <>
                      {' '}· <PreviewLink className="ks-link" href={c.fix}>Fix</PreviewLink>
                    </>
                  )}
                </span>
              </li>
            ))}
          </ul>
        </section>
      </div>

      <section className="ks-panel-flush" aria-labelledby="ex-h">
        <div className="ks-between" style={{ padding: '12px 16px' }}>
          <h2 id="ex-h" className="ks-h3">Most expensive runs this month</h2>
          <PreviewLink className="ks-link" href="/preview/runs">All runs by cost</PreviewLink>
        </div>
        <div className="ks-table-wrap" role="region" aria-label="Most expensive runs" tabIndex={0}>
          <table className="ks-table">
            <thead>
              <tr>
                <th scope="col">Run</th>
                <th scope="col">Agent</th>
                <th scope="col">Task</th>
                <th scope="col">Model</th>
                <th scope="col" className="ks-r">Tokens</th>
                <th scope="col" className="ks-r">Cache hit</th>
                <th scope="col" className="ks-r">Cost</th>
              </tr>
            </thead>
            <tbody>
              {expensiveRuns.map((r) => (
                <tr key={r.id}>
                  <td><PreviewLink className="ks-a ks-mono" href={`/preview/runs/${r.id}`}>{r.id}</PreviewLink></td>
                  <td>{r.agent}</td>
                  <td>{r.task}</td>
                  <td className="ks-mono">{r.model}</td>
                  <td className="ks-r">{r.tokens}</td>
                  <td className="ks-r">{r.cache}</td>
                  <td className="ks-r">{r.cost}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      <Dialog
        open={resume}
        onOpenChange={(o) => {
          setResume(o)
          if (!o) setTyped('')
        }}
        title={`Resume ${paused} paused agents?`}
        description="They hit the org limit of $25.00 today. Resuming lets them spend again today, up to a new limit you set."
        footer={
          <>
            <DialogClose><Button variant="outline">Cancel</Button></DialogClose>
            <Button disabled={typed !== ORG} onClick={doResume}>Resume agents</Button>
          </>
        }
      >
        <Field id="rs-new" label="Raise today's limit to">
          <Input id="rs-new" className="ks-num" value={raiseTo} onChange={(e) => setRaiseTo(e.target.value)} autoComplete="off" />
        </Field>
        <TypeToConfirm id="rs-name" expected={ORG} value={typed} onChange={setTyped} />
      </Dialog>

      {addBudget && (
        <AddBudgetDialog
          onClose={() => setAddBudget(false)}
          onAdd={(b) => {
            setList((l) => [...l, b])
            setAddBudget(false)
            setStatus(`Budget added for ${b.name}`)
          }}
        />
      )}
    </PreviewShell>
  )
}

/** The mockup's series order maps a few agents onto the chart colours; every other agent falls under Others. */
function seriesOfAgent(id: string): 'a' | 'b' | 'c' | 'd' {
  if (id === 'lcm-upgrader') return 'a'
  if (id === 'finops-analyst') return 'b'
  if (id === 'researcher') return 'c'
  return 'd'
}

function AddBudgetDialog({ onClose, onAdd }: { onClose: () => void; onAdd: (b: Budget) => void }) {
  const [layer, setLayer] = useState<Budget['layer']>('Agent')
  const [name, setName] = useState('')
  const [limit, setLimit] = useState('')
  const n = Number(limit.replace(/[^0-9.]/g, ''))
  const ok = name.trim() !== '' && n > 0
  function submit(e: FormEvent) {
    e.preventDefault()
    if (ok) onAdd({ layer, name: name.trim(), note: 'Custom', used: 0, limit: n })
  }
  return (
    <Dialog
      open
      onOpenChange={(o) => !o && onClose()}
      title="Add a budget"
      description="A call must fit every layer; the tightest one stops it."
      footer={
        <>
          <DialogClose><Button variant="outline">Cancel</Button></DialogClose>
          <Button type="submit" form="bud-form" disabled={!ok}>Add budget</Button>
        </>
      }
    >
      <form id="bud-form" className="ks-stack-4" onSubmit={submit}>
        <Field id="bud-layer" label="Layer">
          <NativeSelect id="bud-layer" value={layer} onChange={(e) => setLayer(e.target.value as Budget['layer'])}>
            <option>Agent</option>
            <option>API key</option>
          </NativeSelect>
        </Field>
        <Field id="bud-name" label={layer === 'Agent' ? 'Agent' : 'Key name'}>
          <Input id="bud-name" value={name} onChange={(e) => setName(e.target.value)} autoComplete="off" />
        </Field>
        <Field id="bud-limit" label="Daily limit (USD)">
          <Input id="bud-limit" className="ks-num" value={limit} onChange={(e) => setLimit(e.target.value)} placeholder="$10.00" autoComplete="off" />
        </Field>
      </form>
    </Dialog>
  )
}
