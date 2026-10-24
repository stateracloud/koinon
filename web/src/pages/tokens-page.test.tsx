// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { TokenSchema, TokenService, type CreateTokenRequest, type RevokeTokenRequest } from '@/gen/koinon/token/v1alpha1/token_pb'
import { renderApp, whoAmI } from '@/test-utils'

const day = 86_400_000
const now = Date.now()
const root = create(TokenSchema, { id: 't-root', name: 'root', prefix: 'rootpref', role: OrgRole.OWNER, createdAt: timestampFromMs(now - day), expiresAt: timestampFromMs(now + 29 * day), lastUsedAt: timestampFromMs(now - 1000), createdBy: 'u-1', createdByName: 'Maria' })
const child = create(TokenSchema, { id: 't-child', name: 'pipeline', prefix: 'childpre', role: OrgRole.ADMIN, createdAt: timestampFromMs(now - day), expiresAt: timestampFromMs(now + 10 * day), createdBy: 'u-1', createdByName: 'Maria', parentTokenId: 't-root' })
const dead = create(TokenSchema, { id: 't-dead', name: 'old', prefix: 'deadpref', role: OrgRole.ADMIN, createdAt: timestampFromMs(now - 40 * day), expiresAt: timestampFromMs(now - 10 * day), revokedAt: timestampFromMs(now - 20 * day), createdByName: 'Maria' })

function setup(role = OrgRole.OWNER) {
  const creates: CreateTokenRequest[] = []
  const revokes: RevokeTokenRequest[] = []
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role }) })
    service(TokenService, {
      listTokens: () => ({ tokens: [root, child, dead] }),
      createToken: (req) => {
        creates.push(req)
        return { token: create(TokenSchema, { id: 't-new', name: req.name, prefix: 'newpref', role: req.role }), secret: 'koinon_newpref_SECRETVALUE' }
      },
      revokeToken: (req) => {
        revokes.push(req)
        return { token: root }
      },
    })
  })
  renderApp(transport, '/settings/tokens')
  return { creates, revokes }
}

describe('API tokens page', () => {
  it('lists prefix, role, dates, last use and the creation chain', async () => {
    setup()
    const table = await screen.findByRole('table', { name: 'API tokens' })
    const rowOf = (name: string) => within(table).getByRole('rowheader', { name: new RegExp(`^${name}`) }).closest('tr')!
    const r = within(rowOf('root'))
    expect(r.getByText('rootpref')).toBeInTheDocument()
    expect(r.getByText('Owner')).toBeInTheDocument()
    expect(r.getByText('Maria, in the UI')).toBeInTheDocument()
    const c = within(rowOf('pipeline'))
    expect(c.getByText('Maria, through token root (rootpref)')).toBeInTheDocument()
    expect(c.getByText('Never')).toBeInTheDocument()
    expect(within(rowOf('old')).getByText('Revoked')).toBeInTheDocument()
    expect(within(rowOf('old')).queryByRole('button', { name: /Revoke/ })).not.toBeInTheDocument()
  })

  it('creates a token with the default 30 day expiry and shows the secret once', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    const { creates } = setup()
    const form = await screen.findByRole('form', { name: 'Create API token' })
    expect(within(form).getByLabelText('Expires in (days)')).toHaveValue(30)
    await userEvent.type(within(form).getByLabelText('Name'), 'CI')
    await userEvent.click(within(form).getByRole('button', { name: 'Create token' }))
    await waitFor(() => expect(creates).toHaveLength(1))
    expect(creates[0]).toMatchObject({ orgId: 'org-1', name: 'CI', role: OrgRole.ADMIN })
    expect(creates[0].expiresIn?.seconds).toBe(30n * 86400n)

    const shown = await screen.findByRole('region', { name: 'New API token' })
    expect(within(shown).getByTestId('token-secret')).toHaveTextContent('koinon_newpref_SECRETVALUE')
    expect(within(shown).getByText(/You won.t see it again/)).toBeInTheDocument()
    await userEvent.click(within(shown).getByRole('button', { name: 'Copy token' }))
    expect(writeText).toHaveBeenCalledWith('koinon_newpref_SECRETVALUE')
    expect(await within(shown).findByText('Copied to the clipboard.')).toBeInTheDocument()

    await userEvent.click(within(shown).getByRole('button', { name: 'I have copied it' }))
    expect(screen.queryByText(/SECRETVALUE/)).not.toBeInTheDocument()
    expect(screen.getByRole('form', { name: 'Create API token' })).toBeInTheDocument()
  })

  it('refuses an expiry over 90 days before sending', async () => {
    const { creates } = setup()
    const form = await screen.findByRole('form', { name: 'Create API token' })
    await userEvent.type(within(form).getByLabelText('Name'), 'CI')
    await userEvent.clear(within(form).getByLabelText('Expires in (days)'))
    await userEvent.type(within(form).getByLabelText('Expires in (days)'), '91')
    await userEvent.click(within(form).getByRole('button', { name: 'Create token' }))
    expect(within(form).getByText(/from 1 to 90/)).toBeInTheDocument()
    expect(creates).toHaveLength(0)
  })

  it('caps the role at the user\'s own: an admin cannot pick owner', async () => {
    setup(OrgRole.ADMIN)
    const form = await screen.findByRole('form', { name: 'Create API token' })
    const options = within(within(form).getByLabelText('Role')).getAllByRole('option').map((o) => o.textContent)
    expect(options).toEqual(['Admin'])
  })

  it('offers owner to an owner', async () => {
    setup(OrgRole.OWNER)
    const form = await screen.findByRole('form', { name: 'Create API token' })
    expect(within(within(form).getByLabelText('Role')).getAllByRole('option').map((o) => o.textContent)).toEqual(['Admin', 'Owner'])
  })

  it('says revoking cascades, then revokes', async () => {
    const { revokes } = setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Revoke token root' }))
    expect(screen.getByText(/This also revokes every token it created/)).toBeInTheDocument()
    expect(revokes).toHaveLength(0)
    await userEvent.click(screen.getByRole('button', { name: 'Revoke token' }))
    await waitFor(() => expect(revokes).toEqual([expect.objectContaining({ tokenId: 't-root' })]))
  })

  it('is hidden from members', async () => {
    setup(OrgRole.MEMBER)
    expect(await screen.findByRole('alert')).toHaveTextContent('Only org admins and owners can manage API tokens.')
    expect(screen.queryByRole('link', { name: 'API tokens' })).not.toBeInTheDocument()
  })
})
