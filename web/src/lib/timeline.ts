// SPDX-License-Identifier: Apache-2.0

// One timeline per task (ADR 0017): the task's own events (messages,
// column moves, run notes, approvals) and the session updates of its runs,
// by time.

import type { JsonObject } from '@bufbuild/protobuf'
import { timestampMs, type Timestamp } from '@bufbuild/protobuf/wkt'
import type { RunEvent } from '@/gen/koinon/run/v1alpha1/run_pb'
import { ActorKind, type TaskEvent } from '@/gen/koinon/task/v1alpha1/task_pb'
import { arr, obj, str } from '@/lib/json'
import { stateLabel, taskStateFromName } from '@/lib/labels'

export type TimelineItem =
  | { key: string; at: number; type: 'message'; actor: ActorKind; text: string; runId?: string }
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

function approvalTarget(p: JsonObject): string {
  const tool = str(p.tool) ?? 'tool call'
  // Targets are "<connector>:<resource>", e.g. "github:owner/repo".
  const target = str(p.target)?.replace(/^[a-z0-9_-]+:/, '')
  return target ? `${tool} on ${target}` : tool
}

/**
 * approvalText describes an approval task event (old issue 34). The GitHub tools
 * server writes approval.requested ({decision, risk}); package approvals
 * writes the decision ({note}) and the outcome (executed {result: {url}},
 * failed {error}, expired, canceled {reason}). Every payload carries
 * approval_id, tool, target and args_sha256.
 */
function approvalText(kind: string, p: JsonObject): { text: string; url?: string } {
  const what = approvalTarget(p)
  const note = str(p.note)
  const withNote = (t: string) => (note ? `${t}: ${note}` : t)
  switch (kind) {
    case 'approval.requested':
      if (str(p.scope_id)) {
        const until = str(p.scope_expires_at)
        const kind = str(p.scope_kind) === 'task' ? 'this task' : 'this agent'
        return { text: `Running ${what}, allowed by a scope for ${kind}${until ? ` until ${new Date(until).toLocaleDateString()}` : ''}` }
      }
      return { text: str(p.decision) === 'allow' ? `Running ${what} (trusted)` : `Approval requested: ${what}` }
    case 'approval.approved': {
      const until = str(p.scope_expires_at)
      const scoped = str(p.scope_id) ? ` for ${str(p.scope_kind) === 'task' ? 'this task' : 'this agent'}${until ? ` until ${new Date(until).toLocaleDateString()}` : ''}` : ''
      return { text: withNote(`Approved ${what}${scoped}`) }
    }
    case 'approval.denied':
      return { text: withNote(`Denied ${what}`) }
    case 'approval.executed':
      return { text: `Done: ${what}`, url: str(obj(p.result)?.url) }
    case 'approval.failed': {
      const err = str(p.error)
      return { text: `Failed: ${what}${err ? `: ${err}` : ''}` }
    }
    case 'approval.expired':
      return { text: `Expired without a decision: ${what}` }
    case 'approval.canceled': {
      const reason = str(p.reason)
      return { text: `Canceled: ${what}${reason ? ` (${reason})` : ''}` }
    }
    default:
      return { text: `${kind}: ${what}` }
  }
}

function taskItem(ev: TaskEvent): TimelineItem {
  const key = `t:${ev.seq}`
  const at = ms(ev.createdAt)
  const p = ev.payload ?? {}
  const runId = ev.runId || undefined
  if (ev.kind.startsWith('approval.')) {
    const ask = ev.kind === 'approval.requested' && str(p.decision) !== 'allow'
    return { key, at, type: 'approval', runId, approvalId: str(p.approval_id) ?? '', kind: ev.kind, ask, ...approvalText(ev.kind, p) }
  }
  switch (ev.kind) {
    case 'message':
      // A direct message's text carries the trigger's instruction; "display" is what the person wrote.
      return { key, at, type: 'message', actor: ev.actorKind, text: str(p.display) ?? str(p.text) ?? '', runId }
    case 'task.created':
    case 'ticket.created':
      return { key, at, type: 'note', text: `Task created: ${str(p.title) ?? ''}`.trim() }
    case 'status.changed': {
      const from = stateLabel(taskStateFromName(str(p.from)))
      const to = stateLabel(taskStateFromName(str(p.to)))
      return { key, at, type: 'note', text: `Moved from ${from} to ${to}` }
    }
    case 'run.queued':
      return { key, at, type: 'note', text: `Run ${shortId(ev.runId)} queued`, runId }
    case 'run.parked': {
      // agentd parks a run whose turn ended with approvals outstanding (old issue 34):
      // {reason: "approval", approvals: [ids]}.
      const n = Array.isArray(p.approvals) ? p.approvals.length : 0
      return { key, at, type: 'note', text: `Run ${shortId(ev.runId)} paused for ${n === 1 ? '1 approval' : `${n} approvals`}`, runId }
    }
    case 'run.session_reset':
      // agentd starts a new harness session when the sandbox's home volume was
      // replaced since the task's session was stored: {message,
      // previousSessionId, sessionId}.
      return { key, at, type: 'note', text: str(p.message) || "The agent's home was reset, so this run starts a new session.", runId }
    case 'sandbox.recreated':
    case 'sandbox.work_lost':
    case 'agent.unhealthy':
    case 'run.session_restarted': {
      // Koinon's own notes (system actor), each {message}.
      const fallback: Record<string, string> = {
        'sandbox.recreated': 'Sandbox recreated.',
        'sandbox.work_lost': "Unsaved work in the agent's sandbox was lost.",
        'agent.unhealthy': 'The agent was marked unhealthy.',
        'run.session_restarted': 'Session restarted: new instructions.',
      }
      return { key, at, type: 'note', text: str(p.message) || fallback[ev.kind], runId }
    }
    case 'question.asked':
      return { key, at, type: 'note', text: `Asked ${str(p.to_label) ?? 'someone'}: ${str(p.question) ?? ''}`.trim(), runId }
    case 'question.answered':
      return { key, at, type: 'note', text: `${str(p.answered_by_name) || 'Someone'} answered: ${str(p.answer) ?? ''}`.trim(), runId }
    case 'question.dismissed':
      return { key, at, type: 'note', text: `${str(p.answered_by_name) || 'Someone'} dismissed the question; the default applied: ${str(p.answer) ?? ''}`.trim(), runId }
    case 'question.expired':
      return { key, at, type: 'note', text: `Nobody answered in time; the default applied: ${str(p.answer) ?? ''}`.trim(), runId }
    case 'question.canceled':
      return { key, at, type: 'note', text: 'The question was closed: the run ended', runId }
    case 'question.retargeted':
      return { key, at, type: 'note', text: `Nobody it was asked to can answer any more; now asked ${str(p.to_label) ?? 'the org\'s admins'}`, runId }
    case 'run.canceled':
    case 'run.failed': {
      if (ev.kind === 'run.canceled' && str(p.reason) === 'wall_clock_cap') {
        // agentd and the api's sweep write the same note: {reason, message, maxWallClockSeconds, summary?, summarySource}.
        const limit = typeof p.maxWallClockSeconds === 'number' ? ` of ${humanSeconds(p.maxWallClockSeconds)}` : ''
        const summary = str(p.summary)
        const source = str(p.summarySource) === 'notes' ? " (from the agent's notes)" : ''
        return {
          key,
          at,
          type: 'note',
          text: `Run ${shortId(ev.runId)} stopped at the time limit${limit}.${summary ? `\n\nLast summary${source}:\n${summary}` : ''}`,
          runId,
        }
      }
      // The ledger ends a run at its model-request or spend limit (ADR 0081) with the same note as
      // the time limit: {reason, message, limit, ..., summary?, summarySource?}.
      if (ev.kind === 'run.canceled' && (str(p.reason) === 'request_cap' || str(p.reason) === 'run_spend_cap')) {
        const what = str(p.reason) === 'request_cap' ? 'model request limit' : 'spend limit'
        const summary = str(p.summary)
        const source = str(p.summarySource) === 'notes' ? " (from the agent's notes)" : ''
        return {
          key,
          at,
          type: 'note',
          text: `Run ${shortId(ev.runId)} stopped at its ${what}: ${str(p.message) ?? 'the limit was reached'}.${summary ? `\n\nLast summary${source}:\n${summary}` : ''}`,
          runId,
        }
      }
      if (ev.kind === 'run.failed' && str(p.failureClass) === 'loop') {
        // agentd's loopStopReason says when the harness ignored the cancel and the agent was never asked.
        const asked = !(str(p.reason) ?? '').includes('could not be asked')
        const how = asked ? 'after it was asked to change its approach' : 'and could not be asked to change its approach (the harness ignored the cancel)'
        return { key, at, type: 'note', text: `Run ${shortId(ev.runId)} was stopped: the agent kept looping ${how}`, runId }
      }
      if (ev.kind === 'run.failed' && str(p.failureClass) === 'sandbox_unavailable') {
        const cause = sandboxCauses[str(p.sandboxCause) ?? ''] ?? sandboxCauses.unknown
        return { key, at, type: 'note', text: `Run ${shortId(ev.runId)} failed: the sandbox never became ready (${cause})`, runId }
      }
      // scheduler.RecordCancel writes {reason, message}; agentd's finish writes {reason}.
      // A spend limit reached (the ledger, ADR 0056) says which limit and what was spent in its message.
      if (str(p.reason) === 'spend_limit') {
        return { key, at, type: 'note', text: `Run ${shortId(ev.runId)} stopped at a spend limit: ${str(p.message) ?? 'a limit was reached'}`, runId }
      }
      const reason = str(p.message) || str(p.reason)
      const verb = ev.kind === 'run.failed' ? 'failed' : 'canceled'
      return { key, at, type: 'note', text: `Run ${shortId(ev.runId)} ${verb}${reason ? `: ${reason}` : ''}`, runId }
    }
    default: {
      const text = str(p.text) ?? str(p.reason)
      return { key, at, type: 'note', text: text ? `${ev.kind}: ${text}` : ev.kind, runId }
    }
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
