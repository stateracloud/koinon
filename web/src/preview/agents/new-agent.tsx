// SPDX-License-Identifier: Apache-2.0

import { MessageSquare, Pencil } from 'lucide-react'
import { useId, useState, type FormEvent } from 'react'
import {
  AgentAvatar,
  Badge,
  Button,
  ButtonLink,
  EmptyState,
  Field,
  Input,
  NativeSelect,
  Panel,
  PreviewLink,
  PreviewShell,
  Textarea,
  describedBy,
  usePreviewNavigate,
} from '@/preview/ui'
import { templates } from '@/preview/mock/data'
import {
  blankModelOptions,
  blankReportsToOptions,
  blankTrigger,
  categories,
  lcmTemplate,
  reportsToOptions,
  templateCategoryOptions,
  type BlankTrigger,
} from '@/preview/agents/data'

const AGENT_HREF = '/preview/agents/lcm-upgrader'
const GALLERY_HREF = '/preview/agents/new'

export function NewAgentGallery() {
  const [cat, setCat] = useState<(typeof categories)[number]>('All')
  const [q, setQ] = useState('')
  const query = q.trim().toLowerCase()
  const shown = templates.filter(
    (t) =>
      (cat === 'All' || t.category === cat) &&
      (query === '' || [t.name, t.category, t.description, t.skills, t.mcp, t.model].some((s) => s.toLowerCase().includes(query))),
  )
  return (
    <PreviewShell space="org" current="New agent" crumbs={[{ label: 'New agent' }]}>
      <div className="ks-stack" style={{ gap: 4 }}>
        <h1 className="ks-h1">New agent</h1>
        <p className="ks-p ks-muted">Start from a template, or set one up yourself. Either way you review it before it runs.</p>
      </div>
      <div className="ks-grid-4" style={{ gridTemplateColumns: 'repeat(auto-fit, minmax(300px, 1fr))' }}>
        <PreviewLink className="ks-card-a" href="/preview/agents/new/blank">
          <div className="ks-row">
            <span className="ks-tile-icon" aria-hidden="true">
              <Pencil className="ks-icon" />
            </span>
            <span className="ks-stack" style={{ gap: 0 }}>
              <span className="ks-h4">Start blank</span>
              <span className="ks-meta">Name, instructions, model, triggers</span>
            </span>
          </div>
          <span className="ks-muted">For when you know what you want. You can save it as a template at the end.</span>
        </PreviewLink>
        <div className="ks-card-a" aria-disabled="true" style={{ opacity: 0.8 }}>
          <div className="ks-row">
            <span className="ks-tile-icon" aria-hidden="true">
              <MessageSquare className="ks-icon" />
            </span>
            <span className="ks-stack" style={{ gap: 0 }}>
              <span className="ks-h4">Ask the concierge</span>
              <span className="ks-meta">Describe the job; it drafts the agent</span>
            </span>
            <Badge variant="outline" style={{ marginLeft: 'auto' }}>
              v0.3
            </Badge>
          </div>
          <span className="ks-muted">Most agents will be set up this way once the concierge ships. This card stays here so the place is known.</span>
        </div>
      </div>
      <div className="ks-between">
        <h2 className="ks-h3">Templates</h2>
        <div className="ks-row">
          <div className="ks-row" role="group" aria-label="Category">
            {categories.map((c) => (
              <button key={c} type="button" className="ks-pill" aria-pressed={c === cat} onClick={() => setCat(c)}>
                {c}
              </button>
            ))}
          </div>
          <label className="ks-sr" htmlFor="tpl-q">
            Search templates
          </label>
          <input id="tpl-q" className="kn-field kn-input" type="search" placeholder="Search templates" style={{ width: 220 }} value={q} onChange={(e) => setQ(e.target.value)} />
        </div>
      </div>
      {shown.length === 0 ? (
        <EmptyState
          title="No templates match"
          action={
            <Button
              variant="outline"
              onClick={() => {
                setCat('All')
                setQ('')
              }}
            >
              Clear filters
            </Button>
          }
        >
          Try another category or search, or start blank.
        </EmptyState>
      ) : (
        <div className="ks-grid" role="list" aria-label="Templates" aria-live="polite">
          {shown.map((t) => (
            <PreviewLink key={t.id} role="listitem" className="ks-card-a" href="/preview/agents/new/template">
              <div className="ks-row" style={{ flexWrap: 'nowrap' }}>
                <AgentAvatar name={t.name} size="lg" decorative />
                <span className="ks-stack ks-grow" style={{ gap: 0 }}>
                  <span className="ks-h4">{t.name}</span>
                  <span className="ks-meta">{t.category}</span>
                </span>
                <Badge>{t.source}</Badge>
              </div>
              <span className="ks-muted">{t.description}</span>
              <dl className="ks-dl">
                <div>
                  <dt>Default model</dt>
                  <dd className="ks-mono">{t.model}</dd>
                </div>
                <div>
                  <dt>Typical cost</dt>
                  <dd>{t.cost}</dd>
                </div>
                <div>
                  <dt>Skills</dt>
                  <dd>{t.skills}</dd>
                </div>
                <div>
                  <dt>MCP servers</dt>
                  <dd>{t.mcp}</dd>
                </div>
              </dl>
            </PreviewLink>
          ))}
        </div>
      )}
    </PreviewShell>
  )
}

export function NewAgentTemplate() {
  const navigate = usePreviewNavigate()
  const [name, setName] = useState('LCM Upgrader (EU)')
  const [reports, setReports] = useState(reportsToOptions[1])
  const [team, setTeam] = useState('Upgrades')
  const [model, setModel] = useState(lcmTemplate.modelOptions[0])
  const [limit, setLimit] = useState('$5.00')
  const [touched, setTouched] = useState(false)
  const nameError = touched && name.trim() === '' ? 'Give the agent a name.' : undefined
  function submit(e: FormEvent) {
    e.preventDefault()
    setTouched(true)
    if (name.trim() === '') return
    navigate(AGENT_HREF)
  }
  return (
    <PreviewShell
      space="org"
      current="New agent"
      crumbs={[{ label: 'New agent', href: GALLERY_HREF }, { label: 'LCM Upgrader' }]}
    >
      <div className="ks-row" style={{ flexWrap: 'nowrap', alignItems: 'flex-start' }}>
        <AgentAvatar name="LCM Upgrader" size="lg" decorative />
        <div className="ks-stack ks-grow" style={{ gap: 4 }}>
          <div className="ks-row">
            <h1 className="ks-h1">LCM Upgrader</h1>
            <Badge>Platform template</Badge>
            <span className="ks-meta ks-mono">{lcmTemplate.version}</span>
          </div>
          <p className="ks-p ks-muted" style={{ maxWidth: '70ch' }}>
            {lcmTemplate.description}
          </p>
        </div>
      </div>
      <div className="ks-split">
        <div className="ks-main-part ks-stack-4">
          <section className="ks-panel ks-stack-3" aria-labelledby="comes-h">
            <h2 id="comes-h" className="ks-h4">
              What it comes with
            </h2>
            <dl className="ks-kv">
              <dt>Model</dt>
              <dd>
                <span className="ks-mono">gpt-6-luna</span> <span className="ks-meta">· template default; your org default is deepseek-v4.1-flash</span>
              </dd>
              <dt>Triggers</dt>
              <dd>GitHub release on a watched chart · schedule Mon 02:00 (upgrade window)</dd>
              <dt>Skills</dt>
              <dd>
                <span className="ks-mono">helm-upgrade</span> <span className="ks-meta">sha256:3c7a…</span> · <span className="ks-mono">release-notes</span>{' '}
                <span className="ks-meta">sha256:e019…</span> <Badge>Approved</Badge>
              </dd>
              <dt>MCP servers</dt>
              <dd>GitHub (org connection) · Argo CD (read)</dd>
              <dt>Asks first</dt>
              <dd>
                <span className="ks-mono">create_pull_request</span>, <span className="ks-mono">merge_pull_request</span>, writes to staging
              </dd>
              <dt>Typical cost</dt>
              <dd className="ks-num">$2 to $8 a day, mostly upgrade weeks</dd>
            </dl>
          </section>
          <section className="ks-panel ks-stack-3" aria-labelledby="ins-h">
            <h2 id="ins-h" className="ks-h4">
              Instructions
            </h2>
            <pre className="ks-code" style={{ whiteSpace: 'pre-wrap' }}>
              {lcmTemplate.instructions}
            </pre>
          </section>
        </div>
        <form className="ks-aside ks-panel ks-stack-4" aria-labelledby="use-h" style={{ padding: 20 }} onSubmit={submit} noValidate>
          <h2 id="use-h" className="ks-h3">
            Use this template
          </h2>
          <Field id="t-name" label="Name" error={nameError}>
            <Input id="t-name" value={name} onChange={(e) => setName(e.target.value)} aria-invalid={nameError ? true : undefined} aria-describedby={describedBy('t-name', { error: !!nameError })} />
          </Field>
          <Field id="t-rep" label="Reports to" hint="Agents only. This draws the line in the org chart.">
            <NativeSelect id="t-rep" value={reports} onChange={(e) => setReports(e.target.value)} aria-describedby={describedBy('t-rep', { hint: true })}>
              {reportsToOptions.map((o) => (
                <option key={o}>{o}</option>
              ))}
            </NativeSelect>
          </Field>
          <Field id="t-team" label="Team">
            <Input id="t-team" value={team} onChange={(e) => setTeam(e.target.value)} />
          </Field>
          <Field id="t-model" label="Model">
            <NativeSelect id="t-model" value={model} onChange={(e) => setModel(e.target.value)}>
              {lcmTemplate.modelOptions.map((o) => (
                <option key={o}>{o}</option>
              ))}
            </NativeSelect>
          </Field>
          <Field id="t-lim" label="Daily limit" hint="The org default for a new agent. The org has $13.22 left today.">
            <Input id="t-lim" className="ks-num" value={limit} onChange={(e) => setLimit(e.target.value)} aria-describedby={describedBy('t-lim', { hint: true })} />
          </Field>
          <div className="ks-row" style={{ justifyContent: 'flex-end' }}>
            <ButtonLink variant="outline" href={GALLERY_HREF}>
              Back
            </ButtonLink>
            <Button type="submit">Create draft</Button>
          </div>
          <p className="ks-p ks-meta">The draft runs nothing until you activate it.</p>
        </form>
      </div>
    </PreviewShell>
  )
}

export function NewAgentBlank() {
  const navigate = usePreviewNavigate()
  const uid = useId()
  const [name, setName] = useState('Cluster Janitor')
  const [desc, setDesc] = useState('Finds orphaned PVCs, stale namespaces and failed jobs, and proposes clean-ups.')
  const [reports, setReports] = useState(blankReportsToOptions[0])
  const [team, setTeam] = useState('FinOps')
  const [text, setText] = useState(
    'Every night, list PersistentVolumeClaims not mounted for 14 days, namespaces with no workloads for 30 days and Jobs failed for 7 days. Propose deletions as one plan. Never delete anything in kube-system or in a namespace labelled keep=true.',
  )
  const [model, setModel] = useState(blankModelOptions[0])
  const [triggers, setTriggers] = useState<BlankTrigger[]>([blankTrigger])
  const [limit, setLimit] = useState('$2.00')
  const [tpl, setTpl] = useState(true)
  const [tplName, setTplName] = useState('Cluster Janitor')
  const [tplCat, setTplCat] = useState(templateCategoryOptions[0])
  const [touched, setTouched] = useState(false)
  const nameError = touched && name.trim() === '' ? 'Give the agent a name.' : undefined
  const effective = model.startsWith('Use the org default') ? { name: 'deepseek-v4.1-flash', from: 'the org default' } : { name: model.split(' · ')[0], from: 'this agent' }
  function submit(e: FormEvent) {
    e.preventDefault()
    setTouched(true)
    if (name.trim() === '') return
    navigate(AGENT_HREF)
  }
  return (
    <PreviewShell
      space="org"
      current="New agent"
      crumbs={[{ label: 'New agent', href: GALLERY_HREF }, { label: 'Blank' }]}
      bare
    >
      <form className="ks-page ks-page-narrow" aria-labelledby="blank-h" onSubmit={submit} noValidate>
        <div className="ks-stack" style={{ gap: 4 }}>
          <h1 id="blank-h" className="ks-h1">
            Set up an agent
          </h1>
          <p className="ks-p ks-muted">Saved as a draft. Nothing runs until you activate it.</p>
        </div>

        <section className="ks-panel ks-stack-4" aria-labelledby="b-basics">
          <h2 id="b-basics" className="ks-h4">
            Basics
          </h2>
          <Field id="b-name" label="Name" error={nameError}>
            <Input id="b-name" value={name} onChange={(e) => setName(e.target.value)} aria-invalid={nameError ? true : undefined} aria-describedby={describedBy('b-name', { error: !!nameError })} />
          </Field>
          <Field id="b-desc" label="What it does">
            <Input id="b-desc" value={desc} onChange={(e) => setDesc(e.target.value)} />
          </Field>
          <div className="ks-grid-2" style={{ gap: 12 }}>
            <Field id="b-rep" label="Reports to" hint="Another agent, never a person.">
              <NativeSelect id="b-rep" value={reports} onChange={(e) => setReports(e.target.value)} aria-describedby={describedBy('b-rep', { hint: true })}>
                {blankReportsToOptions.map((o) => (
                  <option key={o}>{o}</option>
                ))}
              </NativeSelect>
            </Field>
            <Field id="b-team" label="Team">
              <Input id="b-team" value={team} onChange={(e) => setTeam(e.target.value)} />
            </Field>
          </div>
        </section>

        <section className="ks-panel ks-stack-3" aria-labelledby="b-ins">
          <h2 id="b-ins" className="ks-h4">
            Instructions
          </h2>
          <label className="ks-sr" htmlFor="b-text">
            Instructions
          </label>
          <Textarea id="b-text" style={{ minHeight: 120 }} value={text} onChange={(e) => setText(e.target.value)} />
        </section>

        <section className="ks-panel ks-stack-3" aria-labelledby="b-model">
          <h2 id="b-model" className="ks-h4">
            Model
          </h2>
          <label className="ks-sr" htmlFor="b-m">
            Model
          </label>
          <NativeSelect id="b-m" value={model} onChange={(e) => setModel(e.target.value)}>
            {blankModelOptions.map((o) => (
              <option key={o}>{o}</option>
            ))}
          </NativeSelect>
          <p className="ks-p ks-meta" role="status">
            Effective model: <span className="ks-mono">{effective.name}</span>, from {effective.from}. Pin one here to stop org changes from reaching this agent.
          </p>
        </section>

        <section className="ks-panel ks-stack-3" aria-labelledby="b-trig">
          <div className="ks-between">
            <h2 id="b-trig" className="ks-h4">
              Triggers
            </h2>
            <Button variant="outline" size="xs" disabled={triggers.length > 0} onClick={() => setTriggers([blankTrigger])}>
              Add trigger
            </Button>
          </div>
          {triggers.map((t) => (
            <div key={t.id} className="ks-raised ks-between" style={{ padding: '10px 12px' }}>
              <span>
                <span style={{ fontWeight: 500 }}>{t.kind}</span> <span className="ks-mono ks-meta">{t.cron}</span> <span className="ks-meta">· {t.text}</span>
              </span>
              <Button variant="ghost" size="xs" aria-label={`Remove ${t.kind} trigger`} onClick={() => setTriggers((l) => l.filter((x) => x.id !== t.id))}>
                Remove
              </Button>
            </div>
          ))}
          {triggers.length === 0 && <p className="ks-p ks-muted">No triggers. The agent only runs when you start it.</p>}
          <p className="ks-p ks-meta">Manual runs are always possible from the agent page.</p>
        </section>

        <section className="ks-panel ks-stack-3" aria-labelledby="b-skills">
          <div className="ks-between">
            <h2 id="b-skills" className="ks-h4">
              Skills and MCP servers
            </h2>
            <ButtonLink variant="outline" size="xs" href="/preview/marketplace">
              Browse the Marketplace
            </ButtonLink>
          </div>
          <div className="ks-row">
            <Badge variant="outline">
              skill · k8s-hygiene <span className="ks-mono">sha256:5d2e…</span>
            </Badge>
            <Badge variant="outline">MCP · Kubernetes (read)</Badge>
          </div>
        </section>

        <section className="ks-panel ks-stack-3" aria-labelledby="b-lim">
          <h2 id="b-lim" className="ks-h4">
            Limit
          </h2>
          <div className="ks-row">
            <label className="kn-label" htmlFor="b-l">
              Daily limit
            </label>
            <Input id="b-l" className="ks-num" style={{ width: 120 }} value={limit} onChange={(e) => setLimit(e.target.value)} aria-describedby="b-l-hint" />
            <span id="b-l-hint" className="ks-meta">
              Org default for new agents is $5.00
            </span>
          </div>
        </section>

        <Panel className="ks-stack-3" aria-labelledby={`${uid}-tpl`} role="group">
          <label className="ks-row" style={{ flexWrap: 'nowrap', alignItems: 'flex-start' }}>
            <input type="checkbox" checked={tpl} onChange={(e) => setTpl(e.target.checked)} style={{ marginTop: 3 }} />
            <span className="ks-stack" style={{ gap: 2 }}>
              <span id={`${uid}-tpl`} style={{ fontWeight: 600 }}>
                Save as template
              </span>
              <span className="ks-meta">Saves an org template that anyone in Platform Engineering can start from. A platform admin can promote it to every org.</span>
            </span>
          </label>
          {tpl && (
            <div className="ks-grid-2" style={{ gap: 12 }}>
              <Field id="tp-name" label="Template name">
                <Input id="tp-name" value={tplName} onChange={(e) => setTplName(e.target.value)} />
              </Field>
              <Field id="tp-cat" label="Category">
                <NativeSelect id="tp-cat" value={tplCat} onChange={(e) => setTplCat(e.target.value)}>
                  {templateCategoryOptions.map((o) => (
                    <option key={o}>{o}</option>
                  ))}
                </NativeSelect>
              </Field>
            </div>
          )}
        </Panel>

        <div className="ks-row" style={{ justifyContent: 'flex-end' }}>
          <ButtonLink variant="outline" href={GALLERY_HREF}>
            Cancel
          </ButtonLink>
          <Button type="submit">{tpl ? 'Create draft and template' : 'Create draft'}</Button>
        </div>
      </form>
    </PreviewShell>
  )
}
