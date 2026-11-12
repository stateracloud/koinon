// SPDX-License-Identifier: Apache-2.0

import { AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { Status as ApprovalStatus } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { ConnectionStatus } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { RunState } from '@/gen/koinon/run/v1alpha1/run_pb'
import { Preset } from '@/gen/koinon/template/v1alpha1/template_pb'
import { TaskState } from '@/gen/koinon/task/v1alpha1/task_pb'

// Every enum the UI names, in one place. The Record types make a new enum value a type error
// until it has a label.

/** The board's groups, left to right: the task states of ADR 0065. */
export const boardStates = [
  { state: TaskState.SUBMITTED, title: 'Submitted' },
  { state: TaskState.WORKING, title: 'Working' },
  { state: TaskState.INPUT_REQUIRED, title: 'Input required' },
  { state: TaskState.COMPLETED, title: 'Completed' },
  { state: TaskState.CANCELED, title: 'Canceled' },
  { state: TaskState.FAILED, title: 'Failed' },
] as const

export function stateLabel(s: TaskState): string {
  return boardStates.find((c) => c.state === s)?.title ?? 'Unknown'
}

/** isEnded: completed, canceled and failed tasks take no more work. */
export function isEnded(s: TaskState): boolean {
  return s === TaskState.COMPLETED || s === TaskState.CANCELED || s === TaskState.FAILED
}

/**
 * taskStateFromName maps the database names in event payloads ("input_required"). Events written
 * before the move from tickets name the old board columns, which map as the migration did.
 */
export function taskStateFromName(name: string | undefined): TaskState {
  switch (name) {
    case 'submitted':
    case 'todo':
      return TaskState.SUBMITTED
    case 'working':
    case 'in_progress':
      return TaskState.WORKING
    case 'input_required':
    case 'waiting_on_you':
      return TaskState.INPUT_REQUIRED
    case 'completed':
    case 'done':
      return TaskState.COMPLETED
    case 'canceled':
      return TaskState.CANCELED
    case 'failed':
      return TaskState.FAILED
    default:
      return TaskState.UNSPECIFIED
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

// Display names of the built-in harnesses (ADR 0112). A harness an install registers shows under
// its own name.
const harnessLabels: Record<string, string> = {
  opencode: 'OpenCode',
}

export function harnessLabel(name: string): string {
  if (name === '') return 'Default'
  return harnessLabels[name] ?? name
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
