// SPDX-License-Identifier: Apache-2.0

// Group "onboarding": first login (join, create or ask, then the spend limit) and Idion (meet Daimon, home).

import { useEffect, useId, useRef, useState, type FormEvent } from 'react'
import { Send } from 'lucide-react'
import { Cloudy } from '@/preview/onboarding/cloud'
import { alertThresholds, budget, cannedReply, chat as seedChat, daimon, defaults, doneCopy, invite, memory, reminder, schedules, slugOf, steps, type ChatLine, type OnboardingPath, type OnboardingStep } from '@/preview/onboarding/data'
import { AgeChip, Button, ButtonLink, Composer, EmptyState, Logo, Meter, PreviewLink, PreviewShell, SpaceMark, ThemeSwitch, usePreviewNavigate } from '@/preview/ui'
import { me } from '@/preview/mock/data'

const bareRoot = { position: 'relative', display: 'grid', justifyItems: 'center', alignContent: 'start', gap: 24, padding: '48px 16px', minHeight: '100vh' } as const

function Stepper({ index }: { index: number }) {
  return (
    <ol className="ks-ul ks-row" aria-label="Steps" style={{ gap: 16 }}>
      {steps.map((label, i) => {
        const active = i === index
        return (
          <li key={label} className="ks-row" style={{ gap: 6, fontSize: 13 }} aria-current={active ? 'step' : undefined}>
            <span className="ks-kbd" style={active ? { background: 'var(--primary)', color: 'var(--primary-foreground)', borderColor: 'var(--primary)' } : undefined}>
              {i < index ? '✓' : i + 1}
            </span>
            <span className={active ? '' : 'ks-muted'}>{label}</span>
          </li>
        )
      })}
    </ol>
  )
}

function Check({ checked, onChange, children }: { checked: boolean; onChange: (v: boolean) => void; children: string }) {
  return (
    <label className="ks-pill" aria-pressed={checked}>
      <input type="checkbox" checked={checked} onChange={(e) => onChange(e.target.checked)} /> {children}
    </label>
  )
}

export function FirstLoginPage() {
  const go = usePreviewNavigate()
  const [step, setStep] = useState<OnboardingStep>('choose')
  const [path, setPath] = useState<OnboardingPath>('create')
  const [orgName, setOrgName] = useState(defaults.orgName)
  const [ask, setAsk] = useState(defaults.askText)
  const [limit, setLimit] = useState(defaults.limit)
  const [alerts, setAlerts] = useState<number[]>([...alertThresholds])
  const [pause, setPause] = useState(true)
  const [declined, setDeclined] = useState(false)
  const heading = useRef<HTMLHeadingElement>(null)
  const first = useRef(true)
  const ids = useId()

  // Moving between steps replaces the content, so focus the new heading for keyboard and screen reader users.
  useEffect(() => {
    if (first.current) {
      first.current = false
      return
    }
    heading.current?.focus()
  }, [step])

  const name = orgName.trim() || defaults.orgName
  const amount = Number(limit.replace(',', '.'))
  const limitError = !limit.trim() || Number.isNaN(amount) || amount <= 0 ? 'Enter an amount above $0.00.' : amount > defaults.platformCap ? `The platform cap is $${defaults.platformCap.toFixed(2)}.` : ''
  const amountText = limitError ? defaults.limit : amount.toFixed(2)
  const index = { choose: 0, limit: 1, done: 2 }[step]
  const [doneTitle, doneText] = doneCopy(path, name, amountText, alerts, pause)

  function saveLimit(e: FormEvent) {
    e.preventDefault()
    if (limitError) return
    setStep('done')
  }

  return (
    <Cloudy style={bareRoot}>
      <div style={{ position: 'absolute', top: 16, right: 16 }}>
        <ThemeSwitch id="fl-theme" />
      </div>
      <Logo layout="stacked" size={48} />
      <Stepper index={index} />

      {step === 'choose' && (
        <div className="ks-stack-4" style={{ width: 'min(100%, 1040px)' }}>
          <div className="ks-stack" style={{ textAlign: 'center', justifyItems: 'center' }}>
            <h1 className="ks-h1" ref={heading} tabIndex={-1}>
              Welcome, {me.name.split(' ')[0]}
            </h1>
            <p className="ks-p ks-muted" style={{ maxWidth: '60ch' }}>
              Agents work inside an organization. Join the one you were invited to, create your own, or ask a platform admin for one. Your personal space, Idion, is ready either way.
            </p>
          </div>
          <div className="ks-grid-4" style={{ gridTemplateColumns: 'repeat(auto-fit, minmax(280px, 1fr))', gap: 16 }}>
            <div className="ks-card-a">
              <div className="ks-row">
                <SpaceMark hue="blue">{invite.initials}</SpaceMark>
                <h2 className="ks-h3">Join {invite.org}</h2>
              </div>
              {declined ? (
                <p className="ks-p ks-muted" role="status">
                  Invitation declined. {invite.inviter} can invite you again, and you can join later from the space switcher.
                </p>
              ) : (
                <>
                  <p className="ks-p ks-muted">
                    {invite.inviter} invited you on {invite.date}. Your identity provider also maps the group <span className="ks-mono">{invite.group}</span> to this org.
                  </p>
                  <dl className="ks-kv">
                    <dt>Your role</dt>
                    <dd>{invite.role}</dd>
                    <dt>Agents</dt>
                    <dd>{invite.agents}</dd>
                    <dt>Daily limit</dt>
                    <dd className="ks-num">{invite.dailyLimit}</dd>
                  </dl>
                </>
              )}
              <div className="ks-row">
                {declined ? (
                  <Button variant="outline" onClick={() => setDeclined(false)}>
                    Undo
                  </Button>
                ) : (
                  <>
                    <Button
                      onClick={() => {
                        setPath('join')
                        setStep('done')
                      }}
                    >
                      Join
                    </Button>
                    <Button variant="ghost" onClick={() => setDeclined(true)}>
                      Decline
                    </Button>
                  </>
                )}
              </div>
            </div>

            <form
              className="ks-card-a"
              onSubmit={(e) => {
                e.preventDefault()
                if (!orgName.trim()) return
                setPath('create')
                setStep('limit')
              }}
            >
              <div className="ks-row">
                <span className="ks-tile-icon" aria-hidden="true">
                  <svg className="ks-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                    <path d="M5 12h14" />
                    <path d="M12 5v14" />
                  </svg>
                </span>
                <h2 className="ks-h3">Create an org</h2>
              </div>
              <p className="ks-p ks-muted">Self-service is on for this install. You become the owner and set its spend limit next.</p>
              <div className="ks-stack">
                <label className="kn-label" htmlFor={`${ids}-org`}>
                  Org name
                </label>
                <input id={`${ids}-org`} className="kn-field kn-input" type="text" value={orgName} onChange={(e) => setOrgName(e.target.value)} aria-describedby={`${ids}-addr`} aria-invalid={!orgName.trim()} />
                <span className="ks-meta" id={`${ids}-addr`}>
                  Address: <span className="ks-mono">/o/{slugOf(orgName)}</span>
                </span>
              </div>
              <div className="ks-row">
                <Button type="submit" variant="outline" disabled={!orgName.trim()}>
                  Create and continue
                </Button>
              </div>
            </form>

            <form
              className="ks-card-a"
              onSubmit={(e) => {
                e.preventDefault()
                setPath('ask')
                setStep('done')
              }}
            >
              <div className="ks-row">
                <span className="ks-tile-icon" aria-hidden="true">
                  <Send className="ks-icon" />
                </span>
                <h2 className="ks-h3">Ask a platform admin</h2>
              </div>
              <p className="ks-p ks-muted">When self-service is off, this is the only way to a new org. The request lands in the platform admins' inbox.</p>
              <div className="ks-stack">
                <label className="kn-label" htmlFor={`${ids}-ask`}>
                  What the org is for
                </label>
                <textarea id={`${ids}-ask`} className="kn-field kn-textarea" value={ask} onChange={(e) => setAsk(e.target.value)} />
              </div>
              <div className="ks-row">
                <Button type="submit" variant="outline">
                  Send request
                </Button>
              </div>
            </form>
          </div>
          <p className="ks-p ks-meta" style={{ textAlign: 'center' }}>
            Not now?{' '}
            <PreviewLink className="ks-link" href="/preview/idion/welcome">
              Go to Idion
            </PreviewLink>
            . You can join or create an org later from the space switcher.
          </p>
        </div>
      )}

      {step === 'limit' && (
        <form className="ks-panel ks-stack-4" style={{ width: 'min(100%, 560px)', padding: 24, boxShadow: 'var(--shadow-md)' }} onSubmit={saveLimit} noValidate>
          <div className="ks-stack" style={{ gap: 4 }}>
            <h1 className="ks-h2" ref={heading} tabIndex={-1}>
              Set a spend limit for {name}
            </h1>
            <p className="ks-p ks-muted">Agents spend money on every model call. Koinon asks before the first agent runs, so nothing spends without a limit.</p>
          </div>
          <div className="ks-stack">
            <label className="kn-label" htmlFor={`${ids}-limit`}>
              Daily limit for the org
            </label>
            <div className="ks-row" style={{ flexWrap: 'nowrap' }}>
              <span className="ks-muted" aria-hidden="true">
                $
              </span>
              <input id={`${ids}-limit`} className="kn-field kn-input ks-num" type="text" inputMode="decimal" value={limit} onChange={(e) => setLimit(e.target.value)} style={{ width: 140 }} aria-invalid={!!limitError} aria-describedby={`${ids}-limit-hint`} />
              <span className="ks-muted">per day, in USD</span>
            </div>
            {limitError && (
              <span className="ks-meta" role="alert" style={{ color: 'var(--danger)' }}>
                {limitError}
              </span>
            )}
            <span className="ks-meta" id={`${ids}-limit-hint`}>
              The platform default is ${defaults.platformDefault.toFixed(2)} and the platform cap is ${defaults.platformCap.toFixed(2)}. Each new agent gets a default of ${defaults.agentDefault.toFixed(2)} a day.
            </span>
          </div>
          <fieldset className="ks-stack" style={{ border: 0, padding: 0, margin: 0 }}>
            <legend className="kn-label" style={{ marginBottom: 8 }}>
              Alert the owners at
            </legend>
            <div className="ks-row">
              {alertThresholds.map((t) => (
                <Check key={t} checked={alerts.includes(t)} onChange={(on) => setAlerts((a) => (on ? [...a, t].sort((x, y) => x - y) : a.filter((x) => x !== t)))}>
                  {`${t}%`}
                </Check>
              ))}
            </div>
          </fieldset>
          <label className="ks-row" style={{ alignItems: 'flex-start', flexWrap: 'nowrap' }}>
            <input type="checkbox" checked={pause} onChange={(e) => setPause(e.target.checked)} style={{ marginTop: 3 }} />
            <span className="ks-stack" style={{ gap: 2 }}>
              <span style={{ fontWeight: 500 }}>Pause agents at the limit</span>
              <span className="ks-meta">A hard stop. Agents stay paused until an owner resumes them by typing the org name.</span>
            </span>
          </label>
          <div className="ks-row" style={{ justifyContent: 'flex-end' }}>
            <Button type="button" variant="outline" onClick={() => setStep('choose')}>
              Back
            </Button>
            <Button type="submit">Save limit</Button>
          </div>
        </form>
      )}

      {step === 'done' && (
        <div className="ks-panel" style={{ width: 'min(100%, 560px)' }}>
          <div role="status">
            <EmptyState title={doneTitle}>{doneText}</EmptyState>
          </div>
          <div className="ks-row" style={{ justifyContent: 'center', padding: '0 16px 8px' }}>
            <ButtonLink href="/preview/agents/new">Create your first agent</ButtonLink>
            <ButtonLink variant="outline" href="/preview/overview">
              Open the overview
            </ButtonLink>
            {path === 'ask' && (
              <Button variant="ghost" onClick={() => go('/preview/idion/welcome')}>
                Go to Idion
              </Button>
            )}
          </div>
        </div>
      )}
    </Cloudy>
  )
}

export function IdionFirstRunPage() {
  const go = usePreviewNavigate()
  const [name, setName] = useState(daimon.defaultName)
  const shown = name.trim() || daimon.defaultName
  const id = useId()
  return (
    <Cloudy style={{ ...bareRoot, gap: 28 }}>
      <div style={{ position: 'absolute', top: 16, right: 16 }}>
        <ThemeSwitch id="fr-theme" />
      </div>
      <Logo product="idion" size={28} />
      <form
        className="ks-panel ks-stack-4"
        style={{ width: 'min(100%, 640px)', padding: 28, boxShadow: 'var(--shadow-md)' }}
        onSubmit={(e) => {
          e.preventDefault()
          go('/preview/idion')
        }}
      >
        <div className="ks-row" style={{ flexWrap: 'nowrap', alignItems: 'flex-start', gap: 16 }}>
          <span className="ks-daimon" aria-hidden="true" style={{ width: 72, height: 72, fontSize: 26, flexShrink: 0 }}>
            {shown.charAt(0).toUpperCase()}
          </span>
          <div className="ks-stack" style={{ gap: 6 }}>
            <span className="ks-label ks-idion-accent">Idion · your own space</span>
            <h1 className="ks-h1">Meet {shown}</h1>
            <p className="ks-p ks-muted">{shown} works for you alone: it keeps your notes and reminders, watches your inbox across every org, and asks team agents for help when they allow it.</p>
          </div>
        </div>
        <div className="ks-stack">
          <label className="kn-label" htmlFor={`${id}-name`}>
            What should it be called?
          </label>
          <input id={`${id}-name`} className="kn-field kn-input" type="text" value={name} onChange={(e) => setName(e.target.value)} onBlur={() => setName((n) => n.trim() || daimon.defaultName)} aria-describedby={`${id}-hint`} maxLength={32} />
          <span className="ks-meta" id={`${id}-hint`}>
            Daimon is the default. You can rename it later in Idion's settings.
          </span>
        </div>
        <div className="ks-raised ks-stack" style={{ padding: '12px 14px' }}>
          <span style={{ fontWeight: 600 }}>What {shown} can and can't do</span>
          <ul className="ks-ul ks-stack" style={{ gap: 4, fontSize: 13 }}>
            {daimon.abilities.map((a) => (
              <li key={a}>{a}</li>
            ))}
          </ul>
        </div>
        <dl className="ks-kv">
          <dt>Budget</dt>
          <dd className="ks-num">{daimon.budget}</dd>
          <dt>Model</dt>
          <dd>
            <span className="ks-mono">{daimon.model}</span> <span className="ks-meta">· shared with personal spaces</span>
          </dd>
          <dt>Privacy</dt>
          <dd>{daimon.privacy}</dd>
        </dl>
        <div className="ks-row" style={{ justifyContent: 'flex-end' }}>
          <ButtonLink variant="ghost" href="/preview/overview">
            Later
          </ButtonLink>
          <Button type="submit">Start with {shown}</Button>
        </div>
      </form>
    </Cloudy>
  )
}

function DaimonMark({ small }: { small?: boolean }) {
  return (
    <span className="ks-daimon" aria-hidden="true" style={small ? { width: 28, height: 28, fontSize: 12, flexShrink: 0 } : undefined}>
      D
    </span>
  )
}

export function IdionHomePage() {
  const [lines, setLines] = useState<ChatLine[]>(seedChat)
  const [reminderDone, setReminderDone] = useState(false)
  const log = useRef<HTMLDivElement>(null)
  const sent = useRef(0)
  const timer = useRef<number | undefined>(undefined)
  useEffect(() => () => window.clearTimeout(timer.current), [])

  function send(text: string) {
    const n = ++sent.current
    setLines((l) => [...l, { id: `u${n}`, from: 'me', text }])
    window.clearTimeout(timer.current)
    timer.current = window.setTimeout(() => {
      setLines((l) => [...l, { id: `d${n}`, from: 'daimon', text: cannedReply }])
    }, 600)
  }

  useEffect(() => {
    const el = log.current
    if (el && lines.length > seedChat.length) el.lastElementChild?.scrollIntoView({ block: 'nearest' })
  }, [lines])

  const waiting = reminderDone ? 0 : 1
  const pct = Math.round((budget.used / budget.limit) * 100)

  return (
    <PreviewShell space="idion" current="Home" crumbs={[{ label: 'Home' }]} bare>
      <Cloudy style={{ margin: 'calc(var(--space-6) * -1)', padding: 'var(--space-6)', minHeight: '100%' }}>
        <div className="ks-page">
          <div className="ks-row" style={{ flexWrap: 'nowrap' }}>
            <DaimonMark />
            <div className="ks-stack" style={{ gap: 2 }}>
              <h1 className="ks-h1">Good afternoon, {me.name.split(' ')[0]}</h1>
              <span className="ks-meta">
                Daimon · 2 reminders today · {waiting} {waiting === 1 ? 'thing' : 'things'} waiting on you
              </span>
            </div>
          </div>
          <div className="ks-split">
            <section className="ks-main-part ks-panel ks-stack-4" aria-labelledby="chat-h" style={{ padding: 20 }}>
              <h2 id="chat-h" className="ks-sr">
                Conversation with Daimon
              </h2>
              <div className="ks-stack-3" role="log" aria-label="Conversation" ref={log}>
                {lines.map((l) =>
                  l.from === 'me' ? (
                    <div key={l.id} className="ks-bubble ks-bubble-me">
                      {l.text}
                    </div>
                  ) : (
                    <div key={l.id} className="ks-row" style={{ flexWrap: 'nowrap', alignItems: 'flex-start' }}>
                      <DaimonMark small />
                      <div className="ks-bubble ks-stack" style={{ gap: 6 }}>
                        <span>{l.text}</span>
                        {l.meta && <span className="ks-meta">{l.meta}</span>}
                        {l.actions && (
                          <span className="ks-row">
                            {l.actions.map((a) => (
                              <ButtonLink key={a.label} href={a.href} variant={a.primary ? 'outline' : 'ghost'} size="xs">
                                {a.label}
                              </ButtonLink>
                            ))}
                          </span>
                        )}
                      </div>
                    </div>
                  ),
                )}
              </div>
              <Composer onSend={send} label="Message Daimon" formLabel="Message Daimon" submitLabel="Send" idPrefix="daimon" />
            </section>
            <aside className="ks-aside ks-stack-4">
              <section className="ks-panel ks-stack-3" aria-labelledby="w-h">
                <div className="ks-between">
                  <h2 id="w-h" className="ks-h4">
                    Waiting on you
                  </h2>
                  <PreviewLink className="ks-link" href="/preview/inbox">
                    Inbox
                  </PreviewLink>
                </div>
                {reminderDone ? (
                  <p className="ks-meta" role="status">
                    Nothing is waiting on you.
                  </p>
                ) : (
                  <div className="ks-wait">
                    <div className="ks-between">
                      <span className="ks-label">Reminder</span>
                      <AgeChip hours={reminder.hours} />
                    </div>
                    <span style={{ fontWeight: 500 }}>{reminder.title}</span>
                    <span className="ks-meta">{reminder.meta}</span>
                    <span className="ks-row">
                      <Button variant="outline" size="xs" onClick={() => setReminderDone(true)}>
                        Mark done
                      </Button>
                    </span>
                  </div>
                )}
              </section>
              <section className="ks-panel ks-stack-3" aria-labelledby="t-h">
                <h2 id="t-h" className="ks-h4">
                  Schedules
                </h2>
                <ul className="ks-ul ks-stack" style={{ fontSize: 13 }}>
                  {schedules.map((s) => (
                    <li key={s.id} className="ks-between">
                      <span>{s.name}</span>
                      <span className="ks-meta ks-num">{s.when}</span>
                    </li>
                  ))}
                </ul>
              </section>
              <section className="ks-panel ks-stack-3" aria-labelledby="m-h">
                <div className="ks-between">
                  <h2 id="m-h" className="ks-h4">
                    Memory
                  </h2>
                  <PreviewLink className="ks-link" href="#">
                    Browse
                  </PreviewLink>
                </div>
                <ul className="ks-ul ks-stack" style={{ fontSize: 13 }}>
                  {memory.notes.map((n) => (
                    <li key={n}>{n}</li>
                  ))}
                  <li className="ks-meta">
                    {memory.count} notes · {memory.edited}
                  </li>
                </ul>
              </section>
              <section className="ks-panel ks-stack" aria-labelledby="bd-h">
                <div className="ks-between">
                  <h2 id="bd-h" className="ks-h4">
                    Budget
                  </h2>
                  <span className="ks-num" style={{ fontSize: 12 }}>
                    <span className="ks-strong">${budget.used.toFixed(2)}</span> <span className="ks-muted">of ${budget.limit.toFixed(2)} this month</span>
                  </span>
                </div>
                <Meter used={budget.used} limit={budget.limit} label={`${pct}% of the monthly budget used`} />
                <span className="ks-meta">Paid by the platform. Team agents pay for their own answers.</span>
              </section>
            </aside>
          </div>
        </div>
      </Cloudy>
    </PreviewShell>
  )
}
