// SPDX-License-Identifier: Apache-2.0

import { create, type JsonObject } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { RunEventSchema } from '@/gen/koinon/run/v1alpha1/run_pb'
import { ActorKind, TicketEventSchema } from '@/gen/koinon/ticket/v1alpha1/ticket_pb'
import { buildTimeline } from '@/lib/timeline'

const run = 'r-1234567890'

// Run events as agentd records them: kind "acp.<sessionUpdate>" with the ACP
// SessionUpdate as marshalled by acp-go-sdk, and agentd's own run.* events.
const runEvent = (seq: number, kind: string, payload: JsonObject) =>
  create(RunEventSchema, { runId: run, seq: BigInt(seq), kind, payload, createdAt: timestampFromMs(1000 + seq * 10) })

const ticketEvent = (seq: number, at: number, kind: string, payload: JsonObject, actorKind = ActorKind.SYSTEM) =>
  create(TicketEventSchema, { seq: BigInt(seq), runId: run, kind, actorKind, payload, createdAt: timestampFromMs(at) })

const text = (t: string) => ({ type: 'text', text: t })

describe('buildTimeline', () => {
  it('reads agentd run events', () => {
    const items = buildTimeline([], {
      [run]: [
        runEvent(1, 'run.session', { sessionId: 's', loaded: false, takeovers: 0 }),
        runEvent(2, 'run.prompt', { text: 'Ticket: x', throughSeq: 2, resumed: false }),
        runEvent(3, 'acp.user_message_chunk', { sessionUpdate: 'user_message_chunk', content: text('Ticket: x') }),
        runEvent(4, 'acp.agent_thought_chunk', { sessionUpdate: 'agent_thought_chunk', content: text('Let me ') }),
        runEvent(5, 'acp.agent_thought_chunk', { sessionUpdate: 'agent_thought_chunk', content: text('look.') }),
        runEvent(6, 'acp.tool_call', { sessionUpdate: 'tool_call', toolCallId: 'call-1', title: 'Read README.md', kind: 'read', status: 'pending' }),
        runEvent(7, 'acp.permission', { toolCall: { toolCallId: 'call-1', title: 'Read README.md' }, decision: { kind: 'read', tier: 'read', verdict: 'allow' } }),
        runEvent(8, 'acp.tool_call', { sessionUpdate: 'tool_call', toolCallId: 'call-2', title: 'curl example.com', kind: 'fetch' }),
        runEvent(9, 'acp.tool_call_update', { sessionUpdate: 'tool_call_update', toolCallId: 'call-1', status: 'completed' }),
        runEvent(10, 'acp.permission', { toolCall: { toolCallId: 'call-2', title: 'curl example.com' }, decision: { verdict: 'reject' } }),
        runEvent(11, 'acp.usage_update', { sessionUpdate: 'usage_update', used: 10, size: 100 }),
        runEvent(12, 'acp.agent_message_chunk', { sessionUpdate: 'agent_message_chunk', content: text('Hello ') }),
        runEvent(13, 'acp.agent_message_chunk', { sessionUpdate: 'agent_message_chunk', content: text('world') }),
        runEvent(14, 'run.interrupted', { reason: 'harness exited', exitCode: 1 }),
        runEvent(15, 'run.succeeded', { reason: '' }),
      ],
    })
    expect(items.map((i) => [i.type, 'text' in i ? i.text : `${i.title} ${i.status}`])).toEqual([
      ['agent-text', 'Let me look.'],
      ['tool', 'Read README.md completed'],
      ['tool', 'curl example.com '],
      ['note', 'Not permitted in the sandbox: curl example.com'],
      ['agent-text', 'Hello world'],
      ['note', 'Run r-123456 lost its harness: harness exited'],
    ])
    expect(items[0]).toMatchObject({ thought: true })
  })

  it('shows a requeue and a retry as notes', () => {
    const items = buildTimeline([], {
      [run]: [
        runEvent(1, 'run.requeued', { reason: 'sandbox not ready after 5m0s (pending)' }),
        runEvent(2, 'run.retry', { failureClass: 'transient', attempt: 1, delaySeconds: 2, reason: 'x' }),
      ],
    })
    expect(items.map((i) => ('text' in i ? i.text : ''))).toEqual([
      'Run r-123456 is waiting for a sandbox (sandbox not ready after 5m0s (pending))',
      'Run r-123456 is retrying after a temporary model fault (attempt 1)',
    ])
  })

  it('shows a park and its approval ticket events', () => {
    const base = { approval_id: 'ap-1', tool: 'create_pull_request', target: 'github:Acme/Widgets', args_sha256: 'ab' }
    const items = buildTimeline(
      [
        ticketEvent(1, 100, 'approval.requested', { ...base, decision: 'ask', risk: {} }, ActorKind.AGENT),
        ticketEvent(2, 200, 'run.parked', { reason: 'approval', approvals: ['ap-1'] }),
        ticketEvent(3, 300, 'status.changed', { from: 'in_progress', to: 'waiting_on_you' }),
        ticketEvent(4, 400, 'approval.approved', { ...base, note: 'ship it' }, ActorKind.USER),
        ticketEvent(5, 500, 'approval.executed', { ...base, result: { url: 'https://github.com/Acme/Widgets/pull/7', number: 7 } }),
        ticketEvent(6, 600, 'approval.failed', { ...base, error: 'base branch gone' }),
        ticketEvent(7, 700, 'approval.denied', base, ActorKind.USER),
        ticketEvent(8, 800, 'approval.expired', { ...base, expires_at: '2026-10-29T00:00:00Z' }),
        ticketEvent(9, 900, 'approval.canceled', { ...base, reason: 'run ended' }),
        ticketEvent(10, 950, 'approval.requested', { ...base, decision: 'allow' }, ActorKind.AGENT),
        ticketEvent(11, 1000, 'run.canceled', { reason: 'approval_expired', message: 'nobody decided in time' }),
      ],
      {},
    )
    expect(items.map((i) => ('text' in i ? i.text : ''))).toEqual([
      'Approval requested: create_pull_request on Acme/Widgets',
      'Run r-123456 paused for 1 approval',
      'Moved from In progress to Waiting on you',
      'Approved create_pull_request on Acme/Widgets: ship it',
      'Done: create_pull_request on Acme/Widgets',
      'Failed: create_pull_request on Acme/Widgets: base branch gone',
      'Denied create_pull_request on Acme/Widgets',
      'Expired without a decision: create_pull_request on Acme/Widgets',
      'Canceled: create_pull_request on Acme/Widgets (run ended)',
      'Running create_pull_request on Acme/Widgets (trusted)',
      'Run r-123456 canceled: nobody decided in time',
    ])
    expect(items[4]).toMatchObject({ type: 'approval', approvalId: 'ap-1', url: 'https://github.com/Acme/Widgets/pull/7' })
    // Only a request a human decides gets the approval card.
    expect(items[0]).toMatchObject({ kind: 'approval.requested', ask: true })
    expect(items[9]).toMatchObject({ kind: 'approval.requested', ask: false })
    expect(items[3]).toMatchObject({ ask: false })
  })

  it('shows a session reset as a note', () => {
    const message = "The agent's workspace was reset, so this run starts a new session."
    const items = buildTimeline(
      [
        ticketEvent(1, 100, 'run.session_reset', { message, previousSessionId: 'ses_1', sessionId: 'ses_2' }),
        ticketEvent(2, 200, 'run.session_reset', {}),
      ],
      {},
    )
    expect(items).toEqual([
      { key: 't:1', at: 100, type: 'note', text: message, runId: run },
      { key: 't:2', at: 200, type: 'note', text: message, runId: run },
    ])
  })

  it("shows Koinon's sandbox and health events as notes with their message", () => {
    const items = buildTimeline(
      [
        ticketEvent(1, 100, 'sandbox.recreated', { message: 'Sandbox recreated: stuck process' }),
        ticketEvent(2, 200, 'sandbox.work_lost', { message: 'Unsaved work was lost' }),
        ticketEvent(3, 300, 'agent.unhealthy', { message: 'Agent marked unhealthy: repeated stuck sandboxes' }),
        ticketEvent(4, 400, 'run.session_restarted', { message: 'Session restarted: new instructions.' }),
        ticketEvent(5, 500, 'sandbox.recreated', {}),
      ],
      {},
    )
    expect(items.map((i) => (i.type === 'note' ? i.text : ''))).toEqual([
      'Sandbox recreated: stuck process',
      'Unsaved work was lost',
      'Agent marked unhealthy: repeated stuck sandboxes',
      'Session restarted: new instructions.',
      'Sandbox recreated.',
    ])
    expect(items.every((i) => i.type === 'note')).toBe(true)
  })

  it('gives a system-requested approval (saved work) an approval item that a human decides', () => {
    const items = buildTimeline(
      [ticketEvent(1, 100, 'approval.requested', { system: true, approval_id: 'ap-9', tool: 'push_saved_work', target: 'github:acme/widgets', args_sha256: 'abc' })],
      {},
    )
    expect(items[0]).toMatchObject({ type: 'approval', approvalId: 'ap-9', kind: 'approval.requested', ask: true, text: 'Approval requested: push_saved_work on acme/widgets' })
  })

  const chunk = (seq: number, t: string) => runEvent(seq, 'acp.agent_message_chunk', { sessionUpdate: 'agent_message_chunk', content: text(t) })
  const agentTexts = (items: ReturnType<typeof buildTimeline>) =>
    items.flatMap((i) => (i.type === 'agent-text' || (i.type === 'message' && i.actor === ActorKind.AGENT) ? [i.text] : []))

  it('shows a streamed reply that is not persisted yet', () => {
    const items = buildTimeline([], { [run]: [chunk(1, 'Hello '), chunk(2, 'there')] })
    expect(agentTexts(items)).toEqual(['Hello there'])
  })

  it('shows a streamed reply once when the persisted message arrives', () => {
    const items = buildTimeline([ticketEvent(1, 1100, 'message', { text: 'Hello there' }, ActorKind.AGENT)], {
      [run]: [chunk(1, 'Hello '), chunk(2, 'there')],
    })
    expect(agentTexts(items)).toEqual(['Hello there'])
    expect(items.map((i) => i.type)).toEqual(['message'])
  })

  it('shows each reply once across a park and resume', () => {
    const items = buildTimeline(
      [
        ticketEvent(1, 1100, 'message', { text: 'Asked for approval.' }, ActorKind.AGENT),
        ticketEvent(2, 1200, 'run.parked', { reason: 'approval', approvals: ['a'] }),
        ticketEvent(3, 1500, 'message', { text: 'The pull request is open.' }, ActorKind.AGENT),
      ],
      { [run]: [chunk(1, 'Asked for '), chunk(2, 'approval.'), chunk(30, 'The pull request '), chunk(31, 'is open.')] },
    )
    expect(agentTexts(items)).toEqual(['Asked for approval.', 'The pull request is open.'])
  })

  it('keeps the live text of a resumed turn until its message is persisted', () => {
    const items = buildTimeline([ticketEvent(1, 1100, 'message', { text: 'Asked for approval.' }, ActorKind.AGENT)], {
      [run]: [chunk(1, 'Asked for approval.'), chunk(30, 'Resumed, still typing')],
    })
    expect(agentTexts(items)).toEqual(['Asked for approval.', 'Resumed, still typing'])
  })
})

describe('a spend limit', () => {
  it('shows the ticket note the ledger writes when a run is stopped at a limit', () => {
    const items = buildTimeline(
      [ticketEvent(1, 100, 'run.canceled', { reason: 'spend_limit', limit: 'org_monthly', message: 'Org monthly cap reached ($10.00 of $10.00 since 2026-10-01 00:00 UTC)' })],
      {},
    )
    expect(items).toHaveLength(1)
    expect(items[0]).toMatchObject({ type: 'note', text: 'Run r-123456 stopped at a spend limit: Org monthly cap reached ($10.00 of $10.00 since 2026-10-01 00:00 UTC)' })
  })
})
