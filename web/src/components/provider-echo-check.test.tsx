// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { EchoCheckSchema, EchoDiffSegment_Op, ModelSchema, ProviderSchema, ProviderService, type CheckProviderEchoRequest, type EchoCheck } from '@/gen/koinon/provider/v1alpha1/provider_pb'
import { renderApp, whoAmI } from '@/test-utils'

const line = 'Maria Schneider, Hauptstrasse 12'
const changed = (): EchoCheck =>
  create(EchoCheckSchema, {
    model: 'vendor/a-model',
    unchanged: false,
    sent: line,
    received: '[NAME], Hauptstrasse 12',
    checkedAt: timestampFromMs(Date.UTC(2026, 9, 28, 12, 0)),
    diff: [
      { op: EchoDiffSegment_Op.DELETED, text: 'Maria Schneider' },
      { op: EchoDiffSegment_Op.INSERTED, text: '[NAME]' },
      { op: EchoDiffSegment_Op.EQUAL, text: ', Hauptstrasse 12' },
    ],
  })

function setup(opts: { last?: EchoCheck; result?: EchoCheck; fail?: string; noModels?: boolean } = {}) {
  const checks: CheckProviderEchoRequest[] = []
  let stored = opts.last
  const provider = () => create(ProviderSchema, { id: 'p-1', name: 'openrouter', type: 'openrouter', url: 'https://openrouter.ai/api/v1', hasKey: true, source: 'ui', echoCheck: stored })
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: OrgRole.ADMIN }) })
    service(ProviderService, {
      listProviders: () => ({ providers: [provider()] }),
      listModels: () => ({
        models: opts.noModels ? [] : [create(ModelSchema, { id: 'm-1', providerId: 'p-1', name: 'vendor/a-model', source: 'ui', format: 'openai' }), create(ModelSchema, { id: 'm-2', providerId: 'p-1', name: 'vendor/b-model', source: 'ui', format: 'openai' })],
      }),
      checkProviderEcho: (req) => {
        checks.push(req)
        if (opts.fail) throw new ConnectError(opts.fail, Code.Unavailable)
        stored = opts.result
        return { echoCheck: opts.result }
      },
    })
  })
  renderApp(transport, '/settings/providers')
  return { checks }
}

describe('ProviderEchoCheck', () => {
  it('explains what the check is and why masking matters, and runs nothing by itself', async () => {
    const { checks } = setup()
    const group = await screen.findByRole('group', { name: 'Echo check of openrouter' })
    expect(group).toHaveTextContent('Masking corrupts agent work')
    expect(group).toHaveTextContent('billed like any other')
    expect(group).toHaveTextContent('It never runs on its own')
    expect(within(group).queryByRole('status')).not.toBeInTheDocument()
    expect(checks).toHaveLength(0)
  })

  it('runs the check on the chosen model and shows an unchanged result with its time', async () => {
    const { checks } = setup({ result: create(EchoCheckSchema, { model: 'vendor/b-model', unchanged: true, sent: line, received: line, checkedAt: timestampFromMs(Date.UTC(2026, 9, 28, 12, 0)), diff: [{ op: EchoDiffSegment_Op.EQUAL, text: line }] }) })
    await userEvent.selectOptions(await screen.findByLabelText('Model to check'), 'vendor/b-model')
    await userEvent.click(screen.getByRole('button', { name: 'Run echo check on openrouter' }))
    await waitFor(() => expect(checks).toHaveLength(1))
    expect(checks[0]).toMatchObject({ providerId: 'p-1', model: 'vendor/b-model' })
    const result = await screen.findByRole('status', { name: 'Latest echo check' })
    expect(result).toHaveTextContent('Unchanged')
    expect(result).toHaveTextContent('vendor/b-model')
    expect(result).toHaveTextContent('character for character')
  })

  it('shows a changed result with the removed and the inserted text marked', async () => {
    setup({ last: changed() })
    const result = await screen.findByRole('status', { name: 'Latest echo check' })
    expect(result).toHaveTextContent('Changed')
    expect(result).toHaveTextContent('turn off the masking')
    expect(result.querySelector('del')).toHaveTextContent('Maria Schneider')
    expect(result.querySelector('ins')).toHaveTextContent('[NAME]')
  })

  it('says why the check did not run', async () => {
    setup({ fail: 'the model did not answer the check: the gateway answered 404' })
    await userEvent.click(await screen.findByRole('button', { name: 'Run echo check on openrouter' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('The check did not run: the model did not answer the check')
  })

  it('asks for a model first when the provider has none', async () => {
    setup({ noModels: true })
    expect(await screen.findByText('Add a model with a price to check this provider.')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Run echo check/ })).not.toBeInTheDocument()
  })
})
