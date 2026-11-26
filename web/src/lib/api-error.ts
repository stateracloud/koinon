// SPDX-License-Identifier: Apache-2.0

import { Code, ConnectError } from '@connectrpc/connect'
import { ErrorInfoSchema } from '@/gen/google/rpc/error_details_pb'
import { Reason } from '@/gen/koinon/refusal/v1/refusal_pb'

// The one place that reads an error of the API (ADR 0116 decision 5). A Connect error carries a
// google.rpc.ErrorInfo whose reason is a name of the enum koinon.refusal.v1.Reason. A screen shows
// describeError(e).message and does not read the raw error text itself. The lint rule koinon/no-raw-message
// (lint/koinon-plugin.js) forbids `rawMessage` outside this file.

/** ReasonName is a name of koinon.refusal.v1.Reason, the wire string of a reason. */
export type ReasonName = Exclude<keyof typeof Reason, 'REASON_UNSPECIFIED'>

/** ReasonCopy is the sentence the UI says for a reason, in place of the server's message. */
export type ReasonCopy = Partial<Record<ReasonName, string>>

export type ErrorDescription = {
  /** What to tell the person. Never empty. */
  message: string
  /** The reason, when the error carried one that this build knows. */
  reason?: ReasonName
  /** The reason string as the server sent it, even when this build does not know it. */
  rawReason?: string
  /** The Connect code of the error. A plain failure (no response) is Code.Unknown. */
  code: Code
}

/**
 * defaultCopy is the sentence for a reason that reads the same on every screen. A reason that has no
 * entry shows the server's message: it names the Role that is needed, the field and so on.
 */
export const defaultCopy: ReasonCopy = {
  ADMIN_REQUIRED: 'Only org admins and owners can do that.',
  PLATFORM_ADMIN_REQUIRED: 'Only platform admins can do that.',
}

/** isReasonName tells whether this build knows a reason string. */
export function isReasonName(s: string | undefined): s is ReasonName {
  return !!s && s !== 'REASON_UNSPECIFIED' && Object.prototype.hasOwnProperty.call(Reason, s) && typeof Reason[s as keyof typeof Reason] === 'number'
}

/** errorReason is the ErrorInfo reason string of an error, or undefined when it has none (a plain NOT_FOUND, say). */
export function errorReason(err: ConnectError): string | undefined {
  return err.findDetails(ErrorInfoSchema)[0]?.reason || undefined
}

/**
 * describeError words any error for a person. A reason that this build knows takes its copy (`copy` first,
 * then defaultCopy). Any other error, including a reason that this build does not know (reasons are add-only),
 * shows the server's message. When the server sent no message, it shows `fallback`. It never throws and never
 * returns an empty message.
 */
export function describeError(e: unknown, copy: ReasonCopy = {}, fallback = 'Something went wrong. Try again.'): ErrorDescription {
  const err = ConnectError.from(e)
  const rawReason = errorReason(err)
  const reason = isReasonName(rawReason) ? rawReason : undefined
  const sentence = reason ? (copy[reason] ?? defaultCopy[reason]) : undefined
  return {
    message: sentence ?? (err.rawMessage || fallback),
    reason,
    rawReason,
    code: err.code,
  }
}
