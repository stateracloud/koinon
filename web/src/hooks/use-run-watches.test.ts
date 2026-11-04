// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { LiveChunkSchema, RunEventSchema, RunSchema, RunState } from '@/gen/koinon/run/v1alpha1/run_pb'
import { joinLive, reducer } from '@/hooks/use-run-watches'

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

describe('reducer live text', () => {
  const msg = 'acp.agent_message_chunk'
  const event = (seq: number, kind: string) => create(RunEventSchema, { seq: BigInt(seq), kind })

  it('keeps the live text through events of other kinds and drops it when its item is stored', () => {
    let st = reducer({}, { type: 'live', runId: 'r', chunks: [chunk(msg, 'Hello world')] })
    st = reducer(st, { type: 'event', runId: 'r', event: event(1, 'wrap_up_notice') })
    expect(st.r.live.map((c) => c.text)).toEqual(['Hello world'])
    st = reducer(st, { type: 'event', runId: 'r', event: event(2, msg) })
    expect(st.r.live).toEqual([])
  })

  it('holds the text once when a tool call follows the streamed message', () => {
    let st = reducer({}, { type: 'live', runId: 'r', chunks: [chunk(msg, 'Hello world')] })
    st = reducer(st, { type: 'event', runId: 'r', event: event(1, msg) })
    st = reducer(st, { type: 'event', runId: 'r', event: event(2, 'acp.tool_call') })
    expect(st.r.live).toEqual([])
  })

  it('drops the live text when the stream ends, and when a run succeeds', () => {
    let st = reducer({}, { type: 'live', runId: 'r', chunks: [chunk(msg, 'tail')] })
    st = reducer(st, { type: 'status', runId: 'r', status: 'ended' })
    expect(st.r.live).toEqual([])
    st = reducer({}, { type: 'live', runId: 'r', chunks: [chunk(msg, 'tail')] })
    st = reducer(st, { type: 'run', runId: 'r', run: create(RunSchema, { state: RunState.CANCELED }) })
    expect(st.r.live).toHaveLength(1)
    st = reducer(st, { type: 'run', runId: 'r', run: create(RunSchema, { state: RunState.SUCCEEDED }) })
    expect(st.r.live).toEqual([])
  })
})
