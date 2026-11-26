// SPDX-License-Identifier: Apache-2.0

import { Code } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useId, useState, type FormEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import { AgentService, type Agent } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { describeError } from '@/lib/api-error'

/**
 * AgentDraftEdit lets an admin or owner, or the member who created a never-activated draft, change
 * the draft's name and instructions before activation (UpdateAgent). The change is based on the
 * latest version, so an edit made at the same time in another tab is refused, not overwritten. The
 * model, triggers and limits have their own sections.
 */
export function AgentDraftEdit({ agent }: { agent: Agent }) {
  const [editing, setEditing] = useState(false)
  const headingId = useId()
  return (
    <section aria-labelledby={headingId} className="bg-card grid gap-2 rounded-lg border p-4 text-sm">
      <div className="flex items-center justify-between gap-2">
        <h2 id={headingId} className="font-semibold">
          Draft instructions
        </h2>
        {!editing && (
          <Button variant="outline" size="sm" onClick={() => setEditing(true)}>
            Edit draft
          </Button>
        )}
      </div>
      {editing ? (
        <EditForm agent={agent} onDone={() => setEditing(false)} />
      ) : agent.instructions ? (
        <p className="break-words whitespace-pre-wrap">{agent.instructions}</p>
      ) : (
        <p className="text-muted-foreground">None.</p>
      )}
    </section>
  )
}

function EditForm({ agent, onDone }: { agent: Agent; onDone: () => void }) {
  const id = useId()
  const queryClient = useQueryClient()
  // The version the form was opened on. A refetch can move agent.latestVersion on while the fields
  // still hold the old text; basing the update on the opened version makes the server refuse it.
  const [base] = useState(agent)
  const [name, setName] = useState(base.displayName)
  const [instructions, setInstructions] = useState(base.instructions)
  const [touched, setTouched] = useState(false)
  const update = useMutation(AgentService.method.updateAgent, {
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AgentService, cardinality: undefined }) })
      onDone()
    },
  })
  const problem = name.trim() === '' ? 'Give the agent a name.' : instructions.trim() === '' ? 'Write the instructions.' : undefined

  function submit(e: FormEvent) {
    e.preventDefault()
    setTouched(true)
    if (problem) return
    const paths: string[] = []
    if (name.trim() !== base.displayName) paths.push('display_name')
    if (instructions.trim() !== base.instructions) paths.push('instructions')
    if (paths.length === 0) return onDone()
    update.mutate({
      orgId: base.orgId,
      agentId: base.id,
      baseVersion: base.latestVersion,
      definition: { displayName: name.trim(), instructions: instructions.trim() },
      updateMask: { paths },
    })
  }

  return (
    <form onSubmit={submit} noValidate aria-label="Edit draft" className="grid gap-2">
      <Label htmlFor={`${id}-name`}>Name</Label>
      <Input id={`${id}-name`} value={name} onChange={(e) => setName(e.target.value)} />
      <Label htmlFor={`${id}-instructions`}>Instructions</Label>
      <Textarea id={`${id}-instructions`} rows={8} value={instructions} onChange={(e) => setInstructions(e.target.value)} />
      {touched && problem && (
        <p className="text-destructive text-sm" role="alert">
          {problem}
        </p>
      )}
      {update.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>
            {describeError(update.error).code === Code.FailedPrecondition
              ? 'Someone changed this draft after you opened the form, or it is no longer a draft. Cancel, read the current version, then edit again.'
              : `Could not save the draft: ${describeError(update.error).message}`}
          </AlertDescription>
        </Alert>
      )}
      <div className="flex gap-2">
        <Button type="submit" size="sm" disabled={update.isPending}>
          Save draft
        </Button>
        <Button type="button" variant="ghost" size="sm" onClick={onDone}>
          Cancel
        </Button>
      </div>
    </form>
  )
}
