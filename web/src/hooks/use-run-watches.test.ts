// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { LiveChunkSchema, RunEventSchema, RunSchema, RunState } from '@/gen/koinon/run/v1alpha1/run_pb'
import { joinLive, reducer } from '@/hooks/use-run-watches'

const chunk = (kind: string, text: string, item = '') => create(LiveChunkSchema, { kind, text, item })

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
  const event = (seq: number, kind: string, item?: string) => create(RunEventSchema, { seq: BigInt(seq), kind, payload: item ? { item } : {} })

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

  it('drops the live tail of an item that is a stored event already, whatever order they arrive in', () => {
    let st = reducer({}, { type: 'live', runId: 'r', chunks: [chunk(msg, 'Hello ', '1-1')] })
    st = reducer(st, { type: 'event', runId: 'r', event: event(1, msg, '1-1') })
    expect(st.r.live).toEqual([])
    const late = reducer(st, { type: 'live', runId: 'r', chunks: [chunk(msg, 'world', '1-1')] })
    expect(late.r.live).toEqual([])
  })

  it('keeps the live text of the next item when the previous item is stored', () => {
    let st = reducer({}, { type: 'live', runId: 'r', chunks: [chunk(msg, 'one', '1-1'), chunk(msg, 'two', '1-2')] })
    expect(st.r.live.map((c) => c.text)).toEqual(['one', 'two'])
    st = reducer(st, { type: 'event', runId: 'r', event: event(1, msg, '1-1') })
    expect(st.r.live.map((c) => c.text)).toEqual(['two'])
  })

  it('clears an orphan of an older epoch when the first item of the next epoch is stored', () => {
    let st = reducer({}, { type: 'live', runId: 'r', chunks: [chunk(msg, 'lost', '1-4'), chunk(msg, 'next', '2-2')] })
    st = reducer(st, { type: 'event', runId: 'r', event: event(1, msg, '2-1') })
    expect(st.r.live.map((c) => c.text)).toEqual(['next'])
    const late = reducer(st, { type: 'live', runId: 'r', chunks: [chunk(msg, 'old', '1-9')] })
    expect(late.r.live.map((c) => c.text)).toEqual(['next'])
  })

  it('clears an item that was never stored when a later one of the same epoch is', () => {
    let st = reducer({}, { type: 'live', runId: 'r', chunks: [chunk(msg, 'skipped', '1-2'), chunk(msg, 'next', '1-4')] })
    st = reducer(st, { type: 'event', runId: 'r', event: event(1, msg, '1-3') })
    expect(st.r.live.map((c) => c.text)).toEqual(['next'])
  })

  it('clears item-less live text of the kind next to a named event', () => {
    let st = reducer({}, { type: 'live', runId: 'r', chunks: [chunk(msg, 'old worker'), chunk('acp.agent_thought_chunk', 'thought')] })
    st = reducer(st, { type: 'event', runId: 'r', event: event(1, msg, '1-1') })
    expect(st.r.live.map((c) => c.text)).toEqual(['thought'])
  })

  it('treats a malformed item name as no item', () => {
    let st = reducer({}, { type: 'live', runId: 'r', chunks: [chunk(msg, 'text', 'x-y')] })
    st = reducer(st, { type: 'event', runId: 'r', event: event(1, msg, 'x-y') })
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
