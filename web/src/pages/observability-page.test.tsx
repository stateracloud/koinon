// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import {
  Backend,
  ContentLevel,
  DestinationSchema,
  HealthSchema,
  SettingsSchema,
  TestResultSchema,
  TraceDestinationService,
  type CreateDestinationRequest,
  type DeleteDestinationRequest,
  type TestDestinationRequest,
  type UpdateDestinationRequest,
  type UpdateSettingsRequest,
} from '@/gen/koinon/trace/v1alpha1/trace_pb'
import { renderApp, whoAmI } from '@/test-utils'

const fakeKey = 'koinon_test_not-a-real-token'
const at = timestampFromMs(Date.now() - 120_000)

const langfuse = create(DestinationSchema, {
  id: 'd-lf',
  name: 'langfuse',
  backend: Backend.LANGFUSE,
  endpointUrl: 'https://cloud.langfuse.com/api/public/otel/v1/traces',
  secretFields: ['public_key', 'secret_key'],
  enabled: true,
  configVersion: 7n,
  lastTest: create(TestResultSchema, { ok: true, testedAt: at }),
  health: create(HealthSchema, { lastSuccessAt: at, spansDropped: 1234n }),
})
const broken = create(DestinationSchema, {
  id: 'd-dd',
  name: 'datadog',
  backend: Backend.DATADOG,
  endpointUrl: 'https://api.datadoghq.com/api/intake/otlp/v1/traces',
  secretFields: ['api_key'],
  enabled: true,
  lastTest: create(TestResultSchema, { ok: false, message: 'the backend answered HTTP 403 (Forbidden)', testedAt: at }),
  health: create(HealthSchema, { lastErrorAt: at, lastError: 'status 403' }),
})
const generic = create(DestinationSchema, { id: 'd-g', name: 'collector', backend: Backend.GENERIC, endpointUrl: 'https://c.example.com/v1/traces', secretFields: ['X-Api-Key', 'X-Team'], enabled: false })

type Opts = {
  role?: OrgRole
  level?: ContentLevel
  ceiling?: ContentLevel
  platform?: boolean
  copy?: ContentLevel
  failSave?: string
  updateCode?: Code
  settingsCode?: Code
  settingsFail?: string
}

function setup(opts: Opts = {}) {
  const calls = {
    creates: [] as CreateDestinationRequest[],
    updates: [] as UpdateDestinationRequest[],
    deletes: [] as DeleteDestinationRequest[],
    tests: [] as TestDestinationRequest[],
    settings: [] as UpdateSettingsRequest[],
    listed: 0,
  }
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: opts.role }) })
    service(TraceDestinationService, {
      listDestinations: () => {
        calls.listed++
        return { destinations: [langfuse, broken, generic] }
      },
      getSettings: () => ({
        settings: create(SettingsSchema, {
          contentLevel: opts.level ?? ContentLevel.NONE,
          sampleRate: 0.5,
          platformCopyLevel: opts.copy ?? ContentLevel.NONE,
          ceiling: opts.ceiling ?? ContentLevel.FULL,
          hasPlatformDestination: opts.platform ?? false,
        }),
      }),
      createDestination: (req) => {
        calls.creates.push(req)
        if (opts.failSave) throw new ConnectError(opts.failSave, Code.FailedPrecondition)
        return { destination: create(DestinationSchema, { id: 'd-new', name: req.destination?.name, backend: req.destination?.backend, lastTest: create(TestResultSchema, { ok: true }) }) }
      },
      updateDestination: (req) => {
        calls.updates.push(req)
        if (opts.updateCode) throw new ConnectError('refused', opts.updateCode)
        if (opts.failSave) throw new ConnectError(opts.failSave, Code.FailedPrecondition)
        return { destination: langfuse }
      },
      deleteDestination: (req) => {
        calls.deletes.push(req)
        return {}
      },
      testDestination: (req) => {
        calls.tests.push(req)
        return { destination: broken, result: create(TestResultSchema, { ok: false, message: 'the backend answered HTTP 401 (Unauthorized)' }) }
      },
      updateSettings: (req) => {
        calls.settings.push(req)
        if (opts.settingsCode) throw new ConnectError('platform admins who are not members may only lower', opts.settingsCode)
        return { settings: create(SettingsSchema, { contentLevel: req.contentLevel, sampleRate: req.sampleRate, platformCopyLevel: req.platformCopyLevel, ceiling: opts.ceiling ?? ContentLevel.FULL }) }
      },
    })
  })
  renderApp(transport, '/settings/observability')
  return calls
}

describe('Observability page', () => {
  it('is for admins only and sends no request for a member', async () => {
    const calls = setup({ role: OrgRole.MEMBER })
    expect(await screen.findByText('Only org admins and owners can manage observability.')).toBeInTheDocument()
    expect(calls.listed).toBe(0)
  })

  it('lists destinations with backend, endpoint, enabled, last test and health', async () => {
    setup()
    const list = await screen.findByRole('list', { name: 'Destinations' })
    const lf = within(within(list).getByRole('region', { name: 'langfuse' }))
    expect(lf.getByText('Langfuse')).toBeInTheDocument()
    expect(lf.getByText('Enabled')).toBeInTheDocument()
    expect(lf.getByText('https://cloud.langfuse.com/api/public/otel/v1/traces')).toBeInTheDocument()
    expect(lf.getByText(/^Passed,/)).toBeInTheDocument()
    expect(lf.getByText('1234')).toBeInTheDocument()
    const dd = within(within(list).getByRole('region', { name: 'datadog' }))
    expect(dd.getByText(/^Failed,/)).toBeInTheDocument()
    expect(dd.getByText(/The last test failed: the backend answered HTTP 403 \(Forbidden\)/)).toBeInTheDocument()
    expect(dd.getByText(/status 403/)).toBeInTheDocument()
    expect(within(within(list).getByRole('region', { name: 'collector' })).getByText('Disabled')).toBeInTheDocument()
  })

  it('shows no Full content badge below full', async () => {
    setup({ level: ContentLevel.REDACTED })
    await screen.findByRole('list', { name: 'Destinations' })
    expect(screen.queryByText('Full content')).not.toBeInTheDocument()
  })

  it('shows the Full content badge on the page and on each destination', async () => {
    setup({ level: ContentLevel.FULL })
    const list = await screen.findByRole('list', { name: 'Destinations' })
    await waitFor(() => expect(screen.getAllByText('Full content')).toHaveLength(5))
    expect(within(within(list).getByRole('region', { name: 'langfuse' })).getByText('Full content')).toBeInTheDocument()
  })

  it('never renders a secret and shows the stored one as set', async () => {
    setup()
    await screen.findByRole('list', { name: 'Destinations' })
    await userEvent.click(screen.getByRole('button', { name: 'Edit destination langfuse' }))
    const form = screen.getByRole('form', { name: 'Edit destination langfuse' })
    for (const label of ['Public key', 'Secret key']) {
      expect(within(form).getByLabelText(label)).toHaveValue('')
      expect(within(form).getByLabelText(label)).toHaveAttribute('type', 'password')
      expect(within(form).getByLabelText(label)).toHaveAttribute('placeholder', 'Set')
    }
    expect(document.body.textContent).not.toContain(fakeKey)
  })

  it('adds a Langfuse destination with the public and secret key, then clears the secrets', async () => {
    const calls = setup()
    await screen.findByRole('list', { name: 'Destinations' })
    await userEvent.click(screen.getByRole('button', { name: 'Add destination' }))
    const form = screen.getByRole('form', { name: 'Add destination' })
    await userEvent.type(within(form).getByLabelText('Name'), 'team-lf')
    await userEvent.type(within(form).getByLabelText('Traces endpoint'), 'https://lf.example.com/api/public/otel/v1/traces')
    await userEvent.type(within(form).getByLabelText('Public key'), 'koinon_test_public')
    await userEvent.type(within(form).getByLabelText('Secret key'), fakeKey)
    await userEvent.click(within(form).getByRole('button', { name: 'Test and add' }))
    await waitFor(() => expect(calls.creates).toHaveLength(1))
    expect(calls.creates[0].orgId).toBe('org-1')
    expect(calls.creates[0].destination).toMatchObject({ name: 'team-lf', backend: Backend.LANGFUSE, endpointUrl: 'https://lf.example.com/api/public/otel/v1/traces', secrets: { public_key: 'koinon_test_public', secret_key: fakeKey }, enabled: true })
    expect(await screen.findByText(/Saved team-lf\. Test passed for team-lf/)).toBeInTheDocument()
    expect(screen.queryByRole('form', { name: 'Add destination' })).not.toBeInTheDocument()
  })

  it('shows the backend message when the test fails, saves nothing, and clears the secrets', async () => {
    const calls = setup({ failSave: 'the backend answered HTTP 401 (Unauthorized)' })
    await screen.findByRole('list', { name: 'Destinations' })
    await userEvent.click(screen.getByRole('button', { name: 'Add destination' }))
    const form = screen.getByRole('form', { name: 'Add destination' })
    await userEvent.selectOptions(within(form).getByLabelText('Backend'), 'Datadog')
    await userEvent.type(within(form).getByLabelText('Name'), 'dd')
    await userEvent.type(within(form).getByLabelText('Traces endpoint'), 'https://api.datadoghq.com/x')
    await userEvent.type(within(form).getByLabelText('API key'), fakeKey)
    await userEvent.click(within(form).getByRole('button', { name: 'Test and add' }))
    const alert = await within(form).findByRole('alert')
    expect(alert).toHaveTextContent('Not saved. the backend answered HTTP 401 (Unauthorized)')
    expect(alert).toHaveTextContent('The secrets were cleared from this page')
    expect(within(form).getByLabelText('API key')).toHaveValue('')
    expect(calls.creates).toHaveLength(1)
    expect(screen.getByRole('form', { name: 'Add destination' })).toBeInTheDocument()
  })

  it('validates the preset fields before sending', async () => {
    const calls = setup()
    await screen.findByRole('list', { name: 'Destinations' })
    await userEvent.click(screen.getByRole('button', { name: 'Add destination' }))
    const form = screen.getByRole('form', { name: 'Add destination' })
    await userEvent.click(within(form).getByRole('button', { name: 'Test and add' }))
    expect(within(form).getByText('Name must be 1 to 64 lowercase letters, digits and dashes, starting and ending with a letter or digit, such as langfuse.')).toBeInTheDocument()
    expect(within(form).getByText('Public key is required.')).toBeInTheDocument()
    expect(within(form).getByText('Secret key is required.')).toBeInTheDocument()
    expect(calls.creates).toHaveLength(0)
  })

  it('edits keeping stored secrets: only a typed replacement is sent', async () => {
    const calls = setup()
    await screen.findByRole('list', { name: 'Destinations' })
    await userEvent.click(screen.getByRole('button', { name: 'Edit destination langfuse' }))
    const form = screen.getByRole('form', { name: 'Edit destination langfuse' })
    expect(within(form).getByLabelText('Name')).toBeEnabled()
    await userEvent.type(within(form).getByLabelText('Secret key'), fakeKey)
    expect(within(form).getByRole('button', { name: 'Test and save' })).toBeInTheDocument()
    await userEvent.click(within(form).getByRole('button', { name: 'Test and save' }))
    await waitFor(() => expect(calls.updates).toHaveLength(1))
    expect(calls.updates[0]).toMatchObject({ orgId: 'org-1', destinationId: 'd-lf', expectedVersion: 7n })
    expect(calls.updates[0].destination?.secrets).toEqual({ secret_key: fakeKey })
    expect(calls.updates[0].destination?.removeSecrets).toEqual([])
  })

  it('saves a disable without a test and labels the button Save', async () => {
    const calls = setup()
    await screen.findByRole('list', { name: 'Destinations' })
    await userEvent.click(screen.getByRole('button', { name: 'Edit destination datadog' }))
    const form = screen.getByRole('form', { name: 'Edit destination datadog' })
    expect(within(form).queryByRole('button', { name: 'Test and save' })).not.toBeInTheDocument()
    await userEvent.click(within(form).getByRole('checkbox', { name: 'Enabled' }))
    expect(within(form).getByText(/saved without a test\. The last test result stays/)).toBeInTheDocument()
    await userEvent.click(within(form).getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(calls.updates).toHaveLength(1))
    expect(calls.updates[0].destination).toMatchObject({ enabled: false, secrets: {}, removeSecrets: [] })
  })

  it('changing the endpoint host drops the stored secrets and asks for them again', async () => {
    const calls = setup()
    await screen.findByRole('list', { name: 'Destinations' })
    await userEvent.click(screen.getByRole('button', { name: 'Edit destination langfuse' }))
    const form = screen.getByRole('form', { name: 'Edit destination langfuse' })
    const endpoint = within(form).getByLabelText('Traces endpoint')
    await userEvent.clear(endpoint)
    await userEvent.type(endpoint, 'https://elsewhere.example.com/api/public/otel/v1/traces')
    expect(within(form).getByText(/will not send the stored secrets to the new address/)).toBeInTheDocument()
    expect(within(form).getByLabelText('Secret key')).not.toHaveAttribute('placeholder', 'Set')
    await userEvent.click(within(form).getByRole('button', { name: 'Test and save' }))
    expect(within(form).getByText('Public key is required.')).toBeInTheDocument()
    expect(calls.updates).toHaveLength(0)
  })

  it('reloads and does not overwrite when the destination was changed by someone else', async () => {
    const calls = setup({ updateCode: Code.Aborted })
    await screen.findByRole('list', { name: 'Destinations' })
    const before = calls.listed
    await userEvent.click(screen.getByRole('button', { name: 'Edit destination langfuse' }))
    const form = screen.getByRole('form', { name: 'Edit destination langfuse' })
    await userEvent.click(within(form).getByRole('checkbox', { name: 'Enabled' }))
    await userEvent.click(within(form).getByRole('button', { name: 'Save' }))
    expect(await screen.findByText(/was changed by someone else/)).toBeInTheDocument()
    expect(screen.queryByRole('form', { name: 'Edit destination langfuse' })).not.toBeInTheDocument()
    await waitFor(() => expect(calls.listed).toBeGreaterThan(before))
  })

  it('shows the API message for a refused endpoint', async () => {
    setup({ failSave: 'endpoint must use https' })
    await screen.findByRole('list', { name: 'Destinations' })
    await userEvent.click(screen.getByRole('button', { name: 'Add destination' }))
    const form = screen.getByRole('form', { name: 'Add destination' })
    await userEvent.type(within(form).getByLabelText('Name'), 'lf')
    await userEvent.type(within(form).getByLabelText('Traces endpoint'), 'http://lf.example.com/x')
    await userEvent.type(within(form).getByLabelText('Public key'), 'koinon_test_public')
    await userEvent.type(within(form).getByLabelText('Secret key'), fakeKey)
    await userEvent.click(within(form).getByRole('button', { name: 'Test and add' }))
    expect(await within(form).findByRole('alert')).toHaveTextContent('endpoint must use https')
  })

  it('replaces and removes generic headers by name', async () => {
    const calls = setup()
    await screen.findByRole('list', { name: 'Destinations' })
    await userEvent.click(screen.getByRole('button', { name: 'Edit destination collector' }))
    const form = screen.getByRole('form', { name: 'Edit destination collector' })
    expect(within(form).getByLabelText('Header name 1')).toHaveValue('X-Api-Key')
    expect(within(form).getByLabelText('Header value 1')).toHaveValue('')
    await userEvent.type(within(form).getByLabelText('Header value 1'), fakeKey)
    await userEvent.click(within(form).getByRole('checkbox', { name: 'Remove header X-Team' }))
    await userEvent.click(within(form).getByRole('button', { name: 'Test and save' }))
    await waitFor(() => expect(calls.updates).toHaveLength(1))
    expect(calls.updates[0].destination?.secrets).toEqual({ 'X-Api-Key': fakeKey })
    expect(calls.updates[0].destination?.removeSecrets).toEqual(['X-Team'])
  })

  it('runs the test of a saved destination and shows the backend message', async () => {
    const calls = setup()
    await screen.findByRole('list', { name: 'Destinations' })
    await userEvent.click(screen.getByRole('button', { name: 'Test destination langfuse' }))
    await waitFor(() => expect(calls.tests).toEqual([expect.objectContaining({ orgId: 'org-1', destinationId: 'd-lf' })]))
    expect(await screen.findByText('Test failed for langfuse: the backend answered HTTP 401 (Unauthorized)')).toBeInTheDocument()
  })

  it('deletes after one confirmation', async () => {
    const calls = setup()
    await screen.findByRole('list', { name: 'Destinations' })
    await userEvent.click(screen.getByRole('button', { name: 'Delete destination langfuse' }))
    expect(calls.deletes).toHaveLength(0)
    await userEvent.click(screen.getByRole('button', { name: 'Delete destination' }))
    await waitFor(() => expect(calls.deletes).toEqual([expect.objectContaining({ destinationId: 'd-lf' })]))
  })

  describe('while the org sends full content', () => {
    it('lets an owner add a destination', async () => {
      setup({ level: ContentLevel.FULL })
      await screen.findByRole('list', { name: 'Destinations' })
      expect(await screen.findByRole('button', { name: 'Add destination' })).toBeEnabled()
    })

    it('stops an admin adding a destination or changing a connection, but allows disabling and renaming', async () => {
      const calls = setup({ role: OrgRole.ADMIN, level: ContentLevel.FULL })
      await screen.findByRole('list', { name: 'Destinations' })
      expect(screen.getByRole('button', { name: 'Add destination' })).toBeDisabled()
      expect(screen.getByText(/only an org owner can add a destination or change a destination's connection/)).toBeInTheDocument()
      await userEvent.click(screen.getByRole('button', { name: 'Edit destination langfuse' }))
      const form = screen.getByRole('form', { name: 'Edit destination langfuse' })
      expect(within(form).getByLabelText('Traces endpoint')).toBeDisabled()
      expect(within(form).getByLabelText('Secret key')).toBeDisabled()
      await userEvent.click(within(form).getByRole('checkbox', { name: 'Enabled' }))
      await userEvent.click(within(form).getByRole('button', { name: 'Save' }))
      await waitFor(() => expect(calls.updates).toHaveLength(1))
    })
  })

  describe('content level', () => {
    it('says what each level removes and keeps', async () => {
      setup()
      const form = await screen.findByRole('form', { name: 'Trace settings' })
      expect(within(form).getByText(/email addresses, phone numbers, IBANs, card numbers and high-entropy tokens/)).toBeInTheDocument()
    })

    it('explains a refused raise by a platform admin who is not a member', async () => {
      setup({ settingsCode: Code.PermissionDenied })
      const form = await screen.findByRole('form', { name: 'Trace settings' })
      await userEvent.click(within(form).getByRole('radio', { name: /Redacted/ }))
      await userEvent.click(within(form).getByRole('button', { name: 'Save settings' }))
      expect(await within(form).findByRole('alert')).toHaveTextContent('you can only lower its settings here')
    })

    it('explains each level', async () => {
      setup()
      const form = await screen.findByRole('form', { name: 'Trace settings' })
      expect(within(form).getByRole('radio', { name: /None/ })).toBeChecked()
      expect(within(form).getByText(/No prompts, replies, tool arguments or results/)).toBeInTheDocument()
      expect(within(form).getByText(/removes credentials, email addresses/)).toBeInTheDocument()
      expect(within(form).getByText(/Content is included as is, apart from credentials/)).toBeInTheDocument()
    })

    it('lets only an owner choose full', async () => {
      setup({ role: OrgRole.ADMIN })
      const form = await screen.findByRole('form', { name: 'Trace settings' })
      expect(within(form).getByRole('radio', { name: /Full/ })).toBeDisabled()
      expect(within(form).getByRole('radio', { name: /Redacted/ })).toBeEnabled()
      expect(within(form).getAllByText(/Only an org owner can choose full/).length).toBeGreaterThan(0)
    })

    it('does not let an admin lose the page when full is already set', async () => {
      const calls = setup({ role: OrgRole.ADMIN, level: ContentLevel.FULL })
      const form = await screen.findByRole('form', { name: 'Trace settings' })
      expect(within(form).getByRole('radio', { name: /Full/ })).toBeChecked()
      expect(within(form).getByRole('radio', { name: /Full/ })).toBeEnabled()
      await userEvent.click(within(form).getByRole('button', { name: 'Save settings' }))
      await waitFor(() => expect(calls.settings).toHaveLength(1))
    })

    it('clamps to the platform ceiling and shows it', async () => {
      setup({ ceiling: ContentLevel.REDACTED })
      const form = await screen.findByRole('form', { name: 'Trace settings' })
      expect(within(form).getByText('Redacted', { selector: 'strong' })).toBeInTheDocument()
      expect(within(form).getByRole('radio', { name: /Full/ })).toBeDisabled()
      expect(within(form).getByText(/The platform ceiling is redacted/)).toBeInTheDocument()
      expect(within(form).getByRole('radio', { name: /Redacted/ })).toBeEnabled()
    })

    it('confirms before choosing full, and sends nothing until then', async () => {
      const calls = setup()
      const form = await screen.findByRole('form', { name: 'Trace settings' })
      await userEvent.click(within(form).getByRole('radio', { name: /Full/ }))
      await userEvent.click(within(form).getByRole('button', { name: 'Save settings' }))
      expect(calls.settings).toHaveLength(0)
      const ask = within(form).getByRole('alertdialog', { name: 'Confirm full content' })
      await userEvent.click(within(ask).getByRole('button', { name: 'Cancel' }))
      expect(calls.settings).toHaveLength(0)
      await userEvent.click(within(form).getByRole('button', { name: 'Save settings' }))
      await userEvent.click(within(form).getByRole('button', { name: 'Send full content' }))
      await waitFor(() => expect(calls.settings).toHaveLength(1))
      expect(calls.settings[0]).toMatchObject({ orgId: 'org-1', contentLevel: ContentLevel.FULL })
      expect(calls.settings[0].sampleRate).toBeUndefined()
    })

    it('saves redacted and the sampling rate without a confirmation', async () => {
      const calls = setup()
      const form = await screen.findByRole('form', { name: 'Trace settings' })
      await userEvent.click(within(form).getByRole('radio', { name: /Redacted/ }))
      const rate = within(form).getByLabelText('Share of runs traced (%)')
      expect(rate).toHaveValue('50')
      await userEvent.clear(rate)
      await userEvent.type(rate, '25')
      await userEvent.click(within(form).getByRole('button', { name: 'Save settings' }))
      await waitFor(() => expect(calls.settings).toHaveLength(1))
      expect(calls.settings[0]).toMatchObject({ contentLevel: ContentLevel.REDACTED, sampleRate: 0.25 })
    })

    it('leaves the sampling rate out when it was not changed', async () => {
      const calls = setup()
      const form = await screen.findByRole('form', { name: 'Trace settings' })
      await userEvent.click(within(form).getByRole('radio', { name: /Redacted/ }))
      await userEvent.click(within(form).getByRole('button', { name: 'Save settings' }))
      await waitFor(() => expect(calls.settings).toHaveLength(1))
      expect(calls.settings[0].sampleRate).toBeUndefined()
    })

    it('refuses a sampling rate outside 0 to 100', async () => {
      const calls = setup()
      const form = await screen.findByRole('form', { name: 'Trace settings' })
      const rate = within(form).getByLabelText('Share of runs traced (%)')
      await userEvent.clear(rate)
      await userEvent.type(rate, '150')
      expect(within(form).getByText('Sampling must be a number from 0 to 100.')).toBeInTheDocument()
      await userEvent.click(within(form).getByRole('button', { name: 'Save settings' }))
      expect(calls.settings).toHaveLength(0)
    })
  })

  describe('platform copy', () => {
    it('is hidden without a platform destination', async () => {
      setup()
      const form = await screen.findByRole('form', { name: 'Trace settings' })
      expect(within(form).queryByLabelText(/platform's copy/)).not.toBeInTheDocument()
    })

    it('is held to the org level', async () => {
      const calls = setup({ platform: true, level: ContentLevel.REDACTED, copy: ContentLevel.REDACTED })
      const form = await screen.findByRole('form', { name: 'Trace settings' })
      const copy = within(form).getByLabelText(/platform's copy/)
      expect(within(copy).getByRole('option', { name: 'Full' })).toBeDisabled()
      expect(within(copy).getByRole('option', { name: 'Redacted' })).toBeEnabled()
      expect(copy).toHaveValue(String(ContentLevel.REDACTED))
      // Lowering the org level lowers the copy with it.
      await userEvent.click(within(form).getByRole('radio', { name: /None/ }))
      expect(copy).toHaveValue(String(ContentLevel.NONE))
      await userEvent.click(within(form).getByRole('button', { name: 'Save settings' }))
      await waitFor(() => expect(calls.settings).toHaveLength(1))
      expect(calls.settings[0]).toMatchObject({ contentLevel: ContentLevel.NONE, platformCopyLevel: ContentLevel.NONE })
    })
  })
})
