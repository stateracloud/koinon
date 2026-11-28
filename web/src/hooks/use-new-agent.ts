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

/**
 * TakenSlugs is what the server refuses as the slug of a new org template: a platform template's
 * slug, or one the org already has (CreateTemplateFromAgent). Until the list is loaded nothing can
 * be checked, so a choice to save is refused then too: a refused save comes after the draft exists.
 */
export type TakenSlugs = { state: 'loading' | 'failed' | 'ready'; platform: readonly string[]; org: readonly string[] }

const nameTaken = 'A template with this name exists. Choose another name.'

/** templateChoiceError is the problem with a choice, or undefined. */
export function templateChoiceError(c: TemplateChoice, taken: TakenSlugs): string | undefined {
  if (!c.save) return undefined
  const n = c.displayName.trim().length
  if (n === 0) return 'Give the template a name.'
  if (n > 200) return 'The template name is longer than 200 characters.'
  if (taken.state === 'loading') return 'Koinon is still loading the template names. Try again in a moment.'
  if (taken.state === 'failed') return 'Koinon could not load the template names, so it cannot check this name. Turn off “Save as template”, or try again later.'
  const slug = templateSlugOf(c.displayName)
  if (taken.platform.includes(slug)) return 'A platform template has this name. Choose another name.'
  if (taken.org.includes(slug)) return 'This org has a template with this name. Choose another name.'
  return undefined
}

/** useTakenTemplateSlugs is the slugs a new org template cannot take (the gallery's own query, so it is cached). */
export function useTakenTemplateSlugs(orgId: string): TakenSlugs {
  const list = useQuery(TemplateService.method.listTemplates, { orgId })
  return useMemo(() => {
    const all = list.data?.templates ?? []
    return {
      state: list.isError ? 'failed' : list.data ? 'ready' : 'loading',
      platform: all.filter((t) => t.scope !== TemplateScope.ORG).map((t) => t.slug),
      org: all.filter((t) => t.scope === TemplateScope.ORG).map((t) => t.slug),
    }
  }, [list.data, list.isError])
}

/** isTakenName says whether a name makes a slug that a template already has. */
export function isTakenName(name: string, taken: TakenSlugs): boolean {
  const slug = templateSlugOf(name)
  return taken.platform.includes(slug) || taken.org.includes(slug)
}

/** templateSaveError is the sentence for a failed CreateTemplateFromAgent. */
export function templateSaveError(e: unknown): string {
  const d = describeError(e)
  return d.code === Code.AlreadyExists ? nameTaken : d.message
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
