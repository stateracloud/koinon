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
