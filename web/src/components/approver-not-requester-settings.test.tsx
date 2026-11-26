// SPDX-License-Identifier: Apache-2.0

import { createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { OrgSettingsService } from '@/gen/koinon/orgsettings/v1alpha1/orgsettings_pb'
import { renderApp, whoAmI } from '@/test-utils'

function setup(role: OrgRole) {
  let on = false
  const sets: boolean[] = []
  let gets = 0
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role }) })
    service(OrgSettingsService, {
      getApproverNotRequester: () => {
        gets++
        return { on }
      },
      setApproverNotRequester: (req) => {
        sets.push(req.on)
        on = req.on
        return { on }
      },
    })
  })
  renderApp(transport, '/settings/org')
  return { sets, gets: () => gets }
}

describe('ApproverNotRequesterSettings', () => {
  it('is off by default and an admin turns it on and off', async () => {
    const { sets } = setup(OrgRole.ADMIN)
    const box = await screen.findByRole('checkbox', { name: 'The approver must not be the requester' })
    expect(box).not.toBeChecked()
    expect(screen.getByText(/setting of your GitHub organization/)).toBeInTheDocument()
    await userEvent.click(box)
    await waitFor(() => expect(sets).toEqual([true]))
    await waitFor(() => expect(box).toBeChecked())
    await userEvent.click(box)
    await waitFor(() => expect(sets).toEqual([true, false]))
  })

  it('does not show the rule to a member and sends no request', async () => {
    const { gets } = setup(OrgRole.MEMBER)
    expect(await screen.findByRole('heading', { name: 'Org settings' })).toBeInTheDocument()
    expect(screen.queryByRole('checkbox', { name: 'The approver must not be the requester' })).toBeNull()
    expect(gets()).toBe(0)
  })
})
