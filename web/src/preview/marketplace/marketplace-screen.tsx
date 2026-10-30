// SPDX-License-Identifier: Apache-2.0

import { useMemo, useState } from 'react'
import { marketCategories, marketItems, type MarketItem } from '@/preview/mock/data'
import { Badge, Button, ButtonLink, Dialog, DialogClose, Field, FilterBar, Input, PageHeader, Pill, PreviewLink, PreviewShell, StateDot, Tabs, EmptyState, type FilterChip } from '@/preview/ui'
import { itemCategory } from './data'

type Kind = 'All' | 'MCP servers' | 'Skills'
type Tab = 'catalog' | 'installed' | 'skills-sh'
const kinds: Kind[] = ['All', 'MCP servers', 'Skills']

function isClean(it: MarketItem) {
  return it.scanDot === 'completed'
}

function Card({ it, added, onAct }: { it: MarketItem; added: boolean; onAct: () => void }) {
  const installed = it.installed || added
  const href = it.skillId ? `/preview/skills/${it.skillId}` : undefined
  const opens = it.action === 'Open' || it.action === 'Review before adding'
  return (
    <article className="ks-card-a">
      <div className="ks-row" style={{ flexWrap: 'nowrap' }}>
        <span className="ks-tile-icon ks-mono" aria-hidden="true" style={{ fontSize: 12, fontWeight: 600 }}>
          {it.mono}
        </span>
        <span className="ks-stack ks-grow" style={{ gap: 0 }}>
          {href ? (
            <PreviewLink className="ks-a ks-h4" href={href}>
              {it.name}
            </PreviewLink>
          ) : (
            <span className="ks-h4">{it.name}</span>
          )}
          <span className="ks-meta">{it.publisher}</span>
        </span>
        <Badge variant="outline">{it.kind}</Badge>
      </div>
      <span className="ks-muted">{it.description}</span>
      <div className="ks-row">
        <Badge>
          <StateDot state={it.scanDot} />
          {it.scan}
        </Badge>
        <Badge>
          <StateDot state={it.healthDot} />
          {it.health}
        </Badge>
        {installed && <Badge variant="outline">Installed</Badge>}
      </div>
      <code className="ks-code" style={{ padding: '6px 10px', fontSize: 12 }}>
        {it.cli}
      </code>
      <div className="ks-row">
        {opens && href ? (
          <ButtonLink variant="outline" size="sm" href={href}>
            {it.action}
          </ButtonLink>
        ) : (
          <Button variant="outline" size="sm" onClick={onAct} disabled={added && !it.installed}>
            {added && !it.installed ? 'Added to org' : it.action}
          </Button>
        )}
        {it.name === 'GitHub' && (
          <Button variant="ghost" size="sm">
            Connect your own
          </Button>
        )}
      </div>
    </article>
  )
}

export function MarketplaceScreen() {
  const [tab, setTab] = useState<Tab>('catalog')
  const [kind, setKind] = useState<Kind>('All')
  const [q, setQ] = useState('')
  const [cleanOnly, setCleanOnly] = useState(true)
  const [needsReview, setNeedsReview] = useState(false)
  const [category, setCategory] = useState('All')
  const [added, setAdded] = useState<Set<string>>(new Set())
  const [remote, setRemote] = useState(false)
  const [url, setUrl] = useState('')

  const installedCount = marketItems.filter((i) => i.installed).length

  const items = useMemo(() => {
    const needle = q.trim().toLowerCase()
    return marketItems.filter((it) => {
      if (tab === 'installed' && !(it.installed || added.has(it.name))) return false
      if (tab === 'skills-sh' && !it.publisher.startsWith('skills.sh')) return false
      if (kind === 'MCP servers' && it.kind !== 'MCP server') return false
      if (kind === 'Skills' && it.kind !== 'Skill') return false
      if (cleanOnly && !isClean(it)) return false
      if (needsReview && isClean(it)) return false
      if (category !== 'All' && itemCategory[it.name] !== category) return false
      if (needle && !`${it.name} ${it.description} ${it.publisher} ${it.kind}`.toLowerCase().includes(needle)) return false
      return true
    })
  }, [tab, kind, q, cleanOnly, needsReview, category, added])

  const chips: FilterChip[] = cleanOnly ? [{ key: 'scan', value: 'clean', onRemove: () => setCleanOnly(false) }] : []

  function kubernetesClean() {
    const on = category === 'Kubernetes' && cleanOnly && !needsReview
    setNeedsReview(false)
    setCategory(on ? 'All' : 'Kubernetes')
    setCleanOnly(!on)
  }
  function review() {
    const on = needsReview
    setNeedsReview(!on)
    if (!on) setCleanOnly(false)
  }

  return (
    <PreviewShell space="org" current="Marketplace" crumbs={[{ label: 'Marketplace' }]}>
      <PageHeader
        title="Marketplace"
        description="MCP servers and skills in one catalog. Everything is scanned before an agent can use it."
        actions={
          <>
            <Button variant="outline" onClick={() => setRemote(true)}>
              Connect a remote server
            </Button>
            <ButtonLink href="/preview/skills/add">Add a skill</ButtonLink>
          </>
        }
      />
      <Tabs
        label="Marketplace"
        idPrefix="mk"
        value={tab}
        onChange={setTab}
        tabs={[
          { id: 'catalog', label: 'Catalog' },
          { id: 'installed', label: 'Installed', count: installedCount },
          { id: 'skills-sh', label: 'Browse skills.sh' },
        ]}
      />
      <div className="ks-row">
        <FilterBar id="mk-q" label="Filter the catalog" chips={chips} placeholder="kind: category: health: publisher:" value={q} onChange={setQ} />
        <div className="ks-row" role="group" aria-label="Kind">
          {kinds.map((k) => (
            <Pill key={k} pressed={k === kind} onClick={() => setKind(k)}>
              {k}
            </Pill>
          ))}
        </div>
        <div className="ks-views">
          <span className="ks-meta">Views</span>
          <Pill pressed={category === 'Kubernetes' && cleanOnly} onClick={kubernetesClean}>
            Kubernetes, clean
          </Pill>
          <Pill pressed={needsReview} onClick={review}>
            Needs review
          </Pill>
        </div>
      </div>
      <div className="ks-split">
        <nav className="ks-aside ks-stack" aria-label="Categories" style={{ flex: '0 1 200px' }}>
          <span className="ks-label">Categories</span>
          {marketCategories.map((c) => (
            <a
              key={c.name}
              className="sb-a"
              href="#"
              aria-current={c.name === category ? 'page' : undefined}
              onClick={(e) => {
                e.preventDefault()
                setCategory(c.name)
              }}
            >
              <span style={{ flex: 1 }}>{c.name}</span>
              <span className="ks-meta ks-num">{c.count}</span>
            </a>
          ))}
        </nav>
        <div id={`mk-${tab}-panel`} role="tabpanel" aria-labelledby={`mk-${tab}-tab`} className="ks-main-part ks-grid" style={{ gridTemplateColumns: 'repeat(auto-fill, minmax(300px, 1fr))' }}>
          {items.length === 0 ? (
            <div style={{ gridColumn: '1 / -1' }}>
              <EmptyState
                title="Nothing matches"
                action={
                  <Button
                    variant="outline"
                    onClick={() => {
                      setQ('')
                      setKind('All')
                      setCleanOnly(false)
                      setNeedsReview(false)
                      setCategory('All')
                    }}
                  >
                    Clear filters
                  </Button>
                }
              >
                No server or skill fits these filters. Only 8 of 38 sample entries are shown in this preview.
              </EmptyState>
            </div>
          ) : (
            items.map((it) => <Card key={it.name} it={it} added={added.has(it.name)} onAct={() => setAdded(new Set(added).add(it.name))} />)
          )}
        </div>
      </div>
      <Dialog
        open={remote}
        onOpenChange={setRemote}
        title="Connect a remote server"
        description="Koinon reads the server's tools, pins their hash and scans them before an agent can use it."
        footer={
          <>
            <DialogClose>
              <Button variant="outline">Cancel</Button>
            </DialogClose>
            <DialogClose>
              <Button disabled={!url.trim()}>Connect</Button>
            </DialogClose>
          </>
        }
      >
        <Field id="mk-remote-url" label="Server URL">
          <Input id="mk-remote-url" className="ks-mono" placeholder="https://api.githubcopilot.com/mcp/" value={url} onChange={(e) => setUrl(e.target.value)} />
        </Field>
      </Dialog>
    </PreviewShell>
  )
}
