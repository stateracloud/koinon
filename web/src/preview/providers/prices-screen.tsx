// SPDX-License-Identifier: Apache-2.0

import { useMemo, useState } from 'react'
import { Badge, Button, Drawer, FilterBar, Input, Pill, PageHeader, PreviewShell, Segmented, usePaletteSelection, type FilterChip } from '@/preview/ui'
import { prices, type Price } from '@/preview/mock/data'
import { sonnetHistory, sonnetTiers, type PriceTierRow } from './data'

type Unit = 'm' | 'k'
type View = 'used' | 'overrides' | 'tiered'

const keyOf = (p: Price) => `${p.provider}/${p.model}`
const overrideOf = (p: Price, set: Set<string>) => p.source === 'Override' || set.has(keyOf(p))
/** Providers with spend this month; vllm-gpu-lab has none. */
const usedThisMonth = new Set(['anthropic', 'openrouter', 'deepseek-org'])

function money(n: number | null, unit: Unit) {
  if (n === null) return '—'
  return '$' + (unit === 'm' ? n.toFixed(2) : (n / 1000).toFixed(5))
}

/** A glob such as claude-sonnet-5* against a model string, with or without the provider prefix. */
function globMatches(glob: string, text: string, provider: string) {
  const re = new RegExp('^' + glob.replace(/[.+?^${}()|[\]\\]/g, '\\$&').replace(/\*/g, '.*') + '$')
  const bare = text.startsWith(provider + '/') ? text.slice(provider.length + 1) : text
  return re.test(text) || re.test(bare)
}

export function PricesScreen() {
  const [unit, setUnit] = useState<Unit>('m')
  const [providerChip, setProviderChip] = useState<string[] | null>(['anthropic', 'openrouter'])
  const [query, setQuery] = useState('')
  const [view, setView] = useState<View | null>('used')
  const [sel, setSel] = useState<string | null>('anthropic/claude-sonnet-5')
  const [open, setOpen] = useState(true)
  const [overridden, setOverridden] = useState<Set<string>>(() => new Set())

  const isOverride = (p: Price) => overrideOf(p, overridden)

  const rows = useMemo(() => {
    const q = query.trim().toLowerCase()
    return prices.filter((p) => {
      if (providerChip && !providerChip.includes(p.provider)) return false
      if (view === 'used' && !usedThisMonth.has(p.provider)) return false
      if (view === 'overrides' && !overrideOf(p, overridden)) return false
      if (view === 'tiered' && !p.tiered) return false
      if (!q) return true
      return q.split(/\s+/).every((tok) => {
        const [k, v] = tok.includes(':') ? (tok.split(/:(.*)/, 2) as [string, string]) : ['', tok]
        const hay = k === 'model' ? p.model : k === 'provider' ? p.provider : k === 'override' ? (overrideOf(p, overridden) ? 'yes' : 'no') : k === 'tier' ? (p.tiered ? 'yes' : 'no') : `${p.model} ${p.provider} ${p.match}`
        return hay.toLowerCase().includes(v.toLowerCase())
      })
    })
  }, [providerChip, query, view, overridden])

  const selected = prices.find((p) => keyOf(p) === sel) ?? null
  const chips: FilterChip[] = providerChip ? [{ key: 'provider', value: providerChip.join(','), onRemove: () => setProviderChip(null) }] : []

  usePaletteSelection(
    selected
      ? {
          key: keyOf(selected),
          title: selected.model,
          heading: 'For the selected price',
          actions: [{ label: 'Open price details', onSelect: () => setOpen(true) }],
        }
      : null,
  )

  return (
    <PreviewShell space="org" current="Prices" crumbs={[{ label: 'Prices' }]}>
      <PageHeader
        title="Prices"
        description="What Koinon charges to the ledger for each model, in USD. Platform prices apply unless this org overrides one."
        actions={
          <Segmented<Unit>
            label="Unit"
            value={unit}
            onChange={setUnit}
            options={[
              { id: 'm', label: 'per 1M tokens' },
              { id: 'k', label: 'per 1k tokens' },
            ]}
          />
        }
      />
      <div className="ks-row">
        <FilterBar id="pr-q" label="Filter prices" chips={chips} placeholder="model: override: tier: used:" value={query} onChange={setQuery} />
        <div className="ks-views" role="group" aria-label="Views">
          <span className="ks-meta" aria-hidden="true">Views</span>
          {([['used', 'Used this month'], ['overrides', 'Overrides'], ['tiered', 'Tiered']] as const).map(([id, label]) => (
            <Pill key={id} pressed={view === id} onClick={() => setView(view === id ? null : id)}>
              {label}
            </Pill>
          ))}
        </div>
      </div>
      <div className="ks-panel-flush ks-table-wrap" role="region" aria-label="Prices" tabIndex={0}>
        <table className="ks-table">
          <thead>
            <tr>
              <th scope="col">Model</th>
              <th scope="col">Provider</th>
              <th scope="col">Match</th>
              <th scope="col" className="ks-r">Input</th>
              <th scope="col" className="ks-r">Output</th>
              <th scope="col" className="ks-r">Cache read</th>
              <th scope="col" className="ks-r">Cache write</th>
              <th scope="col">Valid from</th>
              <th scope="col">Source</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((p) => {
              const k = keyOf(p)
              const ov = isOverride(p)
              return (
                <tr key={k} aria-selected={k === sel}>
                  <td>
                    <button
                      type="button"
                      className="ks-a ks-mono"
                      style={{ border: 0, background: 'none', padding: 0, font: 'inherit', cursor: 'pointer' }}
                      onClick={() => {
                        setSel(k)
                        setOpen(true)
                      }}
                    >
                      {p.model}
                    </button>
                    {p.tiered && <> <Badge variant="outline">&gt;200k tier</Badge></>}
                  </td>
                  <td>{p.provider}</td>
                  <td className="ks-mono ks-meta">{p.match}</td>
                  <td className="ks-r">{money(p.input, unit)}</td>
                  <td className="ks-r">{money(p.output, unit)}</td>
                  <td className="ks-r">{money(p.cacheRead, unit)}</td>
                  <td className="ks-r">{money(p.cacheWrite, unit)}</td>
                  <td className="ks-num ks-muted">{p.from}</td>
                  <td>
                    <Badge variant={ov ? 'outline' : 'secondary'}>{ov ? 'Override' : 'Default'}</Badge>
                  </td>
                </tr>
              )
            })}
            {rows.length === 0 && (
              <tr>
                <td colSpan={9} className="ks-muted">
                  No prices match. Remove a filter to see more.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
      <p className="ks-p ks-meta">Costs include cache reads and writes at these prices. The catalog comes from a reviewed models.dev snapshot.</p>

      <Drawer
        open={open && selected !== null}
        onOpenChange={setOpen}
        modal={false}
        title={<span className="ks-mono">{selected?.model}</span>}
        description={selected && `${selected.provider} · ${isOverride(selected) ? 'org override' : 'platform price'} · per ${unit === 'm' ? '1M' : '1k'} tokens`}
      >
        {selected && (
          <PriceDetail
            key={keyOf(selected)}
            price={selected}
            unit={unit}
            overridden={overridden.has(keyOf(selected))}
            onOverride={(on) =>
              setOverridden((s) => {
                const n = new Set(s)
                if (on) n.add(keyOf(selected))
                else n.delete(keyOf(selected))
                return n
              })
            }
          />
        )}
      </Drawer>
    </PreviewShell>
  )
}

function PriceDetail({ price, unit, overridden, onOverride }: { price: Price; unit: Unit; overridden: boolean; onOverride: (on: boolean) => void }) {
  const isSonnet = price.model === 'claude-sonnet-5'
  const tiers: PriceTierRow[] = isSonnet
    ? sonnetTiers
    : [{ label: price.tiered ? 'Up to 200k' : 'All prompt sizes', input: price.input, output: price.output, cacheRead: price.cacheRead ?? NaN, cacheWrite: price.cacheWrite ?? NaN }]
  const history = isSonnet
    ? sonnetHistory
    : [{ date: price.from, what: `First price, ${money(price.input, 'm')} / ${money(price.output, 'm')}`, by: 'catalog sync' }]
  const defaultTest = isSonnet ? 'anthropic/claude-sonnet-5-20261015' : `${price.provider}/${price.model}`
  const [text, setText] = useState(defaultTest)
  const [tested, setTested] = useState(defaultTest)
  const hit = globMatches(price.match, tested, price.provider)
  const cell = (n: number) => (Number.isNaN(n) ? '—' : money(n, unit))
  const disc = (n: number | null) => (n === null ? '—' : money(n * 0.85, 'm'))

  return (
    <>
      <section className="ks-stack" aria-labelledby="dr-tiers-h">
        <h3 id="dr-tiers-h" className="ks-h4">Tiers</h3>
        <table className="ks-table" style={{ border: '1px solid var(--divider)', borderRadius: 'var(--radius-md)' }}>
          <thead>
            <tr>
              <th scope="col">Prompt size</th>
              <th scope="col" className="ks-r">Input</th>
              <th scope="col" className="ks-r">Output</th>
              <th scope="col" className="ks-r">Cache read</th>
              <th scope="col" className="ks-r">Cache write</th>
            </tr>
          </thead>
          <tbody>
            {tiers.map((t) => (
              <tr key={t.label}>
                <td>{t.label}</td>
                <td className="ks-r">{cell(t.input)}</td>
                <td className="ks-r">{cell(t.output)}</td>
                <td className="ks-r">{cell(t.cacheRead)}</td>
                <td className="ks-r">{cell(t.cacheWrite)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>

      <section className="ks-stack" aria-labelledby="dr-match-h">
        <h3 id="dr-match-h" className="ks-h4">Match test</h3>
        <form
          className="ks-row"
          style={{ flexWrap: 'nowrap' }}
          onSubmit={(e) => {
            e.preventDefault()
            setTested(text)
          }}
        >
          <label className="ks-sr" htmlFor="mt">Model string</label>
          <Input id="mt" className="ks-mono" value={text} onChange={(e) => setText(e.target.value)} autoComplete="off" spellCheck={false} />
          <Button type="submit" variant="outline" size="sm">Test</Button>
        </form>
        <span className="ks-meta" role="status">
          {hit ? (
            <>Matches <span className="ks-mono">{price.match}</span> · this row</>
          ) : (
            <>Does not match <span className="ks-mono">{price.match}</span></>
          )}
        </span>
      </section>

      <section className="ks-stack" aria-labelledby="dr-hist-h">
        <h3 id="dr-hist-h" className="ks-h4">History</h3>
        <ol className="ks-ul ks-stack" style={{ fontSize: 13, borderLeft: '1px solid var(--border)', paddingLeft: 12 }}>
          {overridden && (
            <li>
              <span className="ks-num">1 Nov 2026</span> · Org override scheduled <span className="ks-meta">· by Andre Staats</span>
            </li>
          )}
          {history.map((h) => (
            <li key={h.date}>
              <span className="ks-num">{h.date}</span> · {h.what} <span className="ks-meta">· {h.by}</span>
            </li>
          ))}
        </ol>
      </section>

      <section className="ks-raised ks-stack-3" style={{ padding: 12 }} aria-labelledby="dr-ov-h">
        <h3 id="dr-ov-h" className="ks-h4">Override for this org</h3>
        <span className="ks-meta">Use it when your contract price differs. The ledger uses it from the date you pick.</span>
        <div className="ks-diff">
          <div className="ks-del">- input  {money(price.input, 'm')}   output {money(price.output, 'm')}</div>
          <div className="ks-add">+ input  {disc(price.input)}   output {disc(price.output)}   (15% contract discount)</div>
        </div>
        <div className="ks-row" style={{ justifyContent: 'flex-end' }}>
          <Button variant="ghost" size="sm" onClick={() => onOverride(false)} disabled={!overridden}>
            Revert to platform
          </Button>
          <Button size="sm" onClick={() => onOverride(true)} disabled={overridden}>
            {overridden ? 'Override saved from 1 Nov' : 'Save override from 1 Nov'}
          </Button>
        </div>
      </section>
    </>
  )
}

