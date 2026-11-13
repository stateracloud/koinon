// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { Code, createRouterTransport } from '@connectrpc/connect'
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

function setup(opts: { platformAdmin?: boolean; proposals?: ReturnType<typeof proposal>[] } = {}) {
  const calls = { accepts: [] as AcceptPriceProposalRequest[], dismisses: [] as string[], rollbacks: [] as bigint[], checks: 0 }
  let proposals = opts.proposals ?? []
  const versions = [
    create(PlatformPriceVersionSchema, { version: 2n, actor: 'system koinon-api:price-refresh', reason: 'auto', ruleSetVersion: 'rules-1' }),
    create(PlatformPriceVersionSchema, { version: 1n, actor: 'cli prices import', reason: 'first' }),
  ]
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => create(WhoAmIResponseSchema, { ...whoAmI(), platformAdmin: opts.platformAdmin ?? true }) })
    service(PriceService, {
      listPriceProposals: (req) => ({ proposals: proposals.filter((p) => !req.class || p.class === req.class) }),
      acceptPriceProposal: (req) => {
        calls.accepts.push(req)
        const p = proposals.find((x) => x.id === req.id)
        if (p?.class === 'confirm' && req.confirm !== p.model) throw reasoned('type the model id', Code.InvalidArgument, 'CONFIRM_MISMATCH')
        proposals = proposals.filter((x) => x.id !== req.id)
        return { version: 3n, warnings: [] }
      },
      dismissPriceProposal: (req) => {
        calls.dismisses.push(req.id)
        proposals = proposals.filter((x) => x.id !== req.id)
        return {}
      },
      listPlatformPriceVersions: () => ({ versions }),
      getPlatformPriceVersion: (req) => ({
        version: create(PlatformPriceVersionSchema, { version: req.version, entries: [create(PlatformPriceEntrySchema, { entry: entry('gpt-x', '4', '20'), priceRef: `platform@${req.version}#gpt-x`, provenance: prov })] }),
      }),
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
  renderApp(transport, '/platform/prices')
  return calls
}

describe('Platform prices page', () => {
  it('is for platform admins only', async () => {
    setup({ platformAdmin: false })
    expect(await screen.findByText('Only platform admins can manage the platform prices.')).toBeInTheDocument()
  })

  it('shows a proposal with its diff and provenance, and accepts a needs-review one', async () => {
    const calls = setup({ proposals: [proposal('p1', 'gpt-x', 'needs_review')] })
    expect(await screen.findByText(/input \$3 → \$4/)).toBeInTheDocument()
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
    const card = (await screen.findByRole('heading', { name: 'gpt-x' })).closest('li')!
    await userEvent.click(within(card).getByRole('button', { name: /Dismiss/ }))
    await userEvent.click(await screen.findByRole('button', { name: 'Dismiss proposal' }))
    await waitFor(() => expect(calls.dismisses).toEqual(['p5']))
  })

  it('rolls back to an older version after a confirmation, with no rollback on the newest', async () => {
    const calls = setup()
    const v1 = (await screen.findByRole('heading', { name: 'Version 1' })).closest('li')!
    const v2 = screen.getByRole('heading', { name: 'Version 2' }).closest('li')!
    expect(within(v2).queryByRole('button', { name: /Roll back/ })).not.toBeInTheDocument()
    await userEvent.click(within(v1).getByRole('button', { name: /Roll back/ }))
    await userEvent.click(await screen.findByRole('button', { name: 'Roll back' }))
    await waitFor(() => expect(calls.rollbacks).toEqual([1n]))
    expect(await screen.findByText(/Platform price version 3 written, equal to version 1/)).toBeInTheDocument()
  })

  it('shows prices with their price_ref when a version is opened', async () => {
    setup()
    const v2 = (await screen.findByRole('heading', { name: 'Version 2' })).closest('li')!
    await userEvent.click(within(v2).getByRole('button', { name: /Show changes in version 2/ }))
    expect(await within(v2).findByText('platform@2#gpt-x')).toBeInTheDocument()
  })

  it('checks the sources and reports a failed one', async () => {
    const calls = setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Check sources' }))
    expect(await screen.findByText(/Checked 1 source\(s\); 2 proposal\(s\) made or refreshed\. openrouter failed: HTTP 503\./)).toBeInTheDocument()
    expect(calls.checks).toBe(1)
  })

  it('lists the fetch log and the refresh setting', async () => {
    setup()
    expect(await screen.findByRole('table', { name: 'Price source fetches, newest first' })).toBeInTheDocument()
    expect(await screen.findByLabelText('Refresh setting')).toHaveTextContent('Scheduled refresh: auto, every 6h0m0s')
  })
})
