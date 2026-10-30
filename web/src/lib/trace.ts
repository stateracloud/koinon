// SPDX-License-Identifier: Apache-2.0

import { Backend, ContentLevel, type Destination, type TestResult } from '@/gen/koinon/trace/v1alpha1/trace_pb'

/** A destination name is a DNS label of at most 64 characters (the server's rule). */
export const maxDestinationNameLength = 64
const namePattern = /^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$/

export function destinationNameError(name: string): string | undefined {
  return namePattern.test(name) ? undefined : 'Name must be 1 to 64 lowercase letters, digits and dashes, starting and ending with a letter or digit, such as langfuse.'
}

export function endpointError(url: string): string | undefined {
  return /^https?:\/\/\S+$/.test(url.trim()) ? undefined : 'Endpoint must start with http:// or https://.'
}

/** A field a preset asks for: a plain setting (`param`) or a write-only secret. */
export type PresetField = { name: string; label: string; secret: boolean; required: boolean; help?: string }

export type Preset = {
  backend: Backend
  label: string
  endpointExample: string
  endpointHelp: string
  fields: PresetField[]
}

/** presets mirror internal/tracing/export Fields; a generic destination has header rows instead of fields. */
export const presets: readonly Preset[] = [
  {
    backend: Backend.LANGFUSE,
    label: 'Langfuse',
    endpointExample: 'https://cloud.langfuse.com/api/public/otel/v1/traces',
    endpointHelp: 'Your Langfuse host followed by /api/public/otel/v1/traces.',
    fields: [
      { name: 'public_key', label: 'Public key', secret: true, required: true },
      { name: 'secret_key', label: 'Secret key', secret: true, required: true },
    ],
  },
  {
    backend: Backend.DATADOG,
    label: 'Datadog',
    endpointExample: 'https://api.datadoghq.com/api/intake/otlp/v1/traces',
    endpointHelp: 'The OTLP traces endpoint of your Datadog site.',
    fields: [{ name: 'api_key', label: 'API key', secret: true, required: true }],
  },
  {
    backend: Backend.LANGSMITH,
    label: 'LangSmith',
    endpointExample: 'https://api.smith.langchain.com/otel/v1/traces',
    endpointHelp: 'The OTLP traces endpoint of LangSmith.',
    fields: [
      { name: 'api_key', label: 'API key', secret: true, required: true },
      { name: 'project', label: 'Project', secret: false, required: false, help: 'Optional. LangSmith uses its default project when empty.' },
    ],
  },
  {
    backend: Backend.OPIK,
    label: 'Opik',
    endpointExample: 'https://www.comet.com/opik/api/v1/private/otel/v1/traces',
    endpointHelp: 'The OTLP traces endpoint of Opik.',
    fields: [
      { name: 'api_key', label: 'API key', secret: true, required: false, help: 'Optional: leave it empty for a self-hosted Opik without authentication.' },
      { name: 'workspace', label: 'Workspace', secret: false, required: false, help: 'Optional.' },
      { name: 'project', label: 'Project', secret: false, required: false, help: 'Optional.' },
    ],
  },
  {
    backend: Backend.GENERIC,
    label: 'Generic OTLP/HTTP',
    endpointExample: 'https://collector.example.com/v1/traces',
    endpointHelp: 'Any OTLP/HTTP traces endpoint. Koinon sends the headers you give, as given.',
    fields: [],
  },
]

export function presetOf(backend: Backend): Preset {
  return presets.find((p) => p.backend === backend) ?? presets[0]
}

export function backendLabel(backend: Backend): string {
  return backend === Backend.UNSPECIFIED ? 'Unknown' : presetOf(backend).label
}

export type HeaderRow = { name: string; value: string; /** An existing header whose value is stored. */ stored: boolean; remove: boolean }

export type DestinationFormState = {
  name: string
  backend: Backend
  endpoint: string
  /** Non-secret settings of the preset, by field name. */
  params: Record<string, string>
  /** Typed secret values of the preset, by field name; write-only, never prefilled. */
  secrets: Record<string, string>
  /** Generic only. */
  headers: HeaderRow[]
  enabled: boolean
}

export function initialForm(d?: Destination): DestinationFormState {
  const backend = d?.backend || Backend.LANGFUSE
  return {
    name: d?.name ?? '',
    backend,
    endpoint: d?.endpointUrl ?? '',
    params: { ...(d?.params ?? {}) },
    secrets: {},
    headers: backend === Backend.GENERIC ? (d?.secretFields ?? []).map((name) => ({ name, value: '', stored: true, remove: false })) : [],
    enabled: d?.enabled ?? true,
  }
}

/** originOf is scheme, host and port of an endpoint; '' when it is not a URL. */
export function originOf(url: string): string {
  try {
    return new URL(url.trim()).origin
  } catch {
    return ''
  }
}

/**
 * keepsSecrets says the stored secrets stay: the backend and the endpoint's origin are those the
 * destination was saved with. Otherwise the server drops them and every secret must be given again.
 */
export function keepsSecrets(f: { backend: Backend; endpoint: string }, d: Destination | undefined): boolean {
  return !!d && d.backend === f.backend && originOf(d.endpointUrl) === originOf(f.endpoint)
}

/** isStored says the destination already holds that secret (a preset field or a header name). */
export function isStored(d: Destination | undefined, name: string): boolean {
  return !!d && d.secretFields.includes(name)
}

export type FormErrors = { name?: string; endpoint?: string; fields: Record<string, string>; headers: Record<number, string> }

const headerPattern = /^[A-Za-z0-9!#$%&'*+.^_|~-]{1,128}$/

/** formErrors validates what the server would refuse for shape alone; the backend's own verdict comes from the test. */
export function formErrors(f: DestinationFormState, d: Destination | undefined, platform: boolean): FormErrors {
  const errors: FormErrors = { fields: {}, headers: {} }
  if (!platform) errors.name = destinationNameError(f.name)
  errors.endpoint = endpointError(f.endpoint)
  const preset = presetOf(f.backend)
  for (const field of preset.fields) {
    if (!field.required) continue
    if (field.secret) {
      if (!f.secrets[field.name]?.trim() && !(keepsSecrets(f, d) && isStored(d, field.name))) errors.fields[field.name] = `${field.label} is required.`
    } else if (!f.params[field.name]?.trim()) {
      errors.fields[field.name] = `${field.label} is required.`
    }
  }
  if (f.backend === Backend.GENERIC) {
    const seen = new Set<string>()
    const keeps = keepsSecrets(f, d)
    let kept = 0
    f.headers.forEach((h, i) => {
      if (h.stored && h.remove) return
      const name = h.name.trim()
      const needsValue = !h.stored || !keeps
      if (!headerPattern.test(name)) errors.headers[i] = 'Header name must be letters, digits and the usual header symbols, such as X-Api-Key.'
      else if (seen.has(name.toLowerCase())) errors.headers[i] = 'Each header can be given once.'
      else if (needsValue && !h.value) errors.headers[i] = 'Header value is required.'
      seen.add(name.toLowerCase())
      kept++
    })
    if (kept === 0) errors.headers[-1] = 'A generic destination needs at least one header.'
  }
  return errors
}

export function hasErrors(e: FormErrors): boolean {
  return !!e.name || !!e.endpoint || Object.keys(e.fields).length > 0 || Object.keys(e.headers).length > 0
}

/**
 * connectionChanged says the form changes the connection (backend, endpoint, settings or secrets), which the
 * API tests before saving. Anything else (enabled, name) is saved untested.
 */
export function connectionChanged(f: DestinationFormState, d: Destination | undefined): boolean {
  if (!d) return true
  if (f.backend !== d.backend || f.endpoint.trim() !== d.endpointUrl) return true
  const preset = presetOf(f.backend)
  for (const field of preset.fields) {
    if (field.secret) {
      if (f.secrets[field.name]?.trim()) return true
    } else if ((f.params[field.name] ?? '').trim() !== (d.params[field.name] ?? '')) return true
  }
  if (f.backend === Backend.GENERIC) {
    return f.headers.some((h) => !h.stored || h.remove || h.value !== '')
  }
  return false
}

export type DestinationWrite = {
  name: string
  backend: Backend
  endpointUrl: string
  params: Record<string, string>
  secrets: Record<string, string>
  removeSecrets: string[]
  resourceAttributes: Record<string, string>
  enabled: boolean
}

/**
 * buildInput turns the form into the write. Secrets merge by name on the server, so only typed ones
 * are sent and a stored one is kept unless it is replaced or removed. An update replaces the other
 * settings, so the destination's resource attributes are passed through unchanged.
 */
export function buildInput(f: DestinationFormState, d?: Destination): DestinationWrite {
  const preset = presetOf(f.backend)
  const params: Record<string, string> = {}
  const secrets: Record<string, string> = {}
  const removeSecrets: string[] = []
  for (const field of preset.fields) {
    if (field.secret) {
      const v = f.secrets[field.name]?.trim()
      if (v) secrets[field.name] = v
    } else {
      const v = f.params[field.name]?.trim()
      if (v) params[field.name] = v
    }
  }
  for (const h of f.headers) {
    const name = h.name.trim()
    if (h.stored && h.remove) removeSecrets.push(name)
    else if (h.value) secrets[name] = h.value
  }
  return {
    name: f.name,
    backend: f.backend,
    endpointUrl: f.endpoint.trim(),
    params,
    secrets,
    removeSecrets,
    resourceAttributes: { ...(d?.resourceAttributes ?? {}) },
    enabled: f.enabled,
  }
}

export const levels = [
  {
    value: ContentLevel.NONE,
    label: 'None',
    help: 'Names, IDs, counts, timings, statuses and cost. No prompts, replies, tool arguments or results, or agent messages. This is the default.',
  },
  {
    value: ContentLevel.REDACTED,
    label: 'Redacted',
    help: 'Content is included after Koinon removes credentials, email addresses, phone numbers, IBANs, card numbers and high-entropy tokens from it. The scrubber can miss things.',
  },
  {
    value: ContentLevel.FULL,
    label: 'Full',
    help: 'Content is included as is, apart from credentials, which are always removed. Anyone with access to the trace backend can read it.',
  },
] as const

export function levelLabel(level: ContentLevel): string {
  return levels.find((l) => l.value === level)?.label ?? 'None'
}

export function levelRank(level: ContentLevel): number {
  return level === ContentLevel.UNSPECIFIED ? 0 : level - 1
}

/** normalLevel reads an unset level as none. */
export function normalLevel(level: ContentLevel): ContentLevel {
  return level === ContentLevel.UNSPECIFIED ? ContentLevel.NONE : level
}

/** clampLevel holds a level to the platform ceiling. */
export function clampLevel(level: ContentLevel, ceiling: ContentLevel): ContentLevel {
  const l = normalLevel(level)
  const c = normalLevel(ceiling)
  return levelRank(l) > levelRank(c) ? c : l
}

/** levelDisabledReason says why the viewer cannot pick a level, or undefined when they can. */
export function levelDisabledReason(level: ContentLevel, ceiling: ContentLevel, isOwner: boolean, current: ContentLevel): string | undefined {
  if (levelRank(level) > levelRank(normalLevel(ceiling))) return `The platform ceiling is ${levelLabel(normalLevel(ceiling)).toLowerCase()}.`
  if (level === ContentLevel.FULL && !isOwner && normalLevel(current) !== ContentLevel.FULL) return 'Only an org owner can choose full.'
  return undefined
}

/** Sampling is edited as a percentage and stored as a share from 0 to 1. */
export function percentToRate(percent: string): number | undefined {
  const n = Number(percent)
  if (percent.trim() === '' || !Number.isFinite(n) || n < 0 || n > 100) return undefined
  return Math.round(n * 100) / 10000
}

export function rateToPercent(rate: number): string {
  return String(Math.round(rate * 10000) / 100)
}

export function samplingError(percent: string): string | undefined {
  return percentToRate(percent) === undefined ? 'Sampling must be a number from 0 to 100.' : undefined
}

export function formatCount(n: bigint): string {
  return n.toString()
}

/** testNotice words a test result for the status line. */
export function testNotice(name: string, r: TestResult | undefined): string {
  if (!r) return `Tested ${name}.`
  return r.ok ? `Test passed for ${name}. The backend accepted the test trace.` : `Test failed for ${name}${r.message ? `: ${r.message}` : '.'}`
}

/** settingsDeniedMessage explains a refused settings change by a platform admin who is not a member of the org. */
export const settingsDeniedMessage = 'You are not a member of this org, so you can only lower its settings here, not raise them. Ask an org owner to raise them.'
