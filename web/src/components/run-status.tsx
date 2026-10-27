// SPDX-License-Identifier: Apache-2.0

import type { RunWatch } from '@/hooks/use-run-watches'

/** RunStatus is the live line per active run under a task's title. */
export function RunStatus({ runs }: { runs: { id: string; state: string; watch?: RunWatch }[] }) {
  return (
    <div role="status" aria-live="polite" className="text-muted-foreground text-sm">
      {runs.map((r) => (
        <p key={r.id}>
          Run {r.id.slice(0, 8)} is {r.state}
          {r.watch?.status === 'reconnecting' && ' · reconnecting…'}
          {r.watch?.status === 'failed' && ` · live updates stopped: ${r.watch.error ?? 'error'}`}
        </p>
      ))}
    </div>
  )
}
