// SPDX-License-Identifier: Apache-2.0

import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ProviderService, type SetDefaultModelRequest } from '@/gen/koinon/provider/v1alpha1/provider_pb'
import { renderApp, whoAmI } from '@/test-utils'

const available = [
  { name: 'vendor/a-model', provider: 'openrouter' },
  { name: 'vendor/b-model', provider: 'openrouter' },
]

function setup(opts: { model?: string; failSet?: string } = {}) {
  const sets: SetDefaultModelRequest[] = []
  let model = opts.model ?? ''
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: OrgRole.ADMIN }) })
    service(ProviderService, {
      listProviders: () => ({ providers: [] }),
      listModels: () => ({ models: [] }),
      getDefaultModel: () => ({ model, available }),
      setDefaultModel: (req) => {
        sets.push(req)
        if (opts.failSet) throw new ConnectError(opts.failSet, Code.InvalidArgument)
        model = req.model
        return { model }
      },
    })
  })
  renderApp(transport, '/settings/providers')
  return { sets }
}

describe('OrgDefaultModel', () => {
  it('lists the served, priced models with the current default selected', async () => {
    setup({ model: 'vendor/b-model' })
    const pick = await screen.findByLabelText('Model for agents that name none')
    expect(pick).toHaveValue('vendor/b-model')
    expect(screen.getByRole('option', { name: 'vendor/a-model (openrouter)' })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: 'No default model' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Save default model' })).toBeDisabled()
  })

  it('says so when the org has no default model, and saves a pick', async () => {
    const { sets } = setup()
    const pick = await screen.findByLabelText('Model for agents that name none')
    expect(screen.getByText(/The org has no default model/)).toBeInTheDocument()
    await userEvent.selectOptions(pick, 'vendor/a-model')
    await userEvent.click(screen.getByRole('button', { name: 'Save default model' }))
    await waitFor(() => expect(sets).toHaveLength(1))
    expect(sets[0]).toMatchObject({ model: 'vendor/a-model' })
    await waitFor(() => expect(screen.queryByText(/The org has no default model/)).not.toBeInTheDocument())
  })

  it('clears the default', async () => {
    const { sets } = setup({ model: 'vendor/a-model' })
    const pick = await screen.findByLabelText('Model for agents that name none')
    await userEvent.selectOptions(pick, 'No default model')
    await userEvent.click(screen.getByRole('button', { name: 'Save default model' }))
    await waitFor(() => expect(sets).toHaveLength(1))
    expect(sets[0].model).toBe('')
  })

  it('keeps a default that is no longer offered visible', async () => {
    setup({ model: 'vendor/unpriced' })
    const pick = await screen.findByLabelText('Model for agents that name none')
    expect(pick).toHaveValue('vendor/unpriced')
    expect(screen.getByRole('option', { name: 'vendor/unpriced (not served or not priced)' })).toBeInTheDocument()
  })

  it('shows the server reason when saving fails', async () => {
    setup({ failSet: 'the model "vendor/a-model" has no price' })
    const pick = await screen.findByLabelText('Model for agents that name none')
    await userEvent.selectOptions(pick, 'vendor/a-model')
    await userEvent.click(screen.getByRole('button', { name: 'Save default model' }))
    expect(await screen.findByText(/has no price/)).toBeInTheDocument()
  })
})
