// SPDX-License-Identifier: Apache-2.0

import { createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { BudgetService } from '@/gen/koinon/budget/v1alpha1/budget_pb'
import { OrgService } from '@/gen/koinon/org/v1alpha1/org_pb'
import { OrgSettingsService, type SetRunLimitsRequest } from '@/gen/koinon/orgsettings/v1alpha1/orgsettings_pb'
import { ProviderService } from '@/gen/koinon/provider/v1alpha1/provider_pb'
import { renderApp, whoAmI } from '@/test-utils'

type Limits = { maxConcurrentRuns: number; triggerConcurrency: number; triggerRatePerMinute: number; sandboxCreationsPerMinute: number }
const defaults: Limits = { maxConcurrentRuns: 20, triggerConcurrency: 4, triggerRatePerMinute: 60, sandboxCreationsPerMinute: 10 }
const bounds: Record<keyof Limits, [number, number]> = { maxConcurrentRuns: [1, 10000], triggerConcurrency: [1, 1000], triggerRatePerMinute: [1, 100000], sandboxCreationsPerMinute: [1, 10000] }

function setup(opts: { role?: OrgRole; limits?: Partial<Limits>; memberPages?: { name: string; role: OrgRole }[][] } = {}) {
  const limits: Limits = { ...defaults, ...opts.limits }
  const calls = { runLimitSets: [] as SetRunLimitsRequest[], contSets: [] as string[], memberGets: 0, settingGets: 0 }
  let contSpend = ''
  const view = () => ({
    limits: Object.fromEntries(
      (Object.keys(defaults) as (keyof Limits)[]).map((k) => [k, { value: limits[k], defaultValue: defaults[k], min: bounds[k][0], max: bounds[k][1] }]),
    ),
  })
  const pages = opts.memberPages ?? [[{ name: 'Ada', role: OrgRole.OWNER }, { name: 'Grace', role: OrgRole.MEMBER }]]
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: opts.role ?? OrgRole.ADMIN }) })
    service(ProviderService, { getDefaultModel: () => ({ model: 'vendor/a-model', available: [{ name: 'vendor/a-model', provider: 'openrouter' }] }) })
    service(BudgetService, {
      getOrgContinuationLimits: () => ({ limits: { taskSpendUsd: contSpend || '20.00', platformDefaultTaskSpendUsd: '20.00', overridden: contSpend !== '' } }),
      setOrgContinuationTaskSpend: (req) => {
        calls.contSets.push(req.taskSpendUsd)
        contSpend = req.taskSpendUsd
        return { limits: { taskSpendUsd: contSpend || '20.00', platformDefaultTaskSpendUsd: '20.00', overridden: contSpend !== '' } }
      },
    })
    service(OrgSettingsService, {
      getRunContentRetention: () => {
        calls.settingGets++
        return { retention: { days: 30, defaultDays: 30, minDays: 1, maxDays: 365 } }
      },
      getHomeBackupRetention: () => {
        calls.settingGets++
        return { retention: { days: 7, defaultDays: 7, minDays: 1, maxDays: 90 } }
      },
      getApproverNotRequester: () => {
        calls.settingGets++
        return { on: false }
      },
      getRunLimits: () => {
        calls.settingGets++
        return view()
      },
      setRunLimits: (req) => {
        calls.runLimitSets.push(req)
        for (const k of Object.keys(defaults) as (keyof Limits)[]) {
          const v = req[k]
          if (v !== undefined) limits[k] = v === 0 ? defaults[k] : v
        }
        return view()
      },
    })
    service(OrgService, {
      listMembers: (req) => {
        calls.memberGets++
        const i = req.pageToken === '' ? 0 : Number(req.pageToken)
        return {
          members: pages[i].map((m) => ({ userId: `u-${m.name}`, displayName: m.name, role: m.role })),
          nextPageToken: i + 1 < pages.length ? String(i + 1) : '',
        }
      },
    })
  })
  renderApp(transport, '/settings/org')
  return calls
}

describe('OrgSettingsPage', () => {
  it('shows every org setting section in one page', async () => {
    setup()
    expect(await screen.findByRole('heading', { name: 'Org settings', level: 1 })).toBeInTheDocument()
    for (const name of ['Org default model', 'Approvals', 'Task spend cap for continuations', 'Run limits', 'Members']) {
      expect(await screen.findByRole('region', { name })).toBeInTheDocument()
    }
    expect(await screen.findByLabelText('Keep run content for (days)')).toHaveValue(30)
    expect(await screen.findByLabelText('Keep old home checkpoints for (days)')).toHaveValue(7)
  })

  it('shows a member a notice and sends no settings request', async () => {
    const calls = setup({ role: OrgRole.MEMBER })
    expect(await screen.findByRole('heading', { name: 'Org settings', level: 1 })).toBeInTheDocument()
    expect(screen.queryByRole('region', { name: 'Run limits' })).toBeNull()
    expect(screen.queryByRole('region', { name: 'Members' })).toBeNull()
    expect(calls.settingGets).toBe(0)
    expect(calls.memberGets).toBe(0)
  })

  describe('run limits', () => {
    it('shows each limit with its default and bounds, with nothing to save', async () => {
      setup({ limits: { maxConcurrentRuns: 12 } })
      const region = await screen.findByRole('region', { name: 'Run limits' })
      expect(await within(region).findByLabelText('Runs at the same time')).toHaveValue(12)
      expect(within(region).getByLabelText('Trigger items per minute')).toHaveValue(60)
      expect(within(region).getByText(/From 1 to 10000; the default is 20\./)).toBeInTheDocument()
      expect(within(region).getByRole('button', { name: 'Save run limits' })).toBeDisabled()
      expect(within(region).getByRole('button', { name: 'Use the defaults' })).toBeEnabled()
    })

    it('sends only the limits that changed', async () => {
      const calls = setup()
      const region = await screen.findByRole('region', { name: 'Run limits' })
      const runs = await within(region).findByLabelText('Runs at the same time')
      await userEvent.clear(runs)
      await userEvent.type(runs, '50')
      await userEvent.click(within(region).getByRole('button', { name: 'Save run limits' }))
      await waitFor(() => expect(calls.runLimitSets).toHaveLength(1))
      const sent = calls.runLimitSets[0]
      expect(sent.maxConcurrentRuns).toBe(50)
      expect(sent.triggerConcurrency).toBeUndefined()
      expect(sent.triggerRatePerMinute).toBeUndefined()
      expect(sent.sandboxCreationsPerMinute).toBeUndefined()
      expect(await within(region).findByText('Saved the run limits.')).toBeInTheDocument()
      expect(within(region).getByLabelText('Runs at the same time')).toHaveValue(50)
    })

    it('returns the changed limits to the defaults with 0', async () => {
      const calls = setup({ limits: { triggerConcurrency: 8 } })
      const region = await screen.findByRole('region', { name: 'Run limits' })
      await userEvent.click(await within(region).findByRole('button', { name: 'Use the defaults' }))
      await waitFor(() => expect(calls.runLimitSets).toHaveLength(1))
      expect(calls.runLimitSets[0].triggerConcurrency).toBe(0)
      expect(calls.runLimitSets[0].maxConcurrentRuns).toBeUndefined()
      await waitFor(() => expect(within(region).getByLabelText('Trigger items at the same time')).toHaveValue(4))
      expect(within(region).getByRole('button', { name: 'Use the defaults' })).toBeDisabled()
    })

    it('refuses a limit out of its bounds before it sends it', async () => {
      const calls = setup()
      const region = await screen.findByRole('region', { name: 'Run limits' })
      const conc = await within(region).findByLabelText('Trigger items at the same time')
      await userEvent.clear(conc)
      await userEvent.type(conc, '1001')
      expect(within(region).getByRole('alert')).toHaveTextContent('Enter a whole number from 1 to 1000.')
      expect(within(region).getByRole('button', { name: 'Save run limits' })).toBeDisabled()
      expect(calls.runLimitSets).toEqual([])
    })
  })

  it('lists the members with their roles and loads the next page', async () => {
    setup({ memberPages: [[{ name: 'Ada', role: OrgRole.OWNER }], [{ name: 'Grace', role: OrgRole.VIEWER }]] })
    const region = await screen.findByRole('region', { name: 'Members' })
    const table = await within(region).findByRole('table', { name: 'Members' })
    expect(within(table).getByRole('rowheader', { name: 'Ada' })).toBeInTheDocument()
    expect(within(table).getByText('Owner')).toBeInTheDocument()
    await userEvent.click(within(region).getByRole('button', { name: 'Show more members' }))
    expect(await within(table).findByRole('rowheader', { name: 'Grace' })).toBeInTheDocument()
    expect(within(table).getByText('Viewer')).toBeInTheDocument()
    expect(within(region).queryByRole('button', { name: 'Show more members' })).toBeNull()
  })

  it('sets and clears the org default task spend cap for continuations', async () => {
    const calls = setup()
    const cap = await screen.findByRole('region', { name: 'Task spend cap for continuations' })
    expect(await within(cap).findByText(/This is the platform default/)).toBeInTheDocument()
    await userEvent.click(within(cap).getByRole('button', { name: 'Change task spend cap' }))
    const input = within(cap).getByLabelText('Task spend cap in US dollars')
    await userEvent.type(input, 'lots')
    await userEvent.click(within(cap).getByRole('button', { name: 'Save task spend cap' }))
    expect(await within(cap).findByText(/must be a dollar amount/)).toBeInTheDocument()
    expect(calls.contSets).toHaveLength(0)
    await userEvent.clear(input)
    await userEvent.type(input, '35')
    await userEvent.click(within(cap).getByRole('button', { name: 'Save task spend cap' }))
    await waitFor(() => expect(calls.contSets).toEqual(['35']))
    expect(await within(cap).findByText(/The platform default is/)).toBeInTheDocument()
    await userEvent.click(within(cap).getByRole('button', { name: 'Change task spend cap' }))
    await userEvent.clear(within(cap).getByLabelText('Task spend cap in US dollars'))
    await userEvent.click(within(cap).getByRole('button', { name: 'Save task spend cap' }))
    await waitFor(() => expect(calls.contSets.at(-1)).toBe(''))
  })
})
