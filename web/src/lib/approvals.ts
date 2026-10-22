// SPDX-License-Identifier: Apache-2.0

import { Status } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'

const statusLabels: Record<Status, string> = {
  [Status.UNSPECIFIED]: 'Unknown',
  [Status.PENDING]: 'Waiting for a decision',
  [Status.APPROVED]: 'Approved',
  [Status.DENIED]: 'Denied',
  [Status.EXECUTED]: 'Done',
  [Status.FAILED]: 'Failed',
  [Status.EXPIRED]: 'Expired',
  [Status.CANCELED]: 'Canceled',
}

export function approvalStatusLabel(s: Status): string {
  return statusLabels[s] ?? 'Unknown'
}

/** targetRepo strips the connector prefix: "github:acme/widgets" → "acme/widgets". */
export function targetRepo(target: string): string {
  return target.replace(/^[a-z0-9_-]+:/, '')
}

/** isApprover: org admins and owners decide approvals and activate agents (ADR 0020, 0049). */
export function isApprover(role: OrgRole): boolean {
  return role >= OrgRole.ADMIN
}
