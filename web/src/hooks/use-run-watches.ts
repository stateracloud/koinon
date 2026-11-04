// SPDX-License-Identifier: Apache-2.0

import { useTransport } from '@connectrpc/connect-query'
import { useEffect, useReducer, useRef } from 'react'
import type { LiveChunk, Run, RunEvent } from '@/gen/koinon/run/v1alpha1/run_pb'
import { watchRun, type WatchStatus } from '@/lib/run-watch'

export type RunWatch = {
  run?: Run
  /** In seq order, each seq once. */
  events: RunEvent[]
  status: WatchStatus
  error?: string
  /**
   * The text of the message or thought the agent is typing, as far as it has been streamed (#344).
   * A stored event of the same kind replaces the live text of that kind (it carries the whole
   * text); other events leave it alone, so text still being typed stays on screen.
   */
  live: readonly LiveChunk[]
}

type State = Record<string, RunWatch>

type Action =
  | { type: 'run'; runId: string; run: Run }
  | { type: 'event'; runId: string; event: RunEvent }
  | { type: 'live'; runId: string; chunks: readonly LiveChunk[] }
  | { type: 'status'; runId: string; status: WatchStatus; error?: string }

const empty: RunWatch = { events: [], status: 'connecting', live: [] }

/**
 * appendEvent adds ev to events unless its seq is already there. watchRun
 * already drops repeats; this keeps the list right even if two streams for
 * the same run overlap, as when React re-runs an effect.
 */
export function appendEvent(events: RunEvent[], ev: RunEvent): RunEvent[] {
  const last = events.at(-1)
  if (!last || ev.seq > last.seq) return [...events, ev]
  if (events.some((e) => e.seq === ev.seq)) return events
  return [...events, ev].sort((a, b) => (a.seq < b.seq ? -1 : a.seq > b.seq ? 1 : 0))
}

/** joinLive appends chunks to the live text, joining neighbours of one kind. */
export function joinLive(live: readonly LiveChunk[], more: readonly LiveChunk[]): LiveChunk[] {
  const out = [...live]
  for (const c of more) {
    const last = out.at(-1)
    if (last?.kind === c.kind) out[out.length - 1] = { ...last, text: last.text + c.text }
    else out.push(c)
  }
  return out
}

export function reducer(state: State, a: Action): State {
  const cur = state[a.runId] ?? empty
  switch (a.type) {
    case 'run':
      return { ...state, [a.runId]: { ...cur, run: a.run } }
    case 'event': {
      const events = appendEvent(cur.events, a.event)
      return events === cur.events ? state : { ...state, [a.runId]: { ...cur, events, live: cur.live.filter((c) => c.kind !== a.event.kind) } }
    }
    case 'live':
      return { ...state, [a.runId]: { ...cur, live: joinLive(cur.live, a.chunks) } }
    case 'status':
      return { ...state, [a.runId]: { ...cur, status: a.status, error: a.error } }
  }
}

/**
 * useRunWatches streams every run in runIds. A run whose stream ended (it
 * finished and drained) is not watched again; any other run resumes after
 * the last seq it delivered when the effect restarts.
 */
export function useRunWatches(
  orgId: string,
  runIds: readonly string[],
  onRunChange?: (run: Run) => void,
): State {
  const transport = useTransport()
  const [state, dispatch] = useReducer(reducer, {})
  const lastSeq = useRef(new Map<string, bigint>())
  const ended = useRef(new Set<string>())
  const onChange = useRef(onRunChange)
  useEffect(() => {
    onChange.current = onRunChange
  })

  const key = runIds.join(',')
  useEffect(() => {
    const ac = new AbortController()
    for (const runId of key ? key.split(',') : []) {
      if (ended.current.has(runId)) continue
      void watchRun(
        { transport, orgId, runId, afterSeq: lastSeq.current.get(runId) ?? 0n, signal: ac.signal },
        {
          onRun: (run) => {
            dispatch({ type: 'run', runId, run })
            onChange.current?.(run)
          },
          onEvent: (event) => {
            lastSeq.current.set(runId, event.seq)
            dispatch({ type: 'event', runId, event })
          },
          onLive: (chunks) => dispatch({ type: 'live', runId, chunks }),
          onStatus: (status, err) => {
            if (status === 'ended') ended.current.add(runId)
            dispatch({ type: 'status', runId, status, error: err?.rawMessage })
          },
        },
      )
    }
    return () => ac.abort()
  }, [transport, orgId, key])

  return state
}
