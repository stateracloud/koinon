// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { Code, ConnectError } from '@connectrpc/connect'
import { describe, expect, it } from 'vitest'
import { ErrorInfoSchema } from '@/gen/google/rpc/error_details_pb'
import { TestResultSchema } from '@/gen/koinon/trace/v1alpha1/trace_pb'
import { changedMessage, describeTraceError, settingsDeniedMessage } from '@/lib/trace-errors'

function refused(code: Code, message: string, reason: string, extra?: ConnectError['details'][number]): ConnectError {
  const err = new ConnectError(message, code)
  err.details.push({ desc: ErrorInfoSchema, value: create(ErrorInfoSchema, { reason, domain: 'koinon.dev' }) })
  if (extra) err.details.push(extra)
  return err
}

describe('describeTraceError', () => {
  it('asks for a reload when the destination changed', () => {
    expect(describeTraceError(refused(Code.Aborted, 'x', 'CHANGED'))).toEqual({ message: changedMessage, changed: true, reason: 'CHANGED' })
  })

  it('words the member refusal', () => {
    expect(describeTraceError(refused(Code.PermissionDenied, 'x', 'NOT_MEMBER')).message).toBe(settingsDeniedMessage)
  })

  it('says the outcome of a failed test', () => {
    const outcome = { desc: TestResultSchema, value: create(TestResultSchema, { message: 'backend said 401 unauthorized' }) }
    expect(describeTraceError(refused(Code.FailedPrecondition, 'x', 'TEST_FAILED', outcome)).message).toBe('Not saved. Backend said 401 unauthorized.')
  })

  it('falls back to the server message when the failed test has no outcome', () => {
    expect(describeTraceError(refused(Code.FailedPrecondition, 'the backend refused', 'TEST_FAILED')).message).toBe('Not saved. the backend refused')
  })

  it('shows the server message of an error with no reason', () => {
    const d = describeTraceError(new ConnectError('boom', Code.Internal))
    expect(d).toMatchObject({ message: 'boom', changed: false })
  })
})
