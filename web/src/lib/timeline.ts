// SPDX-License-Identifier: Apache-2.0

// One timeline per ticket (ADR 0017): the ticket's own events (messages,
// column moves, run notes) and the session updates of its runs, by time.

import type { JsonObject, JsonValue } from '@bufbuild/protobuf'
import { timestampMs, type Timestamp } from '@bufbuild/protobuf/wkt'
import type { RunEvent } from '@/gen/koinon/run/v1alpha1/run_pb'
import { ActorKind, type TicketEvent } from '@/gen/koinon/ticket/v1alpha1/ticket_pb'
import { statusLabel, ticketStatusFromName } from '@/lib/labels'

export type TimelineItem =
  | { key: string; at: number; type: 'message'; actor: ActorKind; text: string; runId?: string }
  | { key: string; at: number; type: 'agent-text'; runId: string; text: string; thought: boolean }
  | { key: string; at: number; type: 'tool'; runId: string; title: string; status: string }
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

function ticketItem(ev: TicketEvent): TimelineItem {
  const key = `t:${ev.seq}`
  const at = ms(ev.createdAt)
  const p = ev.payload ?? {}
  const runId = ev.runId || undefined
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
    case 'run.canceled': {
      const reason = str(p.message) || str(p.reason)
      return { key, at, type: 'note', text: `Run ${shortId(ev.runId)} canceled${reason ? `: ${reason}` : ''}`, runId }
    }
    default: {
      const text = str(p.text)
      return { key, at, type: 'note', text: text ? `${ev.kind}: ${text}` : ev.kind, runId }
    }
  }
}

/**
 * sessionUpdate reads an ACP session update from a run event payload. It
 * accepts the ACP shape ({sessionUpdate, content: {text}}) and the flattened
 * one agentd may record ({kind, text}).
 */
function sessionUpdate(p: JsonObject): { kind: string; text?: string; title?: string; status?: string; toolCallId?: string } {
  const u = obj(p.update) ?? p
  const kind = str(u.sessionUpdate) ?? str(u.kind) ?? 'other'
  const content = obj(u.content)
  return {
    kind,
    text: str(u.text) ?? str(content?.text),
    title: str(u.title) ?? str(u.toolTitle),
    status: str(u.status) ?? str(u.toolStatus),
    toolCallId: str(u.toolCallId),
  }
}

function runItem(ev: RunEvent): TimelineItem | undefined {
  // run.queued and run.canceled are also written to the ticket, which shows them.
  if (ev.kind.startsWith('run.')) return undefined
  const key = `r:${ev.runId}:${ev.seq}`
  const at = ms(ev.createdAt)
  if (ev.kind !== 'session.update') return { key, at, type: 'note', text: ev.kind, runId: ev.runId }
  const u = sessionUpdate(ev.payload ?? {})
  switch (u.kind) {
    case 'agent_message_chunk':
    case 'agent_thought_chunk':
      return { key, at, type: 'agent-text', runId: ev.runId, text: u.text ?? '', thought: u.kind === 'agent_thought_chunk' }
    case 'tool_call':
    case 'tool_call_update':
      return { key, at, type: 'tool', runId: ev.runId, title: u.title ?? u.toolCallId ?? 'tool call', status: u.status ?? '' }
    case 'user_message_chunk':
      // The user's message is already on the ticket.
      return undefined
    default:
      return undefined
  }
}

/**
 * buildTimeline merges the ticket's events and its runs' events by time.
 * Consecutive agent text chunks of one run become one item; consecutive
 * updates of one tool call collapse to the latest.
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
  for (const { item } of raw) {
    const prev = out.at(-1)
    if (prev?.type === 'agent-text' && item.type === 'agent-text' && prev.runId === item.runId && prev.thought === item.thought) {
      out[out.length - 1] = { ...prev, text: prev.text + item.text }
      continue
    }
    if (prev?.type === 'tool' && item.type === 'tool' && prev.runId === item.runId && prev.title === item.title) {
      out[out.length - 1] = { ...prev, status: item.status || prev.status }
      continue
    }
    out.push(item)
  }
  return out
}
