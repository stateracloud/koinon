// SPDX-License-Identifier: Apache-2.0

import { AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { Status as ApprovalStatus } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { ConnectionStatus } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { RunState } from '@/gen/koinon/run/v1alpha1/run_pb'
import { Flavor, Preset } from '@/gen/koinon/template/v1alpha1/template_pb'
import { TicketStatus } from '@/gen/koinon/ticket/v1alpha1/ticket_pb'

// Every enum the UI names, in one place. The Record types make a new enum value a type error
// until it has a label.

/** The board columns, left to right (ADR 0049). */
export const boardColumns = [
  { status: TicketStatus.TODO, title: 'To do' },
  { status: TicketStatus.IN_PROGRESS, title: 'In progress' },
  { status: TicketStatus.WAITING_ON_YOU, title: 'Waiting on you' },
  { status: TicketStatus.DONE, title: 'Done' },
] as const

export function statusLabel(s: TicketStatus): string {
  return boardColumns.find((c) => c.status === s)?.title ?? 'Unknown'
}

/** ticketStatusFromName maps the database names in event payloads ("in_progress"). */
export function ticketStatusFromName(name: string | undefined): TicketStatus {
  switch (name) {
    case 'todo':
      return TicketStatus.TODO
    case 'in_progress':
      return TicketStatus.IN_PROGRESS
    case 'waiting_on_you':
      return TicketStatus.WAITING_ON_YOU
    case 'done':
      return TicketStatus.DONE
    default:
      return TicketStatus.UNSPECIFIED
  }
}

const runStateLabels: Record<RunState, string> = {
  [RunState.UNSPECIFIED]: 'unknown',
  [RunState.QUEUED]: 'queued',
  [RunState.STARTING]: 'starting',
  [RunState.RUNNING]: 'running',
  [RunState.PARKED]: 'parked',
  [RunState.SUCCEEDED]: 'succeeded',
  [RunState.FAILED]: 'failed',
  [RunState.CANCELED]: 'canceled',
}

export function runStateLabel(s: RunState): string {
  return runStateLabels[s] ?? 'unknown'
}

export function runFinished(s: RunState): boolean {
  return s === RunState.SUCCEEDED || s === RunState.FAILED || s === RunState.CANCELED
}

const flavorLabels: Record<Flavor, string> = {
  [Flavor.UNSPECIFIED]: 'Default',
  [Flavor.OPENCODE]: 'OpenCode',
  [Flavor.HERMES]: 'Hermes',
  [Flavor.PI]: 'pi',
}

export function flavorLabel(f: Flavor): string {
  return flavorLabels[f] ?? 'Unknown'
}

const presetLabels: Record<Preset, string> = {
  [Preset.UNSPECIFIED]: 'Unspecified',
  [Preset.READ_ONLY]: 'Read only',
  [Preset.STANDARD]: 'Standard (writes need your approval)',
  [Preset.TRUSTED]: 'Trusted (writes run without approval)',
}

export function presetLabel(p: Preset): string {
  return presetLabels[p] ?? 'Unknown'
}

const agentStatusLabels: Record<AgentStatus, string> = {
  [AgentStatus.UNSPECIFIED]: 'Unknown',
  [AgentStatus.DRAFT]: 'Draft',
  [AgentStatus.ACTIVE]: 'Active',
  [AgentStatus.ARCHIVED]: 'Archived',
}

export function agentStatusLabel(s: AgentStatus): string {
  return agentStatusLabels[s] ?? 'Unknown'
}

const approvalStatusLabels: Record<ApprovalStatus, string> = {
  [ApprovalStatus.UNSPECIFIED]: 'Unknown',
  [ApprovalStatus.PENDING]: 'Waiting for a decision',
  [ApprovalStatus.APPROVED]: 'Approved',
  [ApprovalStatus.DENIED]: 'Denied',
  [ApprovalStatus.EXECUTED]: 'Done',
  [ApprovalStatus.FAILED]: 'Failed',
  [ApprovalStatus.EXPIRED]: 'Expired',
  [ApprovalStatus.CANCELED]: 'Canceled',
}

export function approvalStatusLabel(s: ApprovalStatus): string {
  return approvalStatusLabels[s] ?? 'Unknown'
}

const connectionStatusLabels: Record<ConnectionStatus, string> = {
  [ConnectionStatus.UNSPECIFIED]: 'Unknown',
  [ConnectionStatus.UNTESTED]: 'Not tested',
  [ConnectionStatus.OK]: 'Working',
  [ConnectionStatus.FAILING]: 'Failing',
}

export function connectionStatusLabel(s: ConnectionStatus): string {
  return connectionStatusLabels[s] ?? 'Unknown'
}

/** The roles an API token can have; viewers and members cannot hold one. */
export function tokenRoleLabel(r: OrgRole): string {
  switch (r) {
    case OrgRole.OWNER:
      return 'Owner'
    case OrgRole.ADMIN:
      return 'Admin'
    default:
      return 'Unknown'
  }
}
