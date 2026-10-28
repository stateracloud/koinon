// SPDX-License-Identifier: Apache-2.0

import { useState } from 'react'
import { Composer } from '@/components/composer'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { AgentStatus, type Agent } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { noDirectMessagesText, takesDirectMessages } from '@/lib/direct-message'

export type DirectMessageFormProps = {
  agents: readonly Agent[]
  /** Sends the text to the agent as a new thread. */
  onSend: (agentId: string, text: string) => Promise<unknown>
  pending: boolean
  error?: string
}

/** DirectMessageForm starts a thread with an agent from the inbox chat (a direct_message trigger). */
export function DirectMessageForm({ agents, onSend, pending, error }: DirectMessageFormProps) {
  const active = agents.filter((a) => a.status === AgentStatus.ACTIVE)
  const [agentId, setAgentId] = useState('')
  const chosen = active.find((a) => a.id === agentId) ?? active.find(takesDirectMessages) ?? active[0]
  if (!chosen) return null
  const open = takesDirectMessages(chosen)
  return (
    <section aria-label="New thread" className="grid gap-3 rounded-lg border p-4">
      <div className="grid gap-2 sm:max-w-xs">
        <Label htmlFor="dm-agent">Message an agent</Label>
        <Select value={chosen.id} onValueChange={setAgentId}>
          <SelectTrigger id="dm-agent" className="w-full" aria-label="Send to">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {active.map((a) => (
              <SelectItem key={a.id} value={a.id}>
                {a.displayName}
                {takesDirectMessages(a) ? '' : ' (no direct messages)'}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      {open ? (
        <Composer idPrefix="dm-composer" label="Message to agent" formLabel="New thread" submitLabel="Start thread" pending={pending} error={error} onSend={(text) => onSend(chosen.id, text)} />
      ) : (
        <p role="status" className="text-muted-foreground text-sm">
          {noDirectMessagesText(chosen.displayName)}
        </p>
      )}
    </section>
  )
}
