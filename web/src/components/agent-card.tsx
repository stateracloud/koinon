// SPDX-License-Identifier: Apache-2.0

import { Link } from '@tanstack/react-router'
import { memo } from 'react'
import { AgentAvatar } from '@/components/agent-avatar'
import { AgentStatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { Meter } from '@/components/ui/meter'
import type { AgentSummary } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { cardState, nextRunText, reportsToText, runText, spendToday, waitingText } from '@/lib/overview'

/**
 * AgentCard is one agent on the Agents tab of the org home (ADR 0126 section 4): the name, the model, the state,
 * "reports to", today's spend against the daily limit, running now or the last run, the next scheduled run and the
 * count of items waiting on the viewer. A draft card has "Review and activate": the agent page holds the review and
 * the activation (ADR 0020), so the button opens it. Memoised: a new page or poll keeps the cards whose agent did not
 * change, so 1k cards do not all render again.
 */
export const AgentCard = memo(function AgentCard({ agent: a, now }: { agent: AgentSummary; now: number }) {
  const state = cardState(a)
  const reportsTo = reportsToText(a)
  const spend = spendToday(a)
  const next = nextRunText(a, now)
  const waiting = waitingText(a.waitingOnYou)
  const titleId = `agent-card-${a.agentId}`
  return (
    <article aria-labelledby={titleId} data-testid="agent-card" className="bg-card grid content-start gap-2 rounded-lg border p-4 text-sm">
      <header className="flex flex-wrap items-start justify-between gap-2">
        <h3 id={titleId} className="flex min-w-0 items-center gap-2 font-medium break-words">
          <AgentAvatar agentId={a.agentId} name={a.displayName} />
          <Link to="/agents/$agentId" params={{ agentId: a.agentId }} className="text-link hover:underline">
            {a.displayName}
          </Link>
        </h3>
        <AgentStatusBadge status={a.status} unhealthy={state === 'unhealthy'} />
      </header>
      <dl className="text-muted-foreground grid gap-1">
        <div>
          <dt className="sr-only">Model</dt>
          <dd className="font-mono text-xs break-all">{a.model || 'No model'}</dd>
        </div>
        {reportsTo && (
          <div>
            <dt className="sr-only">Reports to</dt>
            <dd>{reportsTo}</dd>
          </div>
        )}
        <div>
          <dt className="sr-only">Runs</dt>
          <dd className={a.runningRuns > 0 ? 'text-foreground' : undefined}>{runText(a, now)}</dd>
        </div>
        {next && (
          <div>
            <dt className="sr-only">Next scheduled run</dt>
            <dd>{next}</dd>
          </div>
        )}
        <div>
          <dt className="sr-only">Spend today</dt>
          <dd className="grid gap-1 tabular-nums">
            <span>{spend.text}</span>
            {spend.percent !== undefined && <Meter compact label={`Spend today of ${a.displayName}`} value={spend.percent} max={100} valueText={spend.text} />}
          </dd>
        </div>
      </dl>
      {(waiting || state === 'draft') && (
        <footer className="flex flex-wrap items-center gap-2 pt-1">
          {waiting && (
            <Link to="/inbox" data-testid="agent-card-waiting" className="bg-attention text-attention-foreground rounded-full px-2 py-0.5 text-xs font-semibold hover:underline">
              {waiting}
            </Link>
          )}
          {state === 'draft' && (
            <Button asChild size="xs" className="ml-auto">
              <Link to="/agents/$agentId" params={{ agentId: a.agentId }} aria-label={`Review and activate ${a.displayName}`}>
                Review and activate
              </Link>
            </Button>
          )}
        </footer>
      )}
    </article>
  )
})
