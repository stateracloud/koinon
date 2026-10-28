// SPDX-License-Identifier: Apache-2.0

import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'

// The approval card imports it from here; the label itself lives with the others.
export { approvalStatusLabel } from '@/lib/labels'

/** targetRepo strips the connector prefix: "github:acme/widgets" → "acme/widgets". */
export function targetRepo(target: string): string {
  return target.replace(/^[a-z0-9_-]+:/, '')
}

/** isApprover: org admins and owners decide approvals and activate agents (ADR 0020, 0049). */
export function isApprover(role: OrgRole): boolean {
  return role >= OrgRole.ADMIN
}

/** toolVersion is the short form of a tool's content hash shown on an MCP approval. */
export function toolVersion(contentHash: string): string {
  return contentHash.slice(0, 8)
}

/** toolChanged is true when the tool's description or schema differs from what the request was made against. */
export function toolChanged(contentHash: string, currentContentHash: string): boolean {
  return currentContentHash !== '' && currentContentHash !== contentHash
}

/** approveLabel names the Approve button: the first or second of two approvals, else plain Approve. */
export function approveLabel(approvalsRequired: number, firstApprover: string): string {
  if (approvalsRequired !== 2) return 'Approve'
  return firstApprover ? 'Give second approval' : 'Give first approval'
}

/** What an approver chooses when approving: this call only, this task, or this agent until a date (ADR 0042 amendment). */
export type ScopeChoice = { kind: 'once' } | { kind: 'task' } | { kind: 'agent'; until: Date }

/** scopeLabel describes a scope kind for the agent page and the timeline. */
export function scopeKindLabel(kind: 'task' | 'agent'): string {
  return kind === 'task' ? 'this task' : 'this agent'
}

/** defaultScopeEnd is the date the agent scope offers first: two weeks ahead, as yyyy-mm-dd in local time. */
export function defaultScopeEnd(now: Date = new Date()): string {
  const d = new Date(now.getTime() + 14 * 24 * 3600 * 1000)
  return localDate(d)
}

/** localDate formats a date as yyyy-mm-dd in the viewer's time zone. */
export function localDate(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}

/** endOfLocalDay turns a yyyy-mm-dd value into the end of that day in the viewer's time zone; undefined if it is not a date. */
export function endOfLocalDay(value: string): Date | undefined {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value)
  if (!m) return undefined
  const d = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]), 23, 59, 59)
  return Number.isNaN(d.getTime()) ? undefined : d
}
