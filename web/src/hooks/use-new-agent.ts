// SPDX-License-Identifier: Apache-2.0

import { createConnectQueryKey, useMutation } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import type { Agent } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AgentService } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { TemplateService } from '@/gen/koinon/template/v1alpha1/template_pb'
import { describeError } from '@/lib/api-error'

/** TemplateChoice is the "Save as template" part of both new-agent paths (ADR 0126 section 5). */
export type TemplateChoice = { save: boolean; displayName: string; description: string }

export const noTemplate: TemplateChoice = { save: false, displayName: '', description: '' }

/** templateChoiceError is the problem with a choice, or undefined. */
export function templateChoiceError(c: TemplateChoice): string | undefined {
  if (!c.save) return undefined
  const n = c.displayName.trim().length
  if (n === 0) return 'Give the template a name.'
  if (n > 200) return 'The template name is longer than 200 characters.'
  return undefined
}

/**
 * useFinishDraft is the step after a draft was created on either path: it saves the draft as an
 * org template when the user asked for it (CreateTemplateFromAgent reads the agent's latest
 * version), then opens the draft. When the template cannot be saved, the draft stays and the
 * result says so; the draft page has its own "Save as template".
 */
export function useFinishDraft(orgId: string) {
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const saveTemplate = useMutation(TemplateService.method.createTemplateFromAgent)
  const [failed, setFailed] = useState<{ agentId: string; message: string }>()

  async function finish(agent: Agent | undefined, choice: TemplateChoice) {
    await queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AgentService, cardinality: undefined }) })
    if (!agent) return
    if (choice.save) {
      try {
        await saveTemplate.mutateAsync({ orgId, agentId: agent.id, displayName: choice.displayName.trim(), description: choice.description.trim() })
        await queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: TemplateService, cardinality: undefined }) })
      } catch (e) {
        setFailed({ agentId: agent.id, message: describeError(e).message })
        return
      }
    }
    await navigate({ to: '/agents/$agentId', params: { agentId: agent.id } })
  }

  return { finish, saving: saveTemplate.isPending, failed }
}
