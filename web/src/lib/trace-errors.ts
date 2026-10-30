// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { ErrorInfoSchema } from '@/gen/google/rpc/error_details_pb'
import { TestResultSchema } from '@/gen/koinon/trace/v1alpha1/trace_pb'

// Every error of TraceDestinationService carries a google.rpc.ErrorInfo (domain koinon.dev) whose reason says
// what was refused; the code alone is too coarse. A failed test also carries the TestResult with the outcome.

/** errorReason is the ErrorInfo reason of an error, or undefined when it has none (a plain NOT_FOUND, say). */
export function errorReason(err: ConnectError): string | undefined {
  return err.findDetails(ErrorInfoSchema)[0]?.reason || undefined
}

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
  /** The reason, when the error had one. */
  reason?: string
}

/** describeTraceError words a refusal from its reason, falling back to the server's message. */
export function describeTraceError(e: unknown): TraceRefusal {
  const err = ConnectError.from(e)
  const reason = errorReason(err)
  const say = (message: string, changed = false): TraceRefusal => ({ message, changed, reason })
  switch (reason) {
    case 'CHANGED':
      return say(changedMessage, true)
    case 'ADMIN_REQUIRED':
      return say('Only org admins and owners can change observability.')
    case 'PLATFORM_ADMIN_REQUIRED':
      return say('Only platform admins can do that.')
    case 'OWNER_ONLY':
      return say('Only an org owner can choose full content.')
    case 'OWNER_AT_FULL':
      return say('Your org sends full content, so only an org owner can add a destination or change its connection. You can still rename a destination or turn it on or off.')
    case 'NOT_MEMBER':
      return say(settingsDeniedMessage)
    case 'ABOVE_CEILING':
      return say('That level is above the platform ceiling. Pick a lower one.')
    case 'TEST_FAILED': {
      const outcome = testOutcome(err)
      return say(outcome ? `Not saved. ${sentence(outcome)}` : `Not saved. ${err.rawMessage}`)
    }
    default:
      return say(err.rawMessage)
  }
}
