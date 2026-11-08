// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import {
  FlaggedPriceSchema,
  PriceChangeSchema,
  PriceEntrySchema,
  PriceService,
  PriceVersionSchema,
  type RetirePriceRequest,
  type SetPriceRequest,
} from '@/gen/koinon/budget/v1alpha1/price_pb'
import { renderApp, whoAmI } from '@/test-utils'

const day = 86_400_000
const mine = create(PriceVersionSchema, {
  id: 'pv-1', name: 'sonnet-mine', provider: 'openrouter', match: { glob: 'anthropic/claude-sonnet-*' }, rates: { input: '3', output: '15' },
  tokenizer: 'bytes', ratio: '3.0', maxOutput: 64000, validFrom: timestampFromMs(Date.now() - day),
})
const migrated = create(PriceVersionSchema, {
  id: 'pv-2', name: 'model-custom-abc', provider: 'openai', match: { regex: '(?-i:acme/custom)' }, rates: { input: '1', output: '2' },
  tokenizer: 'bytes', ratio: '3.0', maxOutput: 131072, validFrom: timestampFromMs(Date.now() - day), needsReview: true,
})
const gpt = create(PriceEntrySchema, {
  id: 'openai-gpt', provider: 'openai', match: { glob: 'gpt-*' }, validFrom: '2026-10-01', rates: { input: '2', output: '8' }, tokenizer: 'bytes', ratio: '3.0',
  sourceUrl: 'https://openai.com/api/pricing/', sourceAccessed: '2026-10-20', maxOutput: 32000,
})
const bad = create(PriceEntrySchema, { id: 'bad-link', provider: '*', match: { glob: 'x' }, validFrom: '2026-10-01', rates: { input: '1', output: '1' }, sourceUrl: 'javascript:alert(1)' })

function setup(role = OrgRole.OWNER, warnings: string[] = []) {
  const calls = { sets: [] as SetPriceRequest[], retires: [] as RetirePriceRequest[], listed: 0, flaggedSince: [] as bigint[], gate: undefined as Promise<void> | undefined }
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role }) })
    service(PriceService, {
      listPrices: async () => {
        calls.listed++
        await calls.gate
        return { prices: [mine, migrated] }
      },
      getDefaultPrices: () => ({ version: '2026-10-24.1', entries: [gpt, bad] }),
      getDefaultPriceChanges: () => ({
        fromVersion: '2026-10-23.1',
        toVersion: '2026-10-24.1',
        changes: [create(PriceChangeSchema, { kind: 'changed', id: 'openai-gpt', fields: ['rates.input'] })],
      }),
      listFlaggedPrices: (req) => {
        calls.flaggedSince.push(req.since?.seconds ?? 0n)
        return {
          flagged: [
            create(FlaggedPriceSchema, { model: 'openai/gpt-6', priceRef: 'default@2026-10-24.1#openai-gpt', requests: 4n, gatewayDiff: 4n, costUsd: '0.42', gatewayCostUsd: '0.50', lastSeen: timestampFromMs(Date.now()) }),
            create(FlaggedPriceSchema, { model: 'local/q', requests: 1n, usageMissing: 1n, maxOutputCapped: 1n, lastSeen: timestampFromMs(Date.now()) }),
          ],
        }
      },
      setPrice: (req) => {
        calls.sets.push(req)
        return { price: mine, warnings }
      },
      retirePrice: (req) => {
        calls.retires.push(req)
        return {}
      },
    })
  })
  renderApp(transport, '/settings/prices')
  return calls
}

describe('Prices page', () => {
  it('lists the org prices, the default list with source links and change markers, and flagged requests', async () => {
    setup()
    expect(await screen.findByRole('heading', { name: 'Prices', level: 1 })).toBeInTheDocument()
    const own = await screen.findByRole('list', { name: 'Your prices' })
    expect(within(own).getByText('sonnet-mine')).toBeInTheDocument()
    expect(within(own).getByText('$3 in, $15 out per million tokens')).toBeInTheDocument()

    const defaults = await screen.findByRole('list', { name: 'Default prices' })
    const row = within(defaults).getByRole('heading', { name: 'openai-gpt' }).closest('li')!
    expect(within(row).getByText('Changed since your last upgrade: rates.input')).toBeInTheDocument()
    expect(within(row).getByRole('link', { name: 'https://openai.com/api/pricing/' })).toHaveAttribute('href', 'https://openai.com/api/pricing/')
    const unsafe = within(defaults).getByRole('heading', { name: 'bad-link' }).closest('li')!
    expect(within(unsafe).queryByRole('link')).not.toBeInTheDocument()
    expect(within(defaults).queryByRole('button')).not.toBeInTheDocument()
    expect(screen.getByText(/1 default price changed since 2026-10-23.1/)).toBeInTheDocument()

    const flagged = await screen.findByRole('list', { name: 'Flagged requests' })
    expect(within(flagged).getByText('openai/gpt-6')).toBeInTheDocument()
    expect(within(flagged).getByText('Koinon $0.42, gateway $0.50.')).toBeInTheDocument()
  })

  it('flags a price migrated from model rates whose estimate settings are generic', async () => {
    setup()
    const own = await screen.findByRole('list', { name: 'Your prices' })
    const row = within(own).getByRole('heading', { name: 'model-custom-abc' }).closest('li')!
    expect(within(row).getByText('Needs review')).toBeInTheDocument()
    expect(within(row).getByText(/No default price matched it/)).toBeInTheDocument()
    expect(within(own).getByRole('heading', { name: 'sonnet-mine' }).closest('li')).not.toHaveTextContent('Needs review')
  })

  it('filters flagged requests by reason and by text', async () => {
    setup()
    const flagged = await screen.findByRole('list', { name: 'Flagged requests' })
    await userEvent.selectOptions(screen.getByLabelText('Reason'), 'usage_missing')
    expect(within(flagged).queryByText('openai/gpt-6')).not.toBeInTheDocument()
    expect(within(flagged).getByText('local/q')).toBeInTheDocument()
    await userEvent.selectOptions(screen.getByLabelText('Reason'), '')
    await userEvent.type(screen.getByLabelText('Model or price'), 'gpt')
    expect(within(flagged).queryByText('local/q')).not.toBeInTheDocument()
    await userEvent.type(screen.getByLabelText('Model or price'), 'zzz')
    expect(await screen.findByText('No flagged requests match these filters.')).toBeInTheDocument()
  })

  it('refuses a priced entry without a largest output before sending, then sends the price', async () => {
    const calls = setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Add price' }))
    const form = screen.getByRole('form', { name: 'Add a price' })
    await userEvent.type(within(form).getByLabelText('Name'), 'gpt-mine')
    await userEvent.clear(within(form).getByLabelText('Provider type'))
    await userEvent.type(within(form).getByLabelText('Provider type'), 'openai')
    await userEvent.type(within(form).getByLabelText('Model pattern'), 'gpt-*')
    await userEvent.type(within(form).getByLabelText('Input'), '2')
    await userEvent.type(within(form).getByLabelText('Output'), '8')
    await userEvent.click(within(form).getByRole('button', { name: 'Add price' }))
    expect(await within(form).findByText(/Largest output is required/)).toBeInTheDocument()
    expect(calls.sets).toHaveLength(0)

    await userEvent.type(within(form).getByLabelText('Largest output (tokens)'), '32000')
    await userEvent.click(within(form).getByRole('button', { name: 'Add price' }))
    await waitFor(() => expect(calls.sets).toHaveLength(1))
    expect(calls.sets[0]).toMatchObject({ orgId: 'org-1', name: 'gpt-mine', provider: 'openai', match: { glob: 'gpt-*' }, rates: { input: '2', output: '8' }, maxOutput: 32000, tokenizer: 'bytes' })
    expect(await screen.findByText(/Saved gpt-mine/)).toBeInTheDocument()
  })

  it('adds a new version from the current one and retires after confirming', async () => {
    const calls = setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Edit price sonnet-mine' }))
    const form = screen.getByRole('form', { name: 'New version of sonnet-mine' })
    expect(within(form).getByLabelText('Name')).toBeDisabled()
    expect(within(form).getByLabelText('Input')).toHaveValue('3')
    await userEvent.clear(within(form).getByLabelText('Input'))
    await userEvent.type(within(form).getByLabelText('Input'), '2.5')
    await userEvent.click(within(form).getByRole('button', { name: 'Save new version' }))
    await waitFor(() => expect(calls.sets).toHaveLength(1))
    expect(calls.sets[0]).toMatchObject({ name: 'sonnet-mine', rates: { input: '2.5', output: '15' } })

    await userEvent.click(screen.getByRole('button', { name: 'Retire price sonnet-mine' }))
    expect(calls.retires).toHaveLength(0)
    await userEvent.click(screen.getByRole('button', { name: /^Retire price$/ }))
    await waitFor(() => expect(calls.retires).toEqual([expect.objectContaining({ name: 'sonnet-mine' })]))
  })

  it('keeps the form of another price open while a save is still refreshing (old issue 111)', async () => {
    const calls = setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Edit price sonnet-mine' }))
    const form = screen.getByRole('form', { name: 'New version of sonnet-mine' })
    let release: () => void = () => undefined
    calls.gate = new Promise<void>((r) => (release = r))
    await userEvent.click(within(form).getByRole('button', { name: 'Save new version' }))
    await waitFor(() => expect(calls.sets).toHaveLength(1))

    // The save has returned but its refresh is pending, so the first form is still open.
    await userEvent.click(screen.getByRole('button', { name: 'Edit price model-custom-abc' }))
    expect(screen.getByRole('form', { name: 'New version of model-custom-abc' })).toBeInTheDocument()

    release()
    await waitFor(() => expect(screen.queryByRole('form', { name: 'New version of sonnet-mine' })).not.toBeInTheDocument())
    expect(screen.getByRole('form', { name: 'New version of model-custom-abc' })).toBeInTheDocument()
  })

  it('shows members a notice and sends no request', async () => {
    const calls = setup(OrgRole.MEMBER)
    expect(await screen.findByRole('alert')).toHaveTextContent('Only org admins and owners can manage prices.')
    expect(calls.listed).toBe(0)
    expect(screen.queryByRole('link', { name: 'Prices' })).not.toBeInTheDocument()
  })

  it('shows the warnings of a saved price', async () => {
    const warning = 'Price gemini: the model has a cache_read price but Koinon has no way to turn on prompt caching'
    setup(OrgRole.OWNER, [warning])
    await userEvent.click(await screen.findByRole('button', { name: 'Add price' }))
    const form = screen.getByRole('form', { name: 'Add a price' })
    await userEvent.type(within(form).getByLabelText('Name'), 'gemini')
    await userEvent.clear(within(form).getByLabelText('Provider type'))
    await userEvent.type(within(form).getByLabelText('Provider type'), 'openrouter')
    await userEvent.type(within(form).getByLabelText('Model pattern'), 'google/*')
    await userEvent.type(within(form).getByLabelText('Input'), '2')
    await userEvent.type(within(form).getByLabelText('Output'), '8')
    await userEvent.type(within(form).getByLabelText('Largest output (tokens)'), '32000')
    await userEvent.click(within(form).getByRole('button', { name: 'Add price' }))
    expect(await within(await screen.findByRole('status', { name: 'Warnings' })).findByText(warning)).toBeInTheDocument()
  })
})
