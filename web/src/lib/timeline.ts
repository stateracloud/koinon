// SPDX-License-Identifier: Apache-2.0

// One timeline per task (ADR 0017): the task's own events (messages,
// column moves, run notes, approvals) and the session updates of its runs,
// by time.

import { timestampDate, timestampMs, type Timestamp } from '@bufbuild/protobuf/wkt'
import type { RunEvent } from '@/gen/koinon/run/v1alpha1/run_pb'
import { ActorKind, type TaskEvent } from '@/gen/koinon/task/v1alpha1/task_pb'
import type { ApprovalChanged, ApprovalRequested, ApprovalScope, RunEnded } from '@/gen/koinon/task/v1alpha1/timeline_pb'
import { arr, obj, str } from '@/lib/json'
import { stateLabel, taskStateFromName } from '@/lib/labels'

export type TimelineItem =
  | {
      key: string
      at: number
      type: 'message'
      actor: ActorKind
      actorUserId: string
      actorName: string
      /** The person without a Koinon account who wrote the message in Slack or Teams (ADR 0130). */
      external?: { platform: string; platformUserId: string }
      text: string
      runId?: string
    }
  | { key: string; at: number; type: 'agent-text'; runId: string; text: string; thought: boolean }
  | { key: string; at: number; type: 'tool'; runId: string; toolCallId: string; title: string; status: string }
  | {
      key: string
      at: number
      type: 'approval'
      runId?: string
      approvalId: string
      /** The event kind, e.g. approval.requested. */
      kind: string
      /** A request a human decides (not a Trusted-preset call). */
      ask: boolean
      text: string
      url?: string
    }
  | { key: string; at: number; type: 'note'; text: string; runId?: string }

function ms(t: Timestamp | undefined): number {
  return t ? timestampMs(t) : 0
}

function shortId(id: string): string {
  return id.slice(0, 8)
}

/** humanSeconds formats a duration like agentd's runreply.Human: "1h", "1h30m", "30m", "45s". */
function humanSeconds(total: number): string {
  const h = Math.floor(total / 3600)
  const m = Math.floor((total % 3600) / 60)
  const s = Math.floor(total % 60)
  return `${h > 0 ? `${h}h` : ''}${m > 0 ? `${m}m` : ''}${s > 0 ? `${s}s` : ''}` || '0s'
}

/** num shows a count from an event payload, or a question mark when the payload has none. */
function num(v: unknown): string {
  return typeof v === 'number' ? String(v) : '?'
}

// The loop patterns agentd names in run.loop_nudge (internal/agentd/loopguard).
const loopPatterns: Record<string, string> = {
  same_action: 'the same action with the same result',
  same_error: 'the same action failing',
  monologue: 'messages without a tool call',
  alternating: 'two actions taking turns',
}

/** The words for why a sandbox never became ready; the same as execution.SandboxWaitCause.Describe. */
const sandboxCauses: Record<string, string> = {
  image_pull: 'the image could not be pulled',
  admission: "the cluster's admission policy refused the sandbox",
  no_nodes: 'no node can run the sandbox',
  crash_loop: 'a container in the sandbox keeps crashing',
  container_create: 'a container in the sandbox could not be created',
  unknown: 'the cause is unknown',
}

function approvalTarget(a: ApprovalRequested | ApprovalChanged): string {
  const tool = a.tool || 'tool call'
  // Targets are "<connector>:<resource>", e.g. "github:owner/repo".
  const target = a.target.replace(/^[a-z0-9_-]+:/, '')
  return target ? `${tool} on ${target}` : tool
}

/**
 * scopeText names the scope of an approval: "this task" or "this agent", and the day it ends. A
 * scope without an end has the zero time (approvalscope.Payload), which is not shown.
 */
function scopeText(s: ApprovalScope): string {
  const until = s.expiresAt && timestampMs(s.expiresAt) > 0 ? ` until ${timestampDate(s.expiresAt).toLocaleDateString()}` : ''
  return `${s.kind === 'task' ? 'this task' : 'this agent'}${until}`
}

/** The task event oneof: one case for each kind that the server knows (internal/tasks). */
type TaskEventBody = NonNullable<TaskEvent['event']>['event']
type ApprovalBody = Extract<TaskEventBody, { value: ApprovalRequested | ApprovalChanged }>

/**
 * approvalText describes an approval task event (old issue 34). The GitHub tools server writes
 * approval.requested (decision, risk); package approvals writes the decision (note) and the outcome
 * (executed with a result url, failed with an error, expired, canceled with a reason).
 */
function approvalText(e: ApprovalBody): { text: string; url?: string } {
  const what = approvalTarget(e.value)
  switch (e.case) {
    case 'approvalRequested':
      if (e.value.scope?.id) return { text: `Running ${what}, allowed by a scope for ${scopeText(e.value.scope)}` }
      return { text: e.value.decision === 'allow' ? `Running ${what} (trusted)` : `Approval requested: ${what}` }
    default:
      break
  }
  const { note, scope, error, reason } = e.value
  const withNote = (t: string) => (note ? `${t}: ${note}` : t)
  switch (e.case) {
    case 'approvalFirstApproved':
      return { text: withNote(`First approval of two: ${what}`) }
    case 'approvalApproved':
      return { text: withNote(`Approved ${what}${scope?.id ? ` for ${scopeText(scope)}` : ''}`) }
    case 'approvalDenied':
      return { text: withNote(`Denied ${what}`) }
    case 'approvalExecuted': {
      const url = e.value.result?.url
      return { text: `Done: ${what}`, url: typeof url === 'string' ? url : undefined }
    }
    case 'approvalFailed':
      return { text: `Failed: ${what}${error ? `: ${error}` : ''}` }
    case 'approvalExpired':
      return { text: `Expired without a decision: ${what}` }
    case 'approvalCanceled':
      return { text: `Canceled: ${what}${reason ? ` (${reason})` : ''}` }
  }
}

// blockedReason says why a task waits on its owner after a run hit its time limit and could not be
// continued (issue #99): the reason of the status.changed event the scheduler writes.
function blockedReason(reason: string): string | undefined {
  switch (reason) {
    case 'continuation_limit':
      return 'the task used all its continuation runs'
    case 'task_spend_cap':
      return "the task reached its spend cap, so it was not continued"
    case 'continuation_agent_unavailable':
      return "the agent cannot take runs (it is not active or is marked unhealthy)"
    default:
      return undefined
  }
}

/** runEndedText describes a run.failed or run.canceled task event. */
function runEndedText(failed: boolean, r: RunEnded, runId: string): string {
  const summary = r.summary ? `\n\nLast summary${r.summarySource === 'notes' ? " (from the agent's notes)" : ''}:\n${r.summary}` : ''
  if (!failed && r.reason === 'wall_clock_cap') {
    // agentd and the api's sweep write the same note.
    const limit = r.maxWallClockSeconds > 0 ? ` of ${humanSeconds(r.maxWallClockSeconds)}` : ''
    return `Run ${shortId(runId)} stopped at the time limit${limit}.${summary}`
  }
  // The ledger ends a run at its model-request or spend limit (ADR 0081) with the same note as the time limit.
  if (!failed && (r.reason === 'request_cap' || r.reason === 'run_spend_cap')) {
    const what = r.reason === 'request_cap' ? 'model request limit' : 'spend limit'
    return `Run ${shortId(runId)} stopped at its ${what}: ${r.message || 'the limit was reached'}.${summary}`
  }
  if (failed && r.failureClass === 'loop') {
    // agentd's loopStopReason says when the harness ignored the cancel and the agent was never asked.
    const how = r.reason.includes('could not be asked')
      ? 'and could not be asked to change its approach (the harness ignored the cancel)'
      : 'after it was asked to change its approach'
    return `Run ${shortId(runId)} was stopped: the agent kept looping ${how}`
  }
  if (failed && r.failureClass === 'sandbox_unavailable') {
    const cause = sandboxCauses[r.sandboxCause] ?? sandboxCauses.unknown
    return `Run ${shortId(runId)} failed: the sandbox never became ready (${cause})`
  }
  // A spend limit reached (the ledger, ADR 0056) says which limit and what was spent in its message.
  if (r.reason === 'spend_limit') return `Run ${shortId(runId)} stopped at a spend limit: ${r.message || 'a limit was reached'}`
  // scheduler.RecordCancel writes a reason and a message; agentd's finish writes a reason.
  const why = r.message || r.reason
  return `Run ${shortId(runId)} ${failed ? 'failed' : 'canceled'}${why ? `: ${why}` : ''}`
}

/** The notes that Koinon writes without a message of their own. */
const noteFallback = {
  sandboxRecreated: 'Sandbox recreated.',
  sandboxWorkLost: "Unsaved work in the agent's sandbox was lost.",
  agentUnhealthy: 'The agent was marked unhealthy.',
  runSessionRestarted: 'Session restarted: new instructions.',
  runSessionReset: "The agent's home was reset, so this run starts a new session.",
} as const

/**
 * taskItem reads the typed event (TaskEvent.event, issue #735). The switch has a case for each
 * member of the oneof, so a new kind is a type error here until it has its words. An event that
 * this server cannot type (a kind it does not know) shows its kind.
 */
function taskItem(ev: TaskEvent): TimelineItem {
  const key = `t:${ev.seq}`
  const at = ms(ev.createdAt)
  const runId = ev.runId || undefined
  const note = (text: string): TimelineItem => ({ key, at, type: 'note', text, runId })
  const e: TaskEventBody = ev.event?.event ?? { case: undefined }
  switch (e.case) {
    case undefined:
      return note(ev.kind)
    case 'message': {
      // A direct message's text carries the trigger's instruction; display is what the person wrote.
      const m = e.value
      const x = ev.actorKind === ActorKind.EXTERNAL ? ev.actorExternal : undefined
      const external = x ? { platform: x.platform, platformUserId: x.platformUserId } : undefined
      return { key, at, type: 'message', actor: ev.actorKind, actorUserId: ev.actorUserId, actorName: ev.actorDisplayName, external, text: m.display || m.text, runId }
    }
    case 'taskCreated':
      return { key, at, type: 'note', text: `Task created: ${e.value.title}`.trim() }
    case 'statusChanged': {
      const s = e.value
      const why = blockedReason(s.reason)
      return { key, at, type: 'note', text: `Moved from ${stateLabel(taskStateFromName(s.from))} to ${stateLabel(taskStateFromName(s.to))}${why ? `: ${why}` : ''}` }
    }
    case 'runQueued':
      return note(`Run ${shortId(ev.runId)} queued`)
    case 'runParked': {
      // agentd parks a run whose turn ended with approvals outstanding (old issue 34).
      const n = e.value.approvalIds.length
      return note(`Run ${shortId(ev.runId)} parked for ${n === 1 ? '1 approval' : `${n} approvals`}`)
    }
    case 'runSessionReset':
    case 'runSessionRestarted':
    case 'sandboxRecreated':
    case 'sandboxWorkLost':
    case 'agentUnhealthy':
      // Koinon's own notes (system actor), each with a message. A session reset: agentd starts a new
      // harness session when the sandbox's home volume was replaced since the task's session was stored.
      return note(e.value.message || noteFallback[e.case])
    case 'approvalRequested':
    case 'approvalFirstApproved':
    case 'approvalApproved':
    case 'approvalDenied':
    case 'approvalExecuted':
    case 'approvalFailed':
    case 'approvalExpired':
    case 'approvalCanceled': {
      // Only a request that a human decides (not a Trusted-preset call) gets the approval card.
      const ask = e.case === 'approvalRequested' && e.value.decision !== 'allow'
      return { key, at, type: 'approval', runId, approvalId: e.value.approvalId, kind: ev.kind, ask, ...approvalText(e) }
    }
    case 'questionAsked':
      return note(`Asked ${e.value.toLabel || 'someone'}: ${e.value.question}`.trim())
    case 'questionAnswered':
      return note(`${e.value.answeredByName || 'Someone'} answered: ${e.value.answer}`.trim())
    case 'questionDismissed':
      return note(`${e.value.answeredByName || 'Someone'} dismissed the question; the default applied: ${e.value.answer}`.trim())
    case 'questionExpired':
      return note(`Nobody answered in time; the default applied: ${e.value.answer}`.trim())
    case 'questionCanceled':
      return note('The question was closed: the run ended')
    case 'questionRetargeted':
      return note(`Nobody it was asked to can answer any more; now asked ${e.value.toLabel || "the org's admins"}`)
    case 'runFailed':
    case 'runCanceled':
      return note(runEndedText(e.case === 'runFailed', e.value, ev.runId))
  }
}

/** The run event kinds the purge job blanks (orgsettings.ContentEventKinds). */
const PURGED_KINDS = new Set(['acp.agent_message_chunk', 'acp.agent_thought_chunk', 'acp.user_message_chunk', 'acp.tool_call', 'acp.tool_call_update', 'acp.plan'])

/**
 * runItem reads one of agentd's run events (internal/agentd). Session updates
 * are recorded as kind "acp.<sessionUpdate>" with the ACP SessionUpdate as the
 * payload: {sessionUpdate, content: {type, text}} for message chunks and
 * {sessionUpdate, toolCallId, title, status, kind} for tool calls and their
 * updates (whose title and status are optional). Permission answers are
 * "acp.permission" ({toolCall, decision: {kind, tier, verdict}}). agentd's own
 * run.* events are bookkeeping (session, prompt) or are also written to the
 * task (parked, the outcome), which shows them.
 */
function runItem(ev: RunEvent, retentionDays?: number): TimelineItem | undefined {
  const key = `r:${ev.runId}:${ev.seq}`
  const at = ms(ev.createdAt)
  const runId = ev.runId
  // The purge job blanked the content of this event after the org's retention (issue #69): the
  // run keeps its skeleton, so say so once for the run instead of showing empty messages.
  if (ev.contentPurged && PURGED_KINDS.has(ev.kind)) {
    const after = retentionDays ? ` after ${retentionDays} ${retentionDays === 1 ? 'day' : 'days'}` : ''
    return { key: `r:${runId}:content-expired`, at, type: 'note', text: `Run ${shortId(runId)}: content expired${after}`, runId }
  }
  const p = ev.payload ?? {}
  switch (ev.kind) {
    case 'acp.agent_message_chunk':
    case 'acp.agent_thought_chunk':
      return { key, at, type: 'agent-text', runId, text: str(obj(p.content)?.text) ?? '', thought: ev.kind === 'acp.agent_thought_chunk' }
    case 'acp.tool_call':
    case 'acp.tool_call_update':
      return { key, at, type: 'tool', runId, toolCallId: str(p.toolCallId) ?? '', title: str(p.title) ?? '', status: str(p.status) ?? '' }
    case 'acp.permission': {
      if (str(obj(p.decision)?.verdict) !== 'reject') return undefined
      const title = str(obj(p.toolCall)?.title) || 'a tool call'
      return { key, at, type: 'note', text: `Not permitted in the sandbox: ${title}`, runId }
    }
    case 'run.interrupted': {
      const reason = str(p.reason)
      return { key, at, type: 'note', text: `Run ${shortId(runId)} lost its harness${reason ? `: ${reason}` : ''}`, runId }
    }
    case 'run.wrap_up_notice': {
      // agentd told the agent a run limit is near: {limit, remainingSeconds, maxWallClockSeconds} and,
      // for the request and spend limits, {modelRequests, maxModelRequests} or {spentUSD, maxSpendUSD}.
      const limit = str(p.limit)
      if (limit === 'requests') {
        return { key, at, type: 'note', text: `Run ${shortId(runId)}: ${num(p.modelRequests)} of ${num(p.maxModelRequests)} model requests used; the agent was asked to wrap up`, runId }
      }
      if (limit === 'spend') {
        return { key, at, type: 'note', text: `Run ${shortId(runId)}: $${str(p.spentUSD) ?? '?'} of $${str(p.maxSpendUSD) ?? '?'} spent; the agent was asked to wrap up`, runId }
      }
      const mins = Math.max(1, Math.round((typeof p.remainingSeconds === 'number' ? p.remainingSeconds : 0) / 60))
      return { key, at, type: 'note', text: `Run ${shortId(runId)}: about ${mins} ${mins === 1 ? 'minute' : 'minutes'} left; the agent was asked to wrap up`, runId }
    }
    case 'run.requeued': {
      // agentd put the run back in the queue because its sandbox was not ready in time (ADR 0066).
      const reason = str(p.reason)
      return { key, at, type: 'note', text: `Run ${shortId(runId)} is waiting for a sandbox${reason ? ` (${reason})` : ''}`, runId }
    }
    case 'run.uncached': {
      // agentd: the model has a cache_read price but Koinon has no prompt-cache mode for its
      // provider format, so the run costs the uncached price (ADR 0068). {model, reason}.
      const model = str(p.model)
      return { key, at, type: 'note', text: `Run ${shortId(runId)} runs without prompt caching${model ? ` (model ${model})` : ''}: ${str(p.reason) ?? 'no cache mode for the provider format'}`, runId }
    }
    case 'run.waiting_for_model': {
      // agentd waits before it starts the harness until the gateway serves the run's model: a new
      // provider's key is still reaching the gateway (old issue 174). {model, provider, reason}.
      const provider = str(p.provider)
      const model = str(p.model)
      const what = provider ? `the model provider ${provider}` : 'the model provider'
      return { key, at, type: 'note', text: `Run ${shortId(runId)} is waiting for ${what} to reach the gateway${model ? ` (model ${model})` : ''}`, runId }
    }
    case 'run.waiting_for_forge': {
      // agentd waits before it starts the harness until koinon-gateway serves the agent's forge
      // credential: a new credential is still reaching the gateway (#1086 part C2). {reason}.
      return { key, at, type: 'note', text: `Run ${shortId(runId)} is waiting for the gateway to serve the agent's code-host credential`, runId }
    }
    case 'run.mcp_server_missing': {
      // agentd found an MCP server the agent may use not serving tools before it started the harness
      // (old issue 326). {attempt, servers: [{name, reason}], reason}: the first miss is retried, the second
      // fails the run.
      const names = arr(p.servers)
        .map((s) => str(obj(s)?.name))
        .filter((n): n is string => !!n)
      const what = names.length > 0 ? `the MCP server${names.length > 1 ? 's' : ''} ${names.join(', ')}` : 'an MCP server'
      const tail = p.attempt === 2 ? ' again' : ' and will check once more before it starts'
      return { key, at, type: 'note', text: `Run ${shortId(runId)} found ${what} not serving tools${tail}`, runId }
    }
    case 'run.mcp_server_recovered':
      return { key, at, type: 'note', text: `Run ${shortId(runId)} found its MCP servers serving tools again and started`, runId }
    case 'run.loop_nudge': {
      // agentd told the agent it is repeating itself (once per run): {pattern}.
      return { key, at, type: 'note', text: `Run ${shortId(runId)}: the agent seems to be looping (${loopPatterns[str(p.pattern) ?? ''] ?? 'it repeats itself'}); it was asked to change its approach`, runId }
    }
    case 'run.retry': {
      // agentd retries a turn after a transient model fault: {failureClass, attempt, delaySeconds, reason}.
      const attempt = typeof p.attempt === 'number' ? ` (attempt ${p.attempt})` : ''
      return { key, at, type: 'note', text: `Run ${shortId(runId)} is retrying after a temporary model fault${attempt}`, runId }
    }
    case 'run.sandbox_reverted': {
      // agentd tells the run that the reconciler reverted its crashed sandbox to its last snapshot
      // (#1526): {revert, at, snapshot_at?, lost}. Without snapshot_at the time is not known.
      const snap = str(p.snapshot_at)
      const lost = snap ? `the writes after ${snap} are lost` : 'the writes after its last snapshot are lost'
      return { key, at, type: 'note', text: `Run ${shortId(runId)}: the sandbox crashed and was reverted to its last snapshot; ${lost}`, runId }
    }
    default:
      // The user's message chunks (already on the task), plans, usage and
      // mode updates, and agentd's bookkeeping are not shown.
      return undefined
  }
}

/**
 * buildTimeline merges the task's events and its runs' events by time (at equal time, a
 * persisted agent reply comes after run items, then task events before run events).
 * Consecutive agent text chunks of one run become one item; a tool call's
 * updates fold into the call's item. agentd also persists each reply as an
 * agent message on the task (with the run's id); once that message exists,
 * the streamed text it covers (this run's agent text since its previous
 * message) is dropped so the reply shows once. Until it arrives, the live text
 * stays. A run event the purge job blanked shows as one "content expired after N days" note per run
 * (retentionDays is the org's retention when the caller knows it).
 */
export function buildTimeline(taskEvents: readonly TaskEvent[], runEvents: Readonly<Record<string, readonly RunEvent[]>>, retentionDays?: number): TimelineItem[] {
  const raw: { item: TimelineItem; order: number; rank: number }[] = []
  let order = 0
  const expired = new Set<string>()
  // At one millisecond a persisted agent reply sorts after the run's streamed text it covers, whatever
  // the order the two lists were read in; otherwise dropPersistedReplies would keep the last chunk. The
  // task events that follow the reply at that millisecond share its rank, so they stay behind it.
  let replyAt: number | undefined
  for (const ev of taskEvents) {
    const item = taskItem(ev)
    const reply = item.type === 'message' && item.actor === ActorKind.AGENT && !!item.runId
    if (reply) replyAt = item.at
    raw.push({ item, order: order++, rank: reply || replyAt === item.at ? 1 : 0 })
  }
  for (const events of Object.values(runEvents)) {
    for (const ev of events) {
      const item = runItem(ev, retentionDays)
      if (!item) continue
      // One "content expired" note per run, at its first purged event.
      if (item.key.endsWith(':content-expired')) {
        if (expired.has(item.key)) continue
        expired.add(item.key)
      }
      raw.push({ item, order: order++, rank: 0 })
    }
  }
  raw.sort((a, b) => a.item.at - b.item.at || a.rank - b.rank || a.order - b.order)

  const out: TimelineItem[] = []
  const tools = new Map<string, number>()
  for (const { item } of raw) {
    const prev = out.at(-1)
    if (prev?.type === 'agent-text' && item.type === 'agent-text' && prev.runId === item.runId && prev.thought === item.thought) {
      out[out.length - 1] = { ...prev, text: prev.text + item.text }
      continue
    }
    if (item.type === 'tool') {
      const id = `${item.runId}:${item.toolCallId}`
      const i = item.toolCallId ? tools.get(id) : undefined
      const known = i === undefined ? undefined : out[i]
      if (i !== undefined && known?.type === 'tool') {
        out[i] = { ...known, title: item.title || known.title, status: item.status || known.status }
        continue
      }
      if (item.toolCallId) tools.set(id, out.length)
      out.push({ ...item, title: item.title || item.toolCallId || 'tool call' })
      continue
    }
    out.push(item)
  }
  return dropPersistedReplies(out)
}

function dropPersistedReplies(items: TimelineItem[]): TimelineItem[] {
  const streamed = new Map<string, number[]>()
  const drop = new Set<number>()
  items.forEach((item, i) => {
    if (item.type === 'agent-text' && !item.thought) {
      streamed.set(item.runId, [...(streamed.get(item.runId) ?? []), i])
    } else if (item.type === 'message' && item.actor === ActorKind.AGENT && item.runId) {
      for (const j of streamed.get(item.runId) ?? []) drop.add(j)
      streamed.delete(item.runId)
    }
  })
  return drop.size ? items.filter((_, i) => !drop.has(i)) : items
}

/** isMine says whether a message was written by the viewer: a user actor whose id is the WhoAmI user. */
export function isMine(item: { actor: ActorKind; actorUserId: string }, meUserId: string | undefined): boolean {
  return item.actor === ActorKind.USER && !!meUserId && item.actorUserId === meUserId
}
