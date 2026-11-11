// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AuthService } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { AdminDisabledSchema, OutwardService, OutwardTargetKind, type SetOutwardTargetDisabledRequest } from '@/gen/koinon/outward/v1alpha1/outward_pb'
import { ProviderSchema, ProviderService } from '@/gen/koinon/provider/v1alpha1/provider_pb'
import { renderApp, whoAmI } from '@/test-utils'

const t = timestampFromMs(Date.now())

function setup(opts: { off?: boolean; declared?: boolean; held?: boolean } = {}) {
  const calls: SetOutwardTargetDisabledRequest[] = []
  let off = opts.off ?? false
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({}) })
    service(ProviderService, {
      getDefaultModel: () => ({ model: '', available: [] }),
      listModels: () => ({ models: [] }),
      listProviders: () => ({
        providers: [
          create(ProviderSchema, {
            id: 'p-1',
            name: 'openrouter',
            type: 'openrouter',
            url: 'https://openrouter.ai/api/v1',
            hasKey: true,
            source: 'ui',
            createdAt: t,
            updatedAt: t,
            adminDisabled: off ? create(AdminDisabledSchema, { at: t, by: opts.declared ? 'secret openrouter-secret' : 'user u-1', note: 'restored copy', declared: opts.declared ?? false }) : undefined,
          }),
        ],
      }),
    })
    service(OutwardService, {
      setOutwardTargetDisabled: (req) => {
        calls.push(req)
        if (!req.disabled && opts.held) throw new ConnectError('the install is held for a restore', Code.FailedPrecondition)
        off = req.disabled
        return { state: { kind: req.kind, targetId: req.targetId, name: 'openrouter', disabled: req.disabled } }
      },
    })
  })
  renderApp(transport, '/settings/providers')
  return calls
}

describe('OutwardSwitch on the providers page', () => {
  it('switches a provider off with a reason, then shows who did it', async () => {
    const calls = setup()
    const card = within(await screen.findByRole('region', { name: 'openrouter' }))
    await userEvent.type(card.getByLabelText('Reason for switching off provider openrouter'), 'restored copy')
    await userEvent.click(card.getByRole('button', { name: 'Switch off outward calls of provider openrouter' }))
    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0]).toMatchObject({ kind: OutwardTargetKind.MODEL_PROVIDER, targetId: 'p-1', disabled: true, note: 'restored copy' })
    expect(await card.findByText('Switched off by an admin')).toBeInTheDocument()
    expect(card.getByText(/by user u-1: restored copy/)).toBeInTheDocument()
  })

  it('shows the server refusal when enabling is refused', async () => {
    const calls = setup({ off: true, held: true })
    const card = within(await screen.findByRole('region', { name: 'openrouter' }))
    await userEvent.click(card.getByRole('button', { name: 'Enable outward calls of provider openrouter' }))
    expect(await card.findByRole('alert')).toHaveTextContent('the install is held for a restore')
    expect(calls[0]).toMatchObject({ disabled: false })
  })

  it('offers no enable for a target its Secret disables', async () => {
    setup({ off: true, declared: true })
    const card = within(await screen.findByRole('region', { name: 'openrouter' }))
    expect(await card.findByText(/remove that field from the Secret/)).toBeInTheDocument()
    expect(card.queryByRole('button', { name: /Enable outward calls/ })).not.toBeInTheDocument()
  })
})
