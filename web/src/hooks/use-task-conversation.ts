// SPDX-License-Identifier: Apache-2.0

import { createConnectQueryKey, useQuery, useTransport } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useMemo } from 'react'
import { TaskService } from '@/gen/koinon/task/v1alpha1/task_pb'
import { useRunWatches } from '@/hooks/use-run-watches'
import { buildTimeline } from '@/lib/timeline'

/**
 * useTaskConversation loads a task with its events and runs and merges the live run events into
 * one timeline. The task page and the inbox's chat panel both show a conversation through it.
 */
export function useTaskConversation(orgId: string, taskId: string) {
  const transport = useTransport()
  const queryClient = useQueryClient()
  const task = useQuery(TaskService.method.getTask, { orgId, taskId })
  const taskKey = createConnectQueryKey({ schema: TaskService.method.getTask, input: { orgId, taskId }, transport, cardinality: 'finite' })
  const refresh = () => queryClient.invalidateQueries({ queryKey: taskKey })
  // Run state changes also write task events (queued, canceled, state changes), so a new snapshot re-reads the task.
  const runIds = useMemo(() => (task.data?.runs ?? []).map((r) => r.id), [task.data])
  const watches = useRunWatches(orgId, runIds, () => void refresh())
  const runEvents = useMemo(() => Object.fromEntries(Object.entries(watches).map(([id, w]) => [id, w.events])), [watches])
  const items = useMemo(() => buildTimeline(task.data?.events ?? [], runEvents), [task.data, runEvents])
  return { task, watches, items, refresh }
}
