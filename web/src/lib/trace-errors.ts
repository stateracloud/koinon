// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { TestResultSchema } from '@/gen/koinon/trace/v1alpha1/trace_pb'
import { describeError, type ReasonCopy, type ReasonName } from '@/lib/api-error'

// Every error of TraceDestinationService carries a google.rpc.ErrorInfo (domain koinon.dev) whose reason says
// what was refused; the code alone is too coarse (see api-error.ts). This file words the trace reasons and reads
// the outcome of a failed test.

/** testOutcome is the backend's outcome of a failed test-before-save: the message alone. */
export function testOutcome(err: ConnectError): string | undefined {
  return err.findDetails(TestResultSchema)[0]?.message || undefined
}

export const settingsDeniedMessage = 'You are not a member of this org, so you can only lower its settings here, not raise them. Ask an org owner to raise them.'
export const changedMessage = 'This destination was changed by someone else while you had it open. It has been reloaded; check it and make your change again.'

/** sentence capitalises the first letter and ends with exactly one full stop. */
function sentence(text: string): string {
  const t = text.trim().replace(/[.\s]+$/, '')
  return `${t.charAt(0).toUpperCase()}${t.slice(1)}.`
}

export type TraceRefusal = {
  /** What to tell the person. */
  message: string
  /** The destination changed since it was loaded: close the form and reload. */
  changed: boolean
  /** The reason, when the error had one that this build knows. */
  reason?: ReasonName
}

const traceCopy: ReasonCopy = {
  CHANGED: changedMessage,
  ADMIN_REQUIRED: 'Only org admins and owners can change observability.',
  OWNER_ONLY: 'Only an org owner can choose full content.',
  OWNER_AT_FULL: 'Your org sends full content, so only an org owner can add a destination or change its connection. You can still rename a destination or turn it on or off.',
  NOT_MEMBER: settingsDeniedMessage,
  ABOVE_CEILING: 'That level is above the platform ceiling. Pick a lower one.',
}

/** describeTraceError words a refusal from its reason, falling back to the server's message. */
export function describeTraceError(e: unknown): TraceRefusal {
  const d = describeError(e, traceCopy)
  if (d.reason === 'TEST_FAILED') {
    const outcome = testOutcome(ConnectError.from(e))
    return { message: `Not saved. ${outcome ? sentence(outcome) : d.message}`, changed: false, reason: d.reason }
  }
  return { message: d.message, changed: d.reason === 'CHANGED', reason: d.reason }
}
