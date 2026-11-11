// SPDX-License-Identifier: Apache-2.0

import { create, type JsonObject } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { RunEventSchema } from '@/gen/koinon/run/v1alpha1/run_pb'
import { ActorKind, TaskEventSchema } from '@/gen/koinon/task/v1alpha1/task_pb'
import { buildTimeline } from '@/lib/timeline'

const run = 'r-1234567890'

// Run events as agentd records them: kind "acp.<sessionUpdate>" with the ACP
// SessionUpdate as marshalled by acp-go-sdk, and agentd's own run.* events.
const runEvent = (seq: number, kind: string, payload: JsonObject) =>
  create(RunEventSchema, { runId: run, seq: BigInt(seq), kind, payload, createdAt: timestampFromMs(1000 + seq * 10) })

const taskEvent = (seq: number, at: number, kind: string, payload: JsonObject, actorKind = ActorKind.SYSTEM) =>
  create(TaskEventSchema, { seq: BigInt(seq), runId: run, kind, actorKind, payload, createdAt: timestampFromMs(at) })

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

  it('shows a wait for the model provider as a note', () => {
    const items = buildTimeline([], {
      [run]: [
        runEvent(1, 'run.waiting_for_model', { model: 'deepseek/flash', provider: 'openrouter', reason: 'waiting for the model provider "openrouter" to reach the gateway' }),
        runEvent(2, 'run.waiting_for_model', {}),
      ],
    })
    expect(items.map((i) => ('text' in i ? i.text : ''))).toEqual([
      'Run r-123456 is waiting for the model provider openrouter to reach the gateway (model deepseek/flash)',
      'Run r-123456 is waiting for the model provider to reach the gateway',
    ])
  })

  it('shows a missing MCP server and its recovery as notes', () => {
    const items = buildTimeline([], {
      [run]: [
        runEvent(1, 'run.mcp_server_missing', { attempt: 1, servers: [{ name: 'metrics', reason: 'x' }, { name: 'wiki', reason: 'y' }] }),
        runEvent(2, 'run.mcp_server_recovered', { servers: ['metrics', 'wiki'] }),
        runEvent(3, 'run.mcp_server_missing', { attempt: 2, servers: [{ name: 'metrics', reason: 'x' }] }),
      ],
    })
    expect(items.map((i) => ('text' in i ? i.text : ''))).toEqual([
      'Run r-123456 found the MCP servers metrics, wiki not serving tools and will check once more before it starts',
      'Run r-123456 found its MCP servers serving tools again and started',
      'Run r-123456 found the MCP server metrics not serving tools again',
    ])
  })

  it('shows an uncached run as a note', () => {
    const items = buildTimeline([], { [run]: [runEvent(1, 'run.uncached', { model: 'google/gemini-x', reason: 'no cache mode for the provider format' })] })
    expect(items.map((i) => ('text' in i ? i.text : ''))).toEqual([
      'Run r-123456 runs without prompt caching (model google/gemini-x): no cache mode for the provider format',
    ])
  })

  it('shows a park and its approval task events', () => {
    const base = { approval_id: 'ap-1', tool: 'create_pull_request', target: 'github:Acme/Widgets', args_sha256: 'ab' }
    const items = buildTimeline(
      [
        taskEvent(1, 100, 'approval.requested', { ...base, decision: 'ask', risk: {} }, ActorKind.AGENT),
        taskEvent(2, 200, 'run.parked', { reason: 'approval', approvals: ['ap-1'] }),
        taskEvent(3, 300, 'status.changed', { from: 'working', to: 'input_required' }),
        taskEvent(4, 400, 'approval.approved', { ...base, note: 'ship it' }, ActorKind.USER),
        taskEvent(5, 500, 'approval.executed', { ...base, result: { url: 'https://github.com/Acme/Widgets/pull/7', number: 7 } }),
        taskEvent(6, 600, 'approval.failed', { ...base, error: 'base branch gone' }),
        taskEvent(7, 700, 'approval.denied', base, ActorKind.USER),
        taskEvent(8, 800, 'approval.expired', { ...base, expires_at: '2026-10-29T00:00:00Z' }),
        taskEvent(9, 900, 'approval.canceled', { ...base, reason: 'run ended' }),
        taskEvent(10, 950, 'approval.requested', { ...base, decision: 'allow' }, ActorKind.AGENT),
        taskEvent(11, 1000, 'run.canceled', { reason: 'approval_expired', message: 'nobody decided in time' }),
      ],
      {},
    )
    expect(items.map((i) => ('text' in i ? i.text : ''))).toEqual([
      'Approval requested: create_pull_request on Acme/Widgets',
      'Run r-123456 paused for 1 approval',
      'Moved from Working to Input required',
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

  it('names the scope that allowed a call without a decision', () => {
    const base = { approval_id: 'ap-1', tool: 'create_pull_request', target: 'github:Acme/Widgets', args_sha256: 'ab' }
    const items = buildTimeline(
      [
        taskEvent(1, 100, 'approval.approved', { ...base, scope_id: 'sc-1', scope_kind: 'agent', scope_expires_at: '2099-03-04T12:00:00Z' }, ActorKind.USER),
        taskEvent(2, 200, 'approval.requested', { ...base, decision: 'allow', scope_id: 'sc-1', scope_kind: 'task', scope_expires_at: '2099-03-04T12:00:00Z' }, ActorKind.AGENT),
      ],
      {},
    )
    const texts = items.map((i) => ('text' in i ? i.text : ''))
    expect(texts[0]).toMatch(/^Approved create_pull_request on Acme\/Widgets for this agent until /)
    expect(texts[1]).toMatch(/^Running create_pull_request on Acme\/Widgets, allowed by a scope for this task until /)
    expect(items[1]).toMatchObject({ ask: false })
  })

  it('shows a session reset as a note', () => {
    const message = "The agent's home was reset, so this run starts a new session."
    const items = buildTimeline(
      [
        taskEvent(1, 100, 'run.session_reset', { message, previousSessionId: 'ses_1', sessionId: 'ses_2' }),
        taskEvent(2, 200, 'run.session_reset', {}),
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
        taskEvent(1, 100, 'sandbox.recreated', { message: 'Sandbox recreated: stuck process' }),
        taskEvent(2, 200, 'sandbox.work_lost', { message: 'Unsaved work was lost' }),
        taskEvent(3, 300, 'agent.unhealthy', { message: 'Agent marked unhealthy: repeated stuck sandboxes' }),
        taskEvent(4, 400, 'run.session_restarted', { message: 'Session restarted: new instructions.' }),
        taskEvent(5, 500, 'sandbox.recreated', {}),
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
      [taskEvent(1, 100, 'approval.requested', { system: true, approval_id: 'ap-9', tool: 'push_saved_work', target: 'github:acme/widgets', args_sha256: 'abc' })],
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
    const items = buildTimeline([taskEvent(1, 1100, 'message', { text: 'Hello there' }, ActorKind.AGENT)], {
      [run]: [chunk(1, 'Hello '), chunk(2, 'there')],
    })
    expect(agentTexts(items)).toEqual(['Hello there'])
    expect(items.map((i) => i.type)).toEqual(['message'])
  })

  it('shows each reply once across a park and resume', () => {
    const items = buildTimeline(
      [
        taskEvent(1, 1100, 'message', { text: 'Asked for approval.' }, ActorKind.AGENT),
        taskEvent(2, 1200, 'run.parked', { reason: 'approval', approvals: ['a'] }),
        taskEvent(3, 1500, 'message', { text: 'The pull request is open.' }, ActorKind.AGENT),
      ],
      { [run]: [chunk(1, 'Asked for '), chunk(2, 'approval.'), chunk(30, 'The pull request '), chunk(31, 'is open.')] },
    )
    expect(agentTexts(items)).toEqual(['Asked for approval.', 'The pull request is open.'])
  })

  it('keeps the live text of a resumed turn until its message is persisted', () => {
    const items = buildTimeline([taskEvent(1, 1100, 'message', { text: 'Asked for approval.' }, ActorKind.AGENT)], {
      [run]: [chunk(1, 'Asked for approval.'), chunk(30, 'Resumed, still typing')],
    })
    expect(agentTexts(items)).toEqual(['Asked for approval.', 'Resumed, still typing'])
  })

  it('shows a reply once when its last chunk lands in the same millisecond as the persisted message', () => {
    // chunk(2) is at 1020 ms; the message is persisted in that same millisecond, and the task event sorts first by order.
    const items = buildTimeline([taskEvent(1, 1020, 'message', { text: 'Hello there' }, ActorKind.AGENT)], {
      [run]: [chunk(1, 'Hello '), chunk(2, 'there')],
    })
    expect(agentTexts(items)).toEqual(['Hello there'])
    expect(items.map((i) => i.type)).toEqual(['message'])
  })

  it('keeps a task event that follows the reply in the same millisecond after it', () => {
    const items = buildTimeline(
      [taskEvent(1, 1020, 'message', { text: 'Hello there' }, ActorKind.AGENT), taskEvent(2, 1020, 'message', { text: 'thanks' }, ActorKind.USER)],
      { [run]: [chunk(1, 'Hello '), chunk(2, 'there')] },
    )
    expect(items.map((i) => (i.type === 'message' ? i.text : i.type))).toEqual(['Hello there', 'thanks'])
  })

  it('keeps a user message that shares a millisecond with a chunk in its place', () => {
    const items = buildTimeline([taskEvent(1, 1010, 'message', { text: 'hi' }, ActorKind.USER)], { [run]: [chunk(1, 'Hello')] })
    expect(items.map((i) => i.type)).toEqual(['message', 'agent-text'])
  })

  it('drops the streamed text of each run once, for two runs on one ticket', () => {
    const run2 = 'r-2234567890'
    const chunk2 = (seq: number, t: string) =>
      create(RunEventSchema, { runId: run2, seq: BigInt(seq), kind: 'acp.agent_message_chunk', payload: { sessionUpdate: 'agent_message_chunk', content: text(t) }, createdAt: timestampFromMs(2000 + seq * 10) })
    const reply2 = create(TaskEventSchema, { seq: 2n, runId: run2, kind: 'message', actorKind: ActorKind.AGENT, payload: { text: 'Second' }, createdAt: timestampFromMs(2020) })
    const items = buildTimeline([taskEvent(1, 1020, 'message', { text: 'First' }, ActorKind.AGENT), reply2], {
      [run]: [chunk(1, 'Fir'), chunk(2, 'st')],
      [run2]: [chunk2(1, 'Sec'), chunk2(2, 'ond')],
    })
    expect(agentTexts(items)).toEqual(['First', 'Second'])
  })

  it('keeps a thought chunk when the reply is persisted', () => {
    const thought = runEvent(1, 'acp.agent_thought_chunk', { sessionUpdate: 'agent_thought_chunk', content: text('Thinking') })
    const items = buildTimeline([taskEvent(1, 1020, 'message', { text: 'Done' }, ActorKind.AGENT)], { [run]: [thought, chunk(2, 'Done')] })
    expect(items.map((i) => i.type)).toEqual(['agent-text', 'message'])
    expect(items[0]).toMatchObject({ thought: true, text: 'Thinking' })
  })

  it('keeps the streamed text of a failed run that persisted no reply', () => {
    const items = buildTimeline([taskEvent(1, 1030, 'run.failed', { reason: 'boom' })], { [run]: [chunk(1, 'Half a rep'), chunk(2, 'ly')] })
    expect(agentTexts(items)).toEqual(['Half a reply'])
    expect(items.map((i) => i.type)).toEqual(['agent-text', 'note'])
  })
})

describe('a spend limit', () => {
  it('shows the task note the ledger writes when a run is stopped at a limit', () => {
    const items = buildTimeline(
      [taskEvent(1, 100, 'run.canceled', { reason: 'spend_limit', limit: 'org_monthly', message: 'Org monthly cap reached ($10.00 of $10.00 since 2026-10-01 00:00 UTC)' })],
      {},
    )
    expect(items).toHaveLength(1)
    expect(items[0]).toMatchObject({ type: 'note', text: 'Run r-123456 stopped at a spend limit: Org monthly cap reached ($10.00 of $10.00 since 2026-10-01 00:00 UTC)' })
  })

  it('names states from events written before the move from tickets', () => {
    const items = buildTimeline(
      [taskEvent(1, 100, 'status.changed', { from: 'todo', to: 'waiting_on_you' }), taskEvent(2, 200, 'status.changed', { from: 'input_required', to: 'completed' }), taskEvent(3, 300, 'ticket.created', { title: 'Old' })],
      {},
    )
    expect(items.map((i) => (i.type === 'note' ? i.text : ''))).toEqual(['Moved from Submitted to Input required', 'Moved from Input required to Completed', 'Task created: Old'])
  })

  describe('time limit, sandbox cap and question notes', () => {
    const noteText = (ev: ReturnType<typeof taskEvent> | ReturnType<typeof runEvent>) => {
      const items = buildTimeline(ev.$typeName === 'koinon.task.v1alpha1.TaskEvent' ? [ev as ReturnType<typeof taskEvent>] : [], ev.$typeName === 'koinon.run.v1alpha1.RunEvent' ? { [run]: [ev as ReturnType<typeof runEvent>] } : {})
      return items.map((i) => ('text' in i ? i.text : ''))
    }

    it('says a run stopped at the time limit, with the last summary', () => {
      const [text] = noteText(taskEvent(1, 100, 'run.canceled', { reason: 'wall_clock_cap', maxWallClockSeconds: 1800, summary: 'S', summarySource: 'reply' }))
      expect(text).toContain('stopped at the time limit of 30m')
      expect(text).toContain('Last summary:\nS')
    })

    it('says when the summary came from the agent\'s notes', () => {
      const [text] = noteText(taskEvent(1, 100, 'run.canceled', { reason: 'wall_clock_cap', maxWallClockSeconds: 1800, summary: 'S', summarySource: 'notes' }))
      expect(text).toContain("(from the agent's notes)")
    })

    it('says a run stopped at its model request or spend limit, with the last summary (ADR 0081)', () => {
      const [req] = noteText(taskEvent(1, 100, 'run.canceled', { reason: 'request_cap', message: 'model request limit reached (150 of 150 requests)', summary: 'S', summarySource: 'reply' }))
      expect(req).toContain('stopped at its model request limit: model request limit reached (150 of 150 requests)')
      expect(req).toContain('Last summary:\nS')
      const [spend] = noteText(taskEvent(1, 100, 'run.canceled', { reason: 'run_spend_cap', message: 'run spend limit reached ($2.01 of $2.00)' }))
      expect(spend).toContain('stopped at its spend limit: run spend limit reached ($2.01 of $2.00)')
    })

    it('names why a sandbox never became ready', () => {
      const [text] = noteText(taskEvent(1, 100, 'run.failed', { reason: 'x', failureClass: 'sandbox_unavailable', sandboxCause: 'no_nodes' }))
      expect(text).toContain('no node can run the sandbox')
    })

    it('names a crash loop, an image pull and an unknown cause', () => {
      const text = (cause: string) => noteText(taskEvent(1, 100, 'run.failed', { reason: 'x', failureClass: 'sandbox_unavailable', sandboxCause: cause }))[0]
      expect(text('crash_loop')).toContain('a container in the sandbox keeps crashing')
      expect(text('container_create')).toContain('a container in the sandbox could not be created')
      expect(text('image_pull')).toContain('the image could not be pulled')
      expect(text('unknown')).toContain('the cause is unknown')
    })

    it('shows the one nudge to a looping agent and the stop after it', () => {
      expect(noteText(runEvent(1, 'run.loop_nudge', { pattern: 'same_error' }))[0]).toContain('looping (the same action failing)')
      expect(noteText(taskEvent(1, 100, 'run.failed', { reason: 'x', failureClass: 'loop' }))[0]).toContain('kept looping')
    })

    it('says when the loop stop came without the agent being asked', () => {
      const reason = 'the agent kept looping and could not be asked to change its approach (the harness ignored the cancel): it repeated the same action'
      const t = noteText(taskEvent(1, 100, 'run.failed', { reason, failureClass: 'loop' }))[0]
      expect(t).toContain('could not be asked')
      expect(t).not.toContain('after it was asked')
    })

    it('shows who answered a question', () => {
      expect(noteText(taskEvent(1, 100, 'question.answered', { answered_by_name: 'Ana', answer: 'Yes' }))).toEqual(['Ana answered: Yes'])
    })

    it('shows the other ends of a question', () => {
      expect(noteText(taskEvent(1, 100, 'question.asked', { to_label: "the org's admins", question: 'Ship it?' }))).toEqual(["Asked the org's admins: Ship it?"])
      expect(noteText(taskEvent(1, 100, 'question.dismissed', { answered_by_name: 'Ana', answer: 'No' }))).toEqual(['Ana dismissed the question; the default applied: No'])
      expect(noteText(taskEvent(1, 100, 'question.expired', { answer: 'No' }))).toEqual(['Nobody answered in time; the default applied: No'])
      expect(noteText(taskEvent(1, 100, 'question.canceled', {}))).toEqual(['The question was closed: the run ended'])
      expect(noteText(taskEvent(1, 100, 'question.retargeted', { to_label: "the org's admins" }))[0]).toContain("now asked the org's admins")
    })

    it('shows the wrap-up notice', () => {
      const [text] = noteText(runEvent(1, 'run.wrap_up_notice', { remainingSeconds: 300, maxWallClockSeconds: 1800 }))
      expect(text).toContain('asked to wrap up')
      expect(text).toContain('about 5 minutes left')
    })

    it('shows the wrap-up notice of a request or spend limit', () => {
      expect(noteText(runEvent(1, 'run.wrap_up_notice', { limit: 'requests', modelRequests: 120, maxModelRequests: 150 }))[0]).toContain('120 of 150 model requests used')
      expect(noteText(runEvent(1, 'run.wrap_up_notice', { limit: 'spend', spentUSD: '1.62', maxSpendUSD: '2.00' }))[0]).toContain('$1.62 of $2.00 spent')
    })
  })
})

describe('buildTimeline with purged content', () => {
  const purged = (seq: number, kind: string) => create(RunEventSchema, { runId: run, seq: BigInt(seq), kind, payload: {}, contentPurged: true, createdAt: timestampFromMs(1000 + seq * 10) })

  it('shows one "content expired after N days" note per run', () => {
    const items = buildTimeline([], { [run]: [purged(1, 'acp.agent_message_chunk'), purged(2, 'acp.tool_call'), runEvent(3, 'run.wrap_up_notice', { limit: 'requests', modelRequests: 5, maxModelRequests: 10 })] }, 30)
    expect(items.map((i) => ('text' in i ? i.text : i.type))).toEqual(['Run r-123456: content expired after 30 days', 'Run r-123456: 5 of 10 model requests used; the agent was asked to wrap up'])
  })

  it('says only "content expired" when the retention is not known', () => {
    const items = buildTimeline([], { [run]: [purged(1, 'acp.agent_thought_chunk')] })
    expect(items.map((i) => ('text' in i ? i.text : i.type))).toEqual(['Run r-123456: content expired'])
  })
})
