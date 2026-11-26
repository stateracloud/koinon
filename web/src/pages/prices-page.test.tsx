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
  ModelPriceSourceSchema,
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
const explained = [
  create(ModelPriceSourceSchema, {
    providerName: 'router', providerType: 'openrouter', model: 'anthropic/claude-sonnet-4', source: 'org', priceRef: 'org#pv-1',
    entry: { id: 'sonnet-mine', rates: { input: '2.5', output: '12', cacheRead: '0.3' }, maxOutput: 64000 },
    replaces: { source: 'platform', priceRef: 'platform@3#sonnet', entry: { id: 'sonnet', rates: { input: '3', output: '15', cacheRead: '0.3' } } },
  }),
  create(ModelPriceSourceSchema, {
    providerName: 'router', providerType: 'openrouter', model: 'openai/gpt-6', source: 'platform', priceRef: 'platform@3#gpt-6', platformVersion: 3n,
    entry: { id: 'gpt-6', rates: { input: '4', output: '20' }, maxOutput: 32000, tokenizer: 'tiktoken:o200k_base', ratio: '4.0', tiers: [{ contextOver: 200000n, rates: { input: '8' } }] },
  }),
  create(ModelPriceSourceSchema, {
    providerName: 'openai-main', providerType: 'openai', model: 'gpt-5', source: 'default', priceRef: 'default@2026-10-24.1#openai-gpt', listVersion: '2026-10-24.1',
    entry: { id: 'openai-gpt', rates: { input: '2', output: '8', cacheRead: '0.2' }, maxOutput: 32000 },
  }),
  create(ModelPriceSourceSchema, { providerName: 'home', providerType: 'ollama', model: 'local/q', unpricedReason: 'no_price' }),
]
const bad = create(PriceEntrySchema, { id: 'bad-link', provider: '*', match: { glob: 'x' }, validFrom: '2026-10-01', rates: { input: '1', output: '1' }, sourceUrl: 'javascript:alert(1)' })

function setup(role = OrgRole.OWNER, warnings: string[] = []) {
  const calls = { explained: 0, sets: [] as SetPriceRequest[], retires: [] as RetirePriceRequest[], listed: 0, flaggedSince: [] as bigint[], gate: undefined as Promise<void> | undefined }
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role }) })
    service(PriceService, {
      listPrices: async () => {
        calls.listed++
        await calls.gate
        return { prices: [mine, migrated] }
      },
      getDefaultPrices: () => ({ version: '2026-10-24.1', entries: [gpt, bad] }),
      explainOrgPrices: () => {
        calls.explained++
        return { models: explained }
      },
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

const rowOf = (table: HTMLElement, model: string) => within(table).getByRole('rowheader', { name: model }).closest('tr')!

describe('Prices page', () => {
  it('shows the source of each model in the prices table, with the legend and the replaced price struck through', async () => {
    setup()
    const table = await screen.findByRole('table', { name: 'Prices in use' })
    const legend = screen.getByRole('list', { name: 'Price sources' })
    expect(within(legend).getByText('Org override')).toBeInTheDocument()
    expect(within(legend).getByText('Platform')).toBeInTheDocument()
    expect(within(legend).getByText('Default')).toBeInTheDocument()

    const org = rowOf(table, 'anthropic/claude-sonnet-4')
    expect(within(org).getByText('Org override')).toBeInTheDocument()
    const struck = org.querySelectorAll('del')
    expect([...struck].map((d) => d.textContent)).toEqual(['Platform price was $3', 'Platform price was $15'])
    expect(org).toHaveTextContent('$2.5')
    expect(org).toHaveTextContent('$12')
    expect(within(org).queryByRole('button', { name: /Override/ })).not.toBeInTheDocument()

    const platform = rowOf(table, 'openai/gpt-6')
    expect(within(platform).getByText('Platform')).toBeInTheDocument()
    expect(platform.querySelector('del')).toBeNull()
    expect(await within(platform).findByRole('button', { name: 'Override the price of openai/gpt-6 (router)' })).toBeInTheDocument()

    const def = rowOf(table, 'gpt-5')
    expect(within(def).getByText('Default')).toBeInTheDocument()
    expect(def).toHaveTextContent('openai-main')
    expect(def).toHaveTextContent('$0.2')

    const unpriced = rowOf(table, 'local/q')
    expect(within(unpriced).getByText('No price')).toBeInTheDocument()
  })

  it('finds a model in the prices table by name or by provider', async () => {
    setup()
    const table = await screen.findByRole('table', { name: 'Prices in use' })
    const search = screen.getByLabelText('Search models')
    await userEvent.type(search, 'SONNET')
    expect(within(table).getAllByRole('rowheader').map((h) => h.textContent)).toEqual(['anthropic/claude-sonnet-4'])
    await userEvent.clear(search)
    await userEvent.type(search, 'openai-main')
    expect(within(table).getAllByRole('rowheader').map((h) => h.textContent)).toEqual(['gpt-5'])
    await userEvent.clear(search)
    await userEvent.type(search, 'ollama')
    expect(within(table).getAllByRole('rowheader').map((h) => h.textContent)).toEqual(['local/q'])
    await userEvent.type(search, 'zzz')
    expect(await screen.findByText('No model matches the search.')).toBeInTheDocument()
  })

  it('sorts the prices table by a rate, unset rates last', async () => {
    setup()
    const table = await screen.findByRole('table', { name: 'Prices in use' })
    await userEvent.click(within(table).getByRole('button', { name: 'Input' }))
    expect(within(table).getAllByRole('rowheader').map((h) => h.textContent)).toEqual(['gpt-5', 'anthropic/claude-sonnet-4', 'openai/gpt-6', 'local/q'])
    expect(within(table).getByRole('columnheader', { name: 'Input' })).toHaveAttribute('aria-sort', 'ascending')
    await userEvent.click(within(table).getByRole('button', { name: 'Input' }))
    expect(within(table).getAllByRole('rowheader').map((h) => h.textContent)).toEqual(['openai/gpt-6', 'anthropic/claude-sonnet-4', 'gpt-5', 'local/q'])
  })

  it('overrides a platform price with an org price filled from the price in use', async () => {
    const calls = setup()
    const table = await screen.findByRole('table', { name: 'Prices in use' })
    await userEvent.click(await within(rowOf(table, 'openai/gpt-6')).findByRole('button', { name: /Override/ }))
    const form = screen.getByRole('form', { name: 'Override the price of openai/gpt-6' })
    expect(form.contains(document.activeElement)).toBe(true)
    expect(within(form).getByLabelText('Name')).toHaveValue('openrouter-openai-gpt-6')
    expect(within(form).getByLabelText('Model pattern')).toHaveValue('openai/gpt-6')
    await userEvent.clear(within(form).getByLabelText('Input'))
    await userEvent.type(within(form).getByLabelText('Input'), '3.5')
    await userEvent.click(within(form).getByRole('button', { name: 'Add price' }))
    await waitFor(() => expect(calls.sets).toHaveLength(1))
    expect(calls.sets[0]).toMatchObject({
      orgId: 'org-1', name: 'openrouter-openai-gpt-6', provider: 'openrouter', match: { glob: 'openai/gpt-6' }, rates: { input: '3.5', output: '20' },
      maxOutput: 32000, tokenizer: 'tiktoken:o200k_base', ratio: '4.0', tiers: [{ contextOver: 200000n, rates: { input: '8' } }],
    })
    expect(await screen.findByText(/Saved openrouter-openai-gpt-6/)).toBeInTheDocument()
    expect(screen.queryByRole('form', { name: 'Override the price of openai/gpt-6' })).not.toBeInTheDocument()
    await waitFor(() => expect(calls.explained).toBeGreaterThanOrEqual(2))
  })

  it('shows no Override button until the org prices have loaded, so a name cannot be reused', async () => {
    let release: () => void = () => undefined
    const calls = setup()
    calls.gate = new Promise<void>((r) => (release = r))
    const table = await screen.findByRole('table', { name: 'Prices in use' })
    expect(within(table).queryByRole('button', { name: /Override/ })).not.toBeInTheDocument()
    release()
    expect(await within(table).findByRole('button', { name: 'Override the price of openai/gpt-6 (router)' })).toBeInTheDocument()
  })

  it('returns focus to the Override button of the row when the override form is cancelled', async () => {
    setup()
    const table = await screen.findByRole('table', { name: 'Prices in use' })
    const button = await within(rowOf(table, 'openai/gpt-6')).findByRole('button', { name: /Override/ })
    await userEvent.click(button)
    const form = screen.getByRole('form', { name: 'Override the price of openai/gpt-6' })
    await userEvent.click(within(form).getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('form', { name: 'Override the price of openai/gpt-6' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Override the price of openai/gpt-6 (router)' })).toHaveFocus()
  })

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
