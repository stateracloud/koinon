// SPDX-License-Identifier: Apache-2.0

import { Health, Tier, type Allowance, type Server, type Tool } from '@/gen/koinon/mcpserver/v1alpha1/mcpserver_pb'

/** A server name is a DNS label of at most 24 characters: it is part of the gateway target name. */
export const maxServerNameLength = 24
const namePattern = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/

export function serverNameError(name: string): string | undefined {
  if (name.length === 0 || name.length > maxServerNameLength || !namePattern.test(name)) {
    return `Name must be 1 to ${maxServerNameLength} lowercase letters, digits and dashes, starting and ending with a letter or digit, such as docs.`
  }
  return undefined
}

export function serverUrlError(url: string): string | undefined {
  return /^https?:\/\/\S+$/.test(url.trim()) ? undefined : 'URL must start with http:// or https://.'
}

export const transports = [
  { value: 'streamable-http', label: 'Streamable HTTP' },
  { value: 'sse', label: 'Server-sent events (SSE)' },
] as const

export function transportLabel(t: string): string {
  return transports.find((x) => x.value === t)?.label ?? t
}

export type CredentialMode = 'none' | 'bearer' | 'custom'

export const bearerHeader = 'Authorization'
export const bearerPrefix = 'Bearer '

export type ServerFormState = {
  name: string
  url: string
  transport: string
  mode: CredentialMode
  /** The header name, for the custom mode. */
  header: string
  /** The secret; write-only, never prefilled. */
  value: string
  /** Edit only: drop the stored credential. */
  removeCredential: boolean
}

/** initialMode is the credential mode a stored credential's header and prefix correspond to. */
export function initialMode(s?: Server): CredentialMode {
  if (!s?.hasCredential) return 'none'
  return s.credentialHeader.toLowerCase() === bearerHeader.toLowerCase() && s.credentialPrefix === bearerPrefix ? 'bearer' : 'custom'
}

export function initialForm(s?: Server): ServerFormState {
  return {
    name: s?.name ?? '',
    url: s?.url ?? '',
    transport: s?.transport || 'streamable-http',
    mode: initialMode(s),
    header: s?.hasCredential && initialMode(s) === 'custom' ? s.credentialHeader : '',
    value: '',
    removeCredential: false,
  }
}

export function headerError(f: ServerFormState): string | undefined {
  if (f.mode !== 'custom') return undefined
  return /^[A-Za-z0-9!#$%&'*+.^_`|~-]+$/.test(f.header.trim()) ? undefined : 'Header name must be a valid HTTP header name such as X-Api-Key.'
}

export function valueError(f: ServerFormState, stored: boolean): string | undefined {
  if (f.mode === 'none' || f.removeCredential) return undefined
  // An edit may keep the stored secret; anything else needs one.
  return f.value === '' && !stored ? 'Type the secret value, or choose No credential.' : undefined
}

export function formErrors(f: ServerFormState, server?: Server) {
  return {
    name: server ? undefined : serverNameError(f.name),
    url: serverUrlError(f.url),
    header: headerError(f),
    value: valueError(f, !!server?.hasCredential && initialMode(server) === f.mode),
  }
}

export type CreateServerInput = {
  name: string
  url: string
  transport: string
  credentialHeader: string
  credentialPrefix: string
  credentialValue: string
}

function credentialParts(f: ServerFormState): { header: string; prefix: string } {
  return f.mode === 'bearer' ? { header: bearerHeader, prefix: bearerPrefix } : { header: f.header.trim(), prefix: '' }
}

/** buildCreateRequest turns the form into the CreateServer fields; no credential leaves them all empty. */
export function buildCreateRequest(f: ServerFormState): CreateServerInput {
  const withSecret = f.mode !== 'none' && f.value !== ''
  const c = credentialParts(f)
  return {
    name: f.name,
    url: f.url.trim(),
    transport: f.transport,
    credentialHeader: withSecret ? c.header : '',
    credentialPrefix: withSecret ? c.prefix : '',
    credentialValue: withSecret ? f.value : '',
  }
}

export type UpdateServerInput = {
  url?: string
  transport?: string
  credentialHeader?: string
  credentialPrefix?: string
  credentialValue?: string
  removeCredential?: boolean
}

/** buildUpdateRequest sends only what changed; a stored secret stays unless a new one is typed or removal is asked for. */
export function buildUpdateRequest(s: Server, f: ServerFormState): UpdateServerInput {
  const out: UpdateServerInput = {}
  if (f.url.trim() !== s.url) out.url = f.url.trim()
  if (f.transport !== s.transport) out.transport = f.transport
  if (f.removeCredential || f.mode === 'none') {
    if (s.hasCredential) out.removeCredential = true
    return out
  }
  const c = credentialParts(f)
  if (f.value !== '') {
    out.credentialHeader = c.header
    out.credentialPrefix = c.prefix
    out.credentialValue = f.value
  } else if (s.hasCredential && (c.header.toLowerCase() !== s.credentialHeader.toLowerCase() || c.prefix !== s.credentialPrefix)) {
    out.credentialHeader = c.header
    out.credentialPrefix = c.prefix
  }
  return out
}

/** serverErrorField says which form field a server's refusal is about, so the message sits next to it. */
export function serverErrorField(message: string): 'name' | 'url' | 'credential' | undefined {
  if (/oauth|\burls?\b|address|loopback|private|scheme|https?:|\bhost\b|resolve/i.test(message)) return 'url'
  if (/\bname\b/i.test(message)) return 'name'
  if (/credential|header|secret|token/i.test(message)) return 'credential'
  return undefined
}

export function serverEditable(s: Server): boolean {
  return s.source !== 'secret'
}

/** True when the server can be deleted here: entered here, or declared with its Secret gone. */
export function serverDeletable(s: Server): boolean {
  return s.source !== 'secret' || s.removed
}

export function sourceLabel(s: Server): string {
  return s.source === 'secret' ? `Managed by Secret ${s.managedBySecret}` : 'UI'
}

export const tiers = [
  { value: Tier.ASK, label: 'Ask', help: 'A human approves each call.' },
  { value: Tier.AUTO, label: 'Auto', help: 'The agent calls the tool directly.' },
  { value: Tier.NEVER, label: 'Never', help: 'No agent may call the tool.' },
] as const

export function tierLabel(t: Tier): string {
  return tiers.find((x) => x.value === t)?.label ?? 'Ask'
}

export const tierExplanation = 'Ask means a human approves each call; Auto means the agent calls the tool directly.'

export function healthLabel(h: Health): string {
  switch (h) {
    case Health.HEALTHY:
      return 'Healthy'
    case Health.UNHEALTHY:
      return 'Unhealthy'
    default:
      return 'Unknown'
  }
}

export function toolsOf(tools: readonly Tool[], serverId: string): Tool[] {
  return tools.filter((t) => t.serverId === serverId)
}

/** True when the tool is read-only by the server's own say-so but not yet Auto: a suggestion only. */
export function suggestsAuto(t: Tool): boolean {
  return t.present && t.readOnlyHint && t.tier !== Tier.AUTO
}

export type ToolSummary = { total: number; review: number; suggested: number; gone: number }

/** summarizeTools counts what needs the admin's eyes: reviews and read-only suggestions. */
export function summarizeTools(tools: readonly Tool[]): ToolSummary {
  return {
    total: tools.filter((t) => t.present).length,
    review: tools.filter((t) => t.reviewReason !== '').length,
    suggested: tools.filter(suggestsAuto).length,
    gone: tools.filter((t) => !t.present).length,
  }
}

export function summaryText(s: ToolSummary): string {
  const parts = [`${s.total} ${s.total === 1 ? 'tool' : 'tools'}`]
  if (s.review) parts.push(`${s.review} to review`)
  if (s.suggested) parts.push(`${s.suggested} suggested read-only`)
  return parts.join(', ')
}

/** AllowState is one server's checkbox state in the agent's allowlist editor. */
export type AllowState = { allowed: boolean; limited: boolean; tools: readonly string[] }

export function allowStateFrom(allowances: readonly Allowance[]): Record<string, AllowState> {
  const out: Record<string, AllowState> = {}
  for (const a of allowances) out[a.serverId] = { allowed: true, limited: a.tools.length > 0, tools: [...a.tools] }
  return out
}

/** allowlistProblem names a server whose limit is on but picks no tools (that would allow the whole server). */
export function allowlistProblem(servers: readonly Server[], state: Record<string, AllowState>): string | undefined {
  for (const s of servers) {
    const st = state[s.id]
    if (st?.allowed && st.limited && st.tools.length === 0) return `Pick at least one tool of ${s.name}, or turn off the limit to allow the whole server.`
  }
  return undefined
}

/** buildAllowlist is the SetAgentServers list: allowed servers only; an empty tools list means the whole server. */
export function buildAllowlist(servers: readonly Server[], state: Record<string, AllowState>): { serverId: string; tools: string[] }[] {
  return servers.flatMap((s) => {
    const st = state[s.id]
    if (!st?.allowed) return []
    return [{ serverId: s.id, tools: st.limited ? [...st.tools].sort() : [] }]
  })
}
