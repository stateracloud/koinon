// SPDX-License-Identifier: Apache-2.0

import type { ModelPriceSource, PlatformPriceEntry, PlatformPriceVersion, PriceEntry, Rates } from '@/gen/koinon/budget/v1alpha1/price_pb'
import { matchLabel, type PriceFormValues, type RateFields } from '@/lib/prices'

// The row model of <PriceTable> (components/price-table.tsx): the model prices table with three source badges
// (ADR 0126 section 6, issue #1277). The org page builds rows from ExplainOrgPrices, the platform page from the
// newest platform version.

/** PriceSource is the layer that prices a row: an org price, the platform list or the default list. */
export type PriceSource = 'org' | 'platform' | 'default'

export type PriceRow = {
  /** A stable id: the provider and the model, or the price ref. */
  id: string
  /** The model name, or the model pattern of a list entry. */
  model: string
  /** The provider name an org gave it; on the platform page, the provider type. */
  provider: string
  /** The provider type the price applies to. */
  providerType: string
  /** Empty when no usable price matches the model. */
  source: PriceSource | ''
  rates?: Rates
  /** For an org override: the rates of the layer below that it replaces. */
  replaced?: Rates
  replacedSource?: PriceSource
  priceRef: string
  /** Why a row has no source: no_price or unusable_price. */
  unpricedReason?: string
  /** The entry in use, to fill an override with its tiers and estimate settings. */
  entry?: PriceEntry
}

export const sourceLabels: Record<PriceSource, string> = { org: 'Org override', platform: 'Platform', default: 'Default' }

/** sourceLegend is the one-line legend above the table, one item for each badge. */
export const sourceLegend: { source: PriceSource; help: string }[] = [
  { source: 'org', help: 'a price your org set. It wins over the other two.' },
  { source: 'platform', help: 'a price the platform admin set for all orgs.' },
  { source: 'default', help: 'the list that ships with Koinon.' },
]

function asSource(s: string): PriceSource | '' {
  return s === 'org' || s === 'platform' || s === 'default' ? s : ''
}

/** rowFromModel makes a row from one model of ExplainOrgPrices. */
export function rowFromModel(m: ModelPriceSource): PriceRow {
  const e = m.entry
  return {
    id: `${m.providerName}/${m.model}`,
    model: m.model,
    provider: m.providerName,
    providerType: m.providerType,
    source: asSource(m.source),
    rates: e?.rates,
    replaced: m.replaces?.entry?.rates,
    replacedSource: m.replaces ? asSource(m.replaces.source) || undefined : undefined,
    priceRef: m.priceRef,
    unpricedReason: m.unpricedReason || undefined,
    entry: e,
  }
}

/** rowFromPlatformEntry makes a row from one entry of a platform version. */
export function rowFromPlatformEntry(p: PlatformPriceEntry): PriceRow {
  const e = p.entry
  return {
    id: p.priceRef,
    model: matchLabel(e?.match) || e?.id || p.priceRef,
    provider: !e?.provider || e.provider === '*' ? 'any provider' : e.provider,
    providerType: e?.provider ?? '',
    source: 'platform',
    rates: e?.rates,
    priceRef: p.priceRef,
    entry: e,
  }
}

/** matchesPriceSearch finds a row by model or provider (name or type), without case. Empty finds all. */
export function matchesPriceSearch(r: PriceRow, q: string): boolean {
  const s = q.trim().toLowerCase()
  if (!s) return true
  return [r.model, r.provider, r.providerType].some((v) => v.toLowerCase().includes(s))
}

/** rateKey names the four rate columns of the table. */
export type RateKey = 'input' | 'output' | 'cacheRead' | 'cacheWrite'

/** rateText shows a rate in USD per million tokens; an unset rate is a dash. */
export function rateText(v: string | undefined): string {
  return v ? `$${v}` : '—'
}

/** rateSortValue is the number a rate sorts by; undefined when unset, so the table puts it last in both directions. */
export function rateSortValue(v: string | undefined): number | undefined {
  const n = Number(v)
  return v && Number.isFinite(n) ? n : undefined
}

/** unpricedText says why a model has no price. */
export function unpricedText(reason: string | undefined): string {
  return reason === 'unusable_price' ? 'Its price cannot be used' : 'No price'
}

/** overrideName derives a valid price name from text: lowercase, other characters as "-", at most 128. */
export function overrideName(text: string): string {
  const s = text
    .toLowerCase()
    .replace(/[^a-z0-9._-]+/g, '-')
    .replace(/^[^a-z0-9]+/, '')
    .slice(0, 128)
  return s || 'override'
}

/**
 * uniqueOverrideName makes the name of an override from the provider type and the model. It adds -2, -3, ... when the
 * org already has a price with that name, because a name that exists would add a version to that price.
 */
export function uniqueOverrideName(providerType: string, model: string, taken: Iterable<string>): string {
  const used = new Set(taken)
  const base = overrideName(`${providerType || 'any'}-${model}`).slice(0, 120)
  if (!used.has(base)) return base
  for (let n = 2; ; n++) {
    const name = `${base}-${n}`
    if (!used.has(name)) return name
  }
}

/**
 * overrideMatch matches exactly one model name. A glob has only * and ? as wildcards (pricing.Match) and no escape,
 * so a name that holds one of them becomes a quoted regex.
 */
export function overrideMatch(model: string): Pick<PriceFormValues, 'matchKind' | 'pattern'> {
  if (!/[*?]/.test(model)) return { matchKind: 'glob', pattern: model }
  return { matchKind: 'regex', pattern: model.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') }
}

/**
 * overrideForm fills the price form to override the price of one row for the org: the same model and provider type,
 * and the rates and estimate settings in use now, so the admin changes only what differs. The name is new: it
 * differs from every name in takenNames.
 */
export function overrideForm(r: PriceRow, takenNames: Iterable<string> = []): PriceFormValues {
  const e = r.entry
  const rates = (x?: Rates): RateFields => ({
    input: x?.input ?? '', output: x?.output ?? '', cacheRead: x?.cacheRead ?? '', cacheWrite: x?.cacheWrite ?? '', reasoning: x?.reasoning ?? '',
  })
  return {
    name: uniqueOverrideName(r.providerType, r.model, takenNames),
    provider: r.providerType || '*',
    ...overrideMatch(r.model),
    rates: rates(r.rates),
    tiers: (e?.tiers ?? []).map((t) => ({ contextOver: String(t.contextOver), rates: rates(t.rates) })),
    tokenizer: e?.tokenizer || 'bytes',
    ratio: e?.ratio || '3.0',
    toolOverhead: String(e?.toolOverheadTokens ?? 0),
    hiddenThinking: e?.hiddenThinking ?? false,
    maxOutput: e?.maxOutput ? String(e.maxOutput) : '',
    validFrom: '',
  }
}

/**
 * platformVersionInUse picks the platform version like the server does: the version in use is the one with the
 * latest start that is not in the future (a missing start counts as started, at 0), and a higher version number
 * breaks a tie. The upcoming version is the one with the earliest start in the future (the highest version number
 * breaks a tie). The version number alone does not say which one applies: a newer version can start earlier.
 */
export function platformVersionInUse(
  versions: readonly PlatformPriceVersion[],
  now = new Date(),
): { inUse?: PlatformPriceVersion; upcoming?: PlatformPriceVersion } {
  const nowSec = BigInt(Math.floor(now.getTime() / 1000))
  const start = (v: PlatformPriceVersion) => v.validFrom?.seconds ?? 0n
  let inUse: PlatformPriceVersion | undefined
  let upcoming: PlatformPriceVersion | undefined
  for (const v of versions) {
    if (start(v) <= nowSec) {
      if (!inUse || start(v) > start(inUse) || (start(v) === start(inUse) && v.version > inUse.version)) inUse = v
    } else if (!upcoming || start(v) < start(upcoming) || (start(v) === start(upcoming) && v.version > upcoming.version)) {
      upcoming = v
    }
  }
  return { inUse, upcoming }
}
