// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { describe, expect, it } from 'vitest'
import { RunEventSchema, RunSchema, RunService, RunState, type RunEvent, type WatchRunRequest } from '@/gen/koinon/run/v1alpha1/run_pb'
import { watchRun, type WatchStatus } from '@/lib/run-watch'

const runMsg = (state: RunState) => create(RunSchema, { id: 'r', state })
const ev = (seq: number) => create(RunEventSchema, { runId: 'r', seq: BigInt(seq), kind: 'acp.agent_message_chunk' })

function drive(script: ((req: WatchRunRequest) => AsyncIterable<number | RunState | Error | 'succeeded' | 'running'>)[]) {
  const calls: WatchRunRequest[] = []
  const transport = createRouterTransport(({ service }) => {
    service(RunService, {
      async *watchRun(req) {
        calls.push(req)
        const step = script[calls.length - 1]
        if (!step) throw new ConnectError('no more scripted calls', Code.Internal)
        for await (const x of step(req)) {
          if (x instanceof Error) throw x
          if (x === 'succeeded' || x === 'running') {
            yield { item: { case: 'run', value: runMsg(x === 'succeeded' ? RunState.SUCCEEDED : RunState.RUNNING) } }
            continue
          }
          yield { item: { case: 'event', value: ev(x as number) } }
        }
      },
    })
  })
  return { transport, calls }
}

async function* seq(...xs: (number | Error | 'succeeded' | 'running')[]) {
  for (const x of xs) yield x
}

async function run(transport: ReturnType<typeof drive>['transport']) {
  const got: RunEvent[] = []
  const statuses: WatchStatus[] = []
  const last = await watchRun(
    { transport, orgId: 'o', runId: 'r', signal: new AbortController().signal, backoff: () => 0, sleep: async () => {} },
    { onRun: () => {}, onEvent: (e) => got.push(e), onStatus: (s) => statuses.push(s) },
  )
  return { got: got.map((e) => Number(e.seq)), statuses, last }
}

describe('watchRun', () => {
  it('resumes after the last delivered seq and drops repeats', async () => {
    const unavailable = new ConnectError('reset', Code.Unavailable)
    const { transport, calls } = drive([
      () => seq(1, 2, unavailable),
      () => seq(unavailable), // fails before any message
      () => seq('succeeded', 1, 2, 3, 4), // a server that wrongly replays from the start
    ])
    const { got, statuses, last } = await run(transport)
    expect(got).toEqual([1, 2, 3, 4])
    expect(last).toBe(4n)
    expect(calls.map((c) => c.afterSeq)).toEqual([0n, 2n, 2n])
    expect(statuses.at(-1)).toBe('ended')
    expect(statuses).toContain('reconnecting')
  })

  it('stops on errors a retry cannot fix', async () => {
    const { transport, calls } = drive([() => seq(1, new ConnectError('gone', Code.NotFound))])
    const { got, statuses } = await run(transport)
    expect(got).toEqual([1])
    expect(calls).toHaveLength(1)
    expect(statuses.at(-1)).toBe('failed')
  })

  it('reconnects after a stream cut without an EndStreamResponse', async () => {
    const cut = new ConnectError('protocol error: missing EndStreamResponse', Code.InvalidArgument)
    const { transport, calls } = drive([() => seq('running', 1, 2, cut), () => seq('succeeded', 3)])
    const { got, statuses } = await run(transport)
    expect(got).toEqual([1, 2, 3])
    expect(calls.map((c) => c.afterSeq)).toEqual([0n, 2n])
    expect(statuses).toContain('reconnecting')
    expect(statuses.at(-1)).toBe('ended')
  })

  it('keeps a server InvalidArgument permanent', async () => {
    const bad = new ConnectError('after_seq must not be negative', Code.InvalidArgument)
    const { transport, calls } = drive([() => seq(bad)])
    const { statuses } = await run(transport)
    expect(calls).toHaveLength(1)
    expect(statuses.at(-1)).toBe('failed')
  })

  it('resumes when the stream ends cleanly but the run is still open', async () => {
    const { transport, calls } = drive([() => seq('running', 1), () => seq('running', 2, 'succeeded')])
    const { got, statuses } = await run(transport)
    expect(got).toEqual([1, 2])
    expect(calls.map((c) => c.afterSeq)).toEqual([0n, 1n])
    expect(statuses.at(-1)).toBe('ended')
    expect(statuses).toContain('reconnecting')
  })

  it('escalates the backoff while streams are cut without progress', async () => {
    const cut = new ConnectError('protocol error: missing EndStreamResponse', Code.InvalidArgument)
    const { transport } = drive([
      () => seq('running', cut),
      () => seq('running', cut),
      () => seq('running', cut),
      () => seq('running', 1, cut), // an event is progress: back to the first delay
      () => seq('running', cut),
      () => seq('succeeded'),
    ])
    const attempts: number[] = []
    await watchRun(
      {
        transport, orgId: 'o', runId: 'r', signal: new AbortController().signal, now: () => 0,
        backoff: (n) => (attempts.push(n), 0), sleep: async () => {},
      },
      { onRun: () => {}, onEvent: () => {}, onStatus: () => {} },
    )
    expect(attempts).toEqual([1, 2, 3, 1, 2])
  })

  it('restarts the backoff after a stream that stayed up', async () => {
    const cut = new ConnectError('protocol error: missing EndStreamResponse', Code.InvalidArgument)
    const { transport } = drive([() => seq('running', cut), () => seq('running', cut), () => seq('succeeded')])
    const attempts: number[] = []
    let t = 0
    await watchRun(
      {
        transport, orgId: 'o', runId: 'r', signal: new AbortController().signal,
        now: () => (t += 60_000), // every stream looks long-lived
        backoff: (n) => (attempts.push(n), 0), sleep: async () => {},
      },
      { onRun: () => {}, onEvent: () => {}, onStatus: () => {} },
    )
    expect(attempts).toEqual([1, 1])
  })
})
