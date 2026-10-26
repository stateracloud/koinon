// SPDX-License-Identifier: Apache-2.0

// WatchRun is a server stream, so it uses the plain connect-web client:
// connect-query supports unary methods only (ADR 0022).

import { Code, ConnectError, createClient, type Transport } from '@connectrpc/connect'
import { RunService, RunState, type Run, type RunEvent } from '@/gen/koinon/run/v1alpha1/run_pb'

export type WatchStatus = 'connecting' | 'live' | 'reconnecting' | 'ended' | 'failed'

export type WatchHandlers = {
  /** A run snapshot: the first message, and again whenever the run changes. */
  onRun: (run: Run) => void
  /** A new event, never one at or below a seq already delivered. */
  onEvent: (event: RunEvent) => void
  onStatus: (status: WatchStatus, error?: ConnectError) => void
}

export type WatchOptions = {
  transport: Transport
  orgId: string
  runId: string
  /** Resume after this seq; 0n streams from the first event. */
  afterSeq?: bigint
  signal: AbortSignal
  /** Delay before reconnect attempt n (from 1). */
  backoff?: (attempt: number) => number
  sleep?: (ms: number, signal: AbortSignal) => Promise<void>
}

/** Codes that a retry cannot fix. */
const permanent = new Set([
  Code.Unauthenticated,
  Code.PermissionDenied,
  Code.NotFound,
  Code.InvalidArgument,
  Code.Unimplemented,
])

/**
 * connect-es reports a server stream whose body ended without an
 * EndStreamResponse as InvalidArgument. That is what a proxy or load balancer
 * produces when it cuts a long-lived response, so it is a dropped connection,
 * not a bad request. The server's own InvalidArgument arrives inside the
 * end-stream message with a different message and stays permanent.
 */
function isTruncatedStream(err: ConnectError): boolean {
  return err.code === Code.InvalidArgument && err.rawMessage.includes('missing EndStreamResponse')
}

function finished(state: RunState | undefined): boolean {
  return state === RunState.SUCCEEDED || state === RunState.FAILED || state === RunState.CANCELED
}

export function defaultBackoff(attempt: number): number {
  const base = Math.min(30_000, 500 * 2 ** (attempt - 1))
  return base / 2 + Math.random() * (base / 2)
}

function abortableSleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve()
    const t = setTimeout(done, ms)
    function done() {
      clearTimeout(t)
      signal.removeEventListener('abort', done)
      resolve()
    }
    signal.addEventListener('abort', done)
  })
}

/**
 * watchRun follows one run until it has finished and every event has been
 * delivered, the signal aborts, or a permanent error occurs. When the
 * stream drops it reconnects with after_seq set to the last seq delivered,
 * so events are neither lost nor repeated, even if the server resends one.
 * It resolves with the last seq delivered.
 */
export async function watchRun(opts: WatchOptions, h: WatchHandlers): Promise<bigint> {
  const client = createClient(RunService, opts.transport)
  const backoff = opts.backoff ?? defaultBackoff
  const sleep = opts.sleep ?? abortableSleep
  let after = opts.afterSeq ?? 0n
  let attempt = 0
  let state: RunState | undefined

  h.onStatus('connecting')
  while (!opts.signal.aborted) {
    try {
      const stream = client.watchRun({ orgId: opts.orgId, runId: opts.runId, afterSeq: after }, { signal: opts.signal })
      for await (const res of stream) {
        attempt = 0
        h.onStatus('live')
        if (res.item.case === 'run') {
          state = res.item.value.state
          h.onRun(res.item.value)
        } else if (res.item.case === 'event') {
          const ev = res.item.value
          if (ev.seq <= after) continue
          after = ev.seq
          h.onEvent(ev)
        }
      }
      // The server ends the stream once the run has finished and drained.
      // A clean end while the run is still open (a graceful proxy or server
      // shutdown) is a dropped stream, so resume instead of stopping.
      if (finished(state)) {
        h.onStatus('ended')
        return after
      }
      attempt++
      h.onStatus('reconnecting')
      await sleep(backoff(attempt), opts.signal)
      continue
    } catch (e) {
      if (opts.signal.aborted) break
      const err = ConnectError.from(e)
      if (permanent.has(err.code) && !isTruncatedStream(err)) {
        h.onStatus('failed', err)
        return after
      }
      attempt++
      h.onStatus('reconnecting', err)
      await sleep(backoff(attempt), opts.signal)
    }
  }
  return after
}
