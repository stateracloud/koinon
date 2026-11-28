// SPDX-License-Identifier: Apache-2.0

import { AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { TaskState } from '@/gen/koinon/task/v1alpha1/task_pb'
import { agentStatusLabel, stateLabel } from '@/lib/labels'
import type { TokenStatus } from '@/lib/providers'
import { cn } from '@/lib/utils'

// ADR 0126 section 3. A badge is a tinted fill with a dot and the name as text: colour is never the only signal.
// Only role tokens (index.css); `web/src/lib/contrast.test.ts` checks every text and fill pair in both themes.
const tones = {
  submitted: 'bg-state-submitted-bg text-info',
  working: 'bg-state-working-bg text-link',
  attention: 'bg-attention text-attention-foreground',
  completed: 'bg-state-completed-bg text-success',
  failed: 'bg-state-failed-bg text-danger',
  // Hollow: no fill, a control border.
  neutral: 'border-input text-muted-foreground',
} as const

type Tone = keyof typeof tones

const taskTones: Partial<Record<TaskState, Tone>> = {
  [TaskState.SUBMITTED]: 'submitted',
  [TaskState.WORKING]: 'working',
  [TaskState.INPUT_REQUIRED]: 'attention',
  [TaskState.COMPLETED]: 'completed',
  [TaskState.FAILED]: 'failed',
  [TaskState.CANCELED]: 'neutral',
}

function Pill({ tone, label, testId }: { tone: Tone; label: string; testId?: string }) {
  return (
    <span
      data-slot="status-badge"
      data-tone={tone}
      data-testid={testId}
      className={cn('inline-flex w-fit shrink-0 items-center gap-1.5 rounded-full border border-transparent px-2 py-0.5 text-xs font-medium whitespace-nowrap', tones[tone])}
    >
      <span aria-hidden className="size-1.5 rounded-full bg-current" />
      {label}
    </span>
  )
}

/**
 * StatusBadge shows one task state: its name as text, a dot and the colour of ADR 0126 section 3. For input required it
 * shows the ADR name "Blocked on you" (the board column keeps its title). When `onViewer` is false, the task waits on
 * someone else: the badge then shows the general name ("Input required") and keeps the attention tone.
 */
export function StatusBadge({ state, onViewer = true }: { state: TaskState; onViewer?: boolean }) {
  const label = state === TaskState.INPUT_REQUIRED && onViewer ? 'Blocked on you' : stateLabel(state)
  return <Pill tone={taskTones[state] ?? 'neutral'} label={label} testId="status-badge" />
}

/** AgentStatusBadge shows an agent's status for the cards: Active (green), Draft (hollow) or Unhealthy (red). */
export function AgentStatusBadge({ status, unhealthy = false }: { status: AgentStatus; unhealthy?: boolean }) {
  if (unhealthy) return <Pill tone="failed" label="Unhealthy" testId="status-badge" />
  return <Pill tone={status === AgentStatus.ACTIVE ? 'completed' : 'neutral'} label={agentStatusLabel(status)} testId="status-badge" />
}

const tokenTones = { active: 'completed', expired: 'neutral', revoked: 'failed' } as const satisfies Record<TokenStatus, Tone>
const tokenLabels: Record<TokenStatus, string> = { active: 'Active', expired: 'Expired', revoked: 'Revoked' }

/** TokenStatusBadge shows an API token's status: Active (green), Expired (hollow) or Revoked (red). */
export function TokenStatusBadge({ status }: { status: TokenStatus }) {
  return <Pill tone={tokenTones[status]} label={tokenLabels[status]} testId="status-badge" />
}
