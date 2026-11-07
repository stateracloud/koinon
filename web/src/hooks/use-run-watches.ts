// SPDX-License-Identifier: Apache-2.0

import { useTransport } from '@connectrpc/connect-query'
import { useEffect, useReducer, useRef } from 'react'
import { RunState, type LiveChunk, type Run, type RunEvent } from '@/gen/koinon/run/v1alpha1/run_pb'
import { watchRun, type WatchStatus } from '@/lib/run-watch'

export type RunWatch = {
  run?: Run
  /** In seq order, each seq once. */
  events: RunEvent[]
  status: WatchStatus
  error?: string
  /**
   * The text of the message or thought the agent is typing, as far as it has been streamed (old issue 344).
   * Every chunk names its item. The stored event of an item carries the same name in its payload
   * ("item") and the whole text, so it replaces that item's live text and that of every older item
   * (an item that was never stored is cleared by a later stored one); live text at or below the
   * newest stored item is dropped on arrival, whichever of the two came first. Other events leave
   * the live text alone, so text still being typed stays on screen.
   */
  live: readonly LiveChunk[]
  /** The newest item that is a stored event, so a late live tail of it or of an older item can be dropped. */
  mark?: ItemMark
}

/** An item name `<epoch>-<n>` as numbers. Items are stored in order and epochs only grow within a run. */
export type ItemMark = { epoch: number; n: number }

/** parseItem reads `<epoch>-<n>`; undefined for an empty or malformed name (treated as no item). */
export function parseItem(item: string): ItemMark | undefined {
  const m = /^(\d+)-(\d+)$/.exec(item)
  return m ? { epoch: Number(m[1]), n: Number(m[2]) } : undefined
}

/** atOrBelow reports whether item is the mark or older. */
function atOrBelow(item: ItemMark, mark: ItemMark): boolean {
  return item.epoch < mark.epoch || (item.epoch === mark.epoch && item.n <= mark.n)
}

/** itemOf is the item name a stored event carries, "" for an event that is not an item. */
export function itemOf(ev: RunEvent): string {
  const item = ev.payload?.item
  return typeof item === 'string' ? item : ''
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

/** joinLive appends chunks to the live text, joining neighbours of one kind and item. */
export function joinLive(live: readonly LiveChunk[], more: readonly LiveChunk[]): LiveChunk[] {
  const out = [...live]
  for (const c of more) {
    const last = out.at(-1)
    if (last?.kind === c.kind && last.item === c.item) out[out.length - 1] = { ...last, text: last.text + c.text }
    else out.push(c)
  }
  return out
}

export function reducer(state: State, a: Action): State {
  const cur = state[a.runId] ?? empty
  switch (a.type) {
    case 'run':
      // Live text does not outlive its run. A canceled run keeps it: its worker may still store the
      // item it was typing, which replaces the text (the stream ends after that, ADR 0074 item 8).
      return { ...state, [a.runId]: { ...cur, run: a.run, live: a.run.state === RunState.SUCCEEDED || a.run.state === RunState.FAILED ? [] : cur.live } }
    case 'event': {
      const events = appendEvent(cur.events, a.event)
      if (events === cur.events) return state
      const item = parseItem(itemOf(a.event))
      // An event that names its item replaces the live text of that item and of every older one,
      // and the item-less text of its kind (an older worker). One that does not name it replaces
      // the item-less live text of its kind.
      let mark = cur.mark
      let live: readonly LiveChunk[]
      if (item) {
        if (!mark || !atOrBelow(item, mark)) mark = item
        const m = mark
        live = cur.live.filter((c) => {
          const ci = parseItem(c.item)
          return ci ? !atOrBelow(ci, m) : c.kind !== a.event.kind
        })
      } else {
        // Text that names its item is the current item's; this event (an image, say) did not store it.
        live = cur.live.filter((c) => parseItem(c.item) !== undefined || c.kind !== a.event.kind)
      }
      return { ...state, [a.runId]: { ...cur, events, live, mark } }
    }
    case 'live': {
      const mark = cur.mark
      const chunks = mark
        ? a.chunks.filter((c) => {
            const ci = parseItem(c.item)
            return !ci || !atOrBelow(ci, mark)
          })
        : a.chunks
      return chunks.length === 0 ? state : { ...state, [a.runId]: { ...cur, live: joinLive(cur.live, chunks) } }
    }
    case 'status':
      return { ...state, [a.runId]: { ...cur, status: a.status, error: a.error, live: a.status === 'ended' ? [] : cur.live } }
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
