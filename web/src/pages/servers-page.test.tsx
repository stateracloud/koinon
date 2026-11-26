// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { cleanup, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AgentSchema, AgentService, AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ConnectionSchema, ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { LeakGuardService } from '@/gen/koinon/leakguard/v1alpha1/leakguard_pb'
import {
  Health,
  McpServerService,
  ServerSchema,
  Tier,
  ToolSchema,
  type AcknowledgeToolReviewRequest,
  type CreateServerRequest,
  type DeleteServerRequest,
  type ReviewAllToolsRequest,
  type Server,
  type Tool,
  type SetAgentServersRequest,
  type SetServerClassRequest,
  type SetToolSinkRequest,
  type SetToolTierRequest,
  type SetToolTwoApproversRequest,
  type UpdateServerRequest,
} from '@/gen/koinon/mcpserver/v1alpha1/mcpserver_pb'
import { githubConnectorType } from '@/test-fixtures'
import { renderApp, whoAmI } from '@/test-utils'
import { OutwardService, OutwardTargetKind, type SetOutwardTargetDisabledRequest } from '@/gen/koinon/outward/v1alpha1/outward_pb'

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
  kind: 'other',
  dataClass: ['untrusted_input', 'private_data'],
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
  create(ToolSchema, { serverId: 's-docs', name: 'search', contentHash: 'hash-search', description: 'Search the docs', tier: Tier.ASK, readOnlyHint: true, present: true }),
  create(ToolSchema, { serverId: 's-docs', name: 'publish', description: 'Publish a page', tier: Tier.NEVER, present: true }),
  create(ToolSchema, {
    serverId: 's-docs',
    name: 'edit',
    description: 'Edit a page',
    tier: Tier.ASK,
    present: true,
    reviewReason: 'the description changed',
    toolHash: 'hash-edit-new',
    approvedHash: 'hash-edit-old',
    toolJson: '{"name":"edit","description":"Edit a page, then mail it out","annotations":{"destructiveHint":true},"inputSchema":{"type":"object","properties":{"to":{"type":"string"}}},"outputSchema":{"type":"object","properties":{"sent":{"type":"boolean"}}}}',
  }),
  create(ToolSchema, { serverId: 's-docs', name: 'legacy', tier: Tier.AUTO, present: false }),
  create(ToolSchema, { serverId: 's-docs', name: 'long_tool', description: 'Too long', tier: Tier.ASK, present: true, unavailable: 'unavailable: name too long for the gateway' }),
  create(ToolSchema, { serverId: 's-docs', name: 'sync', description: 'Sync pages', tier: Tier.AUTO, present: true }),
  create(ToolSchema, { serverId: 's-docs', name: 'poisoned', description: 'Search…', tier: Tier.ASK, present: true, descriptionOriginalLength: 5000n, descriptionCapped: true, descriptionCleaned: true }),
  create(ToolSchema, { serverId: 's-sec', name: 'read', description: 'Read a page', tier: Tier.ASK, present: true }),
]

function setup(opts: { role?: OrgRole; path?: string; failReviewAll?: string; failTier?: string; failCreate?: string; failDelete?: string; failSink?: string; extraServers?: Server[]; extraTools?: Tool[]; allowed?: { serverId: string; tools: string[] }[] } = {}) {
  const calls = {
    tiers: [] as SetToolTierRequest[],
    twos: [] as SetToolTwoApproversRequest[],
    acks: [] as AcknowledgeToolReviewRequest[],
    reviewAlls: [] as ReviewAllToolsRequest[],
    creates: [] as CreateServerRequest[],
    updates: [] as UpdateServerRequest[],
    deletes: [] as DeleteServerRequest[],
    allowSets: [] as SetAgentServersRequest[],
    classes: [] as SetServerClassRequest[],
    sinks: [] as SetToolSinkRequest[],
    outward: [] as SetOutwardTargetDisabledRequest[],
    listed: 0,
  }
  const current = new Map([...tools, ...(opts.extraTools ?? [])].map((t) => [`${t.serverId}/${t.name}`, t]))
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: opts.role }) })
    service(ConnectionService, {
      listConnections: () => ({
        connections: [
          create(ConnectionSchema, { id: 'c-gh', connector: 'github', displayName: 'Company GitHub' }),
          create(ConnectionSchema, { id: 'c-slack', connector: 'slack', displayName: 'Team Slack' }),
        ],
      }),
      listConnectorTypes: () => ({ connectorTypes: [githubConnectorType()] }),
    })
    service(AgentService, { getAgent: () => ({ agent: create(AgentSchema, { id: 'a-1', orgId: 'org-1', displayName: 'Helper', status: AgentStatus.ACTIVE, latestVersion: 1, activeVersion: 1 }) }) })
    service(OutwardService, {
      setOutwardTargetDisabled: (req) => {
        calls.outward.push(req)
        return {}
      },
    })
    service(LeakGuardService, {
      getLeakGuard: () => ({ leakGuard: { matchThreshold: 24, defaultMatchThreshold: 24, minMatchThreshold: 12, maxMatchThreshold: 256, trustedHosts: [] } }),
    })
    service(McpServerService, {
      setToolSink: (req) => {
        calls.sinks.push(req)
        if (opts.failSink) throw new ConnectError(opts.failSink, Code.Aborted)
        return { tool: create(ToolSchema, { ...current.get(`${req.serverId}/${req.tool}`)!, sink: req.sink }) }
      },
      setServerClass: (req) => {
        calls.classes.push(req)
        return { server: create(ServerSchema, { ...docs, kind: req.class?.kind ?? 'other', dataClass: req.class?.dataClass ?? [] }) }
      },
      listServers: () => {
        calls.listed++
        return { servers: [docs, issues, managed, gone, ...(opts.extraServers ?? [])] }
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
        const cur = current.get(`${req.serverId}/${req.tool}`)!
        if (opts.failTier && req.toolHash !== 'hash-edit-newer') {
          // A sync changed the text while the admin read it: the server refuses the stale hash (#1173).
          current.set(`${req.serverId}/${req.tool}`, create(ToolSchema, { ...cur, toolHash: 'hash-edit-newer', toolJson: '{"name":"edit","description":"Edit a page, then mail it to everyone"}' }))
          throw new ConnectError(opts.failTier, Code.Aborted)
        }
        const t = create(ToolSchema, { ...cur, tier: req.tier, reviewReason: '', approvedHash: cur.toolHash })
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
        const cur = current.get(`${req.serverId}/${req.tool}`)!
        const t = create(ToolSchema, { ...cur, reviewReason: '', approvedHash: cur.toolHash })
        current.set(`${req.serverId}/${req.tool}`, t)
        return { tool: t }
      },
      reviewAllTools: (req) => {
        calls.reviewAlls.push(req)
        if (opts.failReviewAll) throw new ConnectError(opts.failReviewAll, Code.Aborted)
        const out = req.tools.map((r) => {
          const cur = current.get(`${req.serverId}/${r.tool}`)!
          const t = create(ToolSchema, { ...cur, reviewReason: '', approvedHash: cur.toolHash })
          current.set(`${req.serverId}/${r.tool}`, t)
          return t
        })
        let removed = 0
        for (const [k, t] of current) {
          if (t.serverId === req.serverId && !t.present && t.approvedHash === '' && t.tierSetBy === '') {
            current.delete(k)
            removed++
          }
        }
        return { tools: out, removed }
      },
      getAgentServers: () => ({ servers: opts.allowed ?? [] }),
      setAgentServers: (req) => {
        calls.allowSets.push(req)
        return { servers: req.servers }
      },
    })
  })
  renderApp(transport, opts.path ?? '/mcp-servers/s-docs')
  return calls
}

/** serverPage waits for the detail page of one server and returns queries over it. */
async function serverPage(name = 'docs') {
  await screen.findByRole('heading', { name, level: 1 })
  return within(document.body)
}

describe('Servers page', () => {
  it('lists servers in a table with state, tools waiting, source and the outward switch', async () => {
    setup({ path: '/mcp-servers' })
    const table = await screen.findByRole('table', { name: 'MCP servers' })
    const row = (name: string) => within(within(table).getByRole('link', { name }).closest('tr') as HTMLElement)
    expect(row('docs').getByText('Healthy')).toBeInTheDocument()
    expect(row('docs').getByText('UI')).toBeInTheDocument()
    expect(row('docs').getByRole('link', { name: '1 tool waits for review: review the tools of docs' })).toHaveAttribute('href', '/mcp-servers/s-docs')
    expect(row('docs').getByRole('button', { name: 'Switch off outward calls of server docs' })).toBeInTheDocument()
    expect(row('issues').getByText('Unhealthy', { selector: '[data-slot=badge]' })).toBeInTheDocument()
    expect(row('wiki').getByText('Managed by Secret wiki-secret')).toBeInTheDocument()
    expect(row('old').getByText('Removed')).toBeInTheDocument()
    expect(within(table).getByRole('link', { name: 'docs' })).toHaveAttribute('href', '/mcp-servers/s-docs')
    expect(screen.getByRole('link', { name: 'Connect a remote server' })).toHaveAttribute('href', '/mcp-servers/new')
  })

  it('switches off the outward calls of a server from its row and records the reason (ADR 0110)', async () => {
    const calls = setup({ path: '/mcp-servers' })
    const table = await screen.findByRole('table', { name: 'MCP servers' })
    const row = within(within(table).getByRole('link', { name: 'issues' }).closest('tr') as HTMLElement)
    await userEvent.type(row.getByRole('textbox', { name: 'Reason for switching off server issues' }), 'leaks ticket text')
    await userEvent.click(row.getByRole('button', { name: 'Switch off outward calls of server issues' }))
    await waitFor(() => expect(calls.outward).toHaveLength(1))
    expect(calls.outward[0]).toMatchObject({ orgId: 'org-1', kind: OutwardTargetKind.MCP_SERVER, targetId: 's-issues', disabled: true, note: 'leaks ticket text' })
  })

  it('shows a switched-off server in its row with who and why', async () => {
    const off = create(ServerSchema, { id: 's-off', name: 'halted', url: 'https://halted.example/mcp', source: 'ui', adminDisabled: { by: 'admin@example.com', note: 'incident 42' } })
    setup({ path: '/mcp-servers', extraServers: [off] })
    const table = await screen.findByRole('table', { name: 'MCP servers' })
    const row = within(within(table).getByRole('link', { name: 'halted' }).closest('tr') as HTMLElement)
    expect(row.getByRole('note')).toHaveTextContent('Switched off by admin@example.com: incident 42')
    expect(row.getByRole('button', { name: 'Enable outward calls of server halted' })).toBeInTheDocument()
  })

  it('shows a new tool as waiting for review and lets an admin review it (#1216)', async () => {
    const fresh = create(ServerSchema, { id: 's-fresh', name: 'fresh', url: 'https://fresh.example/mcp', source: 'ui', health: Health.HEALTHY })
    const newTool = create(ToolSchema, { serverId: 's-fresh', name: 'write_item', description: 'Write an item', tier: Tier.ASK, present: true, toolHash: 'hash-write-item', toolJson: '{"name":"write_item","description":"Write an item"}' })
    const calls = setup({ path: '/mcp-servers/s-fresh', extraServers: [fresh], extraTools: [newTool] })
    const tl = within(await screen.findByRole('list', { name: 'Tools of fresh' }))
    const row = within(tl.getAllByText('write_item')[0].closest('li')!)
    expect(row.getByText('Waits for review', { selector: '[data-slot=badge]' })).toBeInTheDocument()
    expect(row.getByText(/New tool: agents do not get it until an admin reviews its text/)).toBeInTheDocument()
    await userEvent.click(row.getByRole('button', { name: 'Mark reviewed: write_item' }))
    await waitFor(() => expect(calls.acks).toHaveLength(1))
    expect(calls.acks[0]).toMatchObject({ orgId: 'org-1', serverId: 's-fresh', tool: 'write_item', toolHash: 'hash-write-item' })
    await waitFor(() => expect(row.queryByText('Waits for review')).not.toBeInTheDocument())
  })

  it('shows one server with health as text, credential state and gateway target', async () => {
    setup()
    const d = await serverPage()
    expect(d.getByText('Healthy')).toBeInTheDocument()
    expect(d.getByText('UI')).toBeInTheDocument()
    expect(d.getByText('https://mcp.example.com/mcp')).toBeInTheDocument()
    expect(d.getByText('Streamable HTTP')).toBeInTheDocument()
    expect(d.getByText('Stored encrypted; it cannot be shown')).toBeInTheDocument()
    expect(d.getByText('default-docs')).toBeInTheDocument()
    expect(d.getByText(/public roots/)).toBeInTheDocument()
    expect(d.getByRole('button', { name: 'Edit server docs' })).toBeInTheDocument()
  })

  it('says why an unhealthy server has no tools for agents', async () => {
    setup({ path: '/mcp-servers/s-issues' })
    const i = await serverPage('issues')
    expect(i.getByText('Unhealthy', { selector: '[data-slot=badge]' })).toBeInTheDocument()
    expect(i.getByText(/connection refused/)).toBeInTheDocument()
    expect(i.getByText(/withdrawn from agents/)).toBeInTheDocument()
  })

  it('does not offer edit or delete for a server that a Secret declares', async () => {
    setup({ path: '/mcp-servers/s-sec' })
    const m = await serverPage('wiki')
    expect(m.getByText('Unknown')).toBeInTheDocument()
    expect(m.getByText('Managed by Secret wiki-secret')).toBeInTheDocument()
    expect(m.queryByRole('button', { name: /Edit server/ })).not.toBeInTheDocument()
    expect(m.queryByRole('button', { name: /Delete server/ })).not.toBeInTheDocument()
  })

  it('offers only delete for a removed server', async () => {
    setup({ path: '/mcp-servers/s-gone' })
    const r = await serverPage('old')
    expect(r.getAllByText('Removed')[0]).toBeInTheDocument()
    expect(r.getByRole('button', { name: 'Delete server old' })).toBeInTheDocument()
    expect(r.queryByRole('button', { name: /Edit server/ })).not.toBeInTheDocument()
  })

  it('says when a server does not exist', async () => {
    setup({ path: '/mcp-servers/s-none' })
    expect(await screen.findByRole('heading', { name: 'Server not found', level: 1 })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Back to MCP servers' })).toHaveAttribute('href', '/mcp-servers')
  })

  it('never renders a credential value', async () => {
    setup()
    await serverPage()
    await userEvent.click(screen.getByRole('button', { name: 'Edit server docs' }))
    const form = screen.getByRole('form', { name: 'Edit server docs' })
    expect(within(form).getByLabelText('Token')).toHaveValue('')
    expect(within(form).getByLabelText('Token')).toHaveAttribute('type', 'password')
    expect(within(form).getByText(/A credential is stored\. Leave empty to keep it\./)).toBeInTheDocument()
    expect(document.body.textContent).not.toContain('not-a-real-token')
  })

  it('shows the tools that wait for review and reviews them all at the hashes shown (#1216)', async () => {
    const calls = setup()
    const docsCard = await serverPage()
    const note = within(docsCard.getByRole('note', { name: 'Tools waiting for review: docs' }))
    expect(note.getByText(/1 tool waits for review/)).toBeInTheDocument()
    await userEvent.click(note.getByRole('button', { name: 'Review all tools of docs' }))
    expect(note.getByText(/approves the current text of 1 tool \(edit\) without changing a tier, and deletes 1 tool that the server no longer lists/)).toBeInTheDocument()
    await userEvent.click(note.getByRole('button', { name: 'Review all' }))
    await waitFor(() => expect(calls.reviewAlls).toHaveLength(1))
    expect(calls.reviewAlls[0]).toMatchObject({ orgId: 'org-1', serverId: 's-docs', tools: [{ tool: 'edit', toolHash: 'hash-edit-new' }] })
    expect(await screen.findByText('Reviewed 1 tool of docs. Deleted 1.')).toBeInTheDocument()
    await waitFor(() => expect(docsCard.queryByRole('note', { name: 'Tools waiting for review: docs' })).not.toBeInTheDocument())
  })

  it('says when a server is at the limit of unreviewed tools, and shows a refused review', async () => {
    const capped = create(ServerSchema, { id: 's-cap', name: 'junk', url: 'https://junk.example.com/mcp', source: 'ui', health: Health.HEALTHY, unreviewedToolsCappedAt: checked })
    setup({ path: '/mcp-servers/s-cap', extraServers: [capped] })
    const note = within(await screen.findByRole('note', { name: 'Tools waiting for review: junk' }))
    expect(note.getByText(/at the limit of tools that no admin reviewed/)).toBeInTheDocument()
    expect(note.queryByRole('button', { name: /Review all/ })).not.toBeInTheDocument()
  })

  it('shows a refused review of all tools', async () => {
    setup({ failReviewAll: 'the tool changed since it was read; read it again and review the new description' })
    await serverPage()
    const docsNote = within(screen.getByRole('note', { name: 'Tools waiting for review: docs' }))
    await userEvent.click(docsNote.getByRole('button', { name: 'Review all tools of docs' }))
    await userEvent.click(docsNote.getByRole('button', { name: 'Review all' }))
    expect(await screen.findByText(/the tool changed since it was read/)).toBeInTheDocument()
  })

  it('shows each tool with its tier, description, review flag and missing state', async () => {
    setup()
    const tl = within(await screen.findByRole('list', { name: 'Tools of docs' }))
    const search = within(tl.getByText('search').closest('li')!)
    expect(search.getByText('Search the docs')).toBeInTheDocument()
    expect(search.getByRole('radio', { name: 'Ask' })).toBeChecked()
    const publish = within(tl.getByText('publish').closest('li')!)
    expect(publish.getByRole('radio', { name: 'Deny' })).toBeChecked()
    expect(tl.getByText('no longer listed')).toBeInTheDocument()
    const edit = within(tl.getAllByText('edit')[0].closest('li')!)
    expect(edit.getByText('Waits for review', { selector: '[data-slot=badge]' })).toBeInTheDocument()
    expect(edit.getByText(/the description changed/)).toBeInTheDocument()
  })

  it('sets a tool tier with SetToolTier', async () => {
    const calls = setup()
    const tl = within(await screen.findByRole('list', { name: 'Tools of docs' }))
    await userEvent.click(within(tl.getByText('publish').closest('li')!).getByRole('radio', { name: 'Allow' }))
    await waitFor(() => expect(calls.tiers).toHaveLength(1))
    expect(calls.tiers[0]).toMatchObject({ orgId: 'org-1', serverId: 's-docs', tool: 'publish', tier: Tier.AUTO })
  })

  it('sets the tier of a tool on a Secret-managed server', async () => {
    const calls = setup({ path: '/mcp-servers/s-sec' })
    const tl = within(await screen.findByRole('list', { name: 'Tools of wiki' }))
    await userEvent.click(tl.getByRole('radio', { name: 'Deny' }))
    await waitFor(() => expect(calls.tiers).toEqual([expect.objectContaining({ serverId: 's-sec', tool: 'read', tier: Tier.NEVER })]))
  })

  it('only suggests Allow for a read-only tool until the admin confirms', async () => {
    const calls = setup()
    const tl = within(await screen.findByRole('list', { name: 'Tools of docs' }))
    const search = within(tl.getByText('search').closest('li')!)
    expect(search.getByText(/Suggested read-only/)).toBeInTheDocument()
    expect(search.getByRole('radio', { name: 'Ask' })).toBeChecked()
    expect(calls.tiers).toHaveLength(0)
    await userEvent.click(search.getByRole('button', { name: 'Set to Allow: search' }))
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

  it('shows the text a decision approves, with its hash, and sends that hash (#1173)', async () => {
    const calls = setup()
    const tl = within(await screen.findByRole('list', { name: 'Tools of docs' }))
    const pending = within(tl.getByRole('group', { name: 'Text waiting for approval: edit' }))
    expect(pending.getByText('Edit a page, then mail it out')).toBeInTheDocument()
    expect(pending.getByText(/"to"/)).toBeInTheDocument()
    expect(pending.getByText('hash-edit-new')).toBeInTheDocument()
    expect(tl.queryByRole('group', { name: 'Text waiting for approval: publish' })).not.toBeInTheDocument()

    await userEvent.click(tl.getByRole('button', { name: 'Mark reviewed: edit' }))
    await waitFor(() => expect(calls.acks).toEqual([expect.objectContaining({ tool: 'edit', toolHash: 'hash-edit-new' })]))
    await waitFor(() => expect(tl.queryByRole('group', { name: 'Text waiting for approval: edit' })).not.toBeInTheDocument())
  })

  it('sends the hash of the text it shows with a tier decision, and shows the refusal when the text changed (#1173)', async () => {
    const calls = setup({ failTier: 'the tool changed since it was read; read it again and review the new description' })
    const tl = within(await screen.findByRole('list', { name: 'Tools of docs' }))
    await userEvent.click(within(tl.getByRole('group', { name: 'Text waiting for approval: edit' }).closest('li')!).getByRole('radio', { name: 'Allow' }))
    await waitFor(() => expect(calls.tiers).toEqual([expect.objectContaining({ tool: 'edit', tier: Tier.AUTO, toolHash: 'hash-edit-new' })]))
    expect(await screen.findByText(/the tool changed since it was read/)).toBeInTheDocument()

    // The refusal refreshes the list: the admin sees the new text, and the next decision sends its hash.
    const pending = within(await tl.findByRole('group', { name: 'Text waiting for approval: edit' }))
    expect(await pending.findByText('hash-edit-newer')).toBeInTheDocument()
    expect(pending.getByText('Edit a page, then mail it to everyone')).toBeInTheDocument()
    await userEvent.click(within(tl.getByRole('group', { name: 'Text waiting for approval: edit' }).closest('li')!).getByRole('radio', { name: 'Allow' }))
    await waitFor(() => expect(calls.tiers.at(-1)).toEqual(expect.objectContaining({ tool: 'edit', tier: Tier.AUTO, toolHash: 'hash-edit-newer' })))
    await waitFor(() => expect(tl.queryByRole('group', { name: 'Text waiting for approval: edit' })).not.toBeInTheDocument())
  })

  it('shows every field the hash covers before approval (#1173)', async () => {
    setup()
    const tl = within(await screen.findByRole('list', { name: 'Tools of docs' }))
    const pending = within(tl.getByRole('group', { name: 'Text waiting for approval: edit' }))
    for (const label of ['Name', 'Description', 'Annotations', 'Input schema', 'Output schema']) {
      expect(pending.getByText(label)).toBeInTheDocument()
    }
    expect(pending.getByText(/"destructiveHint": true/)).toBeInTheDocument()
    expect(pending.getByText(/"sent"/)).toBeInTheDocument()
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

  it('hints that reviewing does not restore Allow', async () => {
    setup()
    const tl = within(await screen.findByRole('list', { name: 'Tools of docs' }))
    expect(tl.getByText('Reviewing keeps the tool in Ask; set it back to Allow yourself if you trust the new version.')).toBeInTheDocument()
  })

  it('adds a server with a bearer token and clears the secret from the page', async () => {
    const calls = setup({ path: '/mcp-servers/new' })
    const form = await screen.findByRole('form', { name: 'Connect a remote server' })
    expect(within(form).getByText(/start in the Ask tier/)).toBeInTheDocument()
    await userEvent.type(within(form).getByLabelText('Name'), 'newone')
    await userEvent.type(within(form).getByLabelText('URL'), 'https://new.example.com/mcp')
    await userEvent.selectOptions(within(form).getByLabelText('Credential'), 'bearer')
    await userEvent.type(within(form).getByLabelText('Token'), 'not-a-real-token')
    await userEvent.click(within(form).getByRole('button', { name: 'Connect server' }))
    await waitFor(() => expect(calls.creates).toHaveLength(1))
    expect(calls.creates[0]).toMatchObject({ name: 'newone', url: 'https://new.example.com/mcp', transport: 'streamable-http', credentialHeader: 'Authorization', credentialPrefix: 'Bearer ', credentialValue: 'not-a-real-token' })
    // After the save, the page of the new server opens.
    await waitFor(() => expect(screen.queryByRole('form', { name: 'Connect a remote server' })).not.toBeInTheDocument())
    expect(await screen.findByText('Connected server newone. Its tools show on its page after the first sync.')).toBeInTheDocument()
    expect(document.body.textContent).not.toContain('not-a-real-token')
  })

  it('adds a server with a private CA certificate', async () => {
    const calls = setup({ path: '/mcp-servers/new' })
    const form = await screen.findByRole('form', { name: 'Connect a remote server' })
    const pem = '-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----'
    await userEvent.type(within(form).getByLabelText('Name'), 'private')
    await userEvent.type(within(form).getByLabelText('URL'), 'https://mcp.internal.example/mcp')
    await userEvent.click(within(form).getByLabelText('CA certificate (optional)'))
    await userEvent.paste(pem)
    await userEvent.click(within(form).getByRole('button', { name: 'Connect server' }))
    await waitFor(() => expect(calls.creates).toHaveLength(1))
    expect(calls.creates[0]).toMatchObject({ name: 'private', url: 'https://mcp.internal.example/mcp', caBundle: pem })
  })

  it('refuses a CA certificate on an http URL before it sends anything', async () => {
    const calls = setup({ path: '/mcp-servers/new' })
    const form = await screen.findByRole('form', { name: 'Connect a remote server' })
    await userEvent.type(within(form).getByLabelText('Name'), 'private')
    await userEvent.type(within(form).getByLabelText('URL'), 'http://mcp.internal.example/mcp')
    await userEvent.click(within(form).getByLabelText('CA certificate (optional)'))
    await userEvent.paste('-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----')
    await userEvent.click(within(form).getByRole('button', { name: 'Connect server' }))
    expect(await within(form).findByText('A CA certificate needs an https URL.')).toBeInTheDocument()
    expect(calls.creates).toHaveLength(0)
  })

  it('adds a server that skips certificate verification, with a warning', async () => {
    const calls = setup({ path: '/mcp-servers/new' })
    const form = await screen.findByRole('form', { name: 'Connect a remote server' })
    await userEvent.type(within(form).getByLabelText('Name'), 'scratch')
    await userEvent.type(within(form).getByLabelText('URL'), 'https://scratch.example/mcp')
    expect(within(form).queryByText(/impostor/)).not.toBeInTheDocument()
    await userEvent.click(within(form).getByLabelText('Skip certificate verification (insecure)'))
    expect(within(form).getByText(/can be sent to an impostor/)).toBeInTheDocument()
    await userEvent.click(within(form).getByRole('button', { name: 'Connect server' }))
    await waitFor(() => expect(calls.creates).toHaveLength(1))
    expect(calls.creates[0]).toMatchObject({ name: 'scratch', insecureSkipVerify: true, caBundle: '' })
  })

  it('refuses skipped verification on an http URL, and with a CA certificate, before it sends anything', async () => {
    const calls = setup({ path: '/mcp-servers/new' })
    const form = await screen.findByRole('form', { name: 'Connect a remote server' })
    await userEvent.type(within(form).getByLabelText('Name'), 'scratch')
    await userEvent.type(within(form).getByLabelText('URL'), 'http://scratch.example/mcp')
    await userEvent.click(within(form).getByLabelText('Skip certificate verification (insecure)'))
    await userEvent.click(within(form).getByRole('button', { name: 'Connect server' }))
    expect(await within(form).findByText('Skipping verification needs an https URL.')).toBeInTheDocument()
    expect(calls.creates).toHaveLength(0)
  })

  it('shows when verification is off, and when only one CA is trusted', async () => {
    const off = create(ServerSchema, { id: 's-off', name: 'scratch', url: 'https://scratch.example/mcp', source: 'ui', insecureSkipVerify: true })
    const pinned = create(ServerSchema, { id: 's-ca', name: 'private', url: 'https://private.example/mcp', source: 'ui', caBundle: '-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----' })
    setup({ path: '/mcp-servers/s-off', extraServers: [off, pinned] })
    expect((await serverPage('scratch')).getByText(/Not verified/)).toBeInTheDocument()
    cleanup()
    setup({ path: '/mcp-servers/s-ca', extraServers: [off, pinned] })
    expect((await serverPage('private')).getByText(/Only the CA given for this server is trusted/)).toBeInTheDocument()
  })

  it('shows the OAuth refusal at the URL field and keeps the form', async () => {
    const msg = 'this server asks each user to sign in with OAuth, which Koinon does not support yet; use a server that takes a token'
    setup({ failCreate: msg, path: '/mcp-servers/new' })
    const form = await screen.findByRole('form', { name: 'Connect a remote server' })
    await userEvent.type(within(form).getByLabelText('Name'), 'oauthy')
    await userEvent.type(within(form).getByLabelText('URL'), 'https://oauth.example.com/mcp')
    await userEvent.click(within(form).getByRole('button', { name: 'Connect server' }))
    const alert = await within(form).findByRole('alert')
    expect(alert).toHaveTextContent('does not support yet')
    expect(within(form).getByLabelText('URL')).toHaveAccessibleDescription(/does not support yet/)
  })

  it('validates the name before sending', async () => {
    const calls = setup({ path: '/mcp-servers/new' })
    const form = await screen.findByRole('form', { name: 'Connect a remote server' })
    await userEvent.type(within(form).getByLabelText('Name'), 'Bad Name')
    await userEvent.click(within(form).getByRole('button', { name: 'Connect server' }))
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
    expect(screen.queryByRole('link', { name: 'MCP servers' })).not.toBeInTheDocument()
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
  it('shows the data class of each server and changes it', async () => {
    const calls = setup()
    const d = await serverPage()
    expect(d.getByText(/Data class: Other; legs: returns untrusted input, reads private data/)).toBeInTheDocument()
    await userEvent.click(d.getByRole('button', { name: 'Change data class of docs' }))
    await userEvent.selectOptions(d.getByLabelText('Kind'), 'fetch')
    await userEvent.click(d.getByLabelText('Reads private data'))
    await userEvent.click(d.getByRole('button', { name: 'Save class' }))
    await waitFor(() => expect(calls.classes).toHaveLength(1))
    expect(calls.classes[0]).toMatchObject({ serverId: 's-docs', class: { kind: 'fetch', dataClass: ['untrusted_input'] } })
  })

  it('lets a class be set on a server declared by a Secret', async () => {
    setup({ path: '/mcp-servers/s-sec' })
    const w = await serverPage('wiki')
    expect(w.getByRole('button', { name: 'Change data class of wiki' })).toBeInTheDocument()
  })

  it('creates a server with the class chosen in the form, unclassified by default', async () => {
    const calls = setup({ path: '/mcp-servers/new' })
    const form = within(await screen.findByRole('form', { name: 'Connect a remote server' }))
    expect(form.getByLabelText('Kind')).toHaveValue('other')
    for (const l of ['Returns untrusted input', 'Reads private data']) expect(form.getByLabelText(l)).toBeChecked()
    await userEvent.type(form.getByLabelText('Name'), 'fetcher')
    await userEvent.type(form.getByLabelText('URL'), 'https://f.example.com/mcp')
    await userEvent.selectOptions(form.getByLabelText('Kind'), 'fetch')
    await userEvent.click(form.getByLabelText('Reads private data'))
    await userEvent.click(form.getByRole('button', { name: 'Connect server' }))
    await waitFor(() => expect(calls.creates).toHaveLength(1))
    expect(calls.creates[0].class).toMatchObject({ kind: 'fetch', dataClass: ['untrusted_input'] })
  })

  it('shows where the calls of a tool go and declares a derived class from the address arguments', async () => {
    const calls = setup()
    const d = await serverPage()
    expect(d.getAllByText(/Where calls go: External, free text \(not declared\)/).length).toBeGreaterThan(0)
    await userEvent.click(d.getAllByRole('button', { name: 'Declare sink class of search' })[0])
    const form = within(d.getByRole('form', { name: 'Sink class of search' }))
    await userEvent.selectOptions(form.getByLabelText('How the class is decided'), 'code_host')
    expect(form.queryByLabelText('Sink class')).not.toBeInTheDocument()
    await userEvent.type(form.getByLabelText('Address arguments'), 'owner')
    await userEvent.click(form.getByRole('button', { name: 'Save sink class' }))
    expect(await form.findByRole('alert')).toHaveTextContent(/two arguments/)
    expect(calls.sinks).toHaveLength(0)
    await userEvent.type(form.getByLabelText('Address arguments'), ', repo')
    await userEvent.selectOptions(form.getByLabelText('Results carry private data'), 'yes')
    await userEvent.click(form.getByRole('button', { name: 'Save sink class' }))
    await waitFor(() => expect(calls.sinks).toHaveLength(1))
    expect(calls.sinks[0]).toMatchObject({ serverId: 's-docs', tool: 'search', contentHash: 'hash-search', sink: { sinkClass: 'external-high', derivation: 'code_host', addressArgs: ['owner', 'repo'], resultPrivateData: true } })
    expect(calls.sinks[0].sink?.resultUntrustedInput).toBeUndefined()
  })

  it('links a server to a code-host connection', async () => {
    const calls = setup()
    const d = await serverPage()
    expect(d.getByText(/No code-host connection: its code-host tools count as external, free text/)).toBeInTheDocument()
    await userEvent.click(d.getByRole('button', { name: 'Change data class of docs' }))
    const select = await d.findByLabelText('Code-host connection')
    // Only code-host connections can be linked: a Slack connection is not offered (#117).
    await waitFor(() => expect(within(select).getByRole('option', { name: /Company GitHub/ })).toBeInTheDocument())
    expect(within(select).queryByRole('option', { name: /Team Slack/ })).not.toBeInTheDocument()
    await userEvent.selectOptions(select, 'c-gh')
    await userEvent.click(d.getByRole('button', { name: 'Save class' }))
    await waitFor(() => expect(calls.classes).toHaveLength(1))
    expect(calls.classes[0].class?.codeHostConnectionId).toBe('c-gh')
  })

  it('says the tool changed when the declaration is refused as aborted', async () => {
    const calls = setup({ failSink: 'the tool changed since it was read; read it again and review the new description' })
    const d = await serverPage()
    await userEvent.click(d.getAllByRole('button', { name: 'Declare sink class of search' })[0])
    const form = within(d.getByRole('form', { name: 'Sink class of search' }))
    await userEvent.selectOptions(form.getByLabelText('Sink class'), 'none')
    await userEvent.click(form.getByRole('button', { name: 'Save sink class' }))
    expect(await form.findByRole('alert')).toHaveTextContent(/the tool changed since it was read.*review it and declare again/)
    expect(calls.sinks).toHaveLength(1)
  })
})
