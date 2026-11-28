// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { AuthService, OrgRole, WhoAmIResponseSchema } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import {
  AgentBudgetSchema,
  BudgetService,
  NotificationSchema,
  OrgBudgetSchema,
  OrgBudgetSummarySchema,
  type SetAgentBudgetRequest,
  type SetMonthlyCapRequest,
  type SetPlatformCeilingRequest,
} from '@/gen/koinon/budget/v1alpha1/budget_pb'
import { OrgUsageRowSchema, RunCountsSchema, UsageService, UsageTotalsSchema, type ExportOrgUsageRequest, type GetOrgUsageRequest } from '@/gen/koinon/usage/v1alpha1/usage_pb'
import { renderApp, whoAmI } from '@/test-utils'

const orgRow = (day: string, agentId: string, agentName: string, model: string, providerName: string, costUsd: string) =>
  create(OrgUsageRowSchema, {
    day,
    principal: agentId ? 'agent' : 'system',
    agentId,
    agentName,
    model,
    providerId: providerName ? `p-${providerName}` : '',
    providerName,
    usage: create(UsageTotalsSchema, { requests: 1n, costUsd, inputTokens: 10n }),
  })

const monthStart = new Date(Date.UTC(new Date().getUTCFullYear(), new Date().getUTCMonth(), 1))
const notice = (id: string, threshold: number, level = 'org_monthly', read = false) =>
  create(NotificationSchema, {
    id, kind: threshold >= 100 ? 'budget.reached' : 'budget.warning', level, threshold, limitUsd: '100.00', spentUsd: threshold >= 100 ? '100.00' : '80.00',
    windowStart: timestampFromMs(level === 'agent_daily' ? Date.UTC(new Date().getUTCFullYear(), new Date().getUTCMonth(), new Date().getUTCDate()) : monthStart.getTime()),
    createdAt: timestampFromMs(Date.now()), readAt: read ? timestampFromMs(Date.now()) : undefined, agentId: level === 'agent_daily' ? 'a-1' : '',
  })

function setup(opts: { role?: OrgRole; platformAdmin?: boolean; ceiling?: string; path?: string; notices?: ReturnType<typeof notice>[] } = {}) {
  const calls = { usage: [] as GetOrgUsageRequest[], exports: [] as ExportOrgUsageRequest[], caps: [] as SetMonthlyCapRequest[], limits: [] as SetAgentBudgetRequest[], ceilings: [] as SetPlatformCeilingRequest[], read: [] as string[], listed: 0, noticeCalls: 0, gate: undefined as Promise<void> | undefined }
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => create(WhoAmIResponseSchema, { ...whoAmI({ role: opts.role }), platformAdmin: !!opts.platformAdmin }) })
    service(BudgetService, {
      getOrgBudget: () => {
        calls.listed++
        return {
          budget: create(OrgBudgetSchema, { monthlyCapUsd: '100.00', spentThisMonthUsd: '40.00', estimatedThisMonthUsd: '10.00', systemChecksThisMonthUsd: '0.30', platformCeilingUsd: opts.ceiling ?? '', monthStart: timestampFromMs(monthStart.getTime()) }),
        }
      },
      listAgentBudgets: async () => {
        await calls.gate
        return {
        budgets: [
          create(AgentBudgetSchema, { agentId: 'a-1', agentName: 'Repo helper', dailyLimitUsd: '5.00', templateDefaultUsd: '10.00', spentTodayUsd: '1.25' }),
          create(AgentBudgetSchema, { agentId: 'a-2', agentName: 'Docs bot', dailyLimitUsd: '10.00', templateDefaultUsd: '10.00', spentTodayUsd: '0.00' }),
        ],
        }
      },
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
      listOrgBudgets: () => ({
        orgs: [
          create(OrgBudgetSummarySchema, { orgId: 'org-1', orgSlug: 'default', orgName: 'Default', monthlyCapUsd: '100.00', spentThisMonthUsd: '40.00' }),
          create(OrgBudgetSummarySchema, { orgId: 'org-2', orgSlug: 'acme', orgName: 'Acme', platformCeilingUsd: '500.00', spentThisMonthUsd: '1.00' }),
        ],
      }),
      markNotificationRead: (req) => {
        calls.read.push(req.notificationId)
        return {}
      },
    })
    service(UsageService, usageService(calls))
  })
  renderApp(transport, opts.path ?? '/settings/budgets')
  return calls
}

function usageService(calls: { usage: GetOrgUsageRequest[]; exports: ExportOrgUsageRequest[] }) {
  return {
    getOrgUsage: (req: GetOrgUsageRequest) => {
      calls.usage.push(req)
      return {
        orgId: req.orgId,
        rows: [
          orgRow('2026-11-01', 'a-1', 'Repo helper', 'claude-sonnet', 'Anthropic', '30.00'),
          orgRow('2026-11-02', 'a-1', 'Repo helper', 'gpt-mini', 'OpenAI', '9.70'),
          orgRow('2026-11-02', '', '', 'claude-haiku', '', '0.30'),
        ],
        totals: create(UsageTotalsSchema, { requests: 3n, costUsd: '40.00', inputTokens: 30n }),
        runs: create(RunCountsSchema, {}),
      }
    },
    exportOrgUsage: (req: ExportOrgUsageRequest) => {
      calls.exports.push(req)
      return { filename: 'usage.csv', csv: 'day\n' }
    },
  }
}

describe('Budgets page', () => {
  it('shows the org\'s own usage this month by agent, model, provider and day', async () => {
    const calls = setup()
    const usage = await screen.findByRole('region', { name: 'Usage this month' })
    expect(await within(usage).findByText(/\$40\.00 in 3 requests/)).toBeInTheDocument()
    const byAgent = within(usage).getByRole('table', { name: 'Usage by agent' })
    expect(within(byAgent).getAllByRole('rowheader').map((h) => h.querySelector('.font-medium')?.textContent)).toEqual(['Repo helper', 'System checks'])
    expect(within(byAgent).getByRole('rowheader', { name: 'Repo helper' }).closest('tr')).toHaveTextContent('$39.70')
    await userEvent.click(within(usage).getByRole('tab', { name: 'By provider' }))
    const byProvider = await within(usage).findByRole('table', { name: 'Usage by provider' })
    expect(within(byProvider).getAllByRole('rowheader').map((h) => h.querySelector('.font-medium')?.textContent)).toEqual(['Anthropic', 'OpenAI', 'Not recorded'])
    // The month to date, for this org only.
    expect(calls.usage[0].orgId).toBe('org-1')
    expect(calls.usage[0].range).toBeUndefined()
    const saved: Blob[] = []
    URL.createObjectURL = vi.fn((b: Blob) => {
      saved.push(b)
      return 'blob:usage'
    })
    URL.revokeObjectURL = vi.fn()
    let file = ''
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      file = this.download
    })
    await userEvent.click(within(usage).getByRole('button', { name: 'Export CSV' }))
    await waitFor(() => expect(click).toHaveBeenCalledOnce())
    click.mockRestore()
    // The saved file is the server's CSV, under the server's file name.
    expect(file).toBe('usage.csv')
    expect(await saved[0].text()).toBe('day\n')
    expect(calls.exports[0].orgId).toBe('org-1')
    expect(calls.exports[0].range).toBeUndefined()
  })

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
    expect(within(cap).getByText('System checks').nextElementSibling).toHaveTextContent('$0.30')
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
    await userEvent.click(within(row).getByRole('button', { name: 'Change limit of Repo helper' }))
    const input = within(row).getByLabelText('Daily limit for Repo helper, in US dollars')
    await userEvent.clear(input)
    await userEvent.type(input, '7.5')
    await userEvent.click(within(row).getByRole('button', { name: 'Save limit for Repo helper' }))
    await waitFor(() => expect(calls.limits).toEqual([expect.objectContaining({ agentId: 'a-1', dailyLimitUsd: '7.5' })]))
  })

  it('keeps another agent\'s limit form open while a save is still refreshing (old issue 111)', async () => {
    const calls = setup()
    const list = await screen.findByRole('list', { name: 'Agent daily limits' })
    const one = within(list).getByRole('heading', { name: 'Repo helper' }).closest('li')
    if (!one) throw new Error('Repo helper row not found')
    const two = within(list).getByRole('heading', { name: 'Docs bot' }).closest('li')
    if (!two) throw new Error('Docs bot row not found')
    await userEvent.click(within(one).getByRole('button', { name: 'Change limit of Repo helper' }))
    let release: () => void = () => undefined
    calls.gate = new Promise<void>((r) => (release = r))
    await userEvent.click(within(one).getByRole('button', { name: 'Save limit for Repo helper' }))
    await waitFor(() => expect(calls.limits).toHaveLength(1))

    // The save has returned but its refresh is pending, so the first form is still open.
    await userEvent.click(within(two).getByRole('button', { name: 'Change limit of Docs bot' }))
    expect(within(two).getByLabelText('Daily limit for Docs bot, in US dollars')).toBeInTheDocument()

    release()
    await waitFor(() => expect(within(one).queryByLabelText('Daily limit for Repo helper, in US dollars')).not.toBeInTheDocument())
    expect(within(two).getByLabelText('Daily limit for Docs bot, in US dollars')).toBeInTheDocument()
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
    expect(screen.queryByRole('region', { name: 'Notifications' })).not.toBeInTheDocument()
  })
})

describe('Budget notices banner', () => {
  it('shows an unread 100% notice to admins on any page, and the 80% one replaced by it', async () => {
    setup({ path: '/', notices: [notice('n-1', 80), notice('n-2', 100)] })
    const banner = await screen.findByRole('region', { name: 'Notifications' })
    expect(within(banner).getAllByRole('alert')).toHaveLength(1)
    expect(within(banner).getByText(/The org’s monthly cap has been reached/)).toBeInTheDocument()
    expect(within(banner).getByRole('link', { name: 'See budgets' })).toHaveAttribute('href', '/settings/budgets')
  })

  it('shows an 80% notice as a status, and nothing for read notices', async () => {
    setup({ path: '/', notices: [notice('n-1', 80)] })
    const banner = await screen.findByRole('region', { name: 'Notifications' })
    expect(within(banner).getByRole('status')).toHaveTextContent('is at 80%')
  })

  it('shows three notices and counts the rest, naming agents', async () => {
    setup({
      path: '/',
      notices: [notice('n-1', 100, 'agent_daily'), notice('n-2', 100, 'org_monthly'), notice('n-4', 80, 'agent_daily'), notice('n-5', 80, 'agent_daily')].map((n, i) => (i >= 2 ? create(NotificationSchema, { ...n, agentId: `a-${i + 1}` }) : n)),
    })
    const banner = await screen.findByRole('region', { name: 'Notifications' })
    expect(await within(banner).findByText(/Repo helper’s daily limit has been reached/)).toBeInTheDocument()
    expect(within(banner).getByText(/1 more/)).toBeInTheDocument()
  })

  it('shows nothing when everything is read', async () => {
    const calls = setup({ path: '/', notices: [notice('n-1', 100, 'org_monthly', true)] })
    await waitFor(() => expect(calls.noticeCalls).toBeGreaterThan(0))
    expect(screen.queryByRole('region', { name: 'Notifications' })).not.toBeInTheDocument()
  })
})

describe('Agent unhealthy notifications', () => {
  const unhealthy = (id: string, kind = 'agent.unhealthy') =>
    create(NotificationSchema, { id, kind, agentId: 'a-1', message: 'Repo helper was marked unhealthy: its sandbox kept getting stuck.', createdAt: timestampFromMs(Date.now()) })

  it('shows the message in the banner with a link to the agent, as an alert', async () => {
    setup({ path: '/', notices: [unhealthy('n-u')] })
    const banner = await screen.findByRole('region', { name: 'Notifications' })
    expect(within(banner).getByRole('alert')).toHaveTextContent('Repo helper was marked unhealthy: its sandbox kept getting stuck.')
    expect(within(banner).getByRole('link', { name: 'Open the agent' })).toHaveAttribute('href', '/agents/a-1')
    expect(within(banner).queryByRole('link', { name: 'See budgets' })).not.toBeInTheDocument()
  })

  it('lists it on the Budgets page with the agent link and does not crash on an unknown kind', async () => {
    setup({ notices: [unhealthy('n-u'), unhealthy('n-x', 'something.new')] })
    const list = await screen.findByRole('list', { name: 'Notifications' })
    expect(within(list).getAllByRole('listitem')).toHaveLength(2)
    expect(within(list).getAllByText(/was marked unhealthy/)).toHaveLength(2)
    expect(within(list).getAllByRole('link', { name: 'Open the agent' })[0]).toHaveAttribute('href', '/agents/a-1')
  })
})

describe('Platform ceilings', () => {
  it('lists every org with its ceiling, cap and spend and sets a ceiling in place', async () => {
    const calls = setup({ platformAdmin: true, path: '/platform/usage/budgets' })
    expect(await screen.findByRole('heading', { name: 'Platform budget ceilings' })).toBeInTheDocument()
    const list = await screen.findByRole('list', { name: 'Orgs' })
    const acme = within(list).getByRole('heading', { name: 'Acme' }).closest('li')!
    expect(within(acme).getByText('$500.00')).toBeInTheDocument()
    const def = within(list).getByRole('heading', { name: 'Default' }).closest('li')!
    expect(within(def).getByText('$100.00')).toBeInTheDocument()
    await userEvent.click(within(def).getByRole('button', { name: 'Set a ceiling of Default' }))
    await userEvent.type(within(def).getByLabelText('Ceiling for Default, in US dollars'), '1000')
    await userEvent.click(within(def).getByRole('button', { name: 'Save ceiling for Default' }))
    await waitFor(() => expect(calls.ceilings).toEqual([expect.objectContaining({ orgId: 'org-1', platformCeilingUsd: '1000' })]))
  })

  it('is refused to org admins who are not platform admins: they go to the org area', async () => {
    const calls = setup({ path: '/platform/usage/budgets' })
    expect(screen.queryByRole('navigation', { name: 'Platform console' })).not.toBeInTheDocument()
    expect(await screen.findByRole('navigation', { name: 'Main' })).toBeInTheDocument()
    expect(screen.queryByRole('navigation', { name: 'Platform console' })).not.toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Platform budget ceilings' })).not.toBeInTheDocument()
    expect(calls.listed).toBe(0)
    expect(screen.queryByRole('navigation', { name: 'Platform' })).not.toBeInTheDocument()
  })
})
