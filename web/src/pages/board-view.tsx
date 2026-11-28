// SPDX-License-Identifier: Apache-2.0

import { Board } from '@/components/board'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { useInbox } from '@/hooks/use-inbox'
import { describeError } from '@/lib/api-error'

/** BoardView is every task by state: the Board tab of the overview (ADR 0126 section 4). */
export function BoardView() {
  const { tasks, agentNames } = useInbox()
  return (
    <div className="grid gap-6">
      {tasks.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{describeError(tasks.error).message}</AlertDescription>
        </Alert>
      )}
      {tasks.isPending ? <p role="status">Loading tasks…</p> : <Board tasks={tasks.data?.tasks ?? []} agentNames={agentNames} />}
    </div>
  )
}
