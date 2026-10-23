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
