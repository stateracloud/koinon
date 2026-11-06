// SPDX-License-Identifier: Apache-2.0

import { createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { OrgSettingsService } from '@/gen/koinon/orgsettings/v1alpha1/orgsettings_pb'
import { TraceDestinationService } from '@/gen/koinon/trace/v1alpha1/trace_pb'
import { renderApp, whoAmI } from '@/test-utils'

function setup() {
  let days = 7
  const sets: number[] = []
  const view = () => ({ retention: { days, defaultDays: 7, minDays: 1, maxDays: 90 } })
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: OrgRole.ADMIN }) })
    service(TraceDestinationService, { listDestinations: () => ({ destinations: [] }), getSettings: () => ({}) })
    service(OrgSettingsService, {
      getRunContentRetention: () => ({ retention: { days: 30, defaultDays: 30, minDays: 1, maxDays: 365 } }),
      getHomeBackupRetention: () => view(),
      setHomeBackupRetention: (req) => {
        sets.push(req.days)
        days = req.days || 7
        return view()
      },
    })
  })
  renderApp(transport, '/settings/observability')
  return sets
}

describe('HomeBackupRetentionSettings', () => {
  it('starts at the default with nothing to save', async () => {
    setup()
    expect(await screen.findByLabelText('Keep old home backups for (days)')).toHaveValue(7)
    expect(screen.getByRole('button', { name: 'Save backup retention' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Use the default (7)' })).toBeDisabled()
  })

  it('saves a retention and returns to the default', async () => {
    const sets = setup()
    const input = await screen.findByLabelText('Keep old home backups for (days)')
    await userEvent.clear(input)
    await userEvent.type(input, '30')
    await userEvent.click(screen.getByRole('button', { name: 'Save backup retention' }))
    await waitFor(() => expect(sets).toEqual([30]))
    await userEvent.click(await screen.findByRole('button', { name: 'Use the default (7)' }))
    await waitFor(() => expect(sets).toEqual([30, 0]))
  })

  it('refuses a retention outside 1 to 90 days before sending it', async () => {
    const sets = setup()
    const input = await screen.findByLabelText('Keep old home backups for (days)')
    await userEvent.clear(input)
    await userEvent.type(input, '91')
    const form = screen.getByRole('form', { name: 'Home backup retention' })
    expect(within(form).getByRole('alert')).toHaveTextContent('from 1 to 90')
    expect(screen.getByRole('button', { name: 'Save backup retention' })).toBeDisabled()
    expect(sets).toEqual([])
  })
})
