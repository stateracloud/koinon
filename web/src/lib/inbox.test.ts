// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { TaskState } from '@/gen/koinon/task/v1alpha1/task_pb'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { buildInbox, parseInboxTab, stableOrder, taskAt, tasksForTab } from '@/lib/inbox'
import { QuestionState } from '@/gen/koinon/question/v1alpha1/question_pb'
import { AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { agent, approval, question, task } from '@/test-utils'

const names = new Map([['a-1', 'Repo helper']])

describe('buildInbox', () => {
  it('lists decidable approvals and input-required tasks they do not explain', () => {
    const items = buildInbox({
      tasks: [
        task('t-1', 'Fix the typo', TaskState.INPUT_REQUIRED),
        task('t-2', 'Which branch?', TaskState.INPUT_REQUIRED),
        task('t-3', 'Write notes', TaskState.WORKING),
        task('t-4', 'Old', TaskState.COMPLETED),
      ],
      approvals: [approval('ap-1', 't-1'), approval('ap-2', 't-3', { canDecide: false })],
      agentNames: names,
      userId: 'u-1',
    })
    expect(items.map((i) => i.key).sort()).toEqual(['approval:ap-1', 'task-input:t-2'])
    const ap = items.find((i) => i.kind === 'approval')
    expect(ap).toMatchObject({ taskTitle: 'Fix the typo', agentName: 'Repo helper', label: 'create_pull_request on acme/widgets' })
  })

  it('lets an approval the viewer cannot decide explain its task without an item', () => {
    const items = buildInbox({
      tasks: [task('t-1', 'Fix the typo', TaskState.INPUT_REQUIRED)],
      approvals: [approval('ap-1', 't-1', { canDecide: false })],
      agentNames: names,
      userId: 'u-1',
    })
    expect(items).toEqual([])
  })

  it('lists only input-required tasks the viewer created', () => {
    const items = buildInbox({ tasks: [task('t-1', 'Mine', TaskState.INPUT_REQUIRED)], approvals: [], agentNames: names, userId: 'u-2' })
    expect(items).toEqual([])
  })

  it('shows no task-input item for a task waiting on someone else\'s question', () => {
    const t = task('t-1', 'Deploy', TaskState.INPUT_REQUIRED)
    t.waitingOnUserIds = ['u-2']
    t.waitingOnName = 'Sam'
    expect(buildInbox({ tasks: [t], approvals: [], agentNames: names, userId: 'u-1' })).toEqual([])
  })

  it('is empty when nothing waits', () => {
    expect(buildInbox({ tasks: [task('t-1', 'x', TaskState.WORKING)], approvals: [], agentNames: names, userId: 'u-1' })).toEqual([])
  })

  it('lists an open question and lets it explain its task', () => {
    const items = buildInbox({
      tasks: [task('t-1', 'Deploy the app', TaskState.INPUT_REQUIRED)],
      approvals: [],
      questions: [question('q-1', 't-1', { agentId: 'a-1' })],
      agentNames: names,
      userId: 'u-1',
    })
    expect(items.map((i) => i.key)).toEqual(['question:q-1'])
    expect(items[0]).toMatchObject({ kind: 'question', taskTitle: 'Deploy the app', agentName: 'Repo helper' })
  })

  it('lists no question that is resolved or not the viewer\'s to answer', () => {
    const items = buildInbox({
      tasks: [task('t-1', 'Deploy the app', TaskState.WORKING)],
      approvals: [],
      questions: [question('q-1', 't-1', { state: QuestionState.ANSWERED }), question('q-2', 't-1', { canAnswer: false })],
      agentNames: names,
      userId: 'u-1',
    })
    expect(items).toEqual([])
  })

  it('ages a question from when it started waiting on the viewer, not from when it opened', () => {
    const items = buildInbox({
      tasks: [task('t-1', 'Deploy the app', TaskState.INPUT_REQUIRED)],
      approvals: [],
      questions: [question('q-1', 't-1', { waitingSinceMs: 5_000_000, openedAtMs: 1_000_000 })],
      agentNames: names,
      userId: 'u-1',
    })
    expect(items[0].at).toBe(5_000_000)
  })

  it('leaves a task waiting on several people to them', () => {
    const t = task('t-1', 'Deploy', TaskState.INPUT_REQUIRED)
    t.waitingOnUserIds = ['u-2', 'u-3']
    expect(buildInbox({ tasks: [t], approvals: [], agentNames: names, userId: 'u-1' })).toEqual([])
    // The creator is not a recipient; a recipient who is the creator does get it.
    t.waitingOnUserIds = ['u-1', 'u-3']
    expect(buildInbox({ tasks: [t], approvals: [], agentNames: names, userId: 'u-1' }).map((i) => i.key)).toEqual(['task-input:t-1'])
  })

  describe('unhealthy agents', () => {
    const sick = agent('a-1', 'Repo helper', AgentStatus.ACTIVE, { atMs: 2_000_000, reason: 'its sandbox never became ready' })
    it('gives an admin an item for an unhealthy agent', () => {
      const items = buildInbox({ tasks: [], approvals: [], agents: [sick], isAdmin: true, agentNames: names, userId: 'u-1' })
      expect(items).toHaveLength(1)
      expect(items[0]).toMatchObject({ kind: 'agent-unhealthy', key: 'agent-unhealthy:a-1', agentId: 'a-1', reason: 'its sandbox never became ready', at: 2_000_000 })
    })

    it('gives a member none', () => {
      expect(buildInbox({ tasks: [], approvals: [], agents: [sick], isAdmin: false, agentNames: names, userId: 'u-1' })).toEqual([])
    })

    it('gives none for an archived agent, which cannot be resumed', () => {
      const archived = agent('a-2', 'Old', AgentStatus.ARCHIVED, { atMs: 1, reason: 'x' })
      expect(buildInbox({ tasks: [], approvals: [], agents: [archived], isAdmin: true, agentNames: names, userId: 'u-1' })).toEqual([])
    })
  })
})

describe('stableOrder', () => {
  const row = (key: string, at: number) => ({ kind: 'task-input' as const, key, taskId: key, taskTitle: `title ${at}`, agentId: '', agentName: undefined, at })
  it('keeps the frozen order but shows the current row content, drops gone rows, appends new ones', () => {
    const out = stableOrder([row('c', 3), row('a', 9), row('d', 5)], ['a', 'b', 'c'])
    expect(out.map((i) => i.key)).toEqual(['a', 'c', 'd'])
    expect(out[0]).toMatchObject({ taskTitle: 'title 9' })
  })
})

describe('inbox tabs', () => {
  const at = (t: ReturnType<typeof task>, updated: number, finished?: number) => {
    t.updatedAt = timestampFromMs(updated)
    if (finished !== undefined) t.finishedAt = timestampFromMs(finished)
    return t
  }
  const tasks = [
    at(task('s', 's', TaskState.SUBMITTED), 1_000),
    at(task('w', 'w', TaskState.WORKING), 5_000),
    at(task('i', 'i', TaskState.INPUT_REQUIRED), 3_000),
    at(task('c', 'c', TaskState.COMPLETED), 9_000, 2_000),
    at(task('f', 'f', TaskState.FAILED), 4_000, 4_000),
    at(task('x', 'x', TaskState.CANCELED), 6_000),
    // A state this UI does not know (a newer server): only All has it.
    at(task('u', 'u', 99 as TaskState), 7_000),
  ]

  it('Open has the non-terminal states, newest change first', () => {
    expect(tasksForTab(tasks, 'open').map((t) => t.id)).toEqual(['w', 'i', 's'])
  })

  it('Done has the terminal states, by end time (else last change)', () => {
    expect(tasksForTab(tasks, 'done').map((t) => t.id)).toEqual(['x', 'f', 'c'])
    expect(taskAt(tasks[3])).toBe(2_000)
    expect(taskAt(tasks[5])).toBe(6_000)
  })

  it('All has every task, also one in an unknown state', () => {
    expect(tasksForTab(tasks, 'all').map((t) => t.id)).toEqual(['u', 'x', 'w', 'f', 'i', 'c', 's'])
  })

  it('parses only the known tabs', () => {
    expect(['blocked', 'open', 'done', 'all', 'nope', undefined].map(parseInboxTab)).toEqual(['blocked', 'open', 'done', 'all', undefined, undefined])
  })
})
