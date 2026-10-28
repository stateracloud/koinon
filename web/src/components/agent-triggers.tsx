// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation, useQuery } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { useState } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { AgentService, AgentStatus, type Agent } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { QuestionService, type AskableMember } from '@/gen/koinon/question/v1alpha1/question_pb'
import { NotifyTargetKind, NotifyTargetSchema, TriggerSchema, TriggerType, type Trigger } from '@/gen/koinon/template/v1alpha1/template_pb'

/** triggerTypeLabel names the trigger types that can be set in v0.1. */
function triggerTypeLabel(t: TriggerType): string {
  switch (t) {
    case TriggerType.SCHEDULE:
      return 'Schedule'
    case TriggerType.DIRECT_MESSAGE:
      return 'Direct message'
    default:
      return 'Reserved'
  }
}

/** triggerProblem returns what is wrong with a trigger the server would refuse, if anything. */
function triggerProblem(t: Trigger): string | undefined {
  if (!/^[a-z][a-z0-9]*(-[a-z0-9]+)*$/.test(t.id) || t.id.length > 63) return 'The name must be lowercase letters, digits and dashes.'
  if (t.instruction.trim() === '') return 'The instruction is required.'
  if (t.type === TriggerType.SCHEDULE) {
    if (t.cron.trim().split(/\s+/).length !== 5) return 'The schedule needs five cron fields.'
    if (t.timezone.trim() === '') return 'The schedule needs a timezone.'
  }
  if (t.notify?.kind === NotifyTargetKind.USER && t.notify.userId === '') return 'Pick the person who is asked.'
  return undefined
}

/** Who is asked, in plain words: the org's default (admins) when nothing is set. */
function notifyLabel(t: Trigger, members: readonly AskableMember[] | undefined): string {
  const n = t.notify
  if (n?.kind === NotifyTargetKind.USER) return members?.find((m) => m.userId === n.userId)?.displayName ?? 'a person'
  if (n?.kind === NotifyTargetKind.ROLE) {
    if (n.role === OrgRole.OWNER) return "the org's owners"
    if (n.role === OrgRole.MEMBER) return 'all members'
  }
  return "the org's admins"
}

function describe(t: Trigger, members: readonly AskableMember[] | undefined): string {
  if (t.type === TriggerType.SCHEDULE) return `${t.cron} (${t.timezone}) · asks ${notifyLabel(t, members)}`
  return 'a message to the agent in the inbox chat'
}

/** The value of the "Who is asked" select for a trigger's notify target. */
function notifyChoice(t: Trigger): 'default' | 'role:owner' | 'role:member' | 'user' | 'channel' {
  const n = t.notify
  if (n?.kind === NotifyTargetKind.USER) return 'user'
  if (n?.kind === NotifyTargetKind.CHANNEL) return 'channel'
  if (n?.kind === NotifyTargetKind.ROLE && n.role === OrgRole.OWNER) return 'role:owner'
  if (n?.kind === NotifyTargetKind.ROLE && n.role === OrgRole.MEMBER) return 'role:member'
  // Unset, or the admins role, which is the default.
  return 'default'
}

/**
 * AgentTriggers shows what starts work on an agent: the triggers in effect (the active version's) and,
 * for an admin or owner, an editor that proposes a new version. Triggers take effect only when that
 * version is activated (ADR 0066), which the page's Activate button does. An admin can also fire a
 * schedule trigger of the active version at once ("Run now"); the task it creates is marked as a
 * manual fire.
 */
export function AgentTriggers({ agent, canEdit }: { agent: Agent; canEdit: boolean }) {
  const pending = agent.latestVersion !== agent.activeVersion
  const shown = pending ? agent.proposedTriggers : agent.approvedTriggers
  const [editing, setEditing] = useState(false)
  const queryClient = useQueryClient()
  // The members who can be asked: loaded while the editor is open; the read-only list reuses what
  // was loaded to name a person.
  const members = useQuery(QuestionService.method.listAskableMembers, { orgId: agent.orgId }, { enabled: editing })
  const [started, setStarted] = useState<{ trigger: string; taskId: string }>()
  const run = useMutation(AgentService.method.runAgentTrigger, {
    onSuccess: async (res, req) => {
      setStarted({ trigger: req.triggerId ?? '', taskId: res.taskId })
      await queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AgentService, cardinality: undefined }) })
    },
  })
  // Run now fires the active version's schedule triggers; a proposed trigger does nothing yet.
  const runnable = (t: Trigger) =>
    canEdit && agent.status === AgentStatus.ACTIVE && t.type === TriggerType.SCHEDULE && agent.approvedTriggers.some((a) => a.id === t.id && a.type === TriggerType.SCHEDULE)
  return (
    <section aria-labelledby="agent-triggers" className="bg-card grid gap-3 rounded-lg border p-4 text-sm">
      <div className="flex items-center justify-between gap-2">
        <h2 id="agent-triggers" className="font-semibold">
          {pending ? 'Proposed triggers' : 'Triggers'}
        </h2>
        {canEdit && !editing && agent.status !== AgentStatus.ARCHIVED && (
          <Button variant="outline" size="sm" onClick={() => setEditing(true)}>
            Edit triggers
          </Button>
        )}
      </div>
      {run.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>Could not run the trigger: {ConnectError.from(run.error).rawMessage}</AlertDescription>
        </Alert>
      )}
      {pending && <p className="text-muted-foreground">These take effect when version {agent.latestVersion} is activated.</p>}
      {editing ? (
        <Editor agent={agent} initial={shown} members={members} onDone={() => setEditing(false)} />
      ) : shown.length === 0 ? (
        <p className="text-muted-foreground">None. Nothing starts work on this agent except tickets.</p>
      ) : (
        <ul className="grid gap-2">
          {shown.map((t) => (
            <li key={t.id} className="border-t pt-2">
              <p className="font-medium">
                {t.id} · {triggerTypeLabel(t.type)}
              </p>
              <p className="text-muted-foreground">{describe(t, members.data?.members)}</p>
              <p className="break-words whitespace-pre-wrap">{t.instruction}</p>
              {runnable(t) && (
                <div className="mt-2">
                  <Button
                    variant="outline"
                    size="sm"
                    aria-label={`Run now: ${t.id}`}
                    disabled={run.isPending}
                    onClick={() => {
                      setStarted(undefined)
                      run.mutate({ orgId: agent.orgId, agentId: agent.id, triggerId: t.id })
                    }}
                  >
                    Run now
                  </Button>
                </div>
              )}
              {started?.trigger === t.id && (
                <p role="status" className="mt-1">
                  Started a task from {t.id}.{' '}
                  <Link to="/tasks/$taskId" params={{ taskId: started.taskId }} className="underline">
                    Open the task
                  </Link>
                </p>
              )}
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

/** notifyFromChoice is the notify target a "Who is asked" choice stands for; the default is none. */
function notifyFromChoice(choice: string) {
  switch (choice) {
    case 'role:owner':
      return create(NotifyTargetSchema, { kind: NotifyTargetKind.ROLE, role: OrgRole.OWNER })
    case 'role:member':
      return create(NotifyTargetSchema, { kind: NotifyTargetKind.ROLE, role: OrgRole.MEMBER })
    case 'user':
      return create(NotifyTargetSchema, { kind: NotifyTargetKind.USER })
    default:
      return undefined
  }
}

function Editor({
  agent,
  initial,
  members,
  onDone,
}: {
  agent: Agent
  initial: Trigger[]
  members: { data?: { members: AskableMember[] }; error: Error | null; isPending: boolean }
  onDone: () => void
}) {
  const queryClient = useQueryClient()
  const [rows, setRows] = useState<Trigger[]>(() => initial.map((t) => create(TriggerSchema, t)))
  const save = useMutation(AgentService.method.setAgentTriggers, {
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AgentService, cardinality: undefined }) })
      onDone()
    },
  })
  const change = (i: number, p: Partial<Trigger>) => setRows((cur) => cur.map((t, j) => (j === i ? create(TriggerSchema, { ...t, ...p }) : t)))
  const problem = rows.map(triggerProblem).find(Boolean)
  const hasDirect = rows.some((t) => t.type === TriggerType.DIRECT_MESSAGE)

  return (
    <div className="grid gap-3">
      {rows.map((t, i) => (
        <fieldset key={i} className="grid gap-2 rounded-md border p-3">
          <legend className="px-1 font-medium">Trigger {i + 1}</legend>
          <Label htmlFor={`trigger-id-${i}`}>Name</Label>
          <Input id={`trigger-id-${i}`} value={t.id} onChange={(e) => change(i, { id: e.target.value })} />
          <p className="text-muted-foreground">{triggerTypeLabel(t.type)}</p>
          {t.type === TriggerType.SCHEDULE && (
            <>
              <Label htmlFor={`trigger-cron-${i}`}>Cron (minute hour day-of-month month day-of-week)</Label>
              <Input id={`trigger-cron-${i}`} value={t.cron} placeholder="0 9 * * 1-5" onChange={(e) => change(i, { cron: e.target.value })} />
              <Label htmlFor={`trigger-tz-${i}`}>Timezone (IANA name)</Label>
              <Input id={`trigger-tz-${i}`} value={t.timezone} placeholder="Europe/Tallinn" onChange={(e) => change(i, { timezone: e.target.value })} />
            </>
          )}
          <Label htmlFor={`trigger-instruction-${i}`}>Instruction</Label>
          <Textarea id={`trigger-instruction-${i}`} value={t.instruction} onChange={(e) => change(i, { instruction: e.target.value })} />
          {t.type === TriggerType.DIRECT_MESSAGE ? (
            <p className="text-muted-foreground">Asks the person in the chat.</p>
          ) : (
            <>
              <Label htmlFor={`trigger-notify-${i}`}>Who is asked</Label>
              <NativeSelect
                id={`trigger-notify-${i}`}
                value={notifyChoice(t)}
                onChange={(e) => change(i, { notify: notifyFromChoice(e.target.value) })}
              >
                <option value="default">The org's admins (default)</option>
                <option value="role:owner">The org's owners</option>
                <option value="role:member">All members</option>
                <option value="user">A person…</option>
                <option value="channel" disabled>
                  A chat channel (from v0.2)
                </option>
              </NativeSelect>
              {notifyChoice(t) === 'user' && (
                <>
                  <Label htmlFor={`trigger-notify-user-${i}`}>Person</Label>
                  <NativeSelect
                    id={`trigger-notify-user-${i}`}
                    value={t.notify?.userId ?? ''}
                    onChange={(e) => change(i, { notify: create(NotifyTargetSchema, { kind: NotifyTargetKind.USER, userId: e.target.value }) })}
                  >
                    <option value="">Choose a person</option>
                    {members.data?.members.map((m) => (
                      <option key={m.userId} value={m.userId}>
                        {m.displayName} ({m.email})
                      </option>
                    ))}
                  </NativeSelect>
                  {members.error && <p role="alert" className="text-destructive">Could not load the members: {ConnectError.from(members.error).rawMessage}</p>}
                </>
              )}
            </>
          )}
          <div>
            <Button variant="outline" size="sm" onClick={() => setRows((cur) => cur.filter((_, j) => j !== i))}>
              Remove trigger {i + 1}
            </Button>
          </div>
        </fieldset>
      ))}
      <div className="flex flex-wrap gap-2">
        <Button variant="outline" size="sm" onClick={() => setRows((cur) => [...cur, create(TriggerSchema, { type: TriggerType.SCHEDULE, timezone: 'UTC' })])}>
          Add schedule
        </Button>
        <Button
          variant="outline"
          size="sm"
          disabled={hasDirect}
          onClick={() => setRows((cur) => [...cur, create(TriggerSchema, { type: TriggerType.DIRECT_MESSAGE })])}
        >
          Add direct message
        </Button>
      </div>
      {problem && <p className="text-muted-foreground">{problem}</p>}
      {save.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>Could not save: {ConnectError.from(save.error).rawMessage}</AlertDescription>
        </Alert>
      )}
      <div className="flex gap-2">
        <Button
          disabled={save.isPending || !!problem}
          onClick={() => save.mutate({ orgId: agent.orgId, agentId: agent.id, baseVersion: agent.latestVersion, triggers: rows })}
        >
          Propose new version
        </Button>
        <Button variant="outline" onClick={onDone}>
          Cancel
        </Button>
      </div>
    </div>
  )
}
