// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { Code, ConnectError } from '@connectrpc/connect'
import { describe, expect, it } from 'vitest'
import { ErrorInfoSchema } from '@/gen/google/rpc/error_details_pb'
import { Reason } from '@/gen/koinon/refusal/v1/refusal_pb'
import { defaultCopy, describeError, errorReason, isReasonName } from '@/lib/api-error'

// A server error: the code and message, and an ErrorInfo with the reason when one is given.
function refused(code: Code, message: string, reason?: string): ConnectError {
  const err = new ConnectError(message, code)
  if (reason) err.details.push({ desc: ErrorInfoSchema, value: create(ErrorInfoSchema, { reason, domain: 'koinon.dev' }) })
  return err
}

describe('describeError', () => {
  it('shows the server message of an error that has no reason', () => {
    const d = describeError(refused(Code.NotFound, 'not found'))
    expect(d).toMatchObject({ message: 'not found', code: Code.NotFound })
    expect(d.reason).toBeUndefined()
  })

  it('leaves out the code prefix of the Connect message', () => {
    expect(describeError(refused(Code.InvalidArgument, 'name is empty')).message).toBe('name is empty')
  })

  it('uses the default copy of a reason that reads the same everywhere', () => {
    const d = describeError(refused(Code.PermissionDenied, 'only a platform admin can do this', 'PLATFORM_ADMIN_REQUIRED'))
    expect(d.message).toBe(defaultCopy.PLATFORM_ADMIN_REQUIRED)
    expect(d.reason).toBe('PLATFORM_ADMIN_REQUIRED')
  })

  it('shows the server message of a known reason that has no copy', () => {
    const d = describeError(refused(Code.PermissionDenied, 'needs the owner role', 'ROLE_TOO_LOW'))
    expect(d).toMatchObject({ message: 'needs the owner role', reason: 'ROLE_TOO_LOW' })
  })

  it('lets the caller copy win over the default copy', () => {
    const d = describeError(refused(Code.PermissionDenied, 'x', 'ADMIN_REQUIRED'), { ADMIN_REQUIRED: 'Ask an admin.' })
    expect(d.message).toBe('Ask an admin.')
  })

  it('shows the message for a reason that this build does not know', () => {
    const d = describeError(refused(Code.FailedPrecondition, 'a new refusal', 'SOMETHING_NEW'))
    expect(d).toMatchObject({ message: 'a new refusal', rawReason: 'SOMETHING_NEW' })
    expect(d.reason).toBeUndefined()
  })

  it('does not take the unspecified value or an object key for a reason', () => {
    for (const r of ['REASON_UNSPECIFIED', '0', 'toString', '']) {
      expect(isReasonName(r)).toBe(false)
    }
    expect(describeError(refused(Code.Unknown, 'm', 'REASON_UNSPECIFIED')).reason).toBeUndefined()
  })

  it('words a plain Error and a string', () => {
    expect(describeError(new Error('Failed to fetch')).message).toBe('Failed to fetch')
    expect(describeError('boom').message).toBe('boom')
  })

  it('never returns an empty message', () => {
    expect(describeError(refused(Code.Internal, '')).message).not.toBe('')
    expect(describeError(undefined).message).not.toBe('')
  })

  it('shows the fallback of the caller when the server sent no message', () => {
    expect(describeError(refused(Code.Aborted, ''), {}, 'Already answered.').message).toBe('Already answered.')
    expect(describeError(refused(Code.Aborted, 'Ana answered first.'), {}, 'Already answered.').message).toBe('Ana answered first.')
  })

  it('reads the reason of an error', () => {
    expect(errorReason(refused(Code.Aborted, 'm', 'CHANGED'))).toBe('CHANGED')
    expect(errorReason(refused(Code.Aborted, 'm'))).toBeUndefined()
  })

  it('knows every value of the proto enum by its name', () => {
    for (const [name, n] of Object.entries(Reason)) {
      if (typeof n === 'number' && n !== 0) expect(isReasonName(name)).toBe(true)
    }
  })
})
