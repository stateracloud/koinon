// SPDX-License-Identifier: Apache-2.0

import { createConnectQueryKey, useMutation } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { useId, useState } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import type { Agent } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { TemplateService } from '@/gen/koinon/template/v1alpha1/template_pb'
import { isTakenName, templateChoiceError, templateSaveError, useTakenTemplateSlugs, type TakenSlugs, type TemplateChoice } from '@/hooks/use-new-agent'

/**
 * SaveAsTemplateFields is "Save as template" on both new-agent paths (ADR 0126 section 5). When it
 * is on, the draft is also saved as an org template, which then shows in the gallery of this org.
 */
export function SaveAsTemplateFields({
  value,
  onChange,
  showErrors,
  defaultName = '',
  taken,
}: {
  value: TemplateChoice
  onChange: (v: TemplateChoice) => void
  showErrors?: boolean
  /** The template name to start from when the box is turned on with no name: the agent's name. */
  defaultName?: string
  /** The slugs a new org template cannot take; a name that makes one of them is refused. */
  taken: TakenSlugs
}) {
  const id = useId()
  const problem = showErrors ? templateChoiceError(value, taken) : undefined
  // The agent name of a template's draft is that template's name, which the server refuses.
  const startName = isTakenName(defaultName, taken) ? '' : defaultName.trim()
  return (
    <fieldset className="grid gap-2 rounded-lg border p-4" aria-label="Save as template">
      <div className="flex items-start gap-2">
        <input
          id={`${id}-save`}
          type="checkbox"
          className="mt-1"
          checked={value.save}
          onChange={(e) => onChange({ ...value, save: e.target.checked, displayName: e.target.checked && value.displayName === '' ? startName : value.displayName })}
        />
        <div>
          <Label htmlFor={`${id}-save`}>Save as template</Label>
          <p className="text-muted-foreground text-xs">
            Also save this definition as a template of this org. It keeps the instructions, model, grants, limits and triggers, but no connection or repository.
          </p>
        </div>
      </div>
      {value.save && (
        <>
          <Label htmlFor={`${id}-name`}>Template name</Label>
          <Input id={`${id}-name`} value={value.displayName} aria-invalid={!!problem} onChange={(e) => onChange({ ...value, displayName: e.target.value })} />
          <Label htmlFor={`${id}-description`}>Template description (optional)</Label>
          <Textarea id={`${id}-description`} value={value.description} onChange={(e) => onChange({ ...value, description: e.target.value })} />
          {problem && (
            <p className="text-destructive text-sm" role="alert">
              {problem}
            </p>
          )}
        </>
      )}
    </fieldset>
  )
}

/**
 * AgentSaveAsTemplate is "Save as template" on an agent's page: it saves the agent's latest version
 * as an org template (CreateTemplateFromAgent). Members and above, as creating an agent.
 */
export function AgentSaveAsTemplate({ agent }: { agent: Agent }) {
  const queryClient = useQueryClient()
  const [choice, setChoice] = useState<TemplateChoice | undefined>()
  const [touched, setTouched] = useState(false)
  const taken = useTakenTemplateSlugs(agent.orgId)
  const save = useMutation(TemplateService.method.createTemplateFromAgent, {
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: TemplateService, cardinality: undefined }) })
      setChoice(undefined)
    },
  })
  return (
    <div className="grid gap-2 text-sm">
      {choice ? (
        <form
          noValidate
          className="grid gap-2"
          onSubmit={(e) => {
            e.preventDefault()
            setTouched(true)
            if (templateChoiceError(choice, taken)) return
            save.mutate({ orgId: agent.orgId, agentId: agent.id, displayName: choice.displayName.trim(), description: choice.description.trim() })
          }}
        >
          <SaveAsTemplateFields value={choice} onChange={(v) => setChoice(v.save ? v : undefined)} showErrors={touched} taken={taken} />
          {save.error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>Could not save the template: {templateSaveError(save.error)}</AlertDescription>
            </Alert>
          )}
          <div>
            <Button type="submit" size="sm" disabled={save.isPending}>
              Save template
            </Button>
          </div>
        </form>
      ) : (
        <div className="flex flex-wrap items-center gap-2">
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              save.reset()
              setTouched(false)
              setChoice({ save: true, displayName: isTakenName(agent.displayName, taken) ? '' : agent.displayName, description: '' })
            }}
          >
            Save as template
          </Button>
          {save.isSuccess && save.data.template && (
            <p role="status">
              Saved the template “{save.data.template.displayName}”. It shows in the{' '}
              <Link to="/agents/new" className="underline">
                new-agent gallery
              </Link>
              .
            </p>
          )}
        </div>
      )}
    </div>
  )
}

/** TemplateNotSaved says that the draft exists but its template was not saved, and links to the draft. */
export function TemplateNotSaved({ agentId, message }: { agentId: string; message: string }) {
  return (
    <Alert variant="destructive" role="alert">
      <AlertDescription>
        The draft was created, but the template was not saved: {message}{' '}
        <Link to="/agents/$agentId" params={{ agentId }} className="underline">
          Open the draft
        </Link>{' '}
        and use “Save as template” there.
      </AlertDescription>
    </Alert>
  )
}
