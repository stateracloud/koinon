// SPDX-License-Identifier: Apache-2.0

import { useState } from 'react'
import {
  Alert,
  AlertDescription,
  AlertTitle,
  AgentAvatar,
  Badge,
  Button,
  ButtonLink,
  Dialog,
  Panel,
  PreviewLink,
  PreviewShell,
  StatusBadge,
  Table,
  Textarea,
  usePaletteSelection,
  usePreviewNavigate,
} from '@/preview/ui'
import { lcmTriggers } from '@/preview/mock/data'
import { diffLines, modelOptions, olderVersions, rollback, skillRows, type ModelChoice } from '@/preview/agents/data'

const AGENT_HREF = '/preview/agents/lcm-upgrader'
const ROLLBACK_HREF = '/preview/agents/lcm-upgrader/rollback'

type Staged = 'staged' | 'applied' | 'discarded'

export function Agent() {
  const [diff, setDiff] = useState(true)
  const [started, setStarted] = useState(false)
  const [paused, setPaused] = useState(false)
  const [staged, setStaged] = useState<Staged>('staged')
  const [applyTo, setApplyTo] = useState<'new' | 'restart'>('new')
  const [choice, setChoice] = useState<ModelChoice>('pin')

  const runLine = started ? 'Run 2e8f0b71 is starting · manual · you' : 'Run 9c41d07a is running · trigger · 18 min'
  const applied = staged === 'applied'
  const activeVersion = applied ? 'v8' : 'v7'
  const effective = modelOptions.find((m) => m.id === choice)!.model
  const winner = choice === 'inherit' ? 'org' : 'pin'

  usePaletteSelection({
    key: `agent-lcm-${paused}`,
    title: 'LCM Upgrader',
    heading: 'This agent',
    actions: [
      { label: 'Run now', onSelect: () => setStarted(true) },
      { label: paused ? 'Resume' : 'Pause', onSelect: () => setPaused((p) => !p) },
    ],
  })

  const precedence = [
    { id: 'platform', label: 'Platform default', value: 'claude-haiku-5.5' },
    { id: 'org', label: 'Org default', value: 'deepseek-v4.1-flash' },
    { id: 'template', label: 'Template LCM Upgrader v3', value: 'gpt-6-luna' },
    { id: 'pin', label: `Pinned in agent version ${activeVersion}`, value: choice === 'haiku' ? 'claude-haiku-5.5' : 'gpt-6-luna' },
  ]

  return (
    <PreviewShell space="org" current="Overview" crumbs={[{ label: 'Agents', href: '/preview/overview' }, { label: 'LCM Upgrader' }]}>
      <div className="ks-between" style={{ alignItems: 'flex-start' }}>
        <div className="ks-row" style={{ flexWrap: 'nowrap', alignItems: 'flex-start' }}>
          <AgentAvatar name="LCM Upgrader" size="lg" decorative />
          <div className="ks-stack" style={{ gap: 4 }}>
            <div className="ks-row">
              <h1 className="ks-h1">LCM Upgrader</h1>
              <StatusBadge kind="agent" state={paused ? 'paused' : 'active'} />
            </div>
            <span className="ks-meta">Upgrades · reports to no agent · from the LCM Upgrader template v3 · active version {activeVersion}</span>
            <span className="ks-row ks-meta" role="status">
              <span className="ks-dot" data-st="working" aria-hidden="true" />
              {runLine}
            </span>
          </div>
        </div>
        <div className="ks-row">
          <ButtonLink variant="outline" size="sm" href="/preview/runs">
            Runs
          </ButtonLink>
          <ButtonLink variant="outline" size="sm" href="/preview/spend">
            Spend
          </ButtonLink>
          <Button variant="outline" size="sm" onClick={() => setPaused((p) => !p)}>
            {paused ? 'Resume' : 'Pause'}
          </Button>
          <Button size="sm" onClick={() => setStarted(true)} disabled={paused} title={paused ? 'Resume the agent first' : undefined}>
            Run now
          </Button>
        </div>
      </div>

      {applied && (
        <Alert variant="success" role="status">
          <AlertTitle>Version v8 is active</AlertTitle>
          <AlertDescription>
            {applyTo === 'new' ? 'New runs start on v8; run 9c41d07a finishes on v7.' : 'New runs start on v8 and run 9c41d07a was restarted on it.'}
          </AlertDescription>
        </Alert>
      )}

      {staged === 'staged' && (
        <section className="ks-panel ks-stack-3" aria-labelledby="staged-h" style={{ borderColor: 'var(--warning-border)' }}>
          <div className="ks-between">
            <div className="ks-stack" style={{ gap: 2 }}>
              <h2 id="staged-h" className="ks-h4">
                2 staged changes, not applied
              </h2>
              <span className="ks-meta">They become agent version v8 when you apply them.</span>
            </div>
            <div className="ks-row">
              <Button variant="ghost" size="sm" onClick={() => setStaged('discarded')}>
                Discard
              </Button>
              <Button variant="outline" size="sm" aria-expanded={diff} aria-controls="staged-diff" onClick={() => setDiff((d) => !d)}>
                {diff ? 'Hide diff' : 'Review diff'}
              </Button>
            </div>
          </div>
          {diff && (
            <div id="staged-diff" className="ks-stack-3">
              <Alert variant="warning" role="status">
                <AlertTitle>Changed since you opened it</AlertTitle>
                <AlertDescription>Maria Okafor added the external-dns trigger 6 minutes ago. Your diff is against her version.</AlertDescription>
              </Alert>
              <div className="ks-diff" role="group" aria-label="Diff from v7">
                {diffLines.map((l) => (
                  <div key={l.text} className={l.kind === 'add' ? 'ks-add' : l.kind === 'del' ? 'ks-del' : 'ks-muted'}>
                    {l.text}
                  </div>
                ))}
              </div>
              <fieldset className="ks-stack" style={{ border: 0, padding: 0, margin: 0 }}>
                <legend className="kn-label" style={{ marginBottom: 6 }}>
                  Apply to
                </legend>
                <label className="ks-row">
                  <input type="radio" name="apply" checked={applyTo === 'new'} onChange={() => setApplyTo('new')} /> New runs only <span className="ks-meta">· run 9c41d07a finishes on v7</span>
                </label>
                <label className="ks-row">
                  <input type="radio" name="apply" checked={applyTo === 'restart'} onChange={() => setApplyTo('restart')} /> New runs, and restart the 1 running run{' '}
                  <span className="ks-meta">· its progress since the last checkpoint is lost</span>
                </label>
              </fieldset>
              <div className="ks-row" style={{ justifyContent: 'flex-end' }}>
                <span className="ks-meta" style={{ marginRight: 'auto' }}>
                  v8 can be saved now and activated once helm-upgrade v5 is approved.
                </span>
                <Button onClick={() => setStaged('applied')}>Apply as v8</Button>
              </div>
            </div>
          )}
        </section>
      )}

      <div className="ks-grid-2">
        <section className="ks-panel ks-stack-3" aria-labelledby="model-h">
          <h2 id="model-h" className="ks-h4">
            Model
          </h2>
          <div className="ks-stack">
            <label className="kn-label" htmlFor="a-model">
              Model for this agent
            </label>
            <select id="a-model" className="kn-field kn-native-select" value={choice} onChange={(e) => setChoice(e.target.value as ModelChoice)}>
              {modelOptions.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.label}
                </option>
              ))}
            </select>
          </div>
          <div className="ks-raised ks-stack" style={{ padding: 12 }}>
            <span>
              <span className="ks-meta">Effective model </span>
              <span className="ks-mono ks-strong" role="status">
                {effective}
              </span>
            </span>
            <ol className="ks-ul ks-stack" style={{ gap: 4, fontSize: 13 }} aria-label="Model precedence, least to most specific">
              {precedence.map((p) => {
                const wins = p.id === winner
                const lost = !wins
                return (
                  <li key={p.id} className="ks-between">
                    <span className={wins ? undefined : 'ks-muted'} style={wins ? { fontWeight: 500 } : undefined}>
                      {p.label}
                    </span>
                    <span className={wins ? 'ks-mono' : 'ks-mono ks-muted'} style={lost ? { textDecoration: 'line-through' } : undefined}>
                      {p.value}
                    </span>
                  </li>
                )
              })}
            </ol>
            <span className="ks-meta">
              The most specific setting wins. {applied ? 'Applied: claude-haiku-5.5 in v8.' : staged === 'staged' ? 'Staged: claude-haiku-5.5 in v8.' : 'Nothing staged.'}
            </span>
          </div>
        </section>

        <section className="ks-panel ks-stack-3" aria-labelledby="trig-h">
          <div className="ks-between">
            <h2 id="trig-h" className="ks-h4">
              Triggers
            </h2>
            <Button variant="outline" size="xs">
              Add trigger
            </Button>
          </div>
          <ul className="ks-ul ks-stack">
            {lcmTriggers.map((t) => (
              <li key={t.name} className="ks-raised" style={{ padding: '10px 12px', display: 'grid', gridTemplateColumns: '1fr auto', gap: 8, alignItems: 'center' }}>
                <span className="ks-stack" style={{ gap: 2 }}>
                  <span style={{ fontWeight: 500 }}>{t.name}</span>
                  <span className="ks-meta">{t.what}</span>
                  <span className="ks-meta ks-num">{t.when}</span>
                </span>
                <span className="ks-row">
                  <Button variant="outline" size="xs" aria-label={`Run now from ${t.name}`} disabled={paused} onClick={() => setStarted(true)}>
                    Run now
                  </Button>
                </span>
              </li>
            ))}
          </ul>
        </section>
      </div>

      <section className="ks-panel ks-stack-3" aria-labelledby="sk-h">
        <div className="ks-between">
          <h2 id="sk-h" className="ks-h4">
            Skills and MCP servers
          </h2>
          <ButtonLink variant="outline" size="xs" href="/preview/marketplace">
            Add from the Marketplace
          </ButtonLink>
        </div>
        <Table label="Skills and MCP servers">
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">Kind</th>
              <th scope="col">Pinned</th>
              <th scope="col">Status</th>
              <th scope="col" className="ks-r">
                Prompt cost
              </th>
              <th scope="col">Notes</th>
            </tr>
          </thead>
          <tbody>
            {skillRows.map((r) => (
              <tr key={r.name}>
                <td>
                  {r.link ? (
                    <PreviewLink className="ks-a ks-mono" href="/preview/skills/helm-upgrade">
                      {r.name}
                    </PreviewLink>
                  ) : (
                    r.name
                  )}
                </td>
                <td>{r.kind}</td>
                <td className="ks-mono">{r.pinned}</td>
                <td>{r.status.type === 'approved' ? <Badge>Approved</Badge> : <StatusBadge kind="connection" state={r.status.state} />}</td>
                <td className="ks-r">{r.cost}</td>
                <td className={r.notes.tone === 'warn' ? 'ks-warn' : r.notes.tone === 'muted' ? 'ks-muted' : undefined}>
                  {r.notes.tone === 'info' ? <span className="ks-info">{r.notes.text}</span> : r.notes.text}
                  {r.notes.extra && (staged === 'staged' || applied ? r.notes.extra : '')}
                </td>
              </tr>
            ))}
          </tbody>
        </Table>
      </section>

      <section className="ks-panel ks-stack-3" aria-labelledby="ver-h">
        <div className="ks-between">
          <h2 id="ver-h" className="ks-h4">
            Versions
          </h2>
          <span className="ks-meta">Promote makes a version active. Rollback is instant and keeps secrets and memory.</span>
        </div>
        <Table label="Versions">
          <thead>
            <tr>
              <th scope="col">Version</th>
              <th scope="col">Stage</th>
              <th scope="col">Change</th>
              <th scope="col">By</th>
              <th scope="col">When</th>
              <th scope="col">
                <span className="ks-sr">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {staged !== 'discarded' && (
              <tr aria-selected={applied || undefined}>
                <td className="ks-mono">v8</td>
                <td>{applied ? <Badge variant="default">Active</Badge> : <Badge variant="outline">Staged</Badge>}</td>
                <td>Model to claude-haiku-5.5; helm-upgrade v5</td>
                <td>Andre Staats</td>
                <td className="ks-num">now</td>
                <td className="ks-r">
                  {applied ? null : (
                    <Button variant="outline" size="xs" onClick={() => setStaged('applied')}>
                      Verify and promote
                    </Button>
                  )}
                </td>
              </tr>
            )}
            <tr aria-selected={!applied || undefined}>
              <td className="ks-mono">v7</td>
              <td>{applied ? <Badge>Previous</Badge> : <Badge variant="default">Active</Badge>}</td>
              <td>Added the external-dns trigger</td>
              <td>Maria Okafor</td>
              <td className="ks-num">21 Oct</td>
              <td className="ks-r">
                {applied && (
                  <ButtonLink variant="outline" size="xs" href={ROLLBACK_HREF}>
                    Instant rollback
                  </ButtonLink>
                )}
              </td>
            </tr>
            {olderVersions.map((v, i) => (
              <tr key={v.id}>
                <td className="ks-mono">{v.id}</td>
                <td>
                  <Badge>Previous</Badge>
                </td>
                <td>{v.change}</td>
                <td>{v.by}</td>
                <td className="ks-num">{v.when}</td>
                <td className="ks-r">
                  <ButtonLink variant={i === 0 ? 'outline' : 'ghost'} size="xs" href={ROLLBACK_HREF} aria-label={`Instant rollback to ${v.id}`}>
                    Instant rollback
                  </ButtonLink>
                </td>
              </tr>
            ))}
          </tbody>
        </Table>
      </section>
    </PreviewShell>
  )
}

type Step = 'verify' | 'reason' | 'done'

export function AgentRollback() {
  const navigate = usePreviewNavigate()
  const [step, setStep] = useState<Step>('verify')
  const [reason, setReason] = useState(rollback.reason)
  const leave = () => navigate(AGENT_HREF)
  const label = (text: string) => (
    <span className="ks-label" style={{ display: 'block', marginBottom: 4 }}>
      {text}
    </span>
  )
  return (
    <PreviewShell space="org" current="Overview" crumbs={[{ label: 'LCM Upgrader', href: AGENT_HREF }, { label: 'Versions' }]}>
      <h1 className="ks-h1">LCM Upgrader · versions</h1>
      <Panel style={{ height: 360 }} />
      {step === 'verify' && (
        <Dialog
          open
          size="wide"
          onOpenChange={(o) => !o && leave()}
          title={
            <>
              {label('Step 1 of 2 · Verify')}
              Roll back LCM Upgrader from {rollback.from} to {rollback.to}?
            </>
          }
          description={`The switch is instant. New runs start on ${rollback.to}; run 9c41d07a finishes on ${rollback.from}.`}
          footer={
            <>
              <Button variant="outline" onClick={leave}>
                Cancel
              </Button>
              <Button onClick={() => setStep('reason')}>Continue</Button>
            </>
          }
        >
          <div className="ks-grid-2" style={{ gap: 12 }}>
            <div className="ks-raised ks-stack" style={{ padding: 12 }}>
              <h3 className="ks-h4">What changes</h3>
              <ul className="ks-ul ks-stack" style={{ gap: 4, fontSize: 13 }}>
                {rollback.changes.map((c) => (
                  <li key={c}>{c}</li>
                ))}
              </ul>
            </div>
            <div className="ks-raised ks-stack" style={{ padding: 12 }}>
              <h3 className="ks-h4">What does not change</h3>
              <ul className="ks-ul ks-stack" style={{ gap: 4, fontSize: 13 }}>
                {rollback.unchanged.map((c) => (
                  <li key={c}>{c}</li>
                ))}
              </ul>
            </div>
          </div>
          <Alert role="status">
            <AlertTitle>Auto-promote pauses</AlertTitle>
            <AlertDescription>New template versions stop promoting themselves for this agent until you undo the rollback.</AlertDescription>
          </Alert>
        </Dialog>
      )}
      {step === 'reason' && (
        <Dialog
          open
          size="wide"
          onOpenChange={(o) => !o && leave()}
          title={
            <>
              {label('Step 2 of 2 · Reason')}
              Why are you rolling back?
            </>
          }
          description="The reason goes into the audit log and onto the version."
          footer={
            <>
              <Button variant="outline" onClick={() => setStep('verify')}>
                Back
              </Button>
              <Button onClick={() => setStep('done')} disabled={reason.trim() === ''}>
                Roll back to {rollback.to}
              </Button>
            </>
          }
        >
          <div className="ks-stack">
            <label className="kn-label" htmlFor="rb-why">
              Reason
            </label>
            <Textarea id="rb-why" value={reason} onChange={(e) => setReason(e.target.value)} />
          </div>
        </Dialog>
      )}
      {step === 'done' && (
        <Dialog
          open
          size="wide"
          hideClose
          onOpenChange={(o) => !o && leave()}
          title="Rollback done"
          footer={
            <>
              <Button variant="outline" onClick={() => setStep('verify')}>
                Undo rollback
              </Button>
              <Button onClick={leave}>Done</Button>
            </>
          }
        >
          <Alert variant="success" role="status">
            <AlertTitle>LCM Upgrader runs {rollback.to}</AlertTitle>
            <AlertDescription>Rolled back by Andre Staats at 14:22. Auto-promote is paused.</AlertDescription>
          </Alert>
        </Dialog>
      )}
    </PreviewShell>
  )
}
