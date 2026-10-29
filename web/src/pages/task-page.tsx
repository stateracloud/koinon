// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation, useQuery, useTransport } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { getRouteApi, Link } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { AgentNotes } from '@/components/agent-notes'
import { ApprovalView } from '@/components/approval-view'
import { Composer } from '@/components/composer'
import { RunStatus } from '@/components/run-status'
import { Timeline } from '@/components/timeline'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { AgentService } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import type { Cost, Run, RunState } from '@/gen/koinon/run/v1alpha1/run_pb'
import { TaskService, TaskState } from '@/gen/koinon/task/v1alpha1/task_pb'
import { useTaskConversation } from '@/hooks/use-task-conversation'
import { costDetail, costLabel, estimateNote, runProgress } from '@/lib/cost'
import { isEnded, runFinished, runStateLabel, stateLabel } from '@/lib/labels'
import { useOrg } from '@/lib/org-context'

const route = getRouteApi('/app/tasks/$taskId')

export function TaskPage() {
  const { taskId } = route.useParams()
  const { org } = useOrg()
  const orgId = org.orgId
  const canEdit = org.role >= OrgRole.MEMBER
  const transport = useTransport()
  const queryClient = useQueryClient()

  const { task: taskQuery, watches, items, refresh: refreshTask } = useTaskConversation(orgId, taskId)
  const agents = useQuery(AgentService.method.listAgents, { orgId })

  const append = useMutation(TaskService.method.appendMessage, { onSuccess: refreshTask })
  const cancel = useMutation(TaskService.method.cancelTask, {
    onSuccess: async () => {
      await refreshTask()
      await queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: TaskService.method.listTasks, transport, cardinality: undefined }),
      })
    },
  })

  if (taskQuery.isPending) return <p role="status">Loading task…</p>
  if (taskQuery.isError) {
    return (
      <Alert variant="destructive" role="alert">
        <AlertDescription>
          {ConnectError.from(taskQuery.error).rawMessage}.{' '}
          <Link to="/" className="underline">
            Back to the inbox
          </Link>
        </AlertDescription>
      </Alert>
    )
  }

  const t = taskQuery.data.task
  const agent = agents.data?.agents.find((a) => a.id === t?.agentId)
  const agentName = agent?.displayName
  const runs = taskQuery.data.runs.map((r) => watches[r.id]?.run ?? r)
  // Costs come from GetTask only: watched runs carry none.
  const runCosts = taskQuery.data.runs.map((r, i) => ({ id: r.id, state: runs[i].state, cost: r.cost, run: runs[i] }))
  const active = runs.filter((r) => !runFinished(r.state))

  return (
    <div className="mx-auto grid max-w-3xl gap-6">
      <div className="flex flex-wrap items-start gap-3">
        <div className="grid gap-1">
          <Link to="/" className="text-muted-foreground text-sm hover:underline">
            ← Inbox
          </Link>
          <h1 className="text-xl font-semibold">{t?.title}</h1>
          <p className="text-muted-foreground text-sm">
            <Badge variant="secondary">{stateLabel(t?.state ?? TaskState.UNSPECIFIED)}</Badge> {agentName && <>· {agentName}</>}
            {t?.cost && (
              <>
                {' '}
                · <span title={costDetail(t.cost)}>Cost {costLabel(t.cost)}{estimateNote(t.cost) && ` (${estimateNote(t.cost)})`}</span>
              </>
            )}
          </p>
          <RunCosts runs={runCosts} />
        </div>
        {canEdit && t && !isEnded(t.state) && (
          <Button variant="outline" size="sm" className="ml-auto" disabled={cancel.isPending} onClick={() => cancel.mutate({ orgId, taskId })}>
            Cancel task
          </Button>
        )}
      </div>

      {agent?.unhealthyAt && (
        <Alert variant="destructive" role="alert">
          <AlertDescription className="grid gap-1">
            <span className="font-medium">This agent is unhealthy: runs on this task wait until it is resumed.</span>
            {agent.unhealthyReason && <span>Reason: {agent.unhealthyReason}</span>}
            {org.role >= OrgRole.ADMIN ? (
              <Link to="/agents/$agentId" params={{ agentId: agent.id }} className="underline">
                Open the agent to resume it
              </Link>
            ) : (
              <span>An org admin or owner can resume it.</span>
            )}
          </AlertDescription>
        </Alert>
      )}

      <RunStatus runs={active.map((r) => ({ id: r.id, state: runStateLabel(r.state), watch: watches[r.id] }))} />

      <AgentNotes summary={t?.agentSummary ?? ''} at={t?.agentSummaryAt} />

      <Timeline items={items} renderApproval={(id) => <ApprovalView approvalId={id} showPermalink />} />

      {canEdit && (
        <Composer
          pending={append.isPending}
          error={append.error ? ConnectError.from(append.error).rawMessage : undefined}
          onSend={(text) => append.mutateAsync({ orgId, taskId, text })}
        />
      )}
      {cancel.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>Could not cancel the task: {ConnectError.from(cancel.error).rawMessage}</AlertDescription>
        </Alert>
      )}
    </div>
  )
}

function RunCosts({ runs }: { runs: { id: string; state: RunState; cost?: Cost; run: Run }[] }) {
  // The elapsed time of a running run moves on its own.
  const [now, setNow] = useState(() => new Date())
  useEffect(() => {
    const t = setInterval(() => setNow(new Date()), 15_000)
    return () => clearInterval(t)
  }, [])
  if (!runs.some((r) => r.cost)) return null
  return (
    <section aria-label="Run costs" className="text-muted-foreground text-sm">
      <ul className="grid gap-0.5">
        {runs.map((r) => (
          <li key={r.id}>
            Run {r.id.slice(0, 8)} · {runStateLabel(r.state)} ·{' '}
            {r.cost ? <span title={costDetail(r.cost)}>{costLabel(r.cost)}{estimateNote(r.cost) && ` (${estimateNote(r.cost)})`}</span> : 'no cost recorded'}
            {/* The run's use of its limits (ADR 0080): the first one reached ends it. */}
            {runProgress(r.run, r.cost, now).length > 0 && (
              <span title="Requests, spend and time against the run's limits. The first limit reached ends the run; tokens are reported, not limited.">
                {' '}
                · {runProgress(r.run, r.cost, now).join(' · ')}
              </span>
            )}
          </li>
        ))}
      </ul>
    </section>
  )
}
