// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useRef } from 'react'
import { TaskService, type SendDirectMessageResponse } from '@/gen/koinon/task/v1alpha1/task_pb'

export type DirectMessageInput = { agentId: string; text: string; taskId?: string }

/**
 * useDirectMessage sends a direct message to an agent: a new thread, or a reply when taskId is
 * set (TR3). The message ID is chosen here and kept until the send succeeds, so sending the same
 * text again after a failure (a lost response) is the same message and has no second effect.
 */
export function useDirectMessage(orgId: string, onSent?: (res: SendDirectMessageResponse) => void) {
  const queryClient = useQueryClient()
  const pendingId = useRef<{ key: string; id: string }>(undefined)
  const mutation = useMutation(TaskService.method.sendDirectMessage, {
    onSuccess: async (res) => {
      await queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: TaskService, cardinality: undefined }) })
      onSent?.(res)
    },
  })
  async function send({ agentId, text, taskId = '' }: DirectMessageInput) {
    const key = JSON.stringify([agentId, taskId, text])
    if (pendingId.current?.key !== key) pendingId.current = { key, id: crypto.randomUUID() }
    const res = await mutation.mutateAsync({ orgId, agentId, taskId, text, messageId: pendingId.current.id })
    pendingId.current = undefined
    return res
  }
  return { send, pending: mutation.isPending, error: mutation.error ? ConnectError.from(mutation.error).rawMessage : undefined }
}
