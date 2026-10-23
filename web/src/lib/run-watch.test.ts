// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { describe, expect, it } from 'vitest'
import { RunEventSchema, RunService, type RunEvent, type WatchRunRequest } from '@/gen/koinon/run/v1alpha1/run_pb'
import { watchRun, type WatchStatus } from '@/lib/run-watch'

const ev = (seq: number) => create(RunEventSchema, { runId: 'r', seq: BigInt(seq), kind: 'acp.agent_message_chunk' })

function drive(script: ((req: WatchRunRequest) => AsyncIterable<number | Error>)[]) {
  const calls: WatchRunRequest[] = []
  const transport = createRouterTransport(({ service }) => {
    service(RunService, {
      async *watchRun(req) {
        calls.push(req)
        const step = script[calls.length - 1]
        if (!step) throw new ConnectError('no more scripted calls', Code.Internal)
        for await (const x of step(req)) {
          if (x instanceof Error) throw x
          yield { item: { case: 'event', value: ev(x) } }
        }
      },
    })
  })
  return { transport, calls }
}

async function* seq(...xs: (number | Error)[]) {
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
      () => seq(1, 2, 3, 4), // a server that wrongly replays from the start
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
})
