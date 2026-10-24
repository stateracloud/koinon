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
  type UpdateProviderRequest,
} from '@/gen/koinon/provider/v1alpha1/provider_pb'
import { renderApp, whoAmI } from '@/test-utils'

const t = timestampFromMs(Date.now())
const uiProvider = create(ProviderSchema, { id: 'p-ui', name: 'openrouter', type: 'openrouter', url: 'https://openrouter.ai/api/v1', hasKey: true, source: 'ui', createdAt: t, updatedAt: timestampFromMs(Date.now() - 3_600_000) })
const secretProvider = create(ProviderSchema, { id: 'p-sec', name: 'homelab', type: 'ollama', url: 'http://ollama.lab.example.com:11434/v1', source: 'secret', managedBySecret: 'ollama-secret', createdAt: t })
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
const shadowed = create(ProviderSchema, { id: 'p-shadow', name: 'dup', type: 'openai', url: 'https://api.example.com/v1', source: 'ui', disabledReason: 'shadowed by a provider of the same name declared in a Kubernetes Secret' })
const model = create(ModelSchema, { id: 'm-1', providerId: 'p-ui', name: 'deepseek/deepseek-v4.1-flash', guard: true, source: 'ui' })
const secretModel = create(ModelSchema, { id: 'm-2', providerId: 'p-sec', name: 'llama3', source: 'secret', managedBySecret: 'ollama-secret' })

function setup(opts: { role?: OrgRole; failCreate?: string; failDelete?: string } = {}) {
  const calls = { providerCreates: [] as CreateProviderRequest[], providerUpdates: [] as UpdateProviderRequest[], providerDeletes: [] as DeleteProviderRequest[], modelCreates: [] as CreateModelRequest[], listed: 0 }
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: opts.role }) })
    service(ProviderService, {
      listProviders: () => {
        calls.listed++
        return { providers: [uiProvider, secretProvider, removedProvider, shadowed, fresh] }
      },
      listModels: () => ({ models: [model, secretModel] }),
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
        return { model: create(ModelSchema, { id: 'm-new', providerId: req.providerId, name: req.name, source: 'ui' }) }
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

    const sec = within(within(list).getByRole('region', { name: 'homelab' }))
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
})
