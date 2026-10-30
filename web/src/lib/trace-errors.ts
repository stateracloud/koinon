// SPDX-License-Identifier: Apache-2.0

import { fromBinary } from '@bufbuild/protobuf'
import { BinaryReader } from '@bufbuild/protobuf/wire'
import { ConnectError } from '@connectrpc/connect'
import { TestResultSchema } from '@/gen/koinon/trace/v1alpha1/trace_pb'

// Every error of TraceDestinationService carries a google.rpc.ErrorInfo (domain koinon.dev) whose reason says
// what was refused; the code alone is too coarse. The web client has no generated google.rpc types, and the
// message is three fields, so it is read here directly from the wire.

const errorInfoType = 'google.rpc.ErrorInfo'
const testResultType = 'koinon.trace.v1alpha1.TestResult'

function rawDetail(err: ConnectError, type: string): Uint8Array | undefined {
  for (const d of err.details) {
    if ('type' in d && d.type === type && d.value instanceof Uint8Array) return d.value
  }
  return undefined
}

/** errorReason is the ErrorInfo reason of an error, or undefined when it has none (a plain NOT_FOUND, say). */
export function errorReason(err: ConnectError): string | undefined {
  const bytes = rawDetail(err, errorInfoType)
  if (!bytes) return undefined
  const r = new BinaryReader(bytes)
  let reason: string | undefined
  while (r.pos < r.len) {
    const [field, wire] = r.tag()
    if (field === 1 && wire === 2) reason = r.string()
    else r.skip(wire)
  }
  return reason
}

/** testOutcome is the backend's outcome of a failed test-before-save: the message alone. */
export function testOutcome(err: ConnectError): string | undefined {
  const bytes = rawDetail(err, testResultType)
  return bytes ? fromBinary(TestResultSchema, bytes).message || undefined : undefined
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
