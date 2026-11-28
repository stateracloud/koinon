// SPDX-License-Identifier: Apache-2.0

import { Alert, AlertDescription } from "@/components/ui/alert"
import { describeError, type ReasonCopy } from "@/lib/api-error"
import { cn } from "@/lib/utils"

// The one feedback pattern of the UI (issue #1270): success is a toast, failure is an inline error next to the
// action, and the error text comes from describeError (ADR 0116 decision 5). A screen does not build its own
// error or success state.

/** InlineError shows a failure next to the action that failed. It renders nothing without a message. */
export function InlineError({ message, id, className }: { message?: string; id?: string; className?: string }) {
  if (!message) return null
  return (
    <Alert variant="destructive" role="alert" id={id} data-slot="inline-error" className={className}>
      <AlertDescription className="text-destructive">{message}</AlertDescription>
    </Alert>
  )
}

/** QueryError is the inline error of a failed read. */
export function QueryError({ error, copy, className }: { error: unknown; copy?: ReasonCopy; className?: string }) {
  if (!error) return null
  return <InlineError message={describeError(error, copy).message} className={cn(className)} />
}
