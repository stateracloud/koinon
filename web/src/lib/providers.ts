// SPDX-License-Identifier: Apache-2.0

import { timestampDate } from '@bufbuild/protobuf/wkt'
import type { Timestamp } from '@bufbuild/protobuf/wkt'
import type { Model, Provider } from '@/gen/koinon/provider/v1alpha1/provider_pb'
import type { Token } from '@/gen/koinon/token/v1alpha1/token_pb'

/** The provider type that has a routing guard (ADR 0048, 0056). */
export const guardProviderType = 'openrouter'

/** Provider types the form offers; any other agentgateway provider name can be typed in. */
export const providerTypes = [
  { value: 'openrouter', label: 'OpenRouter' },
  { value: 'ollama', label: 'Ollama' },
  { value: 'openai', label: 'OpenAI or any OpenAI-compatible API' },
] as const

export function providerTypeLabel(type: string): string {
  return providerTypes.find((t) => t.value === type)?.label ?? type
}

/** scopedModelName is the name the gateway serves a model under: "<org slug>/<model>" (ADR 0056). */
export function scopedModelName(orgSlug: string, name: string): string {
  return `${orgSlug}/${name}`
}

/** True when a provider's models and key can be changed here. */
export function providerEditable(p: Provider): boolean {
  return p.source !== 'secret'
}

/** True when the provider can be deleted here: entered here, or declared with its Secret gone. */
export function providerDeletable(p: Provider): boolean {
  return p.source !== 'secret' || p.removed
}

export function modelsOf(models: readonly Model[], providerId: string): Model[] {
  return models.filter((m) => m.providerId === providerId)
}

/** How long a new or changed provider takes to reach the gateway, measured on kind (about 170 s). */
export const availabilityMinutes = 3
export const savedNotice = `Saved. It will be available to agents within about ${availabilityMinutes} minutes.`

/** savedRecently is true while a provider or model saved at ts may not be served yet. */
export function savedRecently(ts: Timestamp | undefined, now = Date.now()): boolean {
  return !!ts && now - timestampDate(ts).getTime() < availabilityMinutes * 60_000
}

export const defaultTokenDays = 30
export const maxTokenDays = 90

export function daysToSeconds(days: number): bigint {
  return BigInt(Math.round(days * 24 * 60 * 60))
}

/** tokenDaysError validates the expiry the form asks for: whole days, 1 to 90. */
export function tokenDaysError(value: string): string | undefined {
  const n = Number(value)
  if (!value.trim() || !Number.isInteger(n) || n < 1 || n > maxTokenDays) return `Expiry must be a whole number of days from 1 to ${maxTokenDays}.`
  return undefined
}

export type TokenStatus = 'active' | 'expired' | 'revoked'

export function tokenStatus(t: Token, now = new Date()): TokenStatus {
  if (t.revokedAt) return 'revoked'
  if (t.expiresAt && timestampDate(t.expiresAt) <= now) return 'expired'
  return 'active'
}

export function formatTime(ts: Timestamp | undefined, none = 'Never'): string {
  return ts ? timestampDate(ts).toLocaleString() : none
}

/** createdByLabel names who made a token and, for a token made by a token, which one. */
export function createdByLabel(t: Token, all: readonly Token[]): string {
  const who = t.createdByName || 'unknown user'
  if (!t.parentTokenId) return `${who}, in the UI`
  const parent = all.find((p) => p.id === t.parentTokenId)
  return `${who}, through token ${parent ? `${parent.name} (${parent.prefix})` : 'that no longer exists'}`
}
