// SPDX-License-Identifier: Apache-2.0

import { create, type MessageInitShape } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it } from 'vitest'
import { AgentService, AgentStatus, AgentSummarySchema, type AgentSummary } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { ApprovalService } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { RunState } from '@/gen/koinon/run/v1alpha1/run_pb'
import { TaskService, TaskState } from '@/gen/koinon/task/v1alpha1/task_pb'
import { agent, renderApp, task, whoAmI } from '@/test-utils'

// The org home of ADR 0126 section 4 (issue #1274): the "Waiting on you" strip, the agent cards by team, the Chart tab
// as an ARIA tree view, and the agent list moved from /agents to the Agents tab.

const toAgent = (agentId: string, displayName: string) => ({ target: { case: 'agentId' as const, value: agentId }, displayName })
const toUser = (userId: string, displayName: string) => ({ target: { case: 'userId' as const, value: userId }, displayName })

function summary(id: string, name: string, init: MessageInitShape<typeof AgentSummarySchema> = {}): AgentSummary {
  return create(AgentSummarySchema, { agentId: id, displayName: name, status: AgentStatus.ACTIVE, model: 'claude-sonnet-5-5', spentTodayUsd: '0', ...init })
}

type Opts = { role?: OrgRole; pages?: AgentSummary[][]; tasks?: ReturnType<typeof task>[]; asked?: string[] }

function transport({ role, pages = [[]], tasks = [], asked }: Opts = {}) {
  return createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role }) })
    service(AgentService, {
      listAgents: () => ({ agents: [agent('a-1', 'Repo helper')] }),
      getAgent: () => ({ agent: agent('a-1', 'Repo helper') }),
      listAgentSummaries: (req) => {
        asked?.push(req.pageToken)
        const i = req.pageToken === '' ? 0 : Number(req.pageToken.slice(1))
        return { agents: pages[i] ?? [], nextPageToken: i + 1 < pages.length ? `p${i + 1}` : '' }
      },
    })
    service(TaskService, { listTasks: () => ({ tasks }) })
    service(ApprovalService, { listApprovals: () => ({ approvals: [] }) })
  })
}

const card = async (name: string) => (await screen.findByRole('article', { name })) as HTMLElement

beforeEach(() => localStorage.clear())

describe('agent cards', () => {
  it('shows every field of an active agent', async () => {
    const now = Date.now()
    renderApp(
      transport({
        pages: [
          [
            summary('a-1', 'Repo helper', {
              team: 'Platform',
              reportsTo: toUser('u-2', 'Ada'),
              spentTodayUsd: '1.25',
              dailyLimitUsd: '5',
              lastRunState: RunState.SUCCEEDED,
              lastRunAt: timestampFromMs(now - 2 * 3_600_000),
              nextScheduledAt: timestampFromMs(now + 3 * 3_600_000 + 60_000),
              waitingOnYou: 2,
            }),
          ],
        ],
      }),
      '/',
    )
    const c = within(await card('Repo helper'))
    expect(c.getByRole('link', { name: 'Repo helper' })).toHaveAttribute('href', '/agents/a-1')
    expect(c.getByText('claude-sonnet-5-5')).toBeInTheDocument()
    expect(c.getByText('Active')).toBeInTheDocument()
    expect(c.getByText('Reports to Ada')).toBeInTheDocument()
    expect(c.getByText('$1.25 of $5.00 today')).toBeInTheDocument()
    expect(c.getByRole('meter', { name: 'Spend today of Repo helper' })).toHaveAttribute('aria-valuenow', '25')
    expect(c.getByText('Last run succeeded 2 hours ago')).toBeInTheDocument()
    expect(c.getByText('Next run in 3 hours')).toBeInTheDocument()
    expect(c.getByRole('link', { name: '2 waiting on you' })).toHaveAttribute('href', '/inbox')
    expect(c.queryByRole('link', { name: /Review and activate/ })).not.toBeInTheDocument()
  })

  it('shows running now, no daily limit and no next run', async () => {
    renderApp(transport({ pages: [[summary('a-1', 'Repo helper', { runningRuns: 1, lastRunState: RunState.RUNNING, spentTodayUsd: '0.5' })]] }), '/')
    const c = within(await card('Repo helper'))
    expect(c.getByText('Running now')).toBeInTheDocument()
    expect(c.getByText('$0.5 today, no daily limit')).toBeInTheDocument()
    expect(c.queryByRole('meter')).not.toBeInTheDocument()
    expect(c.queryByText(/Next run/)).not.toBeInTheDocument()
    expect(c.queryByText(/waiting on you/)).not.toBeInTheDocument()
  })

  it('gives a draft "Review and activate", which opens the agent page', async () => {
    const { history } = renderApp(transport({ pages: [[summary('a-1', 'Repo helper', { status: AgentStatus.DRAFT })]] }), '/')
    const c = within(await card('Repo helper'))
    expect(c.getByText('Draft')).toBeInTheDocument()
    expect(c.getByText('No runs yet')).toBeInTheDocument()
    await userEvent.click(c.getByRole('link', { name: 'Review and activate Repo helper' }))
    await waitFor(() => expect(history.location.pathname).toBe('/agents/a-1'))
  })

  it('shows an unhealthy agent as Unhealthy', async () => {
    renderApp(transport({ pages: [[summary('a-1', 'Repo helper', { unhealthyAt: timestampFromMs(Date.now()), lastRunState: RunState.FAILED })]] }), '/')
    const c = within(await card('Repo helper'))
    expect(c.getByText('Unhealthy')).toBeInTheDocument()
    expect(c.queryByText('Active')).not.toBeInTheDocument()
  })

  it('groups the cards by team, with "No team" last', async () => {
    renderApp(transport({ pages: [[summary('a-1', 'Repo helper', { team: 'Platform' }), summary('a-2', 'Docs bot', { team: 'Platform' }), summary('a-3', 'Loner')]] }), '/')
    const platform = await screen.findByRole('region', { name: 'Platform · 2 agents' })
    expect(within(platform).getAllByRole('article').map((a) => a.getAttribute('aria-labelledby'))).toEqual(['agent-card-a-1', 'agent-card-a-2'])
    expect(within(screen.getByRole('region', { name: 'No team · 1 agent' })).getByRole('article', { name: 'Loner' })).toBeInTheDocument()
  })

  it('loads the next page only on "Show more agents"', async () => {
    const asked: string[] = []
    renderApp(transport({ pages: [[summary('a-1', 'First page bot')], [summary('a-2', 'Second page bot')]], asked }), '/')
    await card('First page bot')
    expect(asked).toEqual([''])
    // While pages remain, the last group can be partial.
    expect(screen.getByRole('region', { name: 'No team · 1 agent so far' })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Show more agents' }))
    await card('Second page bot')
    expect(asked).toEqual(['', 'p1'])
    expect(screen.queryByRole('button', { name: 'Show more agents' })).not.toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'No team · 2 agents' })).toBeInTheDocument()
  })

  it.each([
    [OrgRole.ADMIN, 'Create an agent to give it work.', true],
    [OrgRole.VIEWER, 'An org admin creates the agents of this org.', false],
  ])('shows the empty state for role %s', async (role, text, canCreate) => {
    renderApp(transport({ role }), '/')
    expect(await screen.findByText('No agents yet.')).toBeInTheDocument()
    expect(screen.getByText(text)).toBeInTheDocument()
    expect(screen.queryAllByRole('link', { name: 'New agent' }).length > 0).toBe(canCreate)
  })
})

describe('Waiting on you', () => {
  it('shows at most five items and the way to the inbox', async () => {
    const tasks = Array.from({ length: 7 }, (_, i) => task(`t-${i}`, `Task ${i}`, TaskState.INPUT_REQUIRED))
    renderApp(transport({ tasks }), '/')
    const strip = await screen.findByRole('region', { name: 'Waiting on you' })
    expect(await within(strip).findByText('7 items wait on you.')).toBeInTheDocument()
    expect(within(strip).getAllByRole('listitem')).toHaveLength(5)
    expect(within(strip).getAllByRole('link', { name: /needs your reply$/ })[0]).toHaveAttribute('href', expect.stringMatching(/^\/tasks\/t-/))
    expect(within(strip).getByRole('link', { name: 'Open inbox' })).toHaveAttribute('href', '/inbox')
  })

  it('says when nothing waits', async () => {
    renderApp(transport(), '/')
    const strip = await screen.findByRole('region', { name: 'Waiting on you' })
    expect(await within(strip).findByText('Nothing waits on you.')).toBeInTheDocument()
    expect(within(strip).queryByRole('list')).not.toBeInTheDocument()
  })
})

describe('Chart tab', () => {
  const team = [
    summary('lead', 'Lead bot', { team: 'Platform', reportsTo: toUser('u-2', 'Ada') }),
    summary('dev', 'Dev bot', { team: 'Platform', reportsTo: toAgent('lead', 'Lead bot') }),
    summary('qa', 'QA bot', { team: 'Platform', reportsTo: toAgent('lead', 'Lead bot') }),
  ]

  it('loads every page, then shows one tree per team with people as labels only', async () => {
    const asked: string[] = []
    renderApp(transport({ pages: [team.slice(0, 2), [team[2], summary('solo', 'Solo bot')]], asked }), '/?tab=chart')
    const tree = await screen.findByRole('tree', { name: 'Platform · 3 agents' })
    expect(asked).toEqual(['', 'p1'])
    const items = within(tree).getAllByRole('treeitem')
    expect(items.map((i) => [i.textContent, i.getAttribute('aria-level'), i.getAttribute('aria-posinset'), i.getAttribute('aria-setsize')])).toEqual([
      ['Lead botActiveReports to Ada', '1', '1', '1'],
      ['Dev botActive', '2', '1', '2'],
      ['QA botActive', '2', '2', '2'],
    ])
    expect(items[0]).toHaveAttribute('aria-expanded', 'true')
    expect(items[1]).not.toHaveAttribute('aria-expanded')
    // A person is never a box.
    expect(within(tree).queryByRole('treeitem', { name: /^Ada/ })).not.toBeInTheDocument()
    expect(screen.getByRole('tree', { name: 'No team · 1 agent' })).toBeInTheDocument()
  })

  it('moves, closes and opens with the keyboard, and Enter opens the agent', async () => {
    const { history } = renderApp(transport({ pages: [team] }), '/?tab=chart')
    const tree = await screen.findByRole('tree', { name: 'Platform · 3 agents' })
    const item = (name: RegExp) => within(tree).getByRole('treeitem', { name })
    // One tab stop per tree: the first row.
    expect(within(tree).getAllByRole('treeitem').map((i) => i.tabIndex)).toEqual([0, -1, -1])
    item(/^Lead bot/).focus()
    await userEvent.keyboard('{ArrowDown}')
    expect(item(/^Dev bot/)).toHaveFocus()
    await userEvent.keyboard('{End}')
    expect(item(/^QA bot/)).toHaveFocus()
    await userEvent.keyboard('{ArrowLeft}')
    expect(item(/^Lead bot/)).toHaveFocus()
    await userEvent.keyboard('{ArrowLeft}')
    expect(item(/^Lead bot/)).toHaveAttribute('aria-expanded', 'false')
    expect(within(tree).getAllByRole('treeitem')).toHaveLength(1)
    expect(item(/^Lead bot/)).toHaveTextContent('2 direct reports')
    await userEvent.keyboard('{ArrowRight}')
    expect(within(tree).getAllByRole('treeitem')).toHaveLength(3)
    await userEvent.keyboard('{ArrowRight}')
    expect(item(/^Dev bot/)).toHaveFocus()
    await userEvent.keyboard('{Enter}')
    await waitFor(() => expect(history.location.pathname).toBe('/agents/dev'))
  })

  it('keeps the chart, its open branches and the focus when a later poll finds one more page', async () => {
    const pages = [team]
    const { queryClient } = renderApp(transport({ pages }), '/?tab=chart')
    const tree = await screen.findByRole('tree', { name: 'Platform · 3 agents' })
    within(tree).getByRole('treeitem', { name: /^Dev bot/ }).focus()
    pages.push([summary('new', 'New bot', { team: 'Web' })])
    await queryClient.refetchQueries()
    expect(await screen.findByRole('tree', { name: 'Web · 1 agent' })).toBeInTheDocument()
    expect(within(tree).getByRole('treeitem', { name: /^Dev bot/ })).toHaveFocus()
    expect(within(tree).getAllByRole('treeitem')).toHaveLength(3)
  })

  it('moves the focus to the branch row when a mouse click closes the branch', async () => {
    renderApp(transport({ pages: [team] }), '/?tab=chart')
    const tree = await screen.findByRole('tree', { name: 'Platform · 3 agents' })
    within(tree).getByRole('treeitem', { name: /^Dev bot/ }).focus()
    const lead = within(tree).getByRole('treeitem', { name: /^Lead bot/ })
    await userEvent.click(lead.querySelector('[aria-hidden]')!)
    expect(lead).toHaveAttribute('aria-expanded', 'false')
    expect(lead).toHaveFocus()
    expect(lead.tabIndex).toBe(0)
  })

  it('shows more rows when Right moves to a child past the row limit', async () => {
    // 199 roots, then a root with one child: that child is row 201, past the first 200.
    const big = [
      ...Array.from({ length: 199 }, (_, i) => summary(`r-${i}`, `Root ${i}`, { team: 'Big' })),
      summary('last', 'Last root', { team: 'Big' }),
      summary('kid', 'Kid bot', { team: 'Big', reportsTo: toAgent('last', 'Last root') }),
    ]
    renderApp(transport({ pages: [big] }), '/?tab=chart')
    const tree = await screen.findByRole('tree', { name: 'Big · 201 agents' })
    within(tree).getAllByRole('treeitem')[0].focus()
    await userEvent.keyboard('{End}')
    expect(within(tree).getByRole('treeitem', { name: /^Last root/ })).toHaveFocus()
    await userEvent.keyboard('{ArrowRight}')
    expect(screen.getByRole('button', { name: 'Show more of Big (1 more)' })).toBeInTheDocument()
    await userEvent.keyboard('{ArrowRight}')
    await waitFor(() => expect(within(tree).getByRole('treeitem', { name: /^Kid bot/ })).toHaveFocus())
  })

  it('starts a large team with only its roots open', async () => {
    const many = [summary('lead', 'Lead bot', { team: 'Big' }), ...Array.from({ length: 60 }, (_, i) => summary(`w-${i}`, `Worker ${i}`, { team: 'Big', reportsTo: toAgent('lead', 'Lead bot') }))]
    renderApp(transport({ pages: [many] }), '/?tab=chart')
    const tree = await screen.findByRole('tree', { name: 'Big · 61 agents' })
    expect(within(tree).getAllByRole('treeitem')).toHaveLength(1)
    expect(within(tree).getByRole('treeitem')).toHaveAttribute('aria-expanded', 'false')
  })
})

describe('agent list URL and sidebar', () => {
  it('sends /agents to the Agents tab of the overview', async () => {
    const { history } = renderApp(transport({ pages: [[summary('a-1', 'Repo helper')]] }), '/agents')
    await waitFor(() => expect(history.location.pathname).toBe('/'))
    expect(await screen.findByRole('tab', { name: 'Agents', selected: true })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Overview', current: 'page' })).toBeInTheDocument()
  })

  it('marks Overview as the current section on an agent page and on the new-agent flow', async () => {
    for (const path of ['/agents/a-1', '/agents/new']) {
      const { unmount } = renderApp(transport(), path)
      const nav = await screen.findByRole('navigation', { name: 'Main' })
      await waitFor(() => expect(within(nav).getByRole('link', { name: 'Overview' })).toHaveAttribute('aria-current', 'true'))
      expect(within(nav).queryByRole('link', { current: 'page' })).not.toBeInTheDocument()
      unmount()
    }
  })
})
