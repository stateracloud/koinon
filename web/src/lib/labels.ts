// SPDX-License-Identifier: Apache-2.0

import { RunState } from '@/gen/koinon/run/v1alpha1/run_pb'
import { TicketStatus } from '@/gen/koinon/ticket/v1alpha1/ticket_pb'

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
