// SPDX-License-Identifier: Apache-2.0

import { useState, type FormEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Textarea } from '@/components/ui/textarea'
import { AgentStatus, type Agent } from '@/gen/koinon/agent/v1alpha1/agent_pb'

export type NewTicket = { agentId: string; title: string; message: string }

export type CreateTicketFormProps = {
  agents: readonly Agent[]
  onCreate: (t: NewTicket) => void
  pending?: boolean
  error?: string
}

/** CreateTicketForm hands work to one of the org's active agents. */
export function CreateTicketForm({ agents, onCreate, pending, error }: CreateTicketFormProps) {
  const active = agents.filter((a) => a.status === AgentStatus.ACTIVE)
  const [agentId, setAgentId] = useState('')
  const [title, setTitle] = useState('')
  const [message, setMessage] = useState('')
  const chosen = active.some((a) => a.id === agentId) ? agentId : (active[0]?.id ?? '')

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    if (!chosen || !title.trim()) return
    onCreate({ agentId: chosen, title: title.trim(), message: message.trim() })
  }

  if (active.length === 0) {
    return <p className="text-muted-foreground text-sm">No active agents yet. Activate an agent to hand it tickets.</p>
  }
  return (
    <form onSubmit={onSubmit} aria-label="New ticket" className="grid gap-3 md:grid-cols-[1fr_14rem_auto] md:items-end">
      <div className="grid gap-2">
        <Label htmlFor="ticket-title">Title</Label>
        <Input id="ticket-title" required value={title} onChange={(e) => setTitle(e.target.value)} placeholder="What should the agent do?" />
      </div>
      <div className="grid gap-2">
        <Label htmlFor="ticket-agent">Agent</Label>
        <Select value={chosen} onValueChange={setAgentId}>
          <SelectTrigger id="ticket-agent" className="w-full">
            <SelectValue placeholder="Choose an agent" />
          </SelectTrigger>
          <SelectContent>
            {active.map((a) => (
              <SelectItem key={a.id} value={a.id}>
                {a.displayName}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      <Button type="submit" disabled={pending || !title.trim()}>
        Create ticket
      </Button>
      <div className="grid gap-2 md:col-span-3">
        <Label htmlFor="ticket-message">First message (optional)</Label>
        <Textarea id="ticket-message" rows={2} value={message} onChange={(e) => setMessage(e.target.value)} />
      </div>
      {error && (
        <Alert variant="destructive" role="alert" className="md:col-span-3">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
    </form>
  )
}
