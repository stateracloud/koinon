// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromDate } from '@bufbuild/protobuf/wkt'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AuthService, WhoAmIResponseSchema } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import {
  PlatformPriceEntrySchema,
  PlatformPriceVersionSchema,
  PriceEntrySchema,
  PriceMatchSchema,
  PriceProposalSchema,
  PriceProvenanceSchema,
  PriceRateChangeSchema,
  PriceService,
  PriceSourceFetchSchema,
  RatesSchema,
  type AcceptPriceProposalRequest,
} from '@/gen/koinon/budget/v1alpha1/price_pb'
import { reasoned, renderApp, whoAmI } from '@/test-utils'

const rates = (input: string, output: string) => create(RatesSchema, { input, output })
const entry = (id: string, input: string, output: string, maxOutput = 8192) =>
  create(PriceEntrySchema, { id, match: create(PriceMatchSchema, { glob: id }), rates: rates(input, output), maxOutput })
const prov = create(PriceProvenanceSchema, { source: 'litellm', url: 'https://example.invalid/prices.json', sha256: 'ab'.repeat(32), agreeingSources: ['openrouter'] })

function proposal(id: string, model: string, klass: string, maxOutput = 8192) {
  return create(PriceProposalSchema, {
    id,
    source: 'litellm',
    providerType: 'openrouter',
    model,
    class: klass,
    state: 'open',
    failedRules: klass === 'confirm' ? 'The change is above the auto-apply limit.' : '',
    current: entry(model, '3', '15'),
    proposed: entry(model, '4', '20', maxOutput),
    changes: [create(PriceRateChangeSchema, { bucket: 'input', from: '3', to: '4' })],
    provenance: prov,
  })
}

function setup(opts: { platformAdmin?: boolean; proposals?: ReturnType<typeof proposal>[]; noPlatformVersion?: boolean; futureVersion?: boolean } = {}) {
  const calls = { accepts: [] as AcceptPriceProposalRequest[], dismisses: [] as string[], rollbacks: [] as bigint[], checks: 0 }
  let proposals = opts.proposals ?? []
  const versions = [
    ...(opts.futureVersion ? [create(PlatformPriceVersionSchema, { version: 3n, validFrom: timestampFromDate(new Date(Date.now() + 86_400_000 * 3)), actor: 'cli prices import', reason: 'later' })] : []),
    create(PlatformPriceVersionSchema, { version: 2n, actor: 'system koinon-api:price-refresh', reason: 'auto', ruleSetVersion: 'rules-1' }),
    create(PlatformPriceVersionSchema, { version: 1n, actor: 'cli prices import', reason: 'first' }),
  ]
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => create(WhoAmIResponseSchema, { ...whoAmI(), platformAdmin: opts.platformAdmin ?? true }) })
    service(PriceService, {
      listPriceProposals: (req) => ({ proposals: proposals.filter((candidate) => !req.class || candidate.class === req.class) }),
      acceptPriceProposal: (req) => {
        calls.accepts.push(req)
        const target = proposals.find((proposal) => proposal.id === req.id)
        if (target?.class === 'confirm' && req.confirm !== target.model) throw reasoned('type the model id', Code.InvalidArgument, 'CONFIRM_MISMATCH')
        proposals = proposals.filter((remaining) => remaining.id !== req.id)
        return { version: 3n, warnings: [] }
      },
      dismissPriceProposal: (req) => {
        calls.dismisses.push(req.id)
        proposals = proposals.filter((remaining) => remaining.id !== req.id)
        return {}
      },
      listPlatformPriceVersions: () => ({ versions: opts.noPlatformVersion ? [] : versions }),
      getPlatformPriceVersion: (req) => {
        if (req.version === 0n) throw new ConnectError('no platform price version yet', Code.NotFound)
        const version = req.version
        const entries = [create(PlatformPriceEntrySchema, { entry: entry('gpt-x', '4', '20'), priceRef: `platform@${version}#gpt-x`, provenance: prov })]
        if (version === 2n) {
          entries.push(create(PlatformPriceEntrySchema, { entry: { ...entry('anthropic/claude-*', '3', '15'), provider: 'openrouter' }, priceRef: `platform@${version}#claude`, provenance: prov }))
        }
        return { version: create(PlatformPriceVersionSchema, { version, entries }) }
      },
      diffPlatformPriceVersions: () => ({ changes: [] }),
      rollbackPlatformPrices: (req) => {
        calls.rollbacks.push(req.version)
        return { version: 3n }
      },
      getPriceRefreshSettings: () => ({ mode: 'auto', interval: '6h0m0s', maxChangePercent: 20, maxAutoPerDay: 5, recorded: true }),
      listPriceSourceFetches: () => ({ fetches: [create(PriceSourceFetchSchema, { id: 'f1', source: 'litellm', result: 'ok', modelCount: 120, trigger: 'schedule' })] }),
      checkPriceSources: () => {
        calls.checks++
        return { fetched: ['litellm'], failed: { openrouter: 'HTTP 503' }, proposals: 2 }
      },
    })
  })
  renderApp(transport, '/platform/usage/prices')
  return calls
}

// The list item that holds a heading; a missing one fails the spec with a message.
function listItemOf(heading: HTMLElement): HTMLElement {
  const item = heading.closest('li')
  if (!item) throw new Error(`no list item around "${heading.textContent}"`)
  return item
}

describe('Platform prices page', () => {
  it('is for platform admins only: any other user goes to the org area', async () => {
    setup({ platformAdmin: false })
    expect(await screen.findByRole('navigation', { name: 'Main' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Platform prices' })).not.toBeInTheDocument()
  })

  it('shows a proposal with its diff and provenance, and accepts a needs-review one', async () => {
    const calls = setup({ proposals: [proposal('p1', 'gpt-x', 'needs_review')] })
    expect(await screen.findByText(/input \$3 → \$4/u)).toBeInTheDocument()
    expect(screen.getByText('Agreeing sources')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Accept gpt-x' }))
    await waitFor(() => expect(calls.accepts).toHaveLength(1))
    expect(calls.accepts[0]).toMatchObject({ id: 'p1', confirm: '', maxOutput: 0 })
    expect(await screen.findByText(/Accepted gpt-x: platform price version 3 written/)).toBeInTheDocument()
  })

  it('keeps Accept off until the model id is typed for a confirm proposal', async () => {
    const calls = setup({ proposals: [proposal('p2', 'gpt-big', 'confirm')] })
    const accept = await screen.findByRole('button', { name: 'Accept gpt-big' })
    expect(accept).toBeDisabled()
    await userEvent.type(screen.getByLabelText('Type the model id to confirm'), 'gpt-big')
    expect(accept).toBeEnabled()
    await userEvent.click(accept)
    await waitFor(() => expect(calls.accepts[0]).toMatchObject({ id: 'p2', confirm: 'gpt-big' }))
  })

  it('asks for the output limit when the proposal has none', async () => {
    const calls = setup({ proposals: [proposal('p3', 'gpt-new', 'needs_review', 0)] })
    const accept = await screen.findByRole('button', { name: 'Accept gpt-new' })
    expect(accept).toBeDisabled()
    await userEvent.type(screen.getByLabelText('Output limit in tokens'), '16000')
    await userEvent.click(accept)
    await waitFor(() => expect(calls.accepts[0]).toMatchObject({ id: 'p3', maxOutput: 16000 }))
  })

  it('offers no Accept for a refused proposal', async () => {
    setup({ proposals: [proposal('p4', 'gpt-bad', 'refused')] })
    await screen.findByText('gpt-bad')
    expect(screen.queryByRole('button', { name: 'Accept gpt-bad' })).not.toBeInTheDocument()
  })

  it('dismisses after a confirmation', async () => {
    const calls = setup({ proposals: [proposal('p5', 'gpt-x', 'needs_review')] })
    const card = listItemOf(await screen.findByRole('heading', { name: 'gpt-x' }))
    await userEvent.click(within(card).getByRole('button', { name: /Dismiss/ }))
    await userEvent.click(await screen.findByRole('button', { name: 'Dismiss proposal' }))
    await waitFor(() => expect(calls.dismisses).toEqual(['p5']))
  })

  it('rolls back to an older version after a confirmation, with no rollback on the newest', async () => {
    const calls = setup()
    const v1 = listItemOf(await screen.findByRole('heading', { name: 'Version 1' }))
    const v2 = listItemOf(screen.getByRole('heading', { name: 'Version 2' }))
    expect(within(v2).queryByRole('button', { name: /Roll back/ })).not.toBeInTheDocument()
    await userEvent.click(within(v1).getByRole('button', { name: /Roll back/ }))
    await userEvent.click(await screen.findByRole('button', { name: 'Roll back' }))
    await waitFor(() => expect(calls.rollbacks).toEqual([1n]))
    expect(await screen.findByText(/Platform price version 3 written, equal to version 1/)).toBeInTheDocument()
  })

  it('shows prices with their price_ref when a version is opened', async () => {
    setup()
    const v2 = listItemOf(await screen.findByRole('heading', { name: 'Version 2' }))
    await userEvent.click(within(v2).getByRole('button', { name: /Show changes in version 2/ }))
    expect(await within(v2).findByText('platform@2#gpt-x')).toBeInTheDocument()
  })

  it('checks the sources and reports a failed one', async () => {
    const calls = setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Check sources' }))
    expect(await screen.findByText(/Checked 1 source\(s\); 2 proposal\(s\) made or refreshed\. openrouter failed: HTTP 503\./)).toBeInTheDocument()
    expect(calls.checks).toBe(1)
  })

  it('reuses the prices table for the newest platform version, with search and no Override', async () => {
    setup()
    const table = await screen.findByRole('table', { name: 'Platform prices in use' })
    expect(screen.getByText(/^Version 2\. Models that no entry here matches use the default list\./)).toBeInTheDocument()
    const heads = () => within(table).getAllByRole('rowheader').map((h) => h.textContent)
    expect(heads()).toEqual(['anthropic/claude-*', 'gpt-x'])
    const claude = within(table).getByRole('rowheader', { name: 'anthropic/claude-*' }).closest('tr')!
    expect(within(claude).getByText('Platform')).toBeInTheDocument()
    expect(claude).toHaveTextContent('openrouter')
    expect(claude).toHaveTextContent('$15')
    expect(within(table).queryByRole('button', { name: /Override/ })).not.toBeInTheDocument()
    await userEvent.type(screen.getByLabelText('Search models'), 'openrouter')
    expect(heads()).toEqual(['anthropic/claude-*'])
  })

  it('shows the version in use now when a newer version starts later', async () => {
    setup({ futureVersion: true })
    const table = await screen.findByRole('table', { name: 'Platform prices in use' })
    expect(within(table).getByRole('rowheader', { name: 'gpt-x' })).toBeInTheDocument()
    expect(screen.getByText(/^Version 2\. Models that no entry here matches/)).toBeInTheDocument()
    expect(screen.getByText(/Version 3 starts .*; the table shows version 2, in use now\./)).toBeInTheDocument()
  })

  it('says every model uses the default list when no platform version exists', async () => {
    setup({ noPlatformVersion: true })
    expect(await screen.findByText('No platform prices yet. Every model uses the default list.')).toBeInTheDocument()
    expect(screen.queryByRole('table', { name: 'Platform prices in use' })).not.toBeInTheDocument()
  })

  it('lists the fetch log and the refresh setting', async () => {
    setup()
    expect(await screen.findByRole('table', { name: 'Price source fetches, newest first' })).toBeInTheDocument()
    expect(await screen.findByLabelText('Refresh setting')).toHaveTextContent('Scheduled refresh: auto, every 6h0m0s')
  })
})
