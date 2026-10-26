// SPDX-License-Identifier: Apache-2.0

// One timeline per task (ADR 0017): the task's own events (messages,
// column moves, run notes, approvals) and the session updates of its runs,
// by time.

import type { JsonObject } from '@bufbuild/protobuf'
import { timestampMs, type Timestamp } from '@bufbuild/protobuf/wkt'
import type { RunEvent } from '@/gen/koinon/run/v1alpha1/run_pb'
import { ActorKind, type TaskEvent } from '@/gen/koinon/task/v1alpha1/task_pb'
import { obj, str } from '@/lib/json'
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

function approvalTarget(p: JsonObject): string {
  const tool = str(p.tool) ?? 'tool call'
  // Targets are "<connector>:<resource>", e.g. "github:owner/repo".
  const target = str(p.target)?.replace(/^[a-z0-9_-]+:/, '')
  return target ? `${tool} on ${target}` : tool
}

/**
 * approvalText describes an approval task event (#34). The GitHub tools
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
      return { text: str(p.decision) === 'allow' ? `Running ${what} (trusted)` : `Approval requested: ${what}` }
    case 'approval.approved':
      return { text: withNote(`Approved ${what}`) }
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
      return { key, at, type: 'message', actor: ev.actorKind, text: str(p.text) ?? '', runId }
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
      // agentd parks a run whose turn ended with approvals outstanding (#34):
      // {reason: "approval", approvals: [ids]}.
      const n = Array.isArray(p.approvals) ? p.approvals.length : 0
      return { key, at, type: 'note', text: `Run ${shortId(ev.runId)} paused for ${n === 1 ? '1 approval' : `${n} approvals`}`, runId }
    }
    case 'run.session_reset':
      // agentd starts a new harness session when the sandbox's home volume was
      // replaced since the task's session was stored: {message,
      // previousSessionId, sessionId}.
      return { key, at, type: 'note', text: str(p.message) || "The agent's workspace was reset, so this run starts a new session.", runId }
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
    case 'run.canceled':
    case 'run.failed': {
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
function runItem(ev: RunEvent): TimelineItem | undefined {
  const key = `r:${ev.runId}:${ev.seq}`
  const at = ms(ev.createdAt)
  const runId = ev.runId
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
    case 'run.requeued': {
      // agentd put the run back in the queue because its sandbox was not ready in time (ADR 0066).
      const reason = str(p.reason)
      return { key, at, type: 'note', text: `Run ${shortId(runId)} is waiting for a sandbox${reason ? ` (${reason})` : ''}`, runId }
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
 * buildTimeline merges the task's events and its runs' events by time.
 * Consecutive agent text chunks of one run become one item; a tool call's
 * updates fold into the call's item. agentd also persists each reply as an
 * agent message on the task (with the run's id); once that message exists,
 * the streamed text it covers (this run's agent text since its previous
 * message) is dropped so the reply shows once. Until it arrives, the live text
 * stays.
 */
export function buildTimeline(taskEvents: readonly TaskEvent[], runEvents: Readonly<Record<string, readonly RunEvent[]>>): TimelineItem[] {
  const raw: { item: TimelineItem; order: number }[] = []
  let order = 0
  for (const ev of taskEvents) raw.push({ item: taskItem(ev), order: order++ })
  for (const events of Object.values(runEvents)) {
    for (const ev of events) {
      const item = runItem(ev)
      if (item) raw.push({ item, order: order++ })
    }
  }
  raw.sort((a, b) => a.item.at - b.item.at || a.order - b.order)

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
