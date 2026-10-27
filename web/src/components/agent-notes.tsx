// SPDX-License-Identifier: Apache-2.0

import { timestampDate, type Timestamp } from '@bufbuild/protobuf/wkt'

/**
 * AgentNotes shows the agent's own summary of a task (what GetTask returns as agent_summary), collapsed
 * until opened. The text is plain: shown as written, wrapped, never rendered as markdown or HTML. Anyone who
 * can see the task sees it; with no summary nothing is rendered.
 */
export function AgentNotes({ summary, at }: { summary: string; at?: Timestamp }) {
  if (!summary) return null
  return (
    <section aria-label="Agent's notes">
      <details className="bg-card rounded-lg border" data-testid="agent-notes">
        <summary className="cursor-pointer px-3 py-2 text-sm font-medium">Agent&apos;s notes</summary>
        <div className="grid gap-2 border-t p-3">
          {at && (
            <p className="text-muted-foreground text-xs">
              Written <time dateTime={timestampDate(at).toISOString()}>{timestampDate(at).toLocaleString()}</time>
            </p>
          )}
          <p className="text-sm break-words whitespace-pre-wrap" data-testid="agent-notes-text">
            {summary}
          </p>
        </div>
      </details>
    </section>
  )
}
