// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import {
  ModelSchema,
  ProviderSchema,
  ProviderService,
  type CreateModelRequest,
  type CreateProviderRequest,
  type DeleteProviderRequest,
  type ModelFormatsRequest,
  type UpdateModelRequest,
  type UpdateProviderRequest,
} from '@/gen/koinon/provider/v1alpha1/provider_pb'
import { renderApp, whoAmI } from '@/test-utils'

const t = timestampFromMs(Date.now())
const uiProvider = create(ProviderSchema, { id: 'p-ui', name: 'openrouter', type: 'openrouter', url: 'https://openrouter.ai/api/v1', hasKey: true, source: 'ui', formats: [{ id: 'openai', label: 'OpenAI chat completions' }, { id: 'anthropic', label: 'Anthropic Messages' }], createdAt: t, updatedAt: timestampFromMs(Date.now() - 3_600_000) })
const claude = create(ProviderSchema, { id: 'p-claude', name: 'claude', type: 'anthropic', url: 'https://api.anthropic.com/v1', source: 'ui', anthropicWorkspaceId: 'wrkspc_01Keep', formats: [{ id: 'anthropic', label: 'Anthropic Messages' }], createdAt: t, updatedAt: t })
const single = [{ id: 'openai', label: 'OpenAI chat completions' }]
const secretProvider = create(ProviderSchema, { id: 'p-sec', name: 'local-ollama', type: 'ollama', url: 'http://ollama.lab.example.com:11434/v1', source: 'secret', managedBySecret: 'ollama-secret', createdAt: t })
const removedProvider = create(ProviderSchema, {
  id: 'p-gone',
  name: 'old',
  type: 'openai',
  url: 'https://api.example.com/v1',
  source: 'secret',
  managedBySecret: 'old-secret',
  removed: true,
  disabledReason: 'its Kubernetes Secret was deleted; restore the Secret or delete this entry',
})
const fresh = create(ProviderSchema, { id: 'p-fresh', name: 'fresh', type: 'ollama', url: 'http://ollama.lab.example.com:11434/v1', source: 'ui', createdAt: t, updatedAt: t })
const shadowed = create(ProviderSchema, { id: 'p-shadow', name: 'dup', type: 'openai', formats: single, url: 'https://api.example.com/v1', source: 'ui', disabledReason: 'shadowed by a provider of the same name declared in a Kubernetes Secret' })
const model = create(ModelSchema, { id: 'm-1', providerId: 'p-ui', name: 'deepseek/deepseek-v4.1-flash', guard: true, source: 'ui', format: 'openai' })
const secretModel = create(ModelSchema, { id: 'm-2', providerId: 'p-sec', name: 'llama3', source: 'secret', managedBySecret: 'ollama-secret' })

function setup(opts: { role?: OrgRole; failCreate?: string; failDelete?: string; warnings?: string[]; overridden?: boolean; slowTypeInfo?: Promise<void>; withAnthropic?: boolean; holdModelsRefetchAfterCreate?: Promise<void> } = {}) {
  const calls = { providerCreates: [] as CreateProviderRequest[], providerUpdates: [] as UpdateProviderRequest[], providerDeletes: [] as DeleteProviderRequest[], modelCreates: [] as CreateModelRequest[], modelUpdates: [] as UpdateModelRequest[], formatAsks: [] as ModelFormatsRequest[], listed: 0 }
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: opts.role }) })
    service(ProviderService, {
      getDefaultModel: () => ({ model: '', available: [] }),
      listProviders: () => {
        calls.listed++
        return { providers: [uiProvider, secretProvider, removedProvider, shadowed, fresh, ...(opts.withAnthropic ? [claude] : [])] }
      },
      listModels: async () => {
        // Hold the refetch that follows a provider create, so the form's completion arrives late.
        if (opts.holdModelsRefetchAfterCreate && calls.providerCreates.length > 0) await opts.holdModelsRefetchAfterCreate
        return { models: [opts.overridden ? create(ModelSchema, { ...model, format: 'anthropic', formatOverridden: true }) : model, secretModel] }
      },
      modelFormats: (req) => {
        calls.formatAsks.push(req)
        return { defaultFormat: req.name.startsWith('claude') ? 'anthropic' : 'openai' }
      },
      providerTypeInfo: async (req) => {
        if (opts.slowTypeInfo) await opts.slowTypeInfo
        return { formats: [], takesWorkspaceId: req.type === 'anthropic' }
      },
      updateModel: (req) => {
        calls.modelUpdates.push(req)
        return { model }
      },
      createProvider: (req) => {
        calls.providerCreates.push(req)
        if (opts.failCreate) throw new ConnectError(opts.failCreate, Code.InvalidArgument)
        return { provider: create(ProviderSchema, { id: 'p-new', name: req.name, type: req.type, url: req.url, hasKey: !!req.apiKey, source: 'ui' }) }
      },
      updateProvider: (req) => {
        calls.providerUpdates.push(req)
        return { provider: uiProvider }
      },
      deleteProvider: (req) => {
        calls.providerDeletes.push(req)
        if (opts.failDelete) throw new ConnectError(opts.failDelete, Code.FailedPrecondition)
        return {}
      },
      createModel: (req) => {
        calls.modelCreates.push(req)
        return { model: create(ModelSchema, { id: 'm-new', providerId: req.providerId, name: req.name, source: 'ui' }), warnings: opts.warnings ?? [] }
      },
    })
  })
  renderApp(transport, '/settings/providers')
  return calls
}

describe('Providers page', () => {
  it('tells UI providers from Secret-managed ones and shows why one is off', async () => {
    setup()
    const list = await screen.findByRole('list', { name: 'Providers' })
    const ui = within(within(list).getByRole('region', { name: 'openrouter' }))
    expect(ui.getByText('Added in the UI')).toBeInTheDocument()
    expect(ui.getByText('Stored encrypted; it cannot be shown')).toBeInTheDocument()
    expect(ui.getByRole('button', { name: 'Edit provider openrouter' })).toBeInTheDocument()
    expect(ui.getByText('default/deepseek/deepseek-v4.1-flash')).toBeInTheDocument()
    expect(ui.getByText('Routing guard')).toBeInTheDocument()
    expect(ui.queryByText(/per million tokens/)).not.toBeInTheDocument()
    expect(ui.getByRole('link', { name: 'Prices page' })).toHaveAttribute('href', '/settings/prices')

    const sec = within(within(list).getByRole('region', { name: 'local-ollama' }))
    expect(sec.getByText('Managed by Secret ollama-secret')).toBeInTheDocument()
    expect(sec.queryByRole('button', { name: /Edit provider/ })).not.toBeInTheDocument()
    expect(sec.queryByRole('button', { name: /Delete provider/ })).not.toBeInTheDocument()
    expect(sec.queryByRole('button', { name: /Add model/ })).not.toBeInTheDocument()
    expect(sec.queryByRole('button', { name: /model llama3/ })).not.toBeInTheDocument()

    const gone = within(within(list).getByRole('region', { name: 'old' }))
    expect(gone.getAllByText(/Secret was deleted/).length).toBeGreaterThan(0)
    expect(gone.getByRole('button', { name: 'Delete provider old' })).toBeInTheDocument()
    expect(gone.queryByRole('button', { name: /Edit provider/ })).not.toBeInTheDocument()

    const off = within(within(list).getByRole('region', { name: 'dup' }))
    expect(off.getByText(/shadowed by a provider of the same name/)).toBeInTheDocument()
  })

  it('adds a provider with a write-only key and clears the key from the page', async () => {
    const calls = setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Add provider' }))
    const form = screen.getByRole('form', { name: 'Add provider' })
    await userEvent.type(within(form).getByLabelText('Name'), 'second')
    const key = within(form).getByLabelText('API key')
    expect(key).toHaveAttribute('type', 'password')
    await userEvent.type(key, 'sk-or-secret-value')
    await userEvent.click(within(form).getByRole('button', { name: 'Add provider' }))
    await waitFor(() => expect(calls.providerCreates).toHaveLength(1))
    expect(calls.providerCreates[0]).toMatchObject({ orgId: 'org-1', name: 'second', type: 'openrouter', url: 'https://openrouter.ai/api/v1', apiKey: 'sk-or-secret-value' })
    await waitFor(() => expect(screen.queryByRole('form', { name: 'Add provider' })).not.toBeInTheDocument())
    expect(document.body.textContent).not.toContain('sk-or-secret-value')
  })

  it('keeps a model form the person opened while a provider save is still refetching (old issue 520)', async () => {
    let release = () => {}
    const hold = new Promise<void>((r) => (release = r))
    const calls = setup({ holdModelsRefetchAfterCreate: hold })
    await userEvent.click(await screen.findByRole('button', { name: 'Add provider' }))
    const form = screen.getByRole('form', { name: 'Add provider' })
    await userEvent.type(within(form).getByLabelText('Name'), 'second')
    await userEvent.type(within(form).getByLabelText('API key'), 'koinon_test_not-a-real-token')
    await userEvent.click(within(form).getByRole('button', { name: 'Add provider' }))
    await waitFor(() => expect(calls.providerCreates).toHaveLength(1))

    // The create has returned but its refetch is pending, so the provider form is still open.
    await userEvent.click(screen.getByRole('button', { name: 'Add model to openrouter' }))
    expect(screen.getByRole('form', { name: 'Add model to openrouter' })).toBeInTheDocument()

    release()
    await waitFor(() => expect(screen.queryByRole('form', { name: 'Add provider' })).not.toBeInTheDocument())
    expect(screen.getByRole('form', { name: 'Add model to openrouter' })).toBeInTheDocument()
  })

  it('shows the server\'s URL refusal plainly and keeps the form open without the key', async () => {
    const msg = 'the url\'s host "127.0.0.1" is not allowed: it is a loopback, link-local, private, shared or metadata address (a platform admin can allow it with egress.allowedInternalHosts)'
    setup({ failCreate: msg })
    await userEvent.click(await screen.findByRole('button', { name: 'Add provider' }))
    const form = screen.getByRole('form', { name: 'Add provider' })
    await userEvent.type(within(form).getByLabelText('Name'), 'local')
    await userEvent.clear(within(form).getByLabelText('URL'))
    await userEvent.type(within(form).getByLabelText('URL'), 'http://127.0.0.1:11434/v1')
    await userEvent.type(within(form).getByLabelText('API key'), 'hunter2hunter2')
    await userEvent.click(within(form).getByRole('button', { name: 'Add provider' }))
    expect(await within(form).findByRole('alert')).toHaveTextContent(msg)
    expect(within(form).getByLabelText('API key')).toHaveValue('')
  })

  it('validates the name and URL before sending', async () => {
    const calls = setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Add provider' }))
    const form = screen.getByRole('form', { name: 'Add provider' })
    await userEvent.type(within(form).getByLabelText('Name'), 'Bad Name')
    await userEvent.click(within(form).getByRole('button', { name: 'Add provider' }))
    expect(within(form).getByText(/lowercase letters, digits and dashes/)).toBeInTheDocument()
    expect(calls.providerCreates).toHaveLength(0)
  })

  it('edits without touching the key unless a new one is typed or removal is asked for', async () => {
    const calls = setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Edit provider openrouter' }))
    const form = screen.getByRole('form', { name: 'Edit provider openrouter' })
    expect(within(form).getByLabelText('New API key')).toHaveValue('')
    expect(within(form).getByLabelText('Name')).toBeDisabled()
    await userEvent.clear(within(form).getByLabelText('URL'))
    await userEvent.type(within(form).getByLabelText('URL'), 'https://proxy.example.com/v1')
    await userEvent.click(within(form).getByRole('button', { name: 'Save provider' }))
    await waitFor(() => expect(calls.providerUpdates).toHaveLength(1))
    expect(calls.providerUpdates[0].url).toBe('https://proxy.example.com/v1')
    expect(calls.providerUpdates[0].apiKey).toBeUndefined()

    await userEvent.click(await screen.findByRole('button', { name: 'Edit provider openrouter' }))
    const again = screen.getByRole('form', { name: 'Edit provider openrouter' })
    await userEvent.click(within(again).getByLabelText('Remove the stored key'))
    await userEvent.click(within(again).getByRole('button', { name: 'Save provider' }))
    await waitFor(() => expect(calls.providerUpdates).toHaveLength(2))
    expect(calls.providerUpdates[1].apiKey).toBe('')
  })

  it('offers the routing guard only for OpenRouter providers, with its explanation', async () => {
    const calls = setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Add model to openrouter' }))
    const form = screen.getByRole('form', { name: 'Add model to openrouter' })
    expect(within(form).getByLabelText('Routing guard')).toBeInTheDocument()
    expect(within(form).getByText(/zero data retention/)).toBeInTheDocument()
    await userEvent.type(within(form).getByLabelText('Model name'), 'qwen/qwen3')
    expect(within(form).getByText('default/qwen/qwen3')).toBeInTheDocument()
    expect(within(form).queryByLabelText('Input rate')).not.toBeInTheDocument()
    expect(within(form).getByRole('link', { name: 'Prices page' })).toHaveAttribute('href', '/settings/prices')
    await userEvent.click(within(form).getByLabelText('Routing guard'))
    await userEvent.click(within(form).getByRole('button', { name: 'Add model' }))
    await waitFor(() => expect(calls.modelCreates).toHaveLength(1))
    expect(calls.modelCreates[0]).toMatchObject({ providerId: 'p-ui', name: 'qwen/qwen3', guard: true })
    expect(screen.queryByRole('status', { name: 'Warnings' })).not.toBeInTheDocument()
  })

  it('does not offer the routing guard for other providers', async () => {
    setup()
    // dup is a UI provider of type openai
    await userEvent.click(await screen.findByRole('button', { name: 'Add model to dup' }))
    expect(screen.queryByLabelText('Routing guard')).not.toBeInTheDocument()
  })

  it('confirms before deleting a provider and says what goes with it', async () => {
    const calls = setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Delete provider openrouter' }))
    expect(screen.getByText(/Delete openrouter and its 1 model\?/)).toBeInTheDocument()
    expect(calls.providerDeletes).toHaveLength(0)
    await userEvent.click(screen.getByRole('button', { name: 'Delete provider' }))
    await waitFor(() => expect(calls.providerDeletes).toEqual([expect.objectContaining({ providerId: 'p-ui' })]))
  })

  it('shows a refused delete', async () => {
    setup({ failDelete: 'this provider is declared by the Secret x' })
    await userEvent.click(await screen.findByRole('button', { name: 'Delete provider openrouter' }))
    await userEvent.click(screen.getByRole('button', { name: 'Delete provider' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('declared by the Secret x')
  })

  it('is hidden from members: no navigation, no requests', async () => {
    const calls = setup({ role: OrgRole.MEMBER })
    expect(await screen.findByRole('alert')).toHaveTextContent('Only org admins and owners can manage model providers.')
    expect(screen.queryByRole('link', { name: 'Providers' })).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'API tokens' })).not.toBeInTheDocument()
    expect(calls.listed).toBe(0)
  })

  it('shows admins the Settings links', async () => {
    setup({ role: OrgRole.ADMIN })
    const nav = await screen.findByRole('navigation', { name: 'Settings' })
    expect(within(nav).getByRole('link', { name: 'Providers' })).toHaveAttribute('aria-current', 'page')
    expect(within(nav).getByRole('link', { name: 'API tokens' })).toBeInTheDocument()
  })

  it('says a just-saved provider is not available yet, and an older one is active', async () => {
    setup()
    const list = await screen.findByRole('list', { name: 'Providers' })
    expect(within(within(list).getByRole('region', { name: 'fresh' })).getByText(/Saved, available to agents within about 3 minutes/)).toBeInTheDocument()
    const old = within(within(list).getByRole('region', { name: 'openrouter' }))
    expect(old.getByText('Active')).toBeInTheDocument()
    expect(old.queryByText(/Saved, available/)).not.toBeInTheDocument()
  })

  it('says the same in the notice after a save', async () => {
    setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Add provider' }))
    const form = screen.getByRole('form', { name: 'Add provider' })
    await userEvent.type(within(form).getByLabelText('Name'), 'second')
    await userEvent.click(within(form).getByRole('button', { name: 'Add provider' }))
    expect(await screen.findByRole('status')).toHaveTextContent('available to agents within about 3 minutes')
  })

  it('asks for the key again when a save fails after the key was cleared', async () => {
    setup({ failCreate: 'upstream refused' })
    await userEvent.click(await screen.findByRole('button', { name: 'Add provider' }))
    const form = screen.getByRole('form', { name: 'Add provider' })
    await userEvent.type(within(form).getByLabelText('Name'), 'second')
    await userEvent.type(within(form).getByLabelText('API key'), 'sk-secret')
    await userEvent.click(within(form).getByRole('button', { name: 'Add provider' }))
    expect(await within(form).findByRole('alert')).toHaveTextContent('Type the API key again.')
  })

  it('links a field error to its field', async () => {
    setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Add provider' }))
    const form = screen.getByRole('form', { name: 'Add provider' })
    await userEvent.type(within(form).getByLabelText('Name'), 'Bad Name')
    await userEvent.click(within(form).getByRole('button', { name: 'Add provider' }))
    expect(within(form).getByLabelText('Name')).toHaveAccessibleDescription(/lowercase letters, digits and dashes/)
  })

  it('moves focus into the delete question and back to the button when it is dismissed', async () => {
    setup()
    const trigger = await screen.findByRole('button', { name: 'Delete provider openrouter' })
    await userEvent.click(trigger)
    expect(screen.getByRole('button', { name: 'Delete provider' })).toHaveFocus()
    await userEvent.click(screen.getByRole('button', { name: 'Keep it' }))
    expect(screen.getByRole('button', { name: 'Delete provider openrouter' })).toHaveFocus()
  })

  it('shows the warning when a new model would run without prompt caching', async () => {
    const warning = 'Model google/gemini-x: the model has a cache_read price but Koinon has no way to turn on prompt caching'
    setup({ warnings: [warning] })
    await userEvent.click(await screen.findByRole('button', { name: 'Add model to openrouter' }))
    const form = screen.getByRole('form', { name: 'Add model to openrouter' })
    await userEvent.type(within(form).getByLabelText('Model name'), 'google/gemini-x')
    await userEvent.click(within(form).getByRole('button', { name: 'Add model' }))
    expect(await within(await screen.findByRole('status', { name: 'Warnings' })).findByText(warning)).toBeInTheDocument()
  })

  describe('model formats (ADR 0069)', () => {
    it('badges each model with its format, and says when it was set', async () => {
      setup()
      const list = await screen.findByRole('list', { name: 'Providers' })
      const ui = within(within(list).getByRole('region', { name: 'openrouter' }))
      expect(ui.getByText('OpenAI chat completions')).toHaveAttribute('title', 'The default for this model')
    })

    it('shows an overridden format as set', async () => {
      setup({ overridden: true })
      const ui = within(within(await screen.findByRole('list', { name: 'Providers' })).getByRole('region', { name: 'openrouter' }))
      expect(ui.getByText('Anthropic Messages (set)')).toHaveAttribute('title', 'Set for this model')
    })

    it('offers the provider\'s formats and a Default option that names the default', async () => {
      const calls = setup()
      await userEvent.click(await screen.findByRole('button', { name: 'Add model to openrouter' }))
      const form = screen.getByRole('form', { name: 'Add model to openrouter' })
      await userEvent.type(within(form).getByLabelText('Model name'), 'claude-haiku-4-5')
      const select = within(form).getByLabelText('API format')
      await waitFor(() => expect(within(select).getByRole('option', { name: 'Default (Anthropic Messages)' })).toBeInTheDocument())
      expect(within(select).getByRole('option', { name: 'OpenAI chat completions' })).toBeInTheDocument()
      expect(select).toHaveValue('')
      expect(calls.formatAsks.at(-1)).toMatchObject({ providerId: 'p-ui', name: 'claude-haiku-4-5' })
      await userEvent.click(within(form).getByRole('button', { name: 'Add model' }))
      await waitFor(() => expect(calls.modelCreates).toHaveLength(1))
      // The default is followed, not pinned.
      expect(calls.modelCreates[0].format).toBe('')
    })

    it('can pin the format that is the default today, and sends the choice as it is', async () => {
      const calls = setup()
      await userEvent.click(await screen.findByRole('button', { name: 'Add model to openrouter' }))
      const form = screen.getByRole('form', { name: 'Add model to openrouter' })
      await userEvent.type(within(form).getByLabelText('Model name'), 'claude-haiku-4-5')
      const select = within(form).getByLabelText('API format')
      await waitFor(() => expect(within(select).getByRole('option', { name: 'Default (Anthropic Messages)' })).toBeInTheDocument())
      await userEvent.selectOptions(select, 'anthropic')
      await userEvent.click(within(form).getByRole('button', { name: 'Add model' }))
      await waitFor(() => expect(calls.modelCreates).toHaveLength(1))
      expect(calls.modelCreates[0].format).toBe('anthropic')
    })

    it('has nothing to choose on a provider that offers one format', async () => {
      setup()
      await userEvent.click(await screen.findByRole('button', { name: 'Add model to dup' }))
      const form = screen.getByRole('form', { name: 'Add model to dup' })
      await userEvent.type(within(form).getByLabelText('Model name'), 'gpt-5')
      expect(within(form).getByText(/offers one format/)).toBeInTheDocument()
      expect(within(form).getByLabelText('API format')).toBeDisabled()
    })

    it('keeps a pinned format through a guard-only edit', async () => {
      const calls = setup({ overridden: true })
      await userEvent.click(await screen.findByRole('button', { name: 'Edit model deepseek/deepseek-v4.1-flash' }))
      const form = screen.getByRole('form', { name: 'Edit model deepseek/deepseek-v4.1-flash' })
      expect(within(form).getByLabelText('API format')).toHaveValue('anthropic')
      await userEvent.click(within(form).getByLabelText('Routing guard'))
      await userEvent.click(within(form).getByRole('button', { name: 'Save model' }))
      await waitFor(() => expect(calls.modelUpdates).toHaveLength(1))
      expect(calls.modelUpdates[0].guard).toBe(false)
      expect(calls.modelUpdates[0].format).toBeUndefined()
    })

    it('goes back to the default when an edit picks Default', async () => {
      const calls = setup({ overridden: true })
      await userEvent.click(await screen.findByRole('button', { name: 'Edit model deepseek/deepseek-v4.1-flash' }))
      const form = screen.getByRole('form', { name: 'Edit model deepseek/deepseek-v4.1-flash' })
      await userEvent.selectOptions(within(form).getByLabelText('API format'), '')
      await userEvent.click(within(form).getByRole('button', { name: 'Save model' }))
      await waitFor(() => expect(calls.modelUpdates).toHaveLength(1))
      expect(calls.modelUpdates[0].format).toBe('')
    })
  })

  describe('Anthropic providers', () => {
    it('cannot save before the server has said what the type takes, so a stored workspace ID is never cleared', async () => {
      let release!: () => void
      const gate = new Promise<void>((r) => (release = r))
      const calls = setup({ slowTypeInfo: gate })
      await userEvent.click(await screen.findByRole('button', { name: 'Add provider' }))
      const form = screen.getByRole('form', { name: 'Add provider' })
      await userEvent.type(within(form).getByLabelText('Name'), 'quick')
      expect(within(form).getByRole('button', { name: 'Add provider' })).toBeDisabled()
      release()
      await waitFor(() => expect(within(form).getByRole('button', { name: 'Add provider' })).toBeEnabled())
      await userEvent.click(within(form).getByRole('button', { name: 'Add provider' }))
      await waitFor(() => expect(calls.providerCreates).toHaveLength(1))
      expect(calls.providerCreates[0].anthropicWorkspaceId).toBe('')
    })

    it('keeps the stored workspace ID of an Anthropic provider when it is edited', async () => {
      let release!: () => void
      const gate = new Promise<void>((r) => (release = r))
      const calls = setup({ slowTypeInfo: gate, withAnthropic: true })
      await userEvent.click(await screen.findByRole('button', { name: 'Edit provider claude' }))
      const form = screen.getByRole('form', { name: 'Edit provider claude' })
      expect(within(form).getByRole('button', { name: 'Save provider' })).toBeDisabled()
      release()
      const ws = await within(form).findByLabelText('Anthropic workspace ID')
      expect(ws).toHaveValue('wrkspc_01Keep')
      expect(ws).toHaveAccessibleDescription(/not a secret.*wrkspc_ followed by letters/s)
      await userEvent.clear(within(form).getByLabelText('URL'))
      await userEvent.type(within(form).getByLabelText('URL'), 'https://proxy.example.com/v1')
      await userEvent.click(within(form).getByRole('button', { name: 'Save provider' }))
      await waitFor(() => expect(calls.providerUpdates).toHaveLength(1))
      expect(calls.providerUpdates[0].anthropicWorkspaceId).toBeUndefined()
    })

    it('lists Anthropic as a type and asks for the workspace ID for it only', async () => {
      const calls = setup()
      await userEvent.click(await screen.findByRole('button', { name: 'Add provider' }))
      const form = screen.getByRole('form', { name: 'Add provider' })
      expect(within(form).queryByLabelText('Anthropic workspace ID')).not.toBeInTheDocument()
      await userEvent.selectOptions(within(form).getByLabelText('Provider type'), 'anthropic')
      await within(form).findByLabelText('Anthropic workspace ID')
      expect(within(form).getByLabelText('URL')).toHaveValue('https://api.anthropic.com/v1')
      const ws = within(form).getByLabelText('Anthropic workspace ID')
      await userEvent.type(within(form).getByLabelText('Name'), 'claude')
      await userEvent.type(ws, 'wrkspc_01AbCd')
      await userEvent.click(within(form).getByRole('button', { name: 'Add provider' }))
      await waitFor(() => expect(calls.providerCreates).toHaveLength(1))
      expect(calls.providerCreates[0]).toMatchObject({ name: 'claude', type: 'anthropic', anthropicWorkspaceId: 'wrkspc_01AbCd' })
    })

    it('does not send a workspace ID for another type', async () => {
      const calls = setup()
      await userEvent.click(await screen.findByRole('button', { name: 'Add provider' }))
      const form = screen.getByRole('form', { name: 'Add provider' })
      await userEvent.type(within(form).getByLabelText('Name'), 'second')
      await userEvent.click(within(form).getByRole('button', { name: 'Add provider' }))
      await waitFor(() => expect(calls.providerCreates).toHaveLength(1))
      expect(calls.providerCreates[0].anthropicWorkspaceId).toBe('')
    })
  })
})
