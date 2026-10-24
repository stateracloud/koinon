// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AuthService, OrgRole, WhoAmIResponseSchema } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import {
  AgentBudgetSchema,
  BudgetService,
  NotificationSchema,
  OrgBudgetSchema,
  type SetAgentBudgetRequest,
  type SetMonthlyCapRequest,
  type SetPlatformCeilingRequest,
} from '@/gen/koinon/budget/v1alpha1/budget_pb'
import { renderApp, whoAmI } from '@/test-utils'

const monthStart = new Date(Date.UTC(new Date().getUTCFullYear(), new Date().getUTCMonth(), 1))
const notice = (id: string, threshold: number, level = 'org_monthly', read = false) =>
  create(NotificationSchema, {
    id, kind: threshold >= 100 ? 'budget.reached' : 'budget.warning', level, threshold, limitUsd: '100.00', spentUsd: threshold >= 100 ? '100.00' : '80.00',
    windowStart: timestampFromMs(level === 'agent_daily' ? Date.UTC(new Date().getUTCFullYear(), new Date().getUTCMonth(), new Date().getUTCDate()) : monthStart.getTime()),
    createdAt: timestampFromMs(Date.now()), readAt: read ? timestampFromMs(Date.now()) : undefined, agentId: level === 'agent_daily' ? 'a-1' : '',
  })

function setup(opts: { role?: OrgRole; platformAdmin?: boolean; ceiling?: string; path?: string; notices?: ReturnType<typeof notice>[] } = {}) {
  const calls = { caps: [] as SetMonthlyCapRequest[], limits: [] as SetAgentBudgetRequest[], ceilings: [] as SetPlatformCeilingRequest[], read: [] as string[], listed: 0, noticeCalls: 0 }
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => create(WhoAmIResponseSchema, { ...whoAmI({ role: opts.role }), platformAdmin: !!opts.platformAdmin }) })
    service(BudgetService, {
      getOrgBudget: () => {
        calls.listed++
        return {
          budget: create(OrgBudgetSchema, { monthlyCapUsd: '100.00', spentThisMonthUsd: '40.00', estimatedThisMonthUsd: '10.00', platformCeilingUsd: opts.ceiling ?? '', monthStart: timestampFromMs(monthStart.getTime()) }),
        }
      },
      listAgentBudgets: () => ({
        budgets: [
          create(AgentBudgetSchema, { agentId: 'a-1', agentName: 'Repo helper', dailyLimitUsd: '5.00', templateDefaultUsd: '10.00', spentTodayUsd: '1.25' }),
          create(AgentBudgetSchema, { agentId: 'a-2', agentName: 'Docs bot', dailyLimitUsd: '10.00', templateDefaultUsd: '10.00', spentTodayUsd: '0.00' }),
        ],
      }),
      listNotifications: () => {
        calls.noticeCalls++
        return { notifications: opts.notices ?? [] }
      },
      setMonthlyCap: (req) => {
        calls.caps.push(req)
        return { budget: create(OrgBudgetSchema, { monthlyCapUsd: req.monthlyCapUsd }) }
      },
      setAgentBudget: (req) => {
        calls.limits.push(req)
        return { budget: create(AgentBudgetSchema, { agentId: req.agentId, dailyLimitUsd: req.dailyLimitUsd }) }
      },
      setPlatformCeiling: (req) => {
        calls.ceilings.push(req)
        return { budget: create(OrgBudgetSchema, { platformCeilingUsd: req.platformCeilingUsd }) }
      },
      markNotificationRead: (req) => {
        calls.read.push(req.notificationId)
        return {}
      },
    })
  })
  renderApp(transport, opts.path ?? '/settings/budgets')
  return calls
}

describe('Budgets page', () => {
  it('shows spent, remaining, the estimated share and the read-only platform ceiling', async () => {
    setup({ ceiling: '500.00' })
    const cap = await screen.findByRole('region', { name: 'Monthly cap' })
    expect(within(cap).getByText('$100.00')).toBeInTheDocument()
    expect(within(cap).getByText('$40.00')).toBeInTheDocument()
    expect(within(cap).getByText('$60.00')).toBeInTheDocument()
    expect(within(cap).getByText(/25% of the spend/)).toBeInTheDocument()
    expect(within(cap).getByText(/\$500.00. Platform admins set it/)).toBeInTheDocument()
    expect(within(cap).queryByLabelText(/ceiling/i)).not.toBeInTheDocument()
    expect(within(cap).getByLabelText('Share of the monthly cap spent')).toHaveAttribute('value', '40')
  })

  it('sets the cap, and refuses a bad amount before sending', async () => {
    const calls = setup()
    const cap = await screen.findByRole('region', { name: 'Monthly cap' })
    await userEvent.click(within(cap).getByRole('button', { name: 'Change cap' }))
    const input = within(cap).getByLabelText('Monthly cap in US dollars')
    await userEvent.clear(input)
    await userEvent.type(input, 'lots')
    await userEvent.click(within(cap).getByRole('button', { name: 'Save cap' }))
    expect(await within(cap).findByText(/must be a dollar amount/)).toBeInTheDocument()
    expect(calls.caps).toHaveLength(0)
    await userEvent.clear(input)
    await userEvent.type(input, '250')
    await userEvent.click(within(cap).getByRole('button', { name: 'Save cap' }))
    await waitFor(() => expect(calls.caps).toEqual([expect.objectContaining({ orgId: 'org-1', monthlyCapUsd: '250' })]))
  })

  it('lists the agents with their template default and spend, and changes one limit', async () => {
    const calls = setup()
    const list = await screen.findByRole('list', { name: 'Agent daily limits' })
    const row = within(list).getByRole('heading', { name: 'Repo helper' }).closest('li')!
    expect(within(row).getByText('Changed from the template')).toBeInTheDocument()
    expect(within(row).getByText('$1.25')).toBeInTheDocument()
    expect(within(list).getByRole('heading', { name: 'Docs bot' }).closest('li')).toHaveTextContent('Template default')
    await userEvent.click(within(row).getByRole('button', { name: 'Change daily limit of Repo helper' }))
    const input = within(row).getByLabelText('Daily limit for Repo helper, in US dollars')
    await userEvent.clear(input)
    await userEvent.type(input, '7.5')
    await userEvent.click(within(row).getByRole('button', { name: 'Save limit for Repo helper' }))
    await waitFor(() => expect(calls.limits).toEqual([expect.objectContaining({ agentId: 'a-1', dailyLimitUsd: '7.5' })]))
  })

  it('lists notifications and marks one read', async () => {
    const calls = setup({ notices: [notice('n-1', 80), notice('n-2', 100, 'agent_daily', true)] })
    const list = await screen.findByRole('list', { name: 'Notifications' })
    expect(within(list).getAllByRole('listitem')).toHaveLength(2)
    expect(within(list).getByText(/Repo helper’s daily limit has been reached/)).toBeInTheDocument()
    await userEvent.click(within(list).getByRole('button', { name: /Mark as read: The org’s monthly cap is at 80%/ }))
    await waitFor(() => expect(calls.read).toEqual(['n-1']))
  })

  it('shows members a notice, no admin links and sends no request', async () => {
    const calls = setup({ role: OrgRole.MEMBER })
    expect(await screen.findByRole('alert')).toHaveTextContent('Only org admins and owners can manage budgets.')
    expect(calls.listed).toBe(0)
    expect(calls.noticeCalls).toBe(0)
    expect(screen.queryByRole('region', { name: 'Budget notices' })).not.toBeInTheDocument()
  })
})

describe('Budget notices banner', () => {
  it('shows an unread 100% notice to admins on any page, and the 80% one replaced by it', async () => {
    setup({ path: '/', notices: [notice('n-1', 80), notice('n-2', 100)] })
    const banner = await screen.findByRole('region', { name: 'Budget notices' })
    expect(within(banner).getAllByRole('alert')).toHaveLength(1)
    expect(within(banner).getByText(/The org’s monthly cap has been reached/)).toBeInTheDocument()
    expect(within(banner).getByRole('link', { name: 'See budgets' })).toHaveAttribute('href', '/settings/budgets')
  })

  it('shows an 80% notice as a status, and nothing for read notices', async () => {
    setup({ path: '/', notices: [notice('n-1', 80)] })
    const banner = await screen.findByRole('region', { name: 'Budget notices' })
    expect(within(banner).getByRole('status')).toHaveTextContent('is at 80%')
  })

  it('shows nothing when everything is read', async () => {
    const calls = setup({ path: '/', notices: [notice('n-1', 100, 'org_monthly', true)] })
    await waitFor(() => expect(calls.noticeCalls).toBeGreaterThan(0))
    expect(screen.queryByRole('region', { name: 'Budget notices' })).not.toBeInTheDocument()
  })
})

describe('Platform ceiling', () => {
  it('is set by a platform admin in the platform area', async () => {
    const calls = setup({ platformAdmin: true, path: '/platform/budgets' })
    expect(await screen.findByRole('heading', { name: 'Platform budget ceiling' })).toBeInTheDocument()
    await userEvent.click(await screen.findByRole('button', { name: 'Set a ceiling' }))
    await userEvent.type(screen.getByLabelText('Ceiling in US dollars'), '1000')
    await userEvent.click(screen.getByRole('button', { name: 'Save ceiling' }))
    await waitFor(() => expect(calls.ceilings).toEqual([expect.objectContaining({ orgId: 'org-1', platformCeilingUsd: '1000' })]))
  })

  it('is refused to org admins who are not platform admins', async () => {
    const calls = setup({ path: '/platform/budgets' })
    expect(await screen.findByRole('alert')).toHaveTextContent('Only platform admins can set the platform ceiling.')
    expect(calls.listed).toBe(0)
    expect(screen.queryByRole('navigation', { name: 'Platform' })).not.toBeInTheDocument()
  })
})
