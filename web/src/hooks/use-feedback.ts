// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useRef, useState } from 'react'
import { describeError, type ReasonCopy } from '@/lib/api-error'
import { toast } from '@/lib/toast'

// The one feedback pattern of the UI (issue #1270): success is a toast, failure is an inline error next to the
// action (<InlineError>), and the error text comes from describeError (ADR 0116 decision 5).

export type Feedback = {
  /** The inline error of the last action, or undefined. */
  error?: string
  /** Sets the inline error, for a failure that another component reports (ConfirmAction's onError). */
  setError: (message: string | undefined) => void
  /**
   * run runs an action. On success it shows `success` as a toast (when given) and returns the result. On
   * failure it sets the inline error from describeError and returns undefined; it never throws.
   */
  run: <T>(action: () => Promise<T>, success?: string) => Promise<T | undefined>
}

/** useFeedback holds the feedback of one action area: its inline error, and the toast on success. */
export function useFeedback(copy?: ReasonCopy): Feedback {
  const [error, setError] = useState<string>()
  const copyRef = useRef(copy)
  useEffect(() => {
    copyRef.current = copy
  }, [copy])
  const run = useCallback(async <T>(action: () => Promise<T>, success?: string) => {
    setError(undefined)
    try {
      const out = await action()
      if (success) toast(success)
      return out
    } catch (e) {
      setError(describeError(e, copyRef.current).message)
      return undefined
    }
  }, [])
  return { error, setError, run }
}
