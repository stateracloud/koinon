// SPDX-License-Identifier: Apache-2.0

import { createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { OrgSettingsService } from '@/gen/koinon/orgsettings/v1alpha1/orgsettings_pb'
import { TraceDestinationService } from '@/gen/koinon/trace/v1alpha1/trace_pb'
import { renderApp, whoAmI } from '@/test-utils'

function setup() {
  let days = 30
  const sets: number[] = []
  const view = () => ({ retention: { days, defaultDays: 30, minDays: 1, maxDays: 365 } })
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: OrgRole.ADMIN }) })
    service(TraceDestinationService, { listDestinations: () => ({ destinations: [] }), getSettings: () => ({}) })
    service(OrgSettingsService, {
      getRunContentRetention: () => view(),
      getHomeBackupRetention: () => ({ retention: { days: 7, defaultDays: 7, minDays: 1, maxDays: 90 } }),
      setRunContentRetention: (req) => {
        sets.push(req.days)
        days = req.days || 30
        return view()
      },
    })
  })
  renderApp(transport, '/settings/observability')
  return sets
}

describe('RunContentRetentionSettings', () => {
  it('starts at the default with nothing to save', async () => {
    setup()
    expect(await screen.findByLabelText('Keep run content for (days)')).toHaveValue(30)
    expect(screen.getByRole('button', { name: 'Save retention' })).toBeDisabled()
  })

  it('saves a retention and returns to the default', async () => {
    const sets = setup()
    const input = await screen.findByLabelText('Keep run content for (days)')
    await userEvent.clear(input)
    await userEvent.type(input, '7')
    await userEvent.click(screen.getByRole('button', { name: 'Save retention' }))
    await waitFor(() => expect(sets).toEqual([7]))
    await userEvent.click(await screen.findByRole('button', { name: 'Use the default (30)' }))
    await waitFor(() => expect(sets).toEqual([7, 0]))
  })

  it('refuses a retention outside the bounds before sending it', async () => {
    const sets = setup()
    const input = await screen.findByLabelText('Keep run content for (days)')
    await userEvent.clear(input)
    await userEvent.type(input, '400')
    expect(screen.getByRole('alert')).toHaveTextContent('from 1 to 365')
    expect(screen.getByRole('button', { name: 'Save retention' })).toBeDisabled()
    expect(sets).toEqual([])
  })
})
