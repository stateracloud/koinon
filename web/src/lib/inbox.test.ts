// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { TaskState } from '@/gen/koinon/task/v1alpha1/task_pb'
import { buildInbox } from '@/lib/inbox'
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
