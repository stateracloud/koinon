// SPDX-License-Identifier: Apache-2.0

import { useNavigate } from '@tanstack/react-router'
import { ChevronDownIcon, ChevronRightIcon } from 'lucide-react'
import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from 'react'
import { AgentAvatar } from '@/components/agent-avatar'
import { AgentStatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import type { AgentSummary } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { cardState, reportsToForest, type ChartNode, type TeamGroup } from '@/lib/overview'
import { cn } from '@/lib/utils'

/** A team with more agents than this starts with only its roots open; the viewer opens the rest. */
export const chartOpenAllMax = 50
/** The chart renders at most this many rows of a tree at first, and this many more on "Show more". */
export const chartRowStep = 200

type Row = { node: ChartNode; level: number; posinset: number; setsize: number; parentId?: string }

/** visibleRows flattens the open part of the forest in tree order. */
function visibleRows(roots: readonly ChartNode[], open: ReadonlySet<string>): Row[] {
  const out: Row[] = []
  const walk = (nodes: readonly ChartNode[], level: number, parentId?: string) => {
    nodes.forEach((node, i) => {
      out.push({ node, level, posinset: i + 1, setsize: nodes.length, parentId })
      if (node.children.length > 0 && open.has(node.agent.agentId)) walk(node.children, level + 1, node.agent.agentId)
    })
  }
  walk(roots, 1)
  return out
}

function initialOpen(roots: readonly ChartNode[], size: number): Set<string> {
  const open = new Set<string>()
  if (size > chartOpenAllMax) return open
  const walk = (nodes: readonly ChartNode[]) => {
    for (const n of nodes) {
      if (n.children.length > 0) {
        open.add(n.agent.agentId)
        walk(n.children)
      }
    }
  }
  walk(roots)
  return open
}

/** AgentChart is the Chart tab of the org home: one reports-to tree per team (ADR 0126 section 4). */
export function AgentChart({ teams }: { teams: readonly TeamGroup<AgentSummary>[] }) {
  return (
    <div className="grid gap-6">
      {teams.map((t) => (
        <TeamTree key={t.team} team={t} />
      ))}
    </div>
  )
}

/**
 * TeamTree is one team as a WAI-ARIA tree view (APG "Tree View"): the boxes are the tree items, so the chart and its
 * keyboard and screen-reader form are one element. Only agents are boxes; a person shows as the "Reports to" label of
 * a root. Up and Down move, Right opens or goes to the first child, Left closes or goes to the parent, Home and End go
 * to the first and the last row, and Enter opens the agent. The rows are flat with aria-level, aria-setsize and
 * aria-posinset, so a large team renders a bounded number of rows.
 */
function TeamTree({ team }: { team: TeamGroup<AgentSummary> }) {
  const navigate = useNavigate()
  const roots = useMemo(() => reportsToForest(team.agents), [team.agents])
  const [open, setOpen] = useState(() => initialOpen(roots, team.agents.length))
  const [limit, setLimit] = useState(chartRowStep)
  const [focused, setFocused] = useState<string>()
  const refs = useRef(new Map<string, HTMLLIElement>())
  // A row to focus once it renders: Right can move to a child past the row limit, which raises the limit first.
  const pendingFocus = useRef<string>(undefined)
  const rows = useMemo(() => visibleRows(roots, open), [roots, open])
  const shown = rows.slice(0, limit)
  useEffect(() => {
    const id = pendingFocus.current
    if (id === undefined) return
    pendingFocus.current = undefined
    refs.current.get(id)?.focus()
  })
  const focusId = shown.some((r) => r.node.agent.agentId === focused) ? focused : shown[0]?.node.agent.agentId
  const headingId = `chart-team-${team.team === '' ? 'none' : encodeURIComponent(team.team)}`

  const moveTo = (id: string | undefined) => {
    if (id === undefined) return
    const i = rows.findIndex((r) => r.node.agent.agentId === id)
    if (i >= limit) setLimit(Math.ceil((i + 1) / chartRowStep) * chartRowStep)
    setFocused(id)
    const el = refs.current.get(id)
    if (el) el.focus()
    else pendingFocus.current = id
  }
  // A row inside a branch never has the focus when the branch closes: Left closes only the focused row's own branch,
  // and a press on the chevron focuses its row first.
  const toggle = (id: string, to: boolean) => {
    setOpen((prev) => {
      const next = new Set(prev)
      if (to) next.add(id)
      else next.delete(id)
      return next
    })
  }
  const openAgent = (id: string) => void navigate({ to: '/agents/$agentId', params: { agentId: id } })

  const onKeyDown = (e: KeyboardEvent<HTMLLIElement>, i: number) => {
    const row = shown[i]
    const id = row.node.agent.agentId
    const hasChildren = row.node.children.length > 0
    const isOpen = open.has(id)
    switch (e.key) {
      case 'ArrowDown':
        moveTo(shown[i + 1]?.node.agent.agentId)
        break
      case 'ArrowUp':
        moveTo(shown[i - 1]?.node.agent.agentId)
        break
      case 'Home':
        moveTo(shown[0]?.node.agent.agentId)
        break
      case 'End':
        moveTo(shown[shown.length - 1]?.node.agent.agentId)
        break
      case 'ArrowRight':
        if (hasChildren && !isOpen) toggle(id, true)
        else if (hasChildren) moveTo(row.node.children[0].agent.agentId)
        break
      case 'ArrowLeft':
        if (hasChildren && isOpen) toggle(id, false)
        else moveTo(row.parentId)
        break
      case 'Enter':
        openAgent(id)
        break
      default:
        return
    }
    e.preventDefault()
  }

  return (
    <section aria-labelledby={headingId} className="grid gap-2">
      <h2 id={headingId} className="text-sm font-semibold">
        {team.label} <span className="text-muted-foreground font-normal">· {team.agents.length === 1 ? '1 agent' : `${team.agents.length} agents`}</span>
      </h2>
      <ul role="tree" aria-labelledby={headingId} className="grid gap-1.5">
        {shown.map((row, i) => {
          const a = row.node.agent
          const hasChildren = row.node.children.length > 0
          const isOpen = open.has(a.agentId)
          return (
            <li
              key={a.agentId}
              ref={(el) => {
                if (el) refs.current.set(a.agentId, el)
                else refs.current.delete(a.agentId)
              }}
              role="treeitem"
              aria-level={row.level}
              aria-setsize={row.setsize}
              aria-posinset={row.posinset}
              aria-expanded={hasChildren ? isOpen : undefined}
              tabIndex={a.agentId === focusId ? 0 : -1}
              onKeyDown={(e) => onKeyDown(e, i)}
              onFocus={() => setFocused(a.agentId)}
              onClick={() => openAgent(a.agentId)}
              data-testid="chart-node"
              style={{ marginInlineStart: `${(row.level - 1) * 1.5}rem` }}
              className={cn(
                'bg-card hover:bg-accent focus-visible:ring-ring/50 flex max-w-xl cursor-pointer flex-wrap items-center gap-2 rounded-md border px-3 py-2 text-sm outline-none focus-visible:ring-[3px]',
                row.level > 1 && 'border-l-primary border-l-4',
              )}
            >
              {/* The chevron is for the mouse; the keyboard uses Right and Left, and aria-expanded says the state. */}
              <span
                aria-hidden
                className={cn('text-muted-foreground -m-1 rounded p-1', hasChildren ? 'hover:text-foreground' : 'invisible')}
                onClick={(e) => {
                  e.stopPropagation()
                  if (hasChildren) toggle(a.agentId, !isOpen)
                }}
              >
                {isOpen ? <ChevronDownIcon className="size-4" /> : <ChevronRightIcon className="size-4" />}
              </span>
              <AgentAvatar agentId={a.agentId} name={a.displayName} className="size-5 text-[0.625rem]" />
              <span className="font-medium">{a.displayName}</span>
              <AgentStatusBadge status={a.status} unhealthy={cardState(a) === 'unhealthy'} />
              {row.node.reportsToLabel && <span className="text-muted-foreground text-xs">{row.node.reportsToLabel}</span>}
              {hasChildren && !isOpen && (
                <span className="text-muted-foreground text-xs">
                  {row.node.children.length === 1 ? '1 direct report' : `${row.node.children.length} direct reports`}
                </span>
              )}
            </li>
          )
        })}
      </ul>
      {rows.length > shown.length && (
        <Button variant="outline" size="sm" className="w-fit" onClick={() => setLimit((l) => l + chartRowStep)}>
          Show more of {team.label} ({rows.length - shown.length} more)
        </Button>
      )}
    </section>
  )
}
