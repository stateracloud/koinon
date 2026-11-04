// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { LiveChunkSchema } from '@/gen/koinon/run/v1alpha1/run_pb'
import { joinLive } from '@/hooks/use-run-watches'

const chunk = (kind: string, text: string) => create(LiveChunkSchema, { kind, text })

describe('joinLive', () => {
  it('joins neighbours of one kind and keeps the order of kinds', () => {
    const got = joinLive([chunk('acp.agent_thought_chunk', 'think')], [
      chunk('acp.agent_thought_chunk', 'ing'),
      chunk('acp.agent_message_chunk', 'Hel'),
      chunk('acp.agent_message_chunk', 'lo'),
    ])
    expect(got.map((c) => [c.kind, c.text])).toEqual([
      ['acp.agent_thought_chunk', 'thinking'],
      ['acp.agent_message_chunk', 'Hello'],
    ])
  })
})
