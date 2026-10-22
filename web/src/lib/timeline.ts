// SPDX-License-Identifier: Apache-2.0

// One timeline per ticket (ADR 0017): the ticket's own events (messages,
// column moves, run notes, approvals) and the session updates of its runs,
// by time.

import type { JsonObject, JsonValue } from '@bufbuild/protobuf'
import { timestampMs, type Timestamp } from '@bufbuild/protobuf/wkt'
import type { RunEvent } from '@/gen/koinon/run/v1alpha1/run_pb'
import { ActorKind, type TicketEvent } from '@/gen/koinon/ticket/v1alpha1/ticket_pb'
import { statusLabel, ticketStatusFromName } from '@/lib/labels'

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

function str(v: JsonValue | undefined): string | undefined {
  return typeof v === 'string' ? v : undefined
}

function obj(v: JsonValue | undefined): JsonObject | undefined {
  return v && typeof v === 'object' && !Array.isArray(v) ? v : undefined
}

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
 * approvalText describes an approval ticket event (#34). The GitHub tools
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

function ticketItem(ev: TicketEvent): TimelineItem {
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
    case 'ticket.created':
      return { key, at, type: 'note', text: `Ticket created: ${str(p.title) ?? ''}`.trim() }
    case 'status.changed': {
      const from = statusLabel(ticketStatusFromName(str(p.from)))
      const to = statusLabel(ticketStatusFromName(str(p.to)))
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
    case 'run.canceled':
    case 'run.failed': {
      // scheduler.RecordCancel writes {reason, message}; agentd's finish writes {reason}.
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
 * ticket (parked, the outcome), which shows them.
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
    default:
      // The user's message chunks (already on the ticket), plans, usage and
      // mode updates, and agentd's bookkeeping are not shown.
      return undefined
  }
}

/**
 * buildTimeline merges the ticket's events and its runs' events by time.
 * Consecutive agent text chunks of one run become one item; a tool call's
 * updates fold into the call's item.
 */
export function buildTimeline(ticketEvents: readonly TicketEvent[], runEvents: Readonly<Record<string, readonly RunEvent[]>>): TimelineItem[] {
  const raw: { item: TimelineItem; order: number }[] = []
  let order = 0
  for (const ev of ticketEvents) raw.push({ item: ticketItem(ev), order: order++ })
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
  return out
}
