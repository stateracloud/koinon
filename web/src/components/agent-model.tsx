// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation, useQuery } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import { AgentService, AgentStatus, ModelSource, type Agent, type GetAgentModelResponse } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { alwaysThinksText, defaultModelChoice, modelChoices, modelSourceLabel, offOverriddenText, sentence, thinkingSourceLabel } from '@/lib/agent-model'

/**
 * AgentModel is where an admin or owner picks the model an agent runs on, and sees which model the
 * next run uses and why. The pick is a setting of the agent, not a new version: it applies to the
 * next run. Precedence: this setting, then the version's model, then the org default.
 */
export function AgentModel({ agent }: { agent: Agent }) {
  const queryClient = useQueryClient()
  const model = useQuery(AgentService.method.getAgentModel, { orgId: agent.orgId, agentId: agent.id })
  const [picked, setPicked] = useState<string>()
  const save = useMutation(AgentService.method.setAgentModel, {
    onSuccess: async () => {
      setPicked(undefined)
      await queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AgentService, cardinality: undefined }) })
    },
  })
  const m = model.data
  if (model.error) {
    return (
      <Alert variant="destructive" role="alert">
        <AlertDescription>Could not load the model: {ConnectError.from(model.error).rawMessage}</AlertDescription>
      </Alert>
    )
  }
  if (!m) return <p role="status">Loading the model…</p>
  const current = m.setting === '' ? defaultModelChoice : m.setting
  const value = picked ?? current
  const archived = agent.status === AgentStatus.ARCHIVED
  return (
    <section aria-labelledby="agent-model" className="bg-card grid gap-3 rounded-lg border p-4 text-sm">
      <h2 id="agent-model" className="font-semibold">
        Model
      </h2>
      <p>
        Next run: <code>{m.effective || 'no model'}</code>, {modelSourceLabel(m.source)}.
      </p>
      {m.problem && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>
            {sentence(m.problem)}
            {m.source === ModelSource.AGENT && ' Runs fail until you pick another model.'}
          </AlertDescription>
        </Alert>
      )}
      <div className="grid gap-1.5">
        <Label htmlFor="agent-model-pick">Model for this agent</Label>
        <NativeSelect id="agent-model-pick" value={value} onChange={(e) => setPicked(e.target.value)} disabled={archived} aria-describedby="agent-model-help">
          <option value={defaultModelChoice}>
            Default{m.versionModel ? ` (the version's model, ${m.versionModel})` : ' (the org default)'}
          </option>
          {modelChoices(m).map((c) => (
            <option key={c.name} value={c.name}>
              {c.label}
            </option>
          ))}
        </NativeSelect>
        <p id="agent-model-help" className="text-muted-foreground text-xs">
          Changing it applies to the next run and is recorded in the audit log. It is not a new version of the agent. If the model stops being served or priced, the agent&apos;s runs fail with that reason instead of using another model.
        </p>
      </div>
      {save.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>Could not save: {ConnectError.from(save.error).rawMessage}</AlertDescription>
        </Alert>
      )}
      <div>
        <Button
          size="sm"
          disabled={save.isPending || archived || value === current}
          onClick={() => save.mutate({ orgId: agent.orgId, agentId: agent.id, model: value })}
        >
          Save model
        </Button>
      </div>
      <AgentThinking agent={agent} model={m} archived={archived} />
    </section>
  )
}

/**
 * AgentThinking turns the agent's thinking on or off. Thinking is on unless somebody turns it off
 * (ADR 0069 amendment 2026-10-29). Like the model it is a setting of the agent, not a new version,
 * and it applies to the next run. A model that cannot think ignores it, and the section says so. A
 * model that always thinks offers no off choice; an off stored earlier is kept for a model that can
 * stop, and the section says it does not apply (old issue 316). Thinking is billed as output.
 */
function AgentThinking({ agent, model, archived }: { agent: Agent; model: GetAgentModelResponse; archived: boolean }) {
  const queryClient = useQueryClient()
  const [picked, setPicked] = useState<string>()
  const save = useMutation(AgentService.method.setAgentThinking, {
    onSuccess: async () => {
      setPicked(undefined)
      await queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AgentService, cardinality: undefined }) })
    },
  })
  const t = model.thinking
  if (!t) return null
  const current = t.effective === 'off' ? 'off' : 'on'
  const value = picked ?? current
  return (
    <div className="grid gap-1.5 border-t pt-3">
      <Label htmlFor="agent-thinking-pick">Thinking</Label>
      {!t.supported ? (
        <p id="agent-thinking-help" className="text-muted-foreground">
          Not supported by this model.
        </p>
      ) : t.alwaysThinks ? (
        <div id="agent-thinking-help" className="grid gap-1.5">
          <p className="text-muted-foreground">{sentence(alwaysThinksText)}</p>
          {t.offOverridden && (
            <Alert role="status">
              <AlertDescription>{sentence(t.notice || offOverriddenText)}</AlertDescription>
            </Alert>
          )}
        </div>
      ) : (
        <>
          <NativeSelect id="agent-thinking-pick" value={value} onChange={(e) => setPicked(e.target.value)} disabled={archived} aria-describedby="agent-thinking-help">
            <option value="on">On</option>
            <option value="off">Off</option>
          </NativeSelect>
          <p id="agent-thinking-help" className="text-muted-foreground text-xs">
            {thinkingSourceLabel(t.setting, t.versionSetting)} The model thinks before it answers, which can improve hard work. Thinking is billed as output tokens, so it adds to a run&apos;s cost. Changing it applies to the next run and is recorded in the audit log.
          </p>
          {save.error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>Could not save: {ConnectError.from(save.error).rawMessage}</AlertDescription>
            </Alert>
          )}
          <div>
            <Button
              size="sm"
              disabled={save.isPending || archived || value === current}
              onClick={() => save.mutate({ orgId: agent.orgId, agentId: agent.id, thinking: value })}
            >
              Save thinking
            </Button>
          </div>
        </>
      )}
    </div>
  )
}
