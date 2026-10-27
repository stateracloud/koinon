// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { TaskState } from '@/gen/koinon/task/v1alpha1/task_pb'
import { buildInbox } from '@/lib/inbox'
import { approval, task } from '@/test-utils'

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

  it('is empty when nothing waits', () => {
    expect(buildInbox({ tasks: [task('t-1', 'x', TaskState.WORKING)], approvals: [], agentNames: names, userId: 'u-1' })).toEqual([])
  })
})
