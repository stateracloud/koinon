// SPDX-License-Identifier: Apache-2.0

import { Link } from '@tanstack/react-router'
import { Board } from '@/components/board'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { useInbox } from '@/hooks/use-inbox'
import { describeError } from '@/lib/api-error'

/** BoardPage is the secondary view: every task by state. The inbox is the home screen. */
export function BoardPage() {
  const { tasks, agentNames } = useInbox()
  return (
    <div className="grid gap-6">
      <div className="flex items-baseline gap-4">
        <h1 className="text-xl font-semibold">Board</h1>
        <Link to="/" className="text-muted-foreground text-sm hover:underline">
          ← Inbox
        </Link>
      </div>
      {tasks.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{describeError(tasks.error).message}</AlertDescription>
        </Alert>
      )}
      {tasks.isPending ? <p role="status">Loading tasks…</p> : <Board tasks={tasks.data?.tasks ?? []} agentNames={agentNames} />}
    </div>
  )
}
