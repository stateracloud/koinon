// SPDX-License-Identifier: Apache-2.0

// The inbox (Inbox.dc.html): a list of waiting items and one typed card per kind (scoped plan approval, approval,
// question, tool call with editable arguments, needs-a-reply), with the triage keys J K A E R H U I.

import { useEffect, useEffectEvent, useMemo, useState, type ReactNode } from 'react'
import { useRouterState } from '@tanstack/react-router'
import { inboxItems, type InboxItem } from '@/preview/mock/data'
import { costCenterAnswers, declared, planSteps, type InboxAction } from '@/preview/overview/data'
import {
  Alert,
  AlertDescription,
  AlertTitle,
  AgeChip,
  AgentAvatar,
  Badge,
  Button,
  Dialog,
  DialogClose,
  EmptyState,
  Input,
  Kbd,
  Label,

  PreviewLink,
  PreviewShell,
  StatusBadge,
  Tabs,
  Textarea,
  usePaletteSelection,
  type PaletteAction,
} from '@/preview/ui'

type Tab = 'pri' | 'oth'
type Dialogs = 'respond' | 'snooze' | null

const outcome: Record<string, string> = {
  accept: 'Accepted',
  ignore: 'Ignored',
  respond: 'Response sent',
  snooze: 'Snoozed',
  read: 'Marked read',
}

function waitText(h: number): string {
  return h < 24 ? `${h} h` : `${Math.floor(h / 24)} d ${h % 24} h`
}

export function InboxScreen() {
  const search = useRouterState({ select: (s) => s.location.searchStr })
  const wanted = new URLSearchParams(search).get('item')
  const startItem = inboxItems.find((i) => i.id === wanted) ?? inboxItems.find((i) => i.id === 'plan')!

  const [tab, setTab] = useState<Tab>(startItem.priority ? 'pri' : 'oth')
  const [sel, setSel] = useState(startItem.id)
  // item id -> what happened to it. Resolved items leave the list; "Undo" brings the last one back.
  const [done, setDone] = useState<Record<string, string>>({})
  const [last, setLast] = useState<{ id: string; text: string } | null>(null)
  const [dialog, setDialog] = useState<Dialogs>(null)
  const [response, setResponse] = useState('')
  const [snoozeWhen, setSnoozeWhen] = useState('tomorrow 9:00')
  const [args, setArgs] = useState({ title: 'Evaluate pgvector 0.9 against Qdrant and Weaviate', labels: 'research, vector-db' })
  const [answer, setAnswer] = useState('')

  // A link from the Overview (?item=) while this page is mounted.
  const [seenItem, setSeenItem] = useState(wanted)
  if (seenItem !== wanted) {
    setSeenItem(wanted)
    const it = inboxItems.find((i) => i.id === wanted)
    if (it) {
      setTab(it.priority ? 'pri' : 'oth')
      setSel(it.id)
    }
  }

  const open = useMemo(() => inboxItems.filter((i) => !done[i.id]), [done])
  const list = open.filter((i) => (tab === 'pri' ? i.priority : !i.priority))
  const priCount = open.filter((i) => i.priority).length
  const othCount = open.filter((i) => !i.priority).length
  const current = list.find((i) => i.id === sel) ?? list[0] ?? null

  function resolve(item: InboxItem, action: InboxAction, text?: string) {
    const idx = list.findIndex((i) => i.id === item.id)
    const next = list[idx + 1] ?? list[idx - 1]
    setDone((d) => ({ ...d, [item.id]: outcome[action] }))
    setLast({ id: item.id, text: text ?? `${outcome[action]}: ${item.title}` })
    if (next) setSel(next.id)
    setDialog(null)
    setResponse('')
  }

  function act(action: InboxAction) {
    if (!current || !declared[current.id].includes(action)) return
    if (action === 'edit') return void document.getElementById('e-title')?.focus()
    if (action === 'respond') {
      if (current.id === 'q') return void document.getElementById('q-ans')?.focus()
      return void setDialog('respond')
    }
    if (action === 'snooze') {
      if (current.id === 'snooze') return void document.getElementById('snooze')?.focus()
      return void setDialog('snooze')
    }
    const text =
      current.id === 'plan' && action === 'accept'
        ? 'Plan accepted for 30 min. The run resumes and drops back to read-only afterwards.'
        : current.id === 'pr' && action === 'accept'
          ? 'Accepted: the agent merges the pull request.'
          : undefined
    resolve(current, action, text)
  }

  // The keys read the latest state through effect events, so the window listener is attached once.
  const onAct = useEffectEvent(act)
  const onMove = useEffectEvent((d: number) => {
    if (!list.length) return
    const i = Math.max(0, list.findIndex((x) => x.id === current?.id))
    const target = list[Math.min(list.length - 1, Math.max(0, i + d))]
    setSel(target.id)
    document.getElementById(`item-${target.id}`)?.focus()
  })
  useEffect(() => {
    let g = 0
    function typing(t: EventTarget | null) {
      const el = t as HTMLElement | null
      return !!el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT' || el.isContentEditable)
    }
    function onKey(e: KeyboardEvent) {
      if (e.metaKey || e.ctrlKey || e.altKey || e.defaultPrevented || typing(e.target)) return
      if (document.activeElement?.closest('[role="dialog"],[role="menu"]')) return
      const k = e.key.toLowerCase()
      const afterG = Date.now() - g < 1200
      if (k === 'g') g = Date.now()
      if (afterG) return // "G I" goes to the inbox; it is not "ignore".
      const map: Record<string, InboxAction> = { a: 'accept', e: 'edit', r: 'respond', h: 'snooze', u: 'read', i: 'ignore' }
      if (k === 'j' || k === 'arrowdown') {
        if (k === 'arrowdown' && !(e.target as HTMLElement).closest('[aria-label="Items"]')) return
        e.preventDefault()
        onMove(1)
      } else if (k === 'k' || k === 'arrowup') {
        if (k === 'arrowup' && !(e.target as HTMLElement).closest('[aria-label="Items"]')) return
        e.preventDefault()
        onMove(-1)
      } else if (map[k]) {
        e.preventDefault()
        onAct(map[k])
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  const actions: Record<InboxAction, PaletteAction> = {
    accept: { label: current?.id === 'plan' ? 'Accept plan for 30 min' : current?.id === 'pr' ? 'Accept and merge' : 'Accept', keys: ['A'], onSelect: () => act('accept') },
    edit: { label: 'Edit the arguments', keys: ['E'], onSelect: () => act('edit') },
    respond: { label: 'Respond', keys: ['R'], onSelect: () => act('respond') },
    snooze: { label: 'Snooze…', keys: ['H'], onSelect: () => act('snooze') },
    ignore: { label: 'Ignore', keys: ['I'], onSelect: () => act('ignore') },
    read: { label: 'Mark read', keys: ['U'], onSelect: () => act('read') },
  }
  usePaletteSelection(
    current
      ? { key: current.id, title: current.title, heading: 'For the selected inbox item', actions: declared[current.id].map((a) => actions[a]) }
      : null,
  )

  const total = priCount + othCount

  return (
    <PreviewShell space="org" current="Inbox" crumbs={[{ label: 'Inbox' }]}>
      <div className="ks-head">
        <div className="ks-stack" style={{ gap: 4 }}>
          <h1 className="ks-h1">Inbox</h1>
          <p className="ks-p ks-muted" role="status">
            {total === 0 ? 'Nothing is waiting on you.' : `${total} waiting on you across Platform Engineering and Idion. Oldest first.`}
          </p>
        </div>
        <div className="ks-row ks-meta" role="group" aria-label="Keyboard triage">
          <Kbd>J</Kbd>
          <Kbd>K</Kbd> move <Kbd>A</Kbd> accept <Kbd>E</Kbd> edit <Kbd>R</Kbd> respond <Kbd>H</Kbd> snooze <Kbd>U</Kbd> mark read
        </div>
      </div>

      <div className="ks-row" style={{ alignItems: 'center' }}>
        <div style={{ flex: 1, minWidth: 0 }}>
          <Tabs
            label="Inbox"
            idPrefix="ib"
            value={tab}
            onChange={(t) => {
              setTab(t)
              const first = open.find((i) => (t === 'pri' ? i.priority : !i.priority))
              if (first) setSel(first.id)
            }}
            tabs={[
              { id: 'pri', label: <>Priority <span className="kn-count" style={{ marginLeft: 4 }} aria-label={`${priCount} waiting`}>{priCount}</span></> },
              { id: 'oth', label: 'Other', count: othCount },
            ]}
          />
        </div>
        <span className="ks-meta ks-hide-sm">Priority: approvals, plans and questions that block a run</span>
      </div>

      {last && (
        <Alert variant="success" role="status">
          <AlertDescription style={{ color: 'var(--foreground)', display: 'flex', gap: 12, flexWrap: 'wrap', alignItems: 'center' }}>
            <span>{last.text}</span>
            <Button
              variant="outline"
              size="xs"
              onClick={() => {
                setDone((d) => {
                  const { [last.id]: _gone, ...rest } = d
                  return rest
                })
                setSel(last.id)
                setTab(inboxItems.find((i) => i.id === last.id)?.priority ? 'pri' : 'oth')
                setLast(null)
              }}
            >
              Undo
            </Button>
          </AlertDescription>
        </Alert>
      )}

      {list.length === 0 || !current ? (
        <EmptyState title={tab === 'pri' ? 'Nothing blocks a run right now' : 'Nothing else is waiting'}>
          Agents will put approvals, plans and questions here when they need you.
        </EmptyState>
      ) : (
        <div className="ks-split">
          <ul className="ks-ul ks-stack ks-aside" style={{ flex: '1 1 340px', maxWidth: 420 }} aria-label="Items">
            {list.map((it) => {
              const on = it.id === current.id
              return (
                <li key={it.id}>
                  <button
                    type="button"
                    id={`item-${it.id}`}
                    className="ks-wait"
                    onClick={() => setSel(it.id)}
                    aria-current={on ? 'true' : undefined}
                    style={{ width: '100%', font: 'inherit', color: 'inherit', cursor: 'pointer', textAlign: 'left', ...(on ? { background: 'var(--raised)', outline: '2px solid var(--ring)', outlineOffset: 1 } : {}) }}
                  >
                    <span className="ks-between">
                      <span className="ks-label">{it.kind}</span>
                      <AgeChip hours={it.hoursWaiting} />
                    </span>
                    <span style={{ fontWeight: 500, textAlign: 'left' }}>{it.title}</span>
                    <span className="ks-row" style={{ gap: 6 }}>
                      <AgentAvatar name={it.agent} decorative />
                      <span className="ks-meta">
                        {it.agent} · {it.space}
                      </span>
                    </span>
                  </button>
                </li>
              )
            })}
          </ul>

          <div className="ks-main-part">
            {current.id === 'plan' && <PlanCard onAct={act} />}
            {current.id === 'pr' && <PrCard onAct={act} />}
            {current.id === 'q' && (
              <QuestionCard
                answer={answer}
                setAnswer={setAnswer}
                onAct={act}
                onSend={(text) => resolve(current, 'respond', `Answer sent: ${text}`)}
              />
            )}
            {current.id === 'edit' && <EditCard args={args} setArgs={setArgs} onAct={act} onEditAccept={() => resolve(current, 'accept', 'Accepted with your edits: the agent gets your version.')} />}
            {current.id === 'snooze' && (
              <SnoozeCard
                when={snoozeWhen}
                setWhen={setSnoozeWhen}
                onSnooze={() => resolve(current, 'snooze', `Snoozed until ${snoozeWhen}. The age keeps counting.`)}
                onReply={() => setDialog('respond')}
              />
            )}
            {(current.id === 'mention' || current.id === 'reminder') && <OtherCard item={current} onAct={act} />}
          </div>
        </div>
      )}

      <Dialog
        open={dialog === 'respond'}
        onOpenChange={(o) => !o && setDialog(null)}
        title="Respond"
        description={current ? `Your reply goes to ${current.agent} and the run resumes with it.` : undefined}
        footer={
          <>
            <DialogClose>
              <Button variant="outline">Cancel</Button>
            </DialogClose>
            <Button disabled={!response.trim()} onClick={() => current && resolve(current, 'respond', `Response sent to ${current.agent}.`)}>
              Send response
            </Button>
          </>
        }
      >
        <div className="ks-stack">
          <Label htmlFor="respond-text">Your response</Label>
          <Textarea id="respond-text" value={response} onChange={(e) => setResponse(e.target.value)} />
        </div>
      </Dialog>

      <Dialog
        open={dialog === 'snooze'}
        onOpenChange={(o) => !o && setDialog(null)}
        title="Snooze"
        description="Plain words work: “tomorrow”, “Monday”, “in 3 hours”. Snoozing does not reset the age."
        footer={
          <>
            <DialogClose>
              <Button variant="outline">Cancel</Button>
            </DialogClose>
            <Button onClick={() => current && resolve(current, 'snooze', `Snoozed until ${snoozeWhen}. The age keeps counting.`)}>Snooze</Button>
          </>
        }
      >
        <div className="ks-stack">
          <Label htmlFor="snooze-dialog">Snooze until</Label>
          <Input id="snooze-dialog" value={snoozeWhen} onChange={(e) => setSnoozeWhen(e.target.value)} />
        </div>
      </Dialog>
    </PreviewShell>
  )
}

// ---------------------------------------------------------------------------------------------------------------
// Cards

function Card({ children, labelledBy }: { children: ReactNode; labelledBy?: string }) {
  return (
    <article className="ks-panel ks-stack-4" aria-labelledby={labelledBy} style={{ padding: 20 }}>
      {children}
    </article>
  )
}

function Keyed({ children, k }: { children: ReactNode; k: string }) {
  return (
    <>
      {children} <Kbd>{k}</Kbd>
    </>
  )
}

function PlanCard({ onAct }: { onAct: (a: InboxAction) => void }) {
  const [note, setNote] = useState('')
  return (
    <Card labelledBy="plan-h">
      <div className="ks-between" style={{ alignItems: 'flex-start' }}>
        <div className="ks-stack" style={{ gap: 4 }}>
          <span className="ks-label">Plan approval · 3 steps</span>
          <h2 id="plan-h" className="ks-h3">
            Approve LCM Upgrader&apos;s plan to upgrade cert-manager in staging?
          </h2>
          <span className="ks-meta">
            Run{' '}
            <PreviewLink className="ks-link ks-mono" href="/preview/runs/9c41d07a2e">
              9c41d07a
            </PreviewLink>{' '}
            is paused and uses no compute while it waits. Waiting 2 d 2 h.
          </span>
        </div>
        <StatusBadge kind="approval" state="pending" />
      </div>
      <p className="ks-p">The agent normally reads staging only. To finish this task it asks for the access below, once, for 30 minutes. After that it drops back to read-only.</p>
      <div className="ks-panel-flush ks-table-wrap" style={{ boxShadow: 'none' }} role="region" aria-label="Plan steps" tabIndex={0}>
        <table className="ks-table">
          <thead>
            <tr>
              <th scope="col">Step</th>
              <th scope="col">What it does</th>
              <th scope="col">Access it needs</th>
              <th scope="col">Why</th>
            </tr>
          </thead>
          <tbody>
            {planSteps.map((s) => (
              <tr key={s.n}>
                <td className="ks-num">{s.n}</td>
                <td>{s.what}</td>
                <td>
                  <Badge variant={s.badge}>{s.access}</Badge> <span className="ks-mono">{s.target}</span>
                </td>
                <td className="ks-muted">{s.why}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="ks-raised ks-stack" style={{ padding: 12 }}>
        <span style={{ fontWeight: 600 }}>One grant covers the plan</span>
        <dl className="ks-kv">
          <dt>Lasts</dt>
          <dd>30 min from approval, until about 14:52</dd>
          <dt>Scope</dt>
          <dd>Only the namespaces above, only these 3 steps</dd>
          <dt>Limit</dt>
          <dd>You can&apos;t grant more than you hold. Your role covers all three.</dd>
          <dt>Audit</dt>
          <dd>Requester, approver, the agent&apos;s identity and the time are recorded</dd>
        </dl>
      </div>
      <div className="ks-stack">
        <Label htmlFor="plan-note">Note (optional)</Label>
        <Input id="plan-note" placeholder="Shown to the agent and in the audit log" value={note} onChange={(e) => setNote(e.target.value)} />
      </div>
      <div className="ks-row" style={{ justifyContent: 'flex-end' }}>
        <Button variant="ghost" onClick={() => onAct('ignore')}>
          <Keyed k="I">Ignore</Keyed>
        </Button>
        <Button variant="outline" onClick={() => onAct('respond')}>
          <Keyed k="R">Respond</Keyed>
        </Button>
        <Button onClick={() => onAct('accept')}>
          <Keyed k="A">Accept plan for 30 min</Keyed>
        </Button>
      </div>
    </Card>
  )
}

function PrCard({ onAct }: { onAct: (a: InboxAction) => void }) {
  return (
    <Card>
      <div className="ks-between" style={{ alignItems: 'flex-start' }}>
        <div className="ks-stack" style={{ gap: 4 }}>
          <span className="ks-label">Approval · declares Accept, Respond, Ignore</span>
          <h2 className="ks-h3">
            Approve <span className="ks-mono">merge_pull_request</span> on platform-gitops?
          </h2>
          <span className="ks-meta">Waiting 9 d 2 h · past the org&apos;s 7-day rule</span>
        </div>
        <StatusBadge kind="task" state="blocked" ageHours={9 * 24} />
      </div>
      <Alert variant="warning" role="status">
        <AlertTitle>Check before approving</AlertTitle>
        <AlertDescription>Merging deploys ingress-nginx 1.14 to prod through Argo CD. CI ran with the repository&apos;s secrets.</AlertDescription>
      </Alert>
      <dl className="ks-kv">
        <dt>Pull request</dt>
        <dd>
          <span className="ks-mono">#482 lcm/ingress-nginx-1.14 → main</span>
        </dd>
        <dt>Checks</dt>
        <dd className="ks-ok">7 of 7 passed</dd>
        <dt>Changes</dt>
        <dd>2 files, +6 −4</dd>
      </dl>
      <div className="ks-row" style={{ justifyContent: 'flex-end' }}>
        <Button variant="ghost" onClick={() => onAct('ignore')}>
          Ignore
        </Button>
        <Button variant="outline" onClick={() => onAct('respond')}>
          Respond
        </Button>
        <Button onClick={() => onAct('accept')}>Accept and merge</Button>
      </div>
    </Card>
  )
}

function QuestionCard({ answer, setAnswer, onAct, onSend }: { answer: string; setAnswer: (s: string) => void; onAct: (a: InboxAction) => void; onSend: (text: string) => void }) {
  return (
    <Card>
      <div className="ks-stack" style={{ gap: 4 }}>
        <span className="ks-label">Question · declares Respond, Ignore</span>
        <h2 className="ks-h3">Which cost center owns the idle GPU nodes?</h2>
        <span className="ks-meta">FinOps Analyst · Weekly cost report, week 44 · waiting 4 d 2 h</span>
      </div>
      <p className="ks-p ks-muted">Six g6.12xlarge nodes in prod-eu ran at under 3% for 9 days. I need an owner to put them in the report.</p>
      <div className="ks-row">
        {costCenterAnswers.map((a) => (
          <Button key={a} variant="outline" size="xs" onClick={() => onSend(a)}>
            {a}
          </Button>
        ))}
      </div>
      <div className="ks-stack">
        <Label htmlFor="q-ans">Or write an answer</Label>
        <Textarea id="q-ans" value={answer} onChange={(e) => setAnswer(e.target.value)} />
      </div>
      <div className="ks-row" style={{ justifyContent: 'flex-end' }}>
        <Button variant="ghost" onClick={() => onAct('ignore')}>
          Ignore
        </Button>
        <Button
          disabled={!answer.trim()}
          onClick={() => {
            onSend(answer.trim())
            setAnswer('')
          }}
        >
          Send answer
        </Button>
      </div>
      <p className="ks-p ks-meta">Ignoring, or no answer in 7 days, tells the agent: &ldquo;No owner given; list them as unassigned.&rdquo;</p>
    </Card>
  )
}

function EditCard({ args, setArgs, onAct, onEditAccept }: { args: { title: string; labels: string }; setArgs: (a: { title: string; labels: string }) => void; onAct: (a: InboxAction) => void; onEditAccept: () => void }) {
  return (
    <Card>
      <div className="ks-stack" style={{ gap: 4 }}>
        <span className="ks-label">Tool call · declares Accept, Edit, Respond, Ignore</span>
        <h2 className="ks-h3">Create a GitHub issue in platform-gitops?</h2>
        <span className="ks-meta">Researcher · Compare three vector databases · waiting 1 d 2 h</span>
      </div>
      <div className="ks-raised ks-stack-3" style={{ padding: 12 }}>
        <span className="ks-row">
          <span style={{ fontWeight: 600 }}>Edit the arguments</span>
          <span className="ks-meta">the agent gets your version</span>
        </span>
        <div className="ks-stack">
          <Label htmlFor="e-title">title</Label>
          <Input id="e-title" value={args.title} onChange={(e) => setArgs({ ...args, title: e.target.value })} />
        </div>
        <div className="ks-stack">
          <Label htmlFor="e-labels">labels</Label>
          <Input id="e-labels" className="ks-mono" value={args.labels} onChange={(e) => setArgs({ ...args, labels: e.target.value })} />
        </div>
      </div>
      <div className="ks-row" style={{ justifyContent: 'flex-end' }}>
        <Button variant="ghost" onClick={() => onAct('ignore')}>
          Ignore
        </Button>
        <Button variant="outline" onClick={() => onAct('respond')}>
          Respond
        </Button>
        <Button variant="outline" onClick={onEditAccept}>
          Edit and accept
        </Button>
        <Button onClick={() => onAct('accept')}>Accept</Button>
      </div>
    </Card>
  )
}

function SnoozeCard({ when, setWhen, onSnooze, onReply }: { when: string; setWhen: (s: string) => void; onSnooze: () => void; onReply: () => void }) {
  return (
    <Card>
      <div className="ks-stack" style={{ gap: 4 }}>
        <span className="ks-label">Needs your reply</span>
        <h2 className="ks-h3">Rightsize the staging node pool</h2>
        <span className="ks-meta">FinOps Analyst · waiting 5 h</span>
      </div>
      <p className="ks-p">The task needs your go-ahead to resize from 6 to 4 nodes.</p>
      <div className="ks-raised ks-stack" style={{ padding: 12 }}>
        <Label htmlFor="snooze">
          Snooze until <Kbd>H</Kbd>
        </Label>
        <Input id="snooze" value={when} onChange={(e) => setWhen(e.target.value)} />
        <span className="ks-meta">Plain words work: &ldquo;tomorrow&rdquo;, &ldquo;Monday&rdquo;, &ldquo;in 3 hours&rdquo;. Snoozing does not reset the age.</span>
      </div>
      <div className="ks-row" style={{ justifyContent: 'flex-end' }}>
        <Button variant="outline" onClick={onSnooze}>
          Snooze
        </Button>
        <Button onClick={onReply}>Reply</Button>
      </div>
    </Card>
  )
}

/** Mentions and reminders sit under Other; the mockup shows no card for them, so this one is minimal. */
function OtherCard({ item, onAct }: { item: InboxItem; onAct: (a: InboxAction) => void }) {
  return (
    <Card>
      <div className="ks-stack" style={{ gap: 4 }}>
        <span className="ks-label">{item.kind} · does not block a run</span>
        <h2 className="ks-h3">{item.title}</h2>
        <span className="ks-meta">
          {item.agent} · {item.space} · {waitText(item.hoursWaiting)} ago
        </span>
      </div>
      <div className="ks-row" style={{ justifyContent: 'flex-end' }}>
        <Button variant="outline" onClick={() => onAct('snooze')}>
          <Keyed k="H">Snooze</Keyed>
        </Button>
        <Button onClick={() => onAct('read')}>
          <Keyed k="U">Mark read</Keyed>
        </Button>
      </div>
    </Card>
  )
}

