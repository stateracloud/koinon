// SPDX-License-Identifier: Apache-2.0

import { Code } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation, useQuery } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { useMemo, useState } from 'react'
import type { Agent } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AgentService } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { TemplateScope, TemplateService } from '@/gen/koinon/template/v1alpha1/template_pb'
import { describeError } from '@/lib/api-error'

/** TemplateChoice is the "Save as template" part of both new-agent paths (ADR 0126 section 5). */
export type TemplateChoice = { save: boolean; displayName: string; description: string }

export const noTemplate: TemplateChoice = { save: false, displayName: '', description: '' }

/**
 * templateSlugOf is the slug the server makes from a template name when the request has no slug
 * (slugOf in internal/services/templates.go): lowercase, runs of other characters become one dash.
 */
export function templateSlugOf(name: string): string {
  const trim = (s: string) => s.replace(/^-+|-+$/g, '')
  const s = trim(name.toLowerCase().replace(/[^a-z0-9]+/g, '-'))
  return s.length > 64 ? trim(s.slice(0, 64)) : s
}

const platformNameTaken = 'A platform template has this name. Choose another name.'

/**
 * templateChoiceError is the problem with a choice, or undefined. platformSlugs are the slugs of the
 * platform templates: the server refuses an org template that takes one of them.
 */
export function templateChoiceError(c: TemplateChoice, platformSlugs: readonly string[] = []): string | undefined {
  if (!c.save) return undefined
  const n = c.displayName.trim().length
  if (n === 0) return 'Give the template a name.'
  if (n > 200) return 'The template name is longer than 200 characters.'
  if (platformSlugs.includes(templateSlugOf(c.displayName))) return platformNameTaken
  return undefined
}

/** usePlatformTemplateSlugs is the slugs of the platform templates (the gallery's own query, so it is cached). */
export function usePlatformTemplateSlugs(orgId: string): readonly string[] {
  const list = useQuery(TemplateService.method.listTemplates, { orgId })
  return useMemo(() => (list.data?.templates ?? []).filter((t) => t.scope !== TemplateScope.ORG).map((t) => t.slug), [list.data])
}

/** templateSaveError is the sentence for a failed CreateTemplateFromAgent. */
export function templateSaveError(e: unknown): string {
  const d = describeError(e)
  return d.code === Code.AlreadyExists ? platformNameTaken : d.message
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
        setFailed({ agentId: agent.id, message: templateSaveError(e) })
        return
      }
    }
    await navigate({ to: '/agents/$agentId', params: { agentId: agent.id } })
  }

  return { finish, saving: saveTemplate.isPending, failed }
}
