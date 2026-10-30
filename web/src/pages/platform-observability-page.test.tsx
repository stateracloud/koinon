// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AuthService, WhoAmIResponseSchema } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import {
  Backend,
  ContentLevel,
  DestinationSchema,
  TestResultSchema,
  TraceDestinationService,
  type SetPlatformCeilingRequest,
  type SetPlatformDestinationRequest,
} from '@/gen/koinon/trace/v1alpha1/trace_pb'
import { renderApp, whoAmI } from '@/test-utils'

const fakeKey = 'koinon_test_not-a-real-token'
const datadog = create(DestinationSchema, {
  name: '',
  backend: Backend.DATADOG,
  endpointUrl: 'https://api.datadoghq.com/api/intake/otlp/v1/traces',
  secretFields: ['api_key'],
  enabled: true,
  lastTest: create(TestResultSchema, { ok: true }),
})

function setup(opts: { platformAdmin?: boolean; destination?: boolean; ceiling?: ContentLevel; lowered?: number; failSave?: string } = {}) {
  const calls = { sets: [] as SetPlatformDestinationRequest[], ceilings: [] as SetPlatformCeilingRequest[], tests: 0, deletes: 0, gets: 0 }
  let destination = opts.destination ? datadog : undefined
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => create(WhoAmIResponseSchema, { ...whoAmI(), platformAdmin: opts.platformAdmin ?? true }) })
    service(TraceDestinationService, {
      getPlatform: () => {
        calls.gets++
        return { destination, ceiling: opts.ceiling ?? ContentLevel.FULL }
      },
      setPlatformDestination: (req) => {
        calls.sets.push(req)
        if (opts.failSave) throw new ConnectError(opts.failSave, Code.FailedPrecondition)
        destination = create(DestinationSchema, { backend: req.destination?.backend, endpointUrl: req.destination?.endpointUrl, enabled: true, lastTest: create(TestResultSchema, { ok: true }) })
        return { destination }
      },
      deletePlatformDestination: () => {
        calls.deletes++
        destination = undefined
        return {}
      },
      testPlatformDestination: () => {
        calls.tests++
        return { destination, result: create(TestResultSchema, { ok: true }) }
      },
      setPlatformCeiling: (req) => {
        calls.ceilings.push(req)
        return { ceiling: req.ceiling, orgsLowered: opts.lowered ?? 0 }
      },
    })
  })
  renderApp(transport, '/platform/observability')
  return calls
}

describe('Platform observability page', () => {
  it('is for platform admins only', async () => {
    const calls = setup({ platformAdmin: false })
    expect(await screen.findByText('Only platform admins can manage platform observability.')).toBeInTheDocument()
    expect(calls.gets).toBe(0)
  })

  it('offers to set a destination when there is none', async () => {
    setup()
    expect(await screen.findByText(/No platform destination/)).toBeInTheDocument()
  })

  it('sets the platform destination with no name field and reports the test', async () => {
    const calls = setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Set destination' }))
    const form = screen.getByRole('form', { name: 'Add platform destination' })
    expect(within(form).queryByLabelText('Name')).not.toBeInTheDocument()
    await userEvent.selectOptions(within(form).getByLabelText('Backend'), 'Datadog')
    await userEvent.type(within(form).getByLabelText('Traces endpoint'), 'https://api.datadoghq.com/x')
    await userEvent.type(within(form).getByLabelText('API key'), fakeKey)
    await userEvent.click(within(form).getByRole('button', { name: 'Test and add' }))
    await waitFor(() => expect(calls.sets).toHaveLength(1))
    expect(calls.sets[0].destination).toMatchObject({ backend: Backend.DATADOG, secrets: { api_key: fakeKey } })
    expect(await screen.findByText(/Saved the platform destination\. Test passed for the platform destination/)).toBeInTheDocument()
    expect(await screen.findByRole('region', { name: 'Platform destination' })).toBeInTheDocument()
  })

  it('shows the backend message when the test refuses the destination', async () => {
    setup({ failSave: 'the backend answered HTTP 403 (the credentials were refused)' })
    await userEvent.click(await screen.findByRole('button', { name: 'Set destination' }))
    const form = screen.getByRole('form', { name: 'Add platform destination' })
    await userEvent.type(within(form).getByLabelText('Traces endpoint'), 'https://api.datadoghq.com/x')
    await userEvent.type(within(form).getByLabelText('Public key'), 'koinon_test_public')
    await userEvent.type(within(form).getByLabelText('Secret key'), fakeKey)
    await userEvent.click(within(form).getByRole('button', { name: 'Test and add' }))
    expect(await within(form).findByRole('alert')).toHaveTextContent('Not saved. the backend answered HTTP 403 (the credentials were refused)')
  })

  it('shows the destination, tests it and deletes it', async () => {
    const calls = setup({ destination: true })
    const card = within(await screen.findByRole('region', { name: 'Platform destination' }))
    expect(card.getByText('Datadog')).toBeInTheDocument()
    await userEvent.click(card.getByRole('button', { name: 'Test the platform destination' }))
    await waitFor(() => expect(calls.tests).toBe(1))
    expect(await screen.findByText('Test passed for the platform destination. The backend accepted the test trace.')).toBeInTheDocument()
    await userEvent.click(card.getByRole('button', { name: 'Delete the platform destination' }))
    await userEvent.click(screen.getByRole('button', { name: 'Delete destination' }))
    await waitFor(() => expect(calls.deletes).toBe(1))
  })

  it('warns before lowering the ceiling and reports how many orgs were lowered', async () => {
    const calls = setup({ ceiling: ContentLevel.FULL, lowered: 3 })
    const form = await screen.findByRole('form', { name: 'Content ceiling' })
    expect(within(form).getByRole('radio', { name: /Full/ })).toBeChecked()
    expect(within(form).getByRole('button', { name: 'Save ceiling' })).toBeDisabled()
    expect(within(form).queryByText(/Lowering the ceiling lowers every org/)).not.toBeInTheDocument()
    await userEvent.click(within(form).getByRole('radio', { name: /Redacted/ }))
    expect(within(form).getByText(/Lowering the ceiling lowers every org that is above it to redacted/)).toBeInTheDocument()
    await userEvent.click(within(form).getByRole('button', { name: 'Lower the ceiling' }))
    await waitFor(() => expect(calls.ceilings).toEqual([expect.objectContaining({ ceiling: ContentLevel.REDACTED })]))
    expect(await screen.findByText('The ceiling is now redacted. 3 orgs were lowered to it.')).toBeInTheDocument()
  })

  it('raises the ceiling without a warning', async () => {
    const calls = setup({ ceiling: ContentLevel.NONE })
    const form = await screen.findByRole('form', { name: 'Content ceiling' })
    await userEvent.click(within(form).getByRole('radio', { name: /Redacted/ }))
    expect(within(form).queryByText(/Lowering the ceiling/)).not.toBeInTheDocument()
    await userEvent.click(within(form).getByRole('button', { name: 'Save ceiling' }))
    await waitFor(() => expect(calls.ceilings).toHaveLength(1))
    expect(await screen.findByText('The ceiling is now redacted. No org was above it.')).toBeInTheDocument()
  })
})
