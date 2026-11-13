// SPDX-License-Identifier: Apache-2.0

import { timestampDate } from '@bufbuild/protobuf/wkt'
import type { MessageInitShape } from '@bufbuild/protobuf'
import type { FlaggedPrice, PriceChange, PriceEntry, PriceVersion, SetPriceRequestSchema } from '@/gen/koinon/budget/v1alpha1/price_pb'

export const tokenizers = [
  { value: 'bytes', label: 'Bytes (3 bytes per token unless you set a ratio)' },
  { value: 'tiktoken:o200k_base', label: 'OpenAI o200k_base' },
  { value: 'tiktoken:cl100k_base', label: 'OpenAI cl100k_base' },
] as const

export const providerChoices = ['*', 'openrouter', 'openai', 'anthropic', 'ollama'] as const

// Limits the server enforces (pricing/price.go).
const namePattern = /^[a-z0-9][a-z0-9._-]{0,127}$/
const providerPattern = /^(\*|[a-z][a-z0-9-]{0,62})$/
const ratePattern = /^[0-9]{1,6}(\.[0-9]{1,6})?$/
const ratioPattern = /^[0-9]{1,2}(\.[0-9]{1,3})?$/
const maxOverheadTokens = 100_000
const maxOutputTokens = 10_000_000

export type RateFields = { input: string; output: string; cacheRead: string; cacheWrite: string; reasoning: string }
export type TierFields = { contextOver: string; rates: RateFields }
export type PriceFormValues = {
  name: string
  provider: string
  matchKind: 'glob' | 'regex'
  pattern: string
  rates: RateFields
  tiers: TierFields[]
  tokenizer: string
  ratio: string
  toolOverhead: string
  hiddenThinking: boolean
  maxOutput: string
  /** yyyy-mm-dd, UTC; empty means now. */
  validFrom: string
}

export const emptyRates: RateFields = { input: '', output: '', cacheRead: '', cacheWrite: '', reasoning: '' }

export function emptyPriceForm(): PriceFormValues {
  return {
    name: '', provider: '*', matchKind: 'glob', pattern: '', rates: { ...emptyRates }, tiers: [], tokenizer: 'bytes', ratio: '3.0',
    toolOverhead: '0', hiddenThinking: false, maxOutput: '', validFrom: '',
  }
}

/** formFromVersion fills the form with a price's current version, to add a new version of it. */
export function formFromVersion(v: PriceVersion): PriceFormValues {
  const rates = (r?: { input: string; output: string; cacheRead: string; cacheWrite: string; reasoning: string }): RateFields => ({
    input: r?.input ?? '', output: r?.output ?? '', cacheRead: r?.cacheRead ?? '', cacheWrite: r?.cacheWrite ?? '', reasoning: r?.reasoning ?? '',
  })
  return {
    name: v.name, provider: v.provider, matchKind: v.match?.regex ? 'regex' : 'glob', pattern: v.match?.regex || v.match?.glob || '',
    rates: rates(v.rates), tiers: v.tiers.map((t) => ({ contextOver: String(t.contextOver), rates: rates(t.rates) })),
    tokenizer: v.tokenizer || 'bytes', ratio: v.ratio || '3.0', toolOverhead: String(v.toolOverheadTokens), hiddenThinking: v.hiddenThinking,
    maxOutput: v.maxOutput ? String(v.maxOutput) : '', validFrom: '',
  }
}

const rateKeys = ['input', 'output', 'cacheRead', 'cacheWrite', 'reasoning'] as const
const rateLabels: Record<(typeof rateKeys)[number], string> = {
  input: 'Input', output: 'Output', cacheRead: 'Cache read', cacheWrite: 'Cache write', reasoning: 'Reasoning',
}

function rateErrors(prefix: string, r: RateFields, required: boolean): Record<string, string> {
  const out: Record<string, string> = {}
  for (const k of rateKeys) {
    const v = r[k].trim()
    const need = required && (k === 'input' || k === 'output')
    if (!v) {
      if (need) out[`${prefix}${k}`] = `${rateLabels[k]} price is required (a free model needs an explicit 0).`
    } else if (!ratePattern.test(v)) {
      out[`${prefix}${k}`] = `${rateLabels[k]} price must be a decimal such as 0.30, with at most 6 digits after the point.`
    }
  }
  return out
}

function nonzero(r: RateFields): boolean {
  return rateKeys.some((k) => r[k].trim() !== '' && Number(r[k]) !== 0)
}

/**
 * validatePrice checks a price form the way the server does (pricing.Entry.Resolve, prices.SetOverride), so a
 * refusal arrives next to the field instead of after the request. Keys are field names; tier fields are
 * tier<N>contextOver and tier<N><rate>.
 */
export function validatePrice(f: PriceFormValues, editing = false): Record<string, string> {
  const e: Record<string, string> = {}
  if (!editing && !namePattern.test(f.name)) e.name = 'Name is required: lowercase letters, digits, . _ - , starting with a letter or digit.'
  if (!providerPattern.test(f.provider)) e.provider = 'Provider must be a provider type such as openrouter, or * for any.'
  if (!f.pattern.trim()) e.pattern = 'Model pattern is required.'
  else if (f.pattern.length > 1024) e.pattern = 'Model pattern is longer than 1024 characters.'
  else if (f.matchKind === 'regex') {
    try {
      new RegExp(f.pattern)
    } catch {
      e.pattern = 'This is not a valid regular expression.'
    }
  }
  Object.assign(e, rateErrors('', f.rates, true))
  let prev = 0
  f.tiers.forEach((t, i) => {
    const n = Number(t.contextOver)
    if (!/^[0-9]+$/.test(t.contextOver.trim()) || n <= 0) e[`tier${i}contextOver`] = 'Tier threshold must be a positive whole number of tokens.'
    else if (n <= prev) e[`tier${i}contextOver`] = 'Tier thresholds must go up.'
    else prev = n
    Object.assign(e, rateErrors(`tier${i}`, t.rates, false))
    if (!rateKeys.some((k) => t.rates[k].trim() !== '') && !e[`tier${i}contextOver`]) e[`tier${i}contextOver`] = 'A tier must set at least one price.'
  })
  if (!f.tokenizer || !tokenizers.some((t) => t.value === f.tokenizer)) e.tokenizer = 'Choose a tokenizer.'
  const ratio = Number(f.ratio)
  if (!ratioPattern.test(f.ratio.trim()) || ratio < 0.5 || ratio > 16) e.ratio = 'Ratio is bytes per token, between 0.5 and 16, such as 3.0.'
  const overhead = Number(f.toolOverhead)
  if (!/^[0-9]+$/.test(f.toolOverhead.trim()) || overhead > maxOverheadTokens) e.toolOverhead = `Tool overhead is a whole number of tokens, 0 to ${maxOverheadTokens}.`
  const out = f.maxOutput.trim()
  if (out && (!/^[0-9]+$/.test(out) || Number(out) > maxOutputTokens)) e.maxOutput = `Largest output must be a whole number of tokens, up to ${maxOutputTokens.toLocaleString('en-US')}.`
  else if (!out && nonzero(f.rates)) e.maxOutput = 'Largest output is required for a priced entry: an aborted response without usage is charged up to it.'
  if (f.validFrom && !/^\d{4}-\d{2}-\d{2}$/.test(f.validFrom)) e.validFrom = 'Start date must be a date.'
  return e
}

const wire = (r: RateFields) => ({ input: r.input.trim(), output: r.output.trim(), cacheRead: r.cacheRead.trim(), cacheWrite: r.cacheWrite.trim(), reasoning: r.reasoning.trim() })

/** toSetPrice builds the SetPrice request for a form that passed validatePrice. */
export function toSetPrice(orgId: string, f: PriceFormValues): MessageInitShape<typeof SetPriceRequestSchema> {
  return {
    orgId,
    name: f.name,
    provider: f.provider,
    match: f.matchKind === 'glob' ? { glob: f.pattern.trim() } : { regex: f.pattern.trim() },
    rates: wire(f.rates),
    tiers: f.tiers.map((t) => ({ contextOver: BigInt(t.contextOver.trim()), rates: wire(t.rates) })),
    tokenizer: f.tokenizer,
    ratio: f.ratio.trim(),
    toolOverheadTokens: Number(f.toolOverhead.trim() || '0'),
    hiddenThinking: f.hiddenThinking,
    maxOutput: Number(f.maxOutput.trim() || '0'),
    validFrom: f.validFrom ? { seconds: BigInt(Math.floor(Date.parse(`${f.validFrom}T00:00:00Z`) / 1000)), nanos: 0 } : undefined,
  }
}

export type PriceName = { name: string; current?: PriceVersion; versions: PriceVersion[] }

/**
 * groupPrices groups the org's versions by name, newest first. The current version is the newest that has started and
 * is not retired; a price whose newest version has not started yet has none until then.
 */
export function groupPrices(versions: readonly PriceVersion[], now = new Date()): PriceName[] {
  const by = new Map<string, PriceVersion[]>()
  for (const v of versions) by.set(v.name, [...(by.get(v.name) ?? []), v])
  const out: PriceName[] = []
  for (const [name, vs] of by) {
    const sorted = [...vs].sort((a, b) => time(b.validFrom) - time(a.validFrom))
    const current = sorted.find((v) => time(v.validFrom) <= now.getTime() && !(v.retiredAt && time(v.retiredAt) <= now.getTime()))
    out.push({ name, current, versions: sorted })
  }
  return out.sort((a, b) => a.name.localeCompare(b.name))
}

function time(ts: { seconds: bigint } | undefined): number {
  return ts ? Number(ts.seconds) * 1000 : 0
}

export function matchLabel(m: { glob: string; regex: string } | undefined): string {
  if (!m) return ''
  return m.regex ? `regex ${m.regex}` : m.glob
}

/** ratesLine: "$0.30 in, $1.20 out per million tokens", with the optional buckets when set. */
export function ratesLine(r: { input: string; output: string; cacheRead: string; cacheWrite: string; reasoning: string } | undefined): string {
  if (!r) return 'No prices'
  const parts = [`$${r.input} in`, `$${r.output} out`]
  if (r.cacheRead) parts.push(`$${r.cacheRead} cache read`)
  if (r.cacheWrite) parts.push(`$${r.cacheWrite} cache write`)
  if (r.reasoning) parts.push(`$${r.reasoning} reasoning`)
  return `${parts.join(', ')} per million tokens`
}

/** changeById maps the entries the latest default-list update added or changed, to show the marker. */
export function changeById(changes: readonly PriceChange[]): Map<string, PriceChange> {
  const m = new Map<string, PriceChange>()
  for (const c of changes) if (c.kind !== 'removed') m.set(c.id, c)
  return m
}

export function changeLabel(c: PriceChange): string {
  if (c.kind === 'added') return 'New since your last upgrade'
  return `Changed since your last upgrade${c.fields.length ? `: ${c.fields.join(', ')}` : ''}`
}

/** safeSource returns the source URL only when it is an https link, so a bad value never becomes a link. */
export function safeSource(e: PriceEntry): string | undefined {
  try {
    const u = new URL(e.sourceUrl)
    return u.protocol === 'https:' ? u.href : undefined
  } catch {
    return undefined
  }
}

export type FlagReason = 'gateway_diff' | 'usage_missing' | 'max_output_capped' | 'price_error'
export const flagReasons: { value: FlagReason; label: string; help: string }[] = [
  { value: 'gateway_diff', label: 'Cost differs from the gateway by more than 1%', help: 'Koinon’s cost and the gateway’s cost disagree. The gateway’s catalog may be out of date, or the provider bills a bucket this price does not name.' },
  { value: 'usage_missing', label: 'Usage missing', help: 'The provider reported no usage, so Koinon charged the request at the largest output it could have had.' },
  { value: 'max_output_capped', label: 'Capped at the largest output', help: 'An estimated output was cut to this price’s largest output. Raise it if the model can write more.' },
  { value: 'price_error', label: 'Price error', help: 'The stored price could not be used, so the request was recorded without a price.' },
]

export function reasonsOf(f: FlaggedPrice): FlagReason[] {
  const out: FlagReason[] = []
  if (f.gatewayDiff > 0n) out.push('gateway_diff')
  if (f.usageMissing > 0n) out.push('usage_missing')
  if (f.maxOutputCapped > 0n) out.push('max_output_capped')
  if (f.priceError > 0n) out.push('price_error')
  return out
}

export type FlagFilter = { reason: FlagReason | ''; model: string }

/** filterFlagged keeps the entries with the chosen reason and whose model or price contains the text. */
export function filterFlagged(list: readonly FlaggedPrice[], f: FlagFilter): FlaggedPrice[] {
  const q = f.model.trim().toLowerCase()
  return list.filter((x) => (!f.reason || reasonsOf(x).includes(f.reason)) && (!q || x.model.toLowerCase().includes(q) || x.priceRef.toLowerCase().includes(q)))
}

export const sinceChoices = [
  { days: 1, label: 'Last 24 hours' },
  { days: 7, label: 'Last 7 days' },
  { days: 30, label: 'Last 30 days' },
] as const

export function sinceTimestamp(days: number, now = Date.now()) {
  return { seconds: BigInt(Math.floor((now - days * 86_400_000) / 1000)), nanos: 0 }
}

export function formatWhen(ts: { seconds: bigint; nanos: number } | undefined): string {
  return ts ? timestampDate(ts as Parameters<typeof timestampDate>[0]).toLocaleString() : ''
}

/** rateChangeLine says one rate of a proposal in words: "input $3 → $4", "tier over 200000: output added at $9". */
export function rateChangeLine(c: { bucket: string; contextOver: bigint; from: string; to: string }): string {
  const bucket = c.bucket.replaceAll('_', ' ')
  const where = c.contextOver > 0n ? ` (over ${c.contextOver} tokens)` : ''
  if (!c.from) return `${bucket}${where} added at $${c.to}`
  if (!c.to) return `${bucket}${where} removed (was $${c.from})`
  return `${bucket}${where} $${c.from} → $${c.to}`
}

/** classLabel names a proposal class for a person. */
export function classLabel(c: string): string {
  switch (c) {
    case 'auto':
      return 'Auto'
    case 'needs_review':
      return 'Needs review'
    case 'confirm':
      return 'Confirm'
    case 'refused':
      return 'Refused'
    default:
      return c
  }
}

/** actorLabel says who wrote a version or approved a price: the auto-apply, a person or a command. */
export function actorLabel(actor: string): string {
  if (actor.startsWith('system ')) return `Auto (${actor.slice('system '.length)})`
  if (actor.startsWith('user ')) return `User ${actor.slice('user '.length)}`
  if (actor.startsWith('cli ')) return `Command: ${actor.slice('cli '.length)}`
  return actor || 'unknown'
}

/** shortHash shows the start of a hash or commit, enough to compare. */
export function shortHash(h: string): string {
  return h.length > 12 ? h.slice(0, 12) : h
}
