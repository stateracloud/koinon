// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AgentSchema, AgentService, AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import {
  Health,
  McpServerService,
  ServerSchema,
  Tier,
  ToolSchema,
  type AcknowledgeToolReviewRequest,
  type CreateServerRequest,
  type DeleteServerRequest,
  type SetAgentServersRequest,
  type SetToolTierRequest,
  type SetToolTwoApproversRequest,
  type UpdateServerRequest,
} from '@/gen/koinon/mcpserver/v1alpha1/mcpserver_pb'
import { renderApp, whoAmI } from '@/test-utils'

const checked = timestampFromMs(Date.now() - 60_000)
const docs = create(ServerSchema, {
  id: 's-docs',
  name: 'docs',
  url: 'https://mcp.example.com/mcp',
  transport: 'streamable-http',
  hasCredential: true,
  credentialHeader: 'Authorization',
  credentialPrefix: 'Bearer ',
  source: 'ui',
  health: Health.HEALTHY,
  healthCheckedAt: checked,
  gatewayTarget: 'default-docs',
})
const issues = create(ServerSchema, {
  id: 's-issues',
  name: 'issues',
  url: 'https://issues.example.com/mcp',
  transport: 'sse',
  source: 'ui',
  health: Health.UNHEALTHY,
  healthReason: 'connection refused',
  healthCheckedAt: checked,
  gatewayTarget: 'default-issues',
})
const managed = create(ServerSchema, {
  id: 's-sec',
  name: 'wiki',
  url: 'https://wiki.example.com/mcp',
  transport: 'streamable-http',
  hasCredential: true,
  credentialHeader: 'X-Api-Key',
  source: 'secret',
  managedBySecret: 'wiki-secret',
  health: Health.UNKNOWN,
  gatewayTarget: 'default-wiki',
})
const gone = create(ServerSchema, { id: 's-gone', name: 'old', url: 'https://old.example.com/mcp', source: 'secret', managedBySecret: 'old-secret', removed: true, disabledReason: 'its Kubernetes Secret was deleted' })

const tools = [
  create(ToolSchema, { serverId: 's-docs', name: 'search', description: 'Search the docs', tier: Tier.ASK, readOnlyHint: true, present: true }),
  create(ToolSchema, { serverId: 's-docs', name: 'publish', description: 'Publish a page', tier: Tier.NEVER, present: true }),
  create(ToolSchema, { serverId: 's-docs', name: 'edit', description: 'Edit a page', tier: Tier.ASK, present: true, reviewReason: 'the description changed' }),
  create(ToolSchema, { serverId: 's-docs', name: 'legacy', tier: Tier.AUTO, present: false }),
  create(ToolSchema, { serverId: 's-docs', name: 'long_tool', description: 'Too long', tier: Tier.ASK, present: true, unavailable: 'unavailable: name too long for the gateway' }),
  create(ToolSchema, { serverId: 's-docs', name: 'sync', description: 'Sync pages', tier: Tier.AUTO, present: true }),
  create(ToolSchema, { serverId: 's-docs', name: 'poisoned', description: 'Search…', tier: Tier.ASK, present: true, descriptionOriginalLength: 5000n, descriptionCapped: true, descriptionCleaned: true }),
  create(ToolSchema, { serverId: 's-sec', name: 'read', description: 'Read a page', tier: Tier.ASK, present: true }),
]

function setup(opts: { role?: OrgRole; path?: string; failCreate?: string; failDelete?: string; allowed?: { serverId: string; tools: string[] }[] } = {}) {
  const calls = {
    tiers: [] as SetToolTierRequest[],
    twos: [] as SetToolTwoApproversRequest[],
    acks: [] as AcknowledgeToolReviewRequest[],
    creates: [] as CreateServerRequest[],
    updates: [] as UpdateServerRequest[],
    deletes: [] as DeleteServerRequest[],
    allowSets: [] as SetAgentServersRequest[],
    listed: 0,
  }
  const current = new Map(tools.map((t) => [`${t.serverId}/${t.name}`, t]))
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: opts.role }) })
    service(ConnectionService, { listConnections: () => ({ connections: [] }) })
    service(AgentService, { getAgent: () => ({ agent: create(AgentSchema, { id: 'a-1', orgId: 'org-1', displayName: 'Helper', status: AgentStatus.ACTIVE, latestVersion: 1, activeVersion: 1 }) }) })
    service(McpServerService, {
      listServers: () => {
        calls.listed++
        return { servers: [docs, issues, managed, gone] }
      },
      listTools: () => ({ tools: [...current.values()] }),
      createServer: (req) => {
        calls.creates.push(req)
        if (opts.failCreate) throw new ConnectError(opts.failCreate, Code.InvalidArgument)
        return { server: create(ServerSchema, { id: 's-new', name: req.name, url: req.url, source: 'ui' }) }
      },
      updateServer: (req) => {
        calls.updates.push(req)
        return { server: docs }
      },
      deleteServer: (req) => {
        calls.deletes.push(req)
        if (opts.failDelete) throw new ConnectError(opts.failDelete, Code.FailedPrecondition)
        return {}
      },
      setToolTier: (req) => {
        calls.tiers.push(req)
        const t = create(ToolSchema, { ...current.get(`${req.serverId}/${req.tool}`)!, tier: req.tier, reviewReason: '' })
        current.set(`${req.serverId}/${req.tool}`, t)
        return { tool: t }
      },
      setToolTwoApprovers: (req) => {
        calls.twos.push(req)
        const t = create(ToolSchema, { ...current.get(`${req.serverId}/${req.tool}`)!, requiresTwoApprovers: req.required })
        current.set(`${req.serverId}/${req.tool}`, t)
        return { tool: t }
      },
      acknowledgeToolReview: (req) => {
        calls.acks.push(req)
        const t = create(ToolSchema, { ...current.get(`${req.serverId}/${req.tool}`)!, reviewReason: '' })
        current.set(`${req.serverId}/${req.tool}`, t)
        return { tool: t }
      },
      getAgentServers: () => ({ servers: opts.allowed ?? [] }),
      setAgentServers: (req) => {
        calls.allowSets.push(req)
        return { servers: req.servers }
      },
    })
  })
  renderApp(transport, opts.path ?? '/servers')
  return calls
}

describe('Servers page', () => {
  it('lists servers with source, health as text, credential state and gateway target', async () => {
    setup()
    const list = await screen.findByRole('list', { name: 'Servers' })
    const d = within(within(list).getByRole('region', { name: 'docs' }))
    expect(d.getByText('Healthy')).toBeInTheDocument()
    expect(d.getByText('UI')).toBeInTheDocument()
    expect(d.getByText('https://mcp.example.com/mcp')).toBeInTheDocument()
    expect(d.getByText('Streamable HTTP')).toBeInTheDocument()
    expect(d.getByText('Stored encrypted; it cannot be shown')).toBeInTheDocument()
    expect(d.getByText('default-docs')).toBeInTheDocument()
    expect(d.getByRole('button', { name: 'Edit server docs' })).toBeInTheDocument()

    const i = within(within(list).getByRole('region', { name: 'issues' }))
    expect(i.getByText('Unhealthy', { selector: '[data-slot=badge]' })).toBeInTheDocument()
    expect(i.getByText(/connection refused/)).toBeInTheDocument()
    expect(i.getByText(/withdrawn from agents/)).toBeInTheDocument()

    const m = within(within(list).getByRole('region', { name: 'wiki' }))
    expect(m.getByText('Unknown')).toBeInTheDocument()
    expect(m.getByText('Managed by Secret wiki-secret')).toBeInTheDocument()
    expect(m.queryByRole('button', { name: /Edit server/ })).not.toBeInTheDocument()
    expect(m.queryByRole('button', { name: /Delete server/ })).not.toBeInTheDocument()

    const r = within(within(list).getByRole('region', { name: 'old' }))
    expect(r.getByText('Removed')).toBeInTheDocument()
    expect(r.getByRole('button', { name: 'Delete server old' })).toBeInTheDocument()
    expect(r.queryByRole('button', { name: /Edit server/ })).not.toBeInTheDocument()
  })

  it('never renders a credential value', async () => {
    setup()
    await screen.findByRole('list', { name: 'Servers' })
    await userEvent.click(screen.getByRole('button', { name: 'Edit server docs' }))
    const form = screen.getByRole('form', { name: 'Edit server docs' })
    expect(within(form).getByLabelText('Token')).toHaveValue('')
    expect(within(form).getByLabelText('Token')).toHaveAttribute('type', 'password')
    expect(within(form).getByText(/A credential is stored\. Leave empty to keep it\./)).toBeInTheDocument()
    expect(document.body.textContent).not.toContain('not-a-real-token')
  })

  it('shows each tool with its tier, description, review flag and missing state', async () => {
    setup()
    const tl = within(await screen.findByRole('list', { name: 'Tools of docs' }))
    const search = within(tl.getByText('search').closest('li')!)
    expect(search.getByText('Search the docs')).toBeInTheDocument()
    expect(search.getByRole('radio', { name: 'Ask' })).toBeChecked()
    const publish = within(tl.getByText('publish').closest('li')!)
    expect(publish.getByRole('radio', { name: 'Never' })).toBeChecked()
    expect(tl.getByText('no longer listed')).toBeInTheDocument()
    const edit = within(tl.getByText('edit').closest('li')!)
    expect(edit.getByText('Review', { selector: '[data-slot=badge]' })).toBeInTheDocument()
    expect(edit.getByText(/the description changed/)).toBeInTheDocument()
  })

  it('sets a tool tier with SetToolTier', async () => {
    const calls = setup()
    const tl = within(await screen.findByRole('list', { name: 'Tools of docs' }))
    await userEvent.click(within(tl.getByText('publish').closest('li')!).getByRole('radio', { name: 'Auto' }))
    await waitFor(() => expect(calls.tiers).toHaveLength(1))
    expect(calls.tiers[0]).toMatchObject({ orgId: 'org-1', serverId: 's-docs', tool: 'publish', tier: Tier.AUTO })
  })

  it('sets the tier of a tool on a Secret-managed server', async () => {
    const calls = setup()
    const tl = within(await screen.findByRole('list', { name: 'Tools of wiki' }))
    await userEvent.click(tl.getByRole('radio', { name: 'Never' }))
    await waitFor(() => expect(calls.tiers).toEqual([expect.objectContaining({ serverId: 's-sec', tool: 'read', tier: Tier.NEVER })]))
  })

  it('only suggests Auto for a read-only tool until the admin confirms', async () => {
    const calls = setup()
    const tl = within(await screen.findByRole('list', { name: 'Tools of docs' }))
    const search = within(tl.getByText('search').closest('li')!)
    expect(search.getByText(/Suggested read-only/)).toBeInTheDocument()
    expect(search.getByRole('radio', { name: 'Ask' })).toBeChecked()
    expect(calls.tiers).toHaveLength(0)
    await userEvent.click(search.getByRole('button', { name: 'Set to Auto: search' }))
    await waitFor(() => expect(calls.tiers).toEqual([expect.objectContaining({ tool: 'search', tier: Tier.AUTO })]))
  })

  it('acknowledges a review without changing the tier', async () => {
    const calls = setup()
    const tl = within(await screen.findByRole('list', { name: 'Tools of docs' }))
    await userEvent.click(tl.getByRole('button', { name: 'Mark reviewed: edit' }))
    await waitFor(() => expect(calls.acks).toEqual([expect.objectContaining({ serverId: 's-docs', tool: 'edit' })]))
    expect(calls.tiers).toHaveLength(0)
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Mark reviewed: edit' })).not.toBeInTheDocument())
  })

  it('sets Require two approvers in the Ask tier and disables it elsewhere', async () => {
    const calls = setup()
    const tl = within(await screen.findByRole('list', { name: 'Tools of docs' }))
    const search = within(tl.getByText('search').closest('li')!)
    const box = search.getByRole('checkbox', { name: 'Require two approvers' })
    expect(box).not.toBeChecked()
    expect(search.getByText(/Two distinct admins or owners must approve, neither the person who created the task/)).toBeInTheDocument()
    await userEvent.click(box)
    await waitFor(() => expect(calls.twos).toEqual([expect.objectContaining({ orgId: 'org-1', serverId: 's-docs', tool: 'search', required: true })]))
    await waitFor(() => expect(search.getByRole('checkbox', { name: 'Require two approvers' })).toBeChecked())
    const sync = within(tl.getByText('sync').closest('li')!)
    expect(sync.getByRole('checkbox', { name: 'Require two approvers' })).toBeDisabled()
    expect(sync.getByText(/only applies in the Ask tier/)).toBeInTheDocument()
  })

  it('says why an unavailable tool is not offered to agents', async () => {
    setup()
    const tl = within(await screen.findByRole('list', { name: 'Tools of docs' }))
    const row = within(tl.getByText('long_tool').closest('li')!)
    expect(row.getByText(/unavailable: name too long for the gateway/)).toBeInTheDocument()
    expect(row.getByText(/Agents are not offered this tool/)).toBeInTheDocument()
    expect(row.getByRole('radio', { name: 'Ask' })).toBeEnabled()
  })

  it('says when Koinon sanitised a tool description', async () => {
    setup()
    const tl = within(await screen.findByRole('list', { name: 'Tools of docs' }))
    const row = within(tl.getByText('poisoned').closest('li')!)
    expect(row.getByText(/hidden characters removed or lookalike tags escaped; cut to its length cap \(the server published 5000 characters\)/)).toBeInTheDocument()
    expect(within(tl.getByText('sync').closest('li')!).queryByText(/Koinon sanitised/)).not.toBeInTheDocument()
  })

  it('hints that reviewing does not restore Auto', async () => {
    setup()
    const tl = within(await screen.findByRole('list', { name: 'Tools of docs' }))
    expect(tl.getByText('Reviewing keeps the tool in Ask; set it back to Auto yourself if you trust the new version.')).toBeInTheDocument()
  })

  it('adds a server with a bearer token and clears the secret from the page', async () => {
    const calls = setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Add server' }))
    const form = screen.getByRole('form', { name: 'Add server' })
    expect(within(form).getByText(/start in the Ask tier/)).toBeInTheDocument()
    await userEvent.type(within(form).getByLabelText('Name'), 'newone')
    await userEvent.type(within(form).getByLabelText('URL'), 'https://new.example.com/mcp')
    await userEvent.selectOptions(within(form).getByLabelText('Credential'), 'bearer')
    await userEvent.type(within(form).getByLabelText('Token'), 'not-a-real-token')
    await userEvent.click(within(form).getByRole('button', { name: 'Add server' }))
    await waitFor(() => expect(calls.creates).toHaveLength(1))
    expect(calls.creates[0]).toMatchObject({ name: 'newone', url: 'https://new.example.com/mcp', transport: 'streamable-http', credentialHeader: 'Authorization', credentialPrefix: 'Bearer ', credentialValue: 'not-a-real-token' })
    await waitFor(() => expect(screen.queryByRole('form', { name: 'Add server' })).not.toBeInTheDocument())
    expect(document.body.textContent).not.toContain('not-a-real-token')
  })

  it('shows the OAuth refusal at the URL field and keeps the form', async () => {
    const msg = 'this server asks each user to sign in with OAuth, which Koinon does not support yet; use a server that takes a token'
    setup({ failCreate: msg })
    await userEvent.click(await screen.findByRole('button', { name: 'Add server' }))
    const form = screen.getByRole('form', { name: 'Add server' })
    await userEvent.type(within(form).getByLabelText('Name'), 'oauthy')
    await userEvent.type(within(form).getByLabelText('URL'), 'https://oauth.example.com/mcp')
    await userEvent.click(within(form).getByRole('button', { name: 'Add server' }))
    const alert = await within(form).findByRole('alert')
    expect(alert).toHaveTextContent('does not support yet')
    expect(within(form).getByLabelText('URL')).toHaveAccessibleDescription(/does not support yet/)
  })

  it('validates the name before sending', async () => {
    const calls = setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Add server' }))
    const form = screen.getByRole('form', { name: 'Add server' })
    await userEvent.type(within(form).getByLabelText('Name'), 'Bad Name')
    await userEvent.click(within(form).getByRole('button', { name: 'Add server' }))
    expect(within(form).getByText(/lowercase letters, digits and dashes/, { selector: '#server-name-error' })).toBeInTheDocument()
    expect(calls.creates).toHaveLength(0)
  })

  it('edits without touching the credential, then removes it on request', async () => {
    const calls = setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Edit server docs' }))
    const form = screen.getByRole('form', { name: 'Edit server docs' })
    await userEvent.clear(within(form).getByLabelText('URL'))
    await userEvent.type(within(form).getByLabelText('URL'), 'https://moved.example.com/mcp')
    await userEvent.click(within(form).getByRole('button', { name: 'Save server' }))
    await waitFor(() => expect(calls.updates).toHaveLength(1))
    expect(calls.updates[0].url).toBe('https://moved.example.com/mcp')
    expect(calls.updates[0].credentialValue).toBeUndefined()
    expect(calls.updates[0].removeCredential).toBe(false)

    await userEvent.click(await screen.findByRole('button', { name: 'Edit server docs' }))
    const again = screen.getByRole('form', { name: 'Edit server docs' })
    await userEvent.click(within(again).getByLabelText('Remove credential'))
    await userEvent.click(within(again).getByRole('button', { name: 'Save server' }))
    await waitFor(() => expect(calls.updates).toHaveLength(2))
    expect(calls.updates[1].removeCredential).toBe(true)
  })

  it('confirms before deleting a server', async () => {
    const calls = setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Delete server docs' }))
    expect(calls.deletes).toHaveLength(0)
    await userEvent.click(screen.getByRole('button', { name: 'Delete server' }))
    await waitFor(() => expect(calls.deletes).toEqual([expect.objectContaining({ serverId: 's-docs' })]))
  })

  it('refuses members without asking the API', async () => {
    const calls = setup({ role: OrgRole.MEMBER })
    expect(await screen.findByRole('alert')).toHaveTextContent('Only org admins and owners can manage remote MCP servers.')
    expect(calls.listed).toBe(0)
    expect(screen.queryByRole('link', { name: 'Servers' })).not.toBeInTheDocument()
  })
})

describe('Agent MCP allowlist', () => {
  it('saves the whole server by default and a limited list of tools when asked', async () => {
    const calls = setup({ path: '/agents/a-1' })
    const list = await screen.findByRole('list', { name: 'MCP servers of this agent' })
    expect(screen.getByText(/Ask means a human approves each call/)).toBeInTheDocument()
    expect(within(list).getByText(/issues is unhealthy/)).toBeInTheDocument()
    await userEvent.click(within(list).getByRole('checkbox', { name: /Allow docs/ }))
    await userEvent.click(within(list).getByRole('checkbox', { name: /Allow wiki/ }))
    await userEvent.click(within(list).getAllByRole('checkbox', { name: 'Limit to some tools' })[1])
    // Limit on with nothing picked would allow everything: refused.
    expect(screen.getByRole('button', { name: 'Save MCP servers' })).toBeDisabled()
    await userEvent.click(within(list).getByRole('checkbox', { name: /read/ }))
    await userEvent.click(screen.getByRole('button', { name: 'Save MCP servers' }))
    await waitFor(() => expect(calls.allowSets).toHaveLength(1))
    expect(calls.allowSets[0]).toMatchObject({ orgId: 'org-1', agentId: 'a-1' })
    expect(calls.allowSets[0].servers.map((s) => ({ serverId: s.serverId, tools: s.tools }))).toEqual([
      { serverId: 's-docs', tools: [] },
      { serverId: 's-sec', tools: ['read'] },
    ])
    expect(await screen.findByText('Saved.')).toBeInTheDocument()
  })

  it('starts from the saved allowances', async () => {
    setup({ path: '/agents/a-1', allowed: [{ serverId: 's-docs', tools: ['search'] }] })
    const list = await screen.findByRole('list', { name: 'MCP servers of this agent' })
    await waitFor(() => expect(within(list).getByRole('checkbox', { name: /Allow docs/ })).toBeChecked())
    expect(within(list).getByRole('checkbox', { name: 'Limit to some tools' })).toBeChecked()
    expect(within(list).getByRole('checkbox', { name: /search/ })).toBeChecked()
    expect(within(list).getByRole('checkbox', { name: /publish/ })).not.toBeChecked()
  })

  it('is not shown to members', async () => {
    setup({ path: '/agents/a-1', role: OrgRole.MEMBER })
    await screen.findByRole('heading', { name: 'Helper' })
    expect(screen.queryByText('MCP servers')).not.toBeInTheDocument()
  })
})
