// SPDX-License-Identifier: Apache-2.0

// Group "platform": the platform console: orgs (archive and delete flow), usage and cost, shared with orgs,
// settings, agent inventory. Visuals and flow only, driven by mock data (Platform-*.dc.html).

import { useRouterState } from '@tanstack/react-router'
import { useMemo, useState } from 'react'
import {
  platformAgents,
  platformAgentTotal,
  platformOrgs,
  platformSpendByDay,
  platformUsage,
  sharedMcpServers,
  type PlatformOrg,
} from '@/preview/mock/data'
import {
  appliedSettings,
  orgLimitPercent,
  platformTemplates,
  settingLabels,
  sharedProviders,
  stagedSettings,
  type PlatformTemplate,
  type SettingsValues,
  type SharedProvider,
} from '@/preview/platform/data'
import {
  AgentAvatar,
  Badge,
  Button,
  CodeBlock,
  Dialog,
  DialogClose,
  Drawer,
  Field,
  FilterBar,
  Input,
  Label,
  Meter,
  NativeSelect,
  PageHeader,
  Panel,
  Pill,
  PreviewLink,
  PreviewShell,
  Section,
  Segmented,
  StatusBadge,
  Table,
  TabPanel,
  Tabs,
  Textarea,
  TypeToConfirm,
  usePaletteSelection,
  usePreviewNavigate,
  type PaletteSelection,
} from '@/preview/ui'

const warnPanel = { borderColor: 'var(--warning-border)' }

// ---------------------------------------------------------------------------------------------------------------
// Orgs

type OrgRow = PlatformOrg & { deleting?: boolean }
type OrgDialog = { kind: 'archive' | 'delete' | 'create' | 'reply'; org?: OrgRow } | null
type OrgView = 'all' | 'over' | 'archived'

const requestedOrg = 'Release Engineering'

export function PlatformOrgsPage() {
  const [orgs, setOrgs] = useState<OrgRow[]>(platformOrgs)
  const [dialog, setDialog] = useState<OrgDialog>(null)
  const [view, setView] = useState<OrgView>('all')
  const [query, setQuery] = useState('')
  const [statusChip, setStatusChip] = useState(true)
  const [selected, setSelected] = useState<string | null>(null)
  const [request, setRequest] = useState<'open' | 'replied' | 'done'>('open')
  const [reason, setReason] = useState('Team merged into Platform Engineering')
  const [typed, setTyped] = useState('')
  const [newName, setNewName] = useState('')
  const [reply, setReply] = useState('')

  const update = (name: string, patch: Partial<OrgRow>) => setOrgs((rows) => rows.map((o) => (o.name === name ? { ...o, ...patch } : o)))
  const openArchive = (org: OrgRow) => {
    setReason('Team merged into Platform Engineering')
    setDialog({ kind: 'archive', org })
  }
  const openDelete = (org: OrgRow) => {
    setTyped('')
    setDialog({ kind: 'delete', org })
  }
  const openCreate = (name: string) => {
    setNewName(name)
    setDialog({ kind: 'create' })
  }

  const rows = useMemo(() => {
    const q = query.trim().toLowerCase()
    return orgs.filter((o) => {
      if (view === 'archived' && o.status !== 'Archived') return false
      if (view === 'over' && (orgLimitPercent[o.name] ?? 0) < 80) return false
      if (!q) return true
      const m = q.match(/^(owner|provider|org|status):(.*)$/)
      if (m) {
        const term = m[2].trim()
        const hay = m[1] === 'owner' ? o.owners : m[1] === 'provider' ? o.providers : m[1] === 'status' ? o.status : o.name + ' ' + o.slug
        return hay.toLowerCase().includes(term)
      }
      return (o.name + ' ' + o.slug + ' ' + o.owners + ' ' + o.providers).toLowerCase().includes(q)
    })
  }, [orgs, view, query])

  const selectedOrg = orgs.find((o) => o.name === selected) ?? null
  const selection: PaletteSelection | null = selectedOrg
    ? {
        key: `${selectedOrg.name}:${selectedOrg.status}:${selectedOrg.deleting}`,
        title: selectedOrg.name,
        heading: 'For the selected org',
        actions:
          selectedOrg.status === 'Active'
            ? [{ label: 'Archive org', hint: selectedOrg.name, onSelect: () => openArchive(selectedOrg) }]
            : selectedOrg.deleting
              ? [{ label: 'Cancel deletion', hint: selectedOrg.name, onSelect: () => update(selectedOrg.name, { deleting: false, statusNote: 'restored · read-only' }) }]
              : [
                  { label: 'Restore org', hint: selectedOrg.name, onSelect: () => update(selectedOrg.name, { status: 'Active', statusNote: '' }) },
                  { label: 'Delete org…', hint: selectedOrg.name, onSelect: () => openDelete(selectedOrg) },
                ],
      }
    : null
  usePaletteSelection(selection)

  const waiting = request === 'open' || request === 'replied' ? 1 : 0
  const active = orgs.filter((o) => o.status === 'Active').length

  function doArchive() {
    if (dialog?.org) update(dialog.org.name, { status: 'Archived', statusNote: 'today · reversible' })
    setDialog(null)
  }
  function doDelete() {
    if (dialog?.org) update(dialog.org.name, { deleting: true, statusNote: 'purges 4 Nov · cancel until then' })
    setDialog(null)
  }
  function doCreate() {
    const name = newName.trim()
    if (!name) return
    const slug = name.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '')
    setOrgs((rows) => [...rows, { name, slug, owners: 'procoduck', members: 1, agents: 0, status: 'Active', statusNote: '', spend: '$0.00', providers: 'anthropic' }])
    if (name === requestedOrg) setRequest('done')
    setDialog(null)
  }

  const delOrg = dialog?.kind === 'delete' ? dialog.org : undefined
  const arOrg = dialog?.kind === 'archive' ? dialog.org : undefined

  return (
    <PreviewShell space="platform" current="Orgs" crumbs={[{ label: 'Orgs' }]}>
      <PageHeader
        title="Orgs"
        description={`${orgs.length} orgs · ${waiting} request${waiting === 1 ? '' : 's'} waiting · self-service is on (Settings)`}
        actions={<Button onClick={() => openCreate('')}>Create org</Button>}
      />
      {request !== 'done' && (
        <Panel className="ks-between" style={warnPanel}>
          <span>
            <span style={{ fontWeight: 600 }}>procoduck asks for "{requestedOrg}"</span>{' '}
            <span className="ks-meta">· "agents that prepare release notes and cut tags" · 2 h ago{request === 'replied' ? ' · you replied' : ''}</span>
          </span>
          <span className="ks-row">
            <Button variant="outline" size="sm" onClick={() => setDialog({ kind: 'reply' })}>
              Reply
            </Button>
            <Button size="sm" onClick={() => openCreate(requestedOrg)}>
              Create it
            </Button>
          </span>
        </Panel>
      )}
      <div className="ks-row">
        <FilterBar
          id="o-q"
          label="Filter orgs"
          chips={statusChip ? [{ key: 'status', value: 'active,archived', onRemove: () => setStatusChip(false) }] : []}
          placeholder="spend:>100 provider: owner:"
          value={query}
          onChange={setQuery}
        />
        <div className="ks-views">
          <span className="ks-meta">Views</span>
          <Pill pressed={view === 'all'} onClick={() => setView('all')}>
            All
          </Pill>
          <Pill pressed={view === 'over'} onClick={() => setView('over')}>
            Over 80% of limit
          </Pill>
          <Pill pressed={view === 'archived'} onClick={() => setView('archived')}>
            Archived
          </Pill>
        </div>
      </div>
      <Table label="Orgs">
        <thead>
          <tr>
            <th scope="col">Org</th>
            <th scope="col">Owners</th>
            <th scope="col" className="ks-r">Members</th>
            <th scope="col" className="ks-r">Agents</th>
            <th scope="col">Status</th>
            <th scope="col" className="ks-r">Spend, Oct</th>
            <th scope="col">Providers</th>
            <th scope="col">
              <span className="ks-sr">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {rows.map((o) => (
            <tr key={o.name} aria-selected={selected === o.name} onClick={() => setSelected(o.name)}>
              <td>
                <span style={{ fontWeight: 500 }}>{o.name}</span>
                <div className="ks-meta ks-mono">{o.slug}</div>
              </td>
              <td>{o.owners}</td>
              <td className="ks-r">{o.members}</td>
              <td className="ks-r">{o.agents}</td>
              <td>
                <Badge variant={o.status === 'Active' ? 'secondary' : 'outline'}>{o.status}</Badge>
                <div className="ks-meta">{o.statusNote}</div>
              </td>
              <td className="ks-r">{o.spend}</td>
              <td className="ks-meta">{o.providers}</td>
              <td className="ks-r">
                <span className="ks-row" style={{ justifyContent: 'flex-end', flexWrap: 'nowrap' }}>
                  {o.status === 'Active' ? (
                    <Button variant="outline" size="xs" onClick={() => openArchive(o)}>
                      Archive
                    </Button>
                  ) : o.deleting ? (
                    <Button variant="outline" size="xs" onClick={() => update(o.name, { deleting: false, statusNote: 'deletion canceled · read-only' })}>
                      Cancel deletion
                    </Button>
                  ) : (
                    <>
                      <Button variant="ghost" size="xs" onClick={() => update(o.name, { status: 'Active', statusNote: '' })}>
                        Restore
                      </Button>
                      <Button variant="outline" size="xs" onClick={() => openDelete(o)}>
                        Delete…
                      </Button>
                    </>
                  )}
                </span>
              </td>
            </tr>
          ))}
          {rows.length === 0 && (
            <tr>
              <td colSpan={8} className="ks-muted">
                No orgs match.
              </td>
            </tr>
          )}
        </tbody>
      </Table>
      <p className="ks-p ks-meta ks-num">
        {rows.length} of {orgs.length} orgs shown · {active} active. Archive first, delete later. The audit log of a deleted org is kept.
      </p>

      <Dialog
        open={dialog?.kind === 'archive'}
        onOpenChange={(o) => !o && setDialog(null)}
        title={`Archive ${arOrg?.name ?? ''}?`}
        footer={
          <>
            <DialogClose>
              <Button variant="outline">Cancel</Button>
            </DialogClose>
            <Button variant="destructive" onClick={doArchive}>
              Archive org
            </Button>
          </>
        }
      >
        <ul className="ks-ul ks-stack" style={{ fontSize: 14 }}>
          <li>Stops its {arOrg?.agents ?? 0} agents, 4 schedules and 2 triggers now</li>
          <li>Revokes its 5 API keys and gateway keys</li>
          <li>Makes the org read-only for its {arOrg?.members ?? 0} members</li>
          <li>Reversible: Restore brings everything back except the revoked keys</li>
        </ul>
        <Field id="ar-why" label="Reason">
          <Input id="ar-why" value={reason} onChange={(e) => setReason(e.target.value)} />
        </Field>
      </Dialog>

      <Dialog
        open={dialog?.kind === 'delete'}
        onOpenChange={(o) => !o && setDialog(null)}
        title={`Delete ${delOrg?.name ?? ''}?`}
        description="Its agents, tasks, memory and files are purged after a 7-day grace period, on 4 Nov. Until then a platform admin can cancel. The audit log stays."
        footer={
          <>
            <DialogClose>
              <Button variant="outline">Cancel</Button>
            </DialogClose>
            <Button variant="destructive" disabled={typed !== delOrg?.name} onClick={doDelete}>
              Delete after 7 days
            </Button>
          </>
        }
      >
        <TypeToConfirm id="de-name" expected={delOrg?.name ?? ''} value={typed} onChange={setTyped} />
      </Dialog>

      <Dialog
        open={dialog?.kind === 'create'}
        onOpenChange={(o) => !o && setDialog(null)}
        title="Create org"
        description="You become its owner; add more owners afterwards."
        footer={
          <>
            <DialogClose>
              <Button variant="outline">Cancel</Button>
            </DialogClose>
            <Button disabled={!newName.trim()} onClick={doCreate}>
              Create org
            </Button>
          </>
        }
      >
        <Field id="co-name" label="Name">
          <Input id="co-name" value={newName} onChange={(e) => setNewName(e.target.value)} placeholder="Release Engineering" autoComplete="off" />
        </Field>
      </Dialog>

      <Dialog
        open={dialog?.kind === 'reply'}
        onOpenChange={(o) => !o && setDialog(null)}
        title="Reply to procoduck"
        description={`About the request for "${requestedOrg}".`}
        footer={
          <>
            <DialogClose>
              <Button variant="outline">Cancel</Button>
            </DialogClose>
            <Button
              disabled={!reply.trim()}
              onClick={() => {
                setRequest('replied')
                setReply('')
                setDialog(null)
              }}
            >
              Send reply
            </Button>
          </>
        }
      >
        <Field id="rp-text" label="Message">
          <Textarea id="rp-text" rows={4} value={reply} onChange={(e) => setReply(e.target.value)} />
        </Field>
      </Dialog>
    </PreviewShell>
  )
}

// ---------------------------------------------------------------------------------------------------------------
// Usage and cost

type Group = 'org' | 'model' | 'provider'

const dayFormat = (i: number) => {
  // The 56 bars end on 28 Oct; the first is 2 Sep.
  const d = new Date(Date.UTC(2026, 8, 2 + i))
  return `${d.getUTCDate()} ${d.toLocaleString('en-GB', { month: 'short', timeZone: 'UTC' })}`
}

function downloadCsv(group: Group, rows: { name: string; cost: string; tokens: string; calls: string; cache: string; errors: string }[]) {
  const head = [group, 'cost', 'tokens', 'calls', 'cache hit', 'errors']
  const body = rows.map((r) => [r.name, r.cost, r.tokens, r.calls, r.cache, r.errors].map((c) => `"${c.replace(/"/g, '""')}"`).join(','))
  const url = URL.createObjectURL(new Blob([[head.join(','), ...body].join('\n')], { type: 'text/csv' }))
  const a = document.createElement('a')
  a.href = url
  a.download = `usage-by-${group}-2026-10.csv`
  a.click()
  URL.revokeObjectURL(url)
}

export function PlatformUsagePage() {
  const [group, setGroup] = useState<Group>('org')
  const [org, setOrg] = useState('All orgs')
  const rows = platformUsage[group].filter((r) => group !== 'org' || org === 'All orgs' || r.name === org)
  const max = Math.max(...platformSpendByDay)
  const labelOf = { org: 'Org', model: 'Model', provider: 'Provider' }[group]
  const stats = [
    { label: 'Month to date', value: '$750.64', note: 'up 18% on September at this point' },
    { label: 'Projected at month end', value: '$842', note: 'platform cap $200 a day' },
    { label: 'Tokens', value: '412M', note: '18,402 model calls' },
    { label: 'Cache hit rate', value: '57%', note: 'best: FinOps 84%' },
  ]
  return (
    <PreviewShell space="platform" current="Usage & cost" crumbs={[{ label: 'Usage & cost' }]}>
      <PageHeader
        title="Usage & cost"
        description="All orgs · October 2026 · each org pays from its own ledger"
        actions={
          <>
            <label className="ks-sr" htmlFor="pu-org">
              Org
            </label>
            <NativeSelect id="pu-org" style={{ width: 220 }} value={org} onChange={(e) => setOrg(e.target.value)}>
              <option>All orgs</option>
              {platformUsage.org.map((o) => (
                <option key={o.name}>{o.name}</option>
              ))}
            </NativeSelect>
            <Segmented
              label="Group by"
              value={group}
              onChange={setGroup}
              options={[
                { id: 'org', label: 'Org' },
                { id: 'model', label: 'Model' },
                { id: 'provider', label: 'Provider' },
              ]}
            />
            <Button variant="outline" size="sm" onClick={() => downloadCsv(group, rows)}>
              Export CSV
            </Button>
          </>
        }
      />
      <div className="ks-grid-4">
        {stats.map((s) => (
          <div key={s.label} className="ks-panel ks-stack" style={{ gap: 2 }}>
            <span className="ks-meta">{s.label}</span>
            <span className="ks-h2 ks-num">{s.value}</span>
            <span className="ks-meta">{s.note}</span>
          </div>
        ))}
      </div>
      <Section id="pt" panel title="Daily cost, last 8 weeks" meta="Bars link to that day's most expensive runs across orgs">
        <div className="ks-chart" style={{ height: 140 }}>
          {platformSpendByDay.map((v, i) => {
            const label = `${dayFormat(i)}: $${v}.00`
            return (
              <PreviewLink key={i} href="/preview/runs" title={label} aria-label={label}>
                <span className="ks-seg-a" style={{ height: `${(v / 40) * 100}%` }} />
              </PreviewLink>
            )
          })}
        </div>
        <div className="ks-between ks-meta ks-num" aria-hidden="true">
          <span>2 Sep</span>
          <span>28 Oct</span>
        </div>
        <span className="ks-sr">Highest day ${max}.00</span>
      </Section>
      <section className="ks-panel-flush" aria-labelledby="pg-h">
        <div className="ks-between" style={{ padding: '12px 16px' }}>
          <h2 id="pg-h" className="ks-h3">
            By {labelOf.toLowerCase()}
          </h2>
        </div>
        <div className="ks-table-wrap" role="region" aria-label={`Usage by ${labelOf.toLowerCase()}`} tabIndex={0}>
          <table className="ks-table">
            <thead>
              <tr>
                <th scope="col">{labelOf}</th>
                <th scope="col" className="ks-r">Cost</th>
                <th scope="col">Share</th>
                <th scope="col" className="ks-r">Tokens</th>
                <th scope="col" className="ks-r">Calls</th>
                <th scope="col" className="ks-r">Cache hit</th>
                <th scope="col" className="ks-r">Errors</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.name}>
                  <td className={group === 'org' ? '' : 'ks-mono'}>{r.name}</td>
                  <td className="ks-r">{r.cost}</td>
                  <td style={{ minWidth: 160 }}>
                    <Meter used={r.share} limit={100} label={`${r.share}% of spend`} />
                  </td>
                  <td className="ks-r">{r.tokens}</td>
                  <td className="ks-r">{r.calls}</td>
                  <td className="ks-r">{r.cache}</td>
                  <td className="ks-r">{r.errors}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>
    </PreviewShell>
  )
}

// ---------------------------------------------------------------------------------------------------------------
// Shared with orgs

type SharedTab = 'providers' | 'templates' | 'mcp'
const sharedCurrent: Record<SharedTab, string> = { providers: 'Providers', templates: 'Templates', mcp: 'Marketplace' }
type Editing = { kind: 'provider'; item: SharedProvider } | { kind: 'template'; item: PlatformTemplate } | null

function useSharedTab(): SharedTab {
  const search = useRouterState({ select: (s) => s.location.searchStr })
  const t = new URLSearchParams(search).get('tab')
  return t === 'templates' || t === 'mcp' ? t : 'providers'
}

export function PlatformSharedPage() {
  const tab = useSharedTab()
  const go = usePreviewNavigate()
  const [editing, setEditing] = useState<Editing>(null)
  const [diffOpen, setDiffOpen] = useState(false)
  const [templates, setTemplates] = useState(platformTemplates)
  const [promoted, setPromoted] = useState(false)
  const [off, setOff] = useState<Record<string, boolean>>({ 'Notion|Data Science': true })
  const [kindChip, setKindChip] = useState(true)
  const [mcpView, setMcpView] = useState<'remote' | 'hash'>('remote')
  const [mcpQuery, setMcpQuery] = useState('')

  const mcpRows = sharedMcpServers.filter((m) => {
    if (kindChip && m.kind !== 'remote') return false
    if (mcpView === 'hash' && !m.tools.includes('hash changed')) return false
    const q = mcpQuery.trim().toLowerCase()
    return !q || (m.name + ' ' + m.org + ' ' + m.kind + ' ' + m.health + ' ' + m.tools).toLowerCase().includes(q.replace(/^[a-z]+:/, ''))
  })

  function promote() {
    setTemplates((t) => [...t, { name: 'Release Notes Scout', version: 'v1', agents: 1, orgs: 1, skills: 'release-notes' }])
    setPromoted(true)
  }

  const current = sharedCurrent[tab]
  return (
    <PreviewShell space="platform" current={current} crumbs={[{ label: 'Shared with orgs' }, { label: current }]}>
      <PageHeader title="Shared with orgs" description="What the platform offers every org. An org can add its own; one with the same name overrides the platform's for that org." />
      <Tabs
        label="Shared"
        idPrefix="ps"
        value={tab}
        onChange={(id) => go(`/preview/platform/shared?tab=${id}`)}
        tabs={[
          { id: 'providers', label: 'Providers' },
          { id: 'templates', label: 'Templates' },
          { id: 'mcp', label: 'MCP servers' },
        ]}
      />

      {tab === 'providers' && (
        <TabPanel idPrefix="ps" tab="providers">
          <Table label="Shared providers">
            <thead>
              <tr>
                <th scope="col">Provider</th>
                <th scope="col">Models</th>
                <th scope="col">Assigned to</th>
                <th scope="col">Overridden by</th>
                <th scope="col">Idion spaces</th>
                <th scope="col" className="ks-r">Oct spend</th>
                <th scope="col">
                  <span className="ks-sr">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {sharedProviders.map((p) => (
                <tr key={p.name}>
                  <td>
                    <span style={{ fontWeight: 500 }}>{p.name}</span>
                    <div className="ks-meta">{p.credential}</div>
                  </td>
                  <td>{p.models}</td>
                  <td>{p.assigned}</td>
                  <td className={p.overriddenBy === 'none' || p.overriddenBy.includes('pending') ? 'ks-muted' : ''}>{p.overriddenBy}</td>
                  <td>
                    <Badge variant={p.shared ? 'secondary' : 'outline'}>{p.shared ? 'Shared' : 'Not shared'}</Badge>
                  </td>
                  <td className="ks-r">{p.spend}</td>
                  <td className="ks-r">
                    <Button variant="outline" size="xs" aria-label={`Edit ${p.name}`} onClick={() => setEditing({ kind: 'provider', item: p })}>
                      Edit
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </Table>
        </TabPanel>
      )}

      {tab === 'templates' && (
        <TabPanel idPrefix="ps" tab="templates" className="ks-stack-3">
          {!promoted && (
            <>
              <h2 className="ks-h4">Waiting to be promoted</h2>
              <Panel className="ks-between" style={warnPanel}>
                <span className="ks-row" style={{ flexWrap: 'nowrap' }}>
                  <AgentAvatar name="Release Notes Scout" decorative />
                  <span>
                    <span style={{ fontWeight: 500 }}>Release Notes Scout</span>{' '}
                    <span className="ks-meta">· org template of Platform Engineering · Maria Okafor · used by 1 agent</span>
                  </span>
                </span>
                <span className="ks-row">
                  <Button variant="outline" size="sm" onClick={() => setDiffOpen(true)}>
                    View diff
                  </Button>
                  <Button size="sm" onClick={promote}>
                    Promote to platform
                  </Button>
                </span>
              </Panel>
            </>
          )}
          <h2 className="ks-h4">Platform templates</h2>
          <Table label="Platform templates">
            <thead>
              <tr>
                <th scope="col">Template</th>
                <th scope="col">Version</th>
                <th scope="col" className="ks-r">Agents using it</th>
                <th scope="col">Orgs</th>
                <th scope="col">Skills</th>
                <th scope="col">
                  <span className="ks-sr">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {templates.map((t) => (
                <tr key={t.name}>
                  <td>{t.name}</td>
                  <td className="ks-mono">{t.version}</td>
                  <td className="ks-r">{t.agents}</td>
                  <td>{t.orgs}</td>
                  <td className="ks-mono">{t.skills}</td>
                  <td className="ks-r">
                    <Button variant="outline" size="xs" aria-label={`Edit ${t.name}`} onClick={() => setEditing({ kind: 'template', item: t })}>
                      Edit
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </Table>
        </TabPanel>
      )}

      {tab === 'mcp' && (
        <TabPanel idPrefix="ps" tab="mcp" className="ks-stack-3">
          <div className="ks-row">
            <FilterBar
              id="pm-q"
              label="Filter MCP servers"
              chips={kindChip ? [{ key: 'kind', value: 'remote', onRemove: () => setKindChip(false) }] : []}
              placeholder="org: health: tools:"
              value={mcpQuery}
              onChange={setMcpQuery}
            />
            <div className="ks-views">
              <span className="ks-meta">Views</span>
              <Pill pressed={mcpView === 'remote'} onClick={() => setMcpView('remote')}>
                Remote, all orgs
              </Pill>
              <Pill pressed={mcpView === 'hash'} onClick={() => setMcpView('hash')}>
                Tool hash changed
              </Pill>
            </div>
          </div>
          <Table label="MCP servers">
            <thead>
              <tr>
                <th scope="col">Server</th>
                <th scope="col">Org</th>
                <th scope="col">Kind</th>
                <th scope="col">Health</th>
                <th scope="col">Tools</th>
                <th scope="col">Enabled</th>
              </tr>
            </thead>
            <tbody>
              {mcpRows.map((m) => {
                const key = `${m.name}|${m.org}`
                const on = !off[key]
                return (
                  <tr key={key}>
                    <td style={{ fontWeight: 500 }}>{m.name}</td>
                    <td>{m.org}</td>
                    <td>{m.kind}</td>
                    <td>
                      <StatusBadge kind="connection" state={m.health} />
                    </td>
                    <td className="ks-meta">{m.tools}</td>
                    <td>
                      <label className="ks-row">
                        <input type="checkbox" checked={on} aria-label={`${m.name} for ${m.org}`} onChange={() => setOff((o) => ({ ...o, [key]: on }))} />
                        <span className="ks-meta">{on ? 'On' : 'Off, by a platform admin'}</span>
                      </label>
                    </td>
                  </tr>
                )
              })}
              {mcpRows.length === 0 && (
                <tr>
                  <td colSpan={6} className="ks-muted">
                    No servers match.
                  </td>
                </tr>
              )}
            </tbody>
          </Table>
          <p className="ks-p ks-meta">Disabling a server here turns it off for that org at once; its admins see "Disabled by a platform admin".</p>
        </TabPanel>
      )}

      <Drawer
        open={editing !== null}
        onOpenChange={(o) => !o && setEditing(null)}
        title={editing ? (editing.kind === 'provider' ? `Edit ${editing.item.name}` : `Edit ${editing.item.name}`) : 'Edit'}
        description={editing?.kind === 'provider' ? 'A provider the platform shares with orgs.' : 'A template every org can start an agent from.'}
        footer={
          <>
            <DialogClose>
              <Button variant="outline">Close</Button>
            </DialogClose>
            <DialogClose>
              <Button>Save</Button>
            </DialogClose>
          </>
        }
      >
        {editing?.kind === 'provider' && (
          <dl className="ks-kv">
            <dt>Credential</dt>
            <dd>{editing.item.credential}</dd>
            <dt>Models</dt>
            <dd className="ks-mono">{editing.item.models}</dd>
            <dt>Assigned to</dt>
            <dd>{editing.item.assigned}</dd>
            <dt>Overridden by</dt>
            <dd>{editing.item.overriddenBy}</dd>
            <dt>Idion spaces</dt>
            <dd>{editing.item.shared ? 'Shared' : 'Not shared'}</dd>
          </dl>
        )}
        {editing?.kind === 'template' && (
          <dl className="ks-kv">
            <dt>Version</dt>
            <dd className="ks-mono">{editing.item.version}</dd>
            <dt>Agents using it</dt>
            <dd>{editing.item.agents}</dd>
            <dt>Orgs</dt>
            <dd>{editing.item.orgs}</dd>
            <dt>Skills</dt>
            <dd className="ks-mono">{editing.item.skills}</dd>
          </dl>
        )}
      </Drawer>

      <Dialog
        open={diffOpen}
        onOpenChange={setDiffOpen}
        size="wide"
        title="Release Notes Scout"
        description="The org template of Platform Engineering, compared with what the platform would offer."
        footer={
          <>
            <DialogClose>
              <Button variant="outline">Close</Button>
            </DialogClose>
            <DialogClose>
              <Button
                onClick={() => {
                  promote()
                }}
              >
                Promote to platform
              </Button>
            </DialogClose>
          </>
        }
      >
        <CodeBlock>{`  model: deepseek-v4.1-flash
  skills:
+   - release-notes
  owner: Maria Okafor
+ shared with: all orgs`}</CodeBlock>
      </Dialog>
    </PreviewShell>
  )
}

// ---------------------------------------------------------------------------------------------------------------
// Settings

function fmtValue(v: string | boolean): string {
  return typeof v === 'boolean' ? (v ? 'on' : 'off') : v
}

export function PlatformSettingsPage() {
  const [applied, setApplied] = useState<SettingsValues>(appliedSettings)
  const [draft, setDraft] = useState<SettingsValues>(stagedSettings)
  const [diffOpen, setDiffOpen] = useState(false)
  const [justApplied, setJustApplied] = useState(false)

  const set = <K extends keyof SettingsValues>(k: K, v: SettingsValues[K]) => {
    setJustApplied(false)
    setDraft((d) => ({ ...d, [k]: v }))
  }
  const changes = (Object.keys(draft) as (keyof SettingsValues)[]).filter((k) => draft[k] !== applied[k])
  const text = (k: keyof SettingsValues, id: string, label: string, width = 120) => (
    <div className="ks-stack">
      <Label htmlFor={id}>{label}</Label>
      <Input id={id} className="ks-num" value={draft[k] as string} onChange={(e) => set(k, e.target.value as never)} style={{ width: width === 0 ? undefined : width }} />
    </div>
  )
  const first = changes[0]

  usePaletteSelection(
    changes.length
      ? {
          key: `staged:${changes.join(',')}`,
          title: `${changes.length} staged change${changes.length === 1 ? '' : 's'}`,
          heading: 'For the staged changes',
          actions: [
            {
              label: 'Apply staged changes',
              onSelect: () => {
                setApplied(draft)
                setJustApplied(true)
              },
            },
            { label: 'Discard staged changes', onSelect: () => setDraft(applied) },
          ],
        }
      : null,
  )

  return (
    <PreviewShell space="platform" current="Settings" crumbs={[{ label: 'Settings' }]} narrow>
      <h1 className="ks-h1">Platform settings</h1>
      {changes.length > 0 ? (
        <Panel className="ks-between" style={warnPanel} role="status">
          <span>
            <span style={{ fontWeight: 600 }}>
              {changes.length} staged change{changes.length === 1 ? '' : 's'}
            </span>{' '}
            <span className="ks-meta">
              · {settingLabels[first]} {fmtValue(applied[first])} → {fmtValue(draft[first])}
              {changes.length > 1 ? ` and ${changes.length - 1} more` : ''}
            </span>
          </span>
          <span className="ks-row">
            <Button variant="ghost" size="sm" onClick={() => setDraft(applied)}>
              Discard
            </Button>
            <Button variant="outline" size="sm" onClick={() => setDiffOpen(true)}>
              Review diff
            </Button>
            <Button
              size="sm"
              onClick={() => {
                setApplied(draft)
                setJustApplied(true)
              }}
            >
              Apply
            </Button>
          </span>
        </Panel>
      ) : (
        <Panel className="ks-between" role="status">
          <span className="ks-meta">{justApplied ? 'Applied. Changes take effect for every org now.' : 'No staged changes.'}</span>
        </Panel>
      )}

      <section className="ks-panel ks-stack-4" aria-labelledby="s-org">
        <h2 id="s-org" className="ks-h3">Orgs</h2>
        <label className="ks-row" style={{ flexWrap: 'nowrap', alignItems: 'flex-start' }}>
          <input type="checkbox" checked={draft.selfService} onChange={(e) => set('selfService', e.target.checked)} style={{ marginTop: 3 }} />
          <span className="ks-stack" style={{ gap: 2 }}>
            <span style={{ fontWeight: 500 }}>Let people create their own org</span>
            <span className="ks-meta">Off by default. When off, first login offers joining and "Ask a platform admin".</span>
          </span>
        </label>
        <div className="ks-row">
          <Label htmlFor="s-grace">Purge deleted orgs after</Label>
          <Input id="s-grace" className="ks-num" value={draft.grace} onChange={(e) => set('grace', e.target.value)} style={{ width: 120 }} />
        </div>
        <div className="ks-stack">
          <span className="kn-label">Default ageing for "Waiting on you"</span>
          <div className="ks-row">
            <span className="ks-meta">Light orange at</span>
            <Input className="ks-num" value={draft.ageing1} onChange={(e) => set('ageing1', e.target.value)} style={{ width: 72 }} aria-label="First step" />
            <span className="ks-meta">orange at</span>
            <Input className="ks-num" value={draft.ageing2} onChange={(e) => set('ageing2', e.target.value)} style={{ width: 72 }} aria-label="Second step" />
            <span className="ks-meta">dark red at</span>
            <Input className="ks-num" value={draft.ageing3} onChange={(e) => set('ageing3', e.target.value)} style={{ width: 72 }} aria-label="Third step" />
          </div>
          <span className="ks-meta">Each org can set its own steps.</span>
        </div>
      </section>

      <section className="ks-panel ks-stack-4" aria-labelledby="s-bud">
        <h2 id="s-bud" className="ks-h3">Budgets</h2>
        <div className="ks-grid-2" style={{ gap: 12 }}>
          {text('capPlatform', 'b1', 'Platform cap, per day', 0)}
          {text('capOrg', 'b2', 'Default for a new org, per day', 0)}
          {text('capAgent', 'b3', 'Default for a new agent, per day', 0)}
          {text('capKey', 'b4', 'Default for an API key, per day', 0)}
        </div>
        <span className="ks-meta">Alerts at 50, 75 and 100%. Orgs choose whether agents pause at the limit; new orgs start with pausing on.</span>
      </section>

      <section className="ks-panel ks-stack-4" aria-labelledby="s-sk">
        <h2 id="s-sk" className="ks-h3">Skills</h2>
        <fieldset className="ks-stack" style={{ border: 0, padding: 0, margin: 0 }}>
          <legend className="kn-label" style={{ marginBottom: 6 }}>Review gate</legend>
          {(
            [
              ['auto', 'approve once scans complete'],
              ['auto-if-clean', 'approve only with no finding at or above the threshold'],
              ['human', 'a person approves every version'],
            ] as const
          ).map(([id, note]) => (
            <label key={id} className="ks-row">
              <input type="radio" name="gate" checked={draft.gate === id} onChange={() => set('gate', id)} /> {id}{' '}
              <span className="ks-meta">
                · {note}
                {applied.gate === id && draft.gate !== id ? ' (current)' : ''}
                {draft.gate === id && applied.gate !== id ? ' (staged)' : ''}
                {applied.gate === id && draft.gate === id ? ' (current)' : ''}
              </span>
            </label>
          ))}
        </fieldset>
        <div className="ks-row">
          <Label htmlFor="thr">Threshold</Label>
          <NativeSelect id="thr" style={{ width: 140 }} value={draft.threshold} onChange={(e) => set('threshold', e.target.value as SettingsValues['threshold'])}>
            <option>high</option>
            <option>medium</option>
            <option>critical</option>
          </NativeSelect>
          <span className="ks-meta">Orgs can override the gate and the threshold, in either direction. 2 orgs do.</span>
        </div>
        <label className="ks-row">
          <input type="checkbox" checked={draft.osv} onChange={(e) => set('osv', e.target.checked)} /> Look up packages in OSV <span className="ks-meta">· needs egress to api.osv.dev</span>
        </label>
        <label className="ks-row">
          <input type="checkbox" checked={draft.browse} onChange={(e) => set('browse', e.target.checked)} /> Browse skills.sh and configured catalogs
        </label>
      </section>

      <section className="ks-panel ks-stack-4" aria-labelledby="s-id">
        <h2 id="s-id" className="ks-h3">Idion</h2>
        <div className="ks-row">
          <Label htmlFor="id-cap">Default cap per personal space, per month</Label>
          <Input id="id-cap" className="ks-num" value={draft.idionCap} onChange={(e) => set('idionCap', e.target.value)} style={{ width: 120 }} />
        </div>
        <span className="ks-meta">
          Paid by the platform. Providers marked "Shared" serve personal spaces. Private means private from other users, not from platform admins or the audit log.
        </span>
      </section>

      <section className="ks-panel ks-stack-3" aria-labelledby="s-sb">
        <h2 id="s-sb" className="ks-h3">Sandboxes</h2>
        <dl className="ks-kv">
          <dt>OpenShell enforcement</dt>
          <dd className="ks-mono">enforce</dd>
          <dt>Runtime class</dt>
          <dd className="ks-mono">kata</dd>
        </dl>
        <span className="ks-meta">Fixed by the install. They can't be weakened here.</span>
      </section>

      <Dialog
        open={diffOpen}
        onOpenChange={setDiffOpen}
        title="Staged changes"
        description="What Apply changes for every org."
        footer={
          <>
            <DialogClose>
              <Button variant="outline">Close</Button>
            </DialogClose>
            <DialogClose>
              <Button
                onClick={() => {
                  setApplied(draft)
                  setJustApplied(true)
                }}
              >
                Apply
              </Button>
            </DialogClose>
          </>
        }
      >
        <CodeBlock>{changes.map((k) => `- ${settingLabels[k]}: ${fmtValue(applied[k])}\n+ ${settingLabels[k]}: ${fmtValue(draft[k])}`).join('\n')}</CodeBlock>
      </Dialog>
    </PreviewShell>
  )
}

// ---------------------------------------------------------------------------------------------------------------
// Agent inventory

type AgentView = 'all' | 'orphan' | 'unhealthy'

export function PlatformAgentsPage() {
  const [view, setView] = useState<AgentView>('all')
  const [query, setQuery] = useState('')

  const rows = useMemo(() => {
    const byView = platformAgents.filter((a) => (view === 'orphan' ? a.ownerLeft : view === 'unhealthy' ? a.state === 'unhealthy' : true))
    const q = query.trim().toLowerCase()
    if (!q) return byView
    return byView.filter((a) => {
      const m = q.match(/^(org|model|owner|state):(.*)$/)
      if (m) {
        const field = { org: a.org, model: a.model, owner: a.owner, state: a.state }[m[1] as 'org' | 'model' | 'owner' | 'state']
        return field.toLowerCase().includes(m[2].trim())
      }
      return (a.name + ' ' + a.org + ' ' + a.owner + ' ' + a.model).toLowerCase().includes(q)
    })
  }, [view, query])

  return (
    <PreviewShell space="platform" current="Agents" crumbs={[{ label: 'Agents' }]}>
      <PageHeader title="Agents on this install" description="14 agents in 5 orgs. Every agent has an owner who answers for it; an agent whose owner left is flagged." />
      <div className="ks-row">
        <FilterBar id="ag-q" label="Filter agents" placeholder="org: model: owner: state:" value={query} onChange={setQuery} />
        <div className="ks-views">
          <span className="ks-meta">Views</span>
          <Pill pressed={view === 'all'} onClick={() => setView('all')}>
            All
          </Pill>
          <Pill pressed={view === 'orphan'} onClick={() => setView('orphan')}>
            Owner left
          </Pill>
          <Pill pressed={view === 'unhealthy'} onClick={() => setView('unhealthy')}>
            Unhealthy
          </Pill>
        </div>
      </div>
      <Table label="Agents on this install">
        <thead>
          <tr>
            <th scope="col">Agent</th>
            <th scope="col">Org</th>
            <th scope="col">Owner</th>
            <th scope="col">State</th>
            <th scope="col">Model</th>
            <th scope="col">Template</th>
            <th scope="col" className="ks-r">Oct spend</th>
            <th scope="col">Last run</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((a) => (
            <tr key={a.name}>
              <td>
                <span className="ks-row" style={{ gap: 6, flexWrap: 'nowrap' }}>
                  <AgentAvatar name={a.name} decorative />
                  {a.name}
                </span>
              </td>
              <td>{a.org}</td>
              <td className={a.ownerLeft ? 'ks-warn' : ''}>{a.ownerLeft ? `${a.owner} (${a.ownerLeft})` : a.owner}</td>
              <td>
                <StatusBadge kind="agent" state={a.state} />
              </td>
              <td className="ks-mono">{a.model}</td>
              <td className="ks-meta">{a.template}</td>
              <td className="ks-r">{a.spend}</td>
              <td className="ks-meta ks-num">{a.lastRun}</td>
            </tr>
          ))}
          {rows.length === 0 && (
            <tr>
              <td colSpan={8} className="ks-muted">
                No agents match.
              </td>
            </tr>
          )}
        </tbody>
      </Table>
      <p className="ks-p ks-meta ks-num">
        {rows.length} of {platformAgentTotal} agents shown
      </p>
    </PreviewShell>
  )
}
