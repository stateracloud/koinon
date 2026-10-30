// SPDX-License-Identifier: Apache-2.0

// Group "run": the run detail page (Run.dc.html, Run-light.dc.html). Summary, timeline and raw views, the live
// now-doing line, the steer box and the deployment-style log, driven by mock data.

import { useEffect, useRef, useState } from 'react'
import {
  AgentAvatar,
  Alert,
  AlertDescription,
  AlertTitle,
  Button,
  Composer,
  Dialog,
  PreviewLink,
  PreviewShell,
  RunStatus,
  Segmented,
  StateDot,
  usePaletteSelection,
} from '@/preview/ui'
import { runDetail } from '@/preview/mock/data'
import { rawEvents, runFacts, subRuns, timelineTicks } from '@/preview/run/data'

type View = 'sum' | 'tl' | 'raw'
type LogLine = { n: number; time: string; msg: string; level?: 'warn' | 'err'; highlight?: boolean; redactedAfter?: string }

const outcomeLabels = [
  ['var(--blue-700)', 'model'],
  ['var(--green-700)', 'tool ok'],
  ['var(--amber-700)', 'retried'],
  ['var(--red-700)', 'failed'],
] as const

export function RunPage() {
  const [view, setView] = useState<View>('sum')
  const [canceled, setCanceled] = useState(false)
  const [cancelOpen, setCancelOpen] = useState(false)
  const [copied, setCopied] = useState<'run' | 'lines' | null>(null)
  const [range, setRange] = useState<[number, number]>([41, 44])
  const [follow, setFollow] = useState(false)
  const [extra, setExtra] = useState<LogLine[]>([])
  const [steerNote, setSteerNote] = useState<string | null>(null)
  const [seconds, setSeconds] = useState(14)
  const steerRef = useRef<HTMLDivElement>(null)
  const logRef = useRef<HTMLDivElement>(null)

  const log: LogLine[] = [...runDetail.log, ...extra]
  const lo = Math.min(...range)
  const hi = Math.max(...range)
  const rangeLabel = lo === hi ? `L${lo}` : `L${lo}-L${hi}`

  useEffect(() => {
    if (canceled) return
    const t = setInterval(() => setSeconds((s) => s + 1), 1000)
    return () => clearInterval(t)
  }, [canceled])

  useEffect(() => {
    if (new URLSearchParams(window.location.search).get('steer') === '1') focusSteer()
  }, [])

  useEffect(() => {
    if (follow && logRef.current) logRef.current.scrollTop = logRef.current.scrollHeight
  }, [follow, extra])

  function focusSteer() {
    steerRef.current?.scrollIntoView({ block: 'center' })
    steerRef.current?.querySelector('textarea')?.focus()
  }

  function flash(what: 'run' | 'lines') {
    setCopied(what)
    setTimeout(() => setCopied(null), 2000)
  }

  usePaletteSelection({
    key: runFacts.shortId,
    title: `Run ${runFacts.shortId}`,
    heading: 'For this run',
    actions: [
      { label: 'Steer this run…', hint: 'about $0.02 a steer', keys: ['S'], onSelect: focusSteer },
      { label: 'Copy run link', onSelect: () => flash('run') },
      { label: 'Cancel run…', onSelect: () => setCancelOpen(true) },
      { label: 'Open its spend', href: '/preview/spend' },
    ],
  })

  function pick(n: number, extend: boolean) {
    setRange((r) => (extend ? [r[0], n] : [n, n]))
    history.replaceState(null, '', `#L${n}`)
  }

  function steer(text: string) {
    const last = log[log.length - 1]
    setExtra((x) => [...x, { n: last.n + 1, time: '13:21:03', msg: `steer from procoduck queued: ${text}` }])
    setSteerNote('Steer queued. It applies after the current step.')
  }

  const state = canceled ? 'canceled' : 'working'

  return (
    <PreviewShell space="org" current="Runs" crumbs={[{ label: 'Runs', href: '/preview/runs' }, { label: runFacts.shortId, mono: true }]}>
      <section className="ks-panel ks-stack-4" aria-labelledby="run-h" style={{ padding: 20 }}>
        <div className="ks-between" style={{ alignItems: 'flex-start' }}>
          <div className="ks-stack" style={{ gap: 4 }}>
            <div className="ks-row">
              <StateDot state={state} />
              <span className={canceled ? 'ks-meta' : 'ks-info'} style={{ fontWeight: 500 }}>{canceled ? 'Canceled' : 'Running'}</span>
              <span className="ks-meta">· task {canceled ? 'Canceled' : 'Working'}</span>
            </div>
            <h1 id="run-h" className="ks-h1">{runDetail.title}</h1>
            <RunStatus runs={[{ id: runDetail.id, state: canceled ? 'canceled' : 'running' }]} />
          </div>
          <div className="ks-row">
            <Button variant="outline" size="sm" onClick={() => flash('run')}>{copied === 'run' ? 'Link copied' : 'Copy run link'}</Button>
            <Button variant="outline" size="sm" disabled={canceled} onClick={() => setCancelOpen(true)}>Cancel run</Button>
          </div>
        </div>
        <dl className="ks-grid-4" style={{ margin: 0, gap: 16 }}>
          <div>
            <dt className="ks-meta">Agent</dt>
            <dd style={{ margin: '4px 0 0' }}>
              <PreviewLink className="ks-row ks-a" href={`/preview/agents/${runFacts.agentId}`} style={{ gap: 6 }}>
                <AgentAvatar name={runDetail.agent} decorative />
                {runDetail.agent}
              </PreviewLink>
            </dd>
          </div>
          <div>
            <dt className="ks-meta">Model</dt>
            <dd style={{ margin: '4px 0 0' }}><span className="ks-mono">{runFacts.model}</span><span className="ks-meta"> · {runFacts.provider}</span></dd>
          </div>
          <div>
            <dt className="ks-meta">Trigger</dt>
            <dd style={{ margin: '4px 0 0' }}>{runFacts.trigger} · <span className="ks-mono">{runFacts.triggerWhat}</span></dd>
          </div>
          <div>
            <dt className="ks-meta">Agent version</dt>
            <dd style={{ margin: '4px 0 0' }}><span className="ks-mono">{runFacts.version}</span><span className="ks-meta"> · {runFacts.digest}</span></dd>
          </div>
          <div>
            <dt className="ks-meta">Started</dt>
            <dd className="ks-num" style={{ margin: '4px 0 0' }}>{runFacts.started}</dd>
          </div>
          <div>
            <dt className="ks-meta">Tokens</dt>
            <dd className="ks-num" style={{ margin: '4px 0 0' }}>{runFacts.tokens} <span className="ks-meta">· {runFacts.tokenSplit}</span></dd>
          </div>
          <div>
            <dt className="ks-meta">Cost so far</dt>
            <dd className="ks-num" style={{ margin: '4px 0 0' }}><span className="ks-strong">{runFacts.cost}</span> <span className="ks-meta">· {runFacts.agentToday}</span></dd>
          </div>
          <div>
            <dt className="ks-meta">Usage</dt>
            <dd className="ks-row" style={{ margin: '4px 0 0' }}>
              <PreviewLink className="ks-link" href="/preview/spend">This run's usage</PreviewLink>
              <PreviewLink className="ks-link" href="/preview/spend">Agent, today</PreviewLink>
            </dd>
          </div>
        </dl>
      </section>

      <section className="ks-stack-3" aria-labelledby="subs-h">
        <div className="ks-row">
          <h2 id="subs-h" className="ks-h4">Sub-runs</h2>
          <span className="ks-meta">2 · started by this run through ask_agent</span>
        </div>
        <Alert variant="warning">
          <AlertTitle>A sub-run failed</AlertTitle>
          <AlertDescription>Release Notes Scout failed (its provider returned 401). This run kept going without the release notes, so check the result before you approve the pull request.</AlertDescription>
        </Alert>
        <ul className="ks-ul ks-stack" style={{ paddingLeft: 16, borderLeft: '1px solid var(--border)', marginLeft: 4 }}>
          {subRuns.map((s) => (
            <li key={s.id} className="ks-raised ks-between" style={{ padding: '10px 12px' }}>
              <span className="ks-row"><StateDot state={s.state} /><span className="ks-mono">{s.id}</span><span>{s.title}</span></span>
              <span className="ks-row ks-meta ks-num">{s.meta} <PreviewLink className="ks-link" href={`/preview/runs/${s.id}`}>Open</PreviewLink></span>
            </li>
          ))}
        </ul>
      </section>

      <div className="ks-between">
        <Segmented<View>
          label="View"
          value={view}
          onChange={setView}
          options={[
            { id: 'sum', label: 'Summary' },
            { id: 'tl', label: 'Timeline' },
            { id: 'raw', label: 'Raw' },
          ]}
        />
        <span className="ks-meta">{runFacts.summaryMeta}</span>
      </div>

      {view === 'sum' && (
        <ol className="ks-ul ks-panel-flush" aria-label="Steps">
          {runDetail.steps.map((s) => (
            <li key={s.n} style={{ display: 'grid', gridTemplateColumns: '28px 1fr auto', gap: 12, padding: '10px 16px', borderBottom: '1px solid var(--divider)', alignItems: 'start' }}>
              <span className="ks-meta ks-num" style={{ paddingTop: 2 }}>{s.n}</span>
              <div className="ks-stack" style={{ gap: 2 }}>
                <span className="ks-row" style={{ gap: 6 }}>
                  <StateDot state={canceled && s.state === 'working' ? 'canceled' : s.state} />
                  <span style={{ fontWeight: 500 }}>{s.title}</span>
                  {s.repeats ? <span className="kn-badge kn-badge-secondary">×{s.repeats}</span> : null}
                </span>
                <span className="ks-meta">{s.detail}</span>
              </div>
              <span className="ks-meta ks-num" style={{ textAlign: 'right' }}>{canceled && s.state === 'working' ? 'canceled' : s.meta}</span>
            </li>
          ))}
        </ol>
      )}

      {view === 'tl' && (
        <div className="ks-panel ks-stack-3">
          <div className="ks-row ks-meta">
            {outcomeLabels.map(([c, l]) => (
              <span key={l} className="ks-row" style={{ gap: 4 }}><span className="ks-swatch" style={{ background: c }} />{l}</span>
            ))}
            <span className="ks-row" style={{ gap: 4 }}><span className="ks-swatch" style={{ boxShadow: 'inset 0 0 0 2px var(--amber-700)' }} />waiting</span>
          </div>
          <div className="ks-tl">
            {runDetail.bars.map((b, i) => (
              <div key={i} className="ks-tl-row">
                <span className="ks-mono" style={{ fontSize: 12 }}>{b.name}</span>
                <span className="ks-tl-track"><span className="ks-bar" data-o={b.outcome} style={{ left: `${b.left}%`, width: `${b.width}%` }} title={`${b.name}: ${b.label}`} /></span>
                <span className="ks-meta ks-num" style={{ textAlign: 'right' }}>{b.label}</span>
              </div>
            ))}
          </div>
          <div className="ks-ticks" aria-hidden="true" style={{ marginLeft: 232, marginRight: 102 }}>
            {timelineTicks.map((t) => (
              <span key={t.label} style={{ left: `${t.at}%` }}>{t.label}</span>
            ))}
          </div>
        </div>
      )}

      {view === 'raw' && <pre className="ks-code" tabIndex={0} aria-label="Raw events">{rawEvents.join('\n')}</pre>}

      <section className="ks-stack-3" aria-labelledby="log-h">
        <div className="ks-between">
          <div className="ks-row">
            <h2 id="log-h" className="ks-h4">Log</h2>
            <span className="ks-meta">Lines {lo === hi ? `L${lo}` : `L${lo} to L${hi}`} linked · secrets are redacted before they are stored</span>
          </div>
          <div className="ks-row">
            <Button variant="outline" size="xs" onClick={() => flash('lines')}>{copied === 'lines' ? 'Link copied' : `Copy link to ${rangeLabel}`}</Button>
            <Button variant="ghost" size="xs" aria-pressed={follow} onClick={() => setFollow((f) => !f)}>{follow ? 'Following live' : 'Follow live'}</Button>
          </div>
        </div>
        <div className="ks-log" role="log" aria-label="Run log" ref={logRef}>
          {log.map((l) => (
            <div key={l.n} className="ks-line" id={`L${l.n}`} data-hl={l.n >= lo && l.n <= hi ? 'true' : 'false'} data-lv={l.level ?? ''}>
              <a href={`#L${l.n}`} aria-label={`Line ${l.n}`} onClick={(e) => { e.preventDefault(); pick(l.n, e.shiftKey) }}>{l.n}</a>
              <span className="ks-ts">{l.time}</span>
              <span className="ks-msg">
                {l.msg}
                {l.redactedAfter !== undefined && (
                  <>
                    <span className="ks-redacted">[REDACTED]</span>
                    {l.redactedAfter}
                  </>
                )}
              </span>
            </div>
          ))}
        </div>
        <div className="ks-raised ks-row" role="status" style={{ padding: '10px 12px', flexWrap: 'nowrap' }}>
          <StateDot state={state} />
          <span className="ks-grow">
            {canceled ? <span className="ks-meta">Run canceled. Nothing is running.</span> : (
              <>
                <span className="ks-meta">Now </span>Writing the pull request body for <span className="ks-mono">platform-gitops</span>…
              </>
            )}
          </span>
          {!canceled && <span className="ks-meta ks-num">{seconds} s</span>}
        </div>
        <div className="ks-panel ks-stack" ref={steerRef}>
          <Composer onSend={steer} label="Steer this run" formLabel="Steer this run" submitLabel="Send steer" idPrefix="steer" disabled={canceled} />
          {steerNote && <p className="ks-p" role="status" style={{ color: 'var(--success)' }}>{steerNote}</p>}
          <p className="ks-p ks-meta ks-num">Applies after the current step. A steer is one extra model turn: about 6k tokens, roughly $0.02 on gpt-6-luna, counted against LCM Upgrader's limit.</p>
        </div>
      </section>

      <Dialog
        open={cancelOpen}
        onOpenChange={setCancelOpen}
        title="Cancel this run?"
        description="The run stops after the current step. Work already done stays in the log; the pull request is not opened."
        footer={
          <>
            <Button variant="outline" onClick={() => setCancelOpen(false)}>Keep running</Button>
            <Button variant="destructive" onClick={() => { setCanceled(true); setCancelOpen(false) }}>Cancel run</Button>
          </>
        }
      />
    </PreviewShell>
  )
}
