// SPDX-License-Identifier: Apache-2.0

// A fake Koinon API inside the browser: page.route answers the /auth/*
// endpoints and speaks the Connect protocol (JSON) for unary and server-
// streaming RPCs. The real API path (koinon-api, Postgres, fake model) is
// covered by the E1 end-to-end suite.

import type { Page, Route } from '@playwright/test'

type Json = Record<string, unknown>
export type Stream = { messages: Json[]; error?: { code: string; message: string } }

export type Handlers = {
  unary: Record<string, (req: Json) => Json | { error: { code: string; message: string } }>
  stream: Record<string, (req: Json) => Stream>
}

const httpStatus: Record<string, number> = {
  unauthenticated: 401,
  permission_denied: 403,
  not_found: 404,
  invalid_argument: 400,
  failed_precondition: 400,
  aborted: 409,
  unavailable: 503,
}

function envelope(flags: number, body: Json): Buffer {
  const data = Buffer.from(JSON.stringify(body))
  const head = Buffer.alloc(5)
  head.writeUInt8(flags, 0)
  head.writeUInt32BE(data.length, 1)
  return Buffer.concat([head, data])
}

function unenvelope(buf: Buffer): Json {
  const len = buf.readUInt32BE(1)
  return JSON.parse(buf.subarray(5, 5 + len).toString()) as Json
}

export async function mockApi(page: Page, h: Handlers) {
  await page.route(/\/koinon\.[^/]+\/[^/]+$/, async (route: Route) => {
    const req = route.request()
    const procedure = new URL(req.url()).pathname.slice(1)
    const body = req.postDataBuffer() ?? Buffer.alloc(0)
    const unary = h.unary[procedure]
    if (unary) {
      expectHeader(req.headers(), 'connect-protocol-version', '1')
      const res = unary(body.length ? (JSON.parse(body.toString()) as Json) : {})
      if ('error' in res && res.error && typeof res.error === 'object') {
        const err = res.error as { code: string; message: string }
        return route.fulfill({ status: httpStatus[err.code] ?? 500, contentType: 'application/json', body: JSON.stringify(err) })
      }
      return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(res) })
    }
    const stream = h.stream[procedure]
    if (stream) {
      const res = stream(unenvelope(body))
      const parts = res.messages.map((m) => envelope(0, m))
      parts.push(envelope(2, res.error ? { error: res.error } : {}))
      return route.fulfill({ status: 200, contentType: 'application/connect+json', body: Buffer.concat(parts) })
    }
    return route.fulfill({ status: 404, contentType: 'application/json', body: JSON.stringify({ code: 'unimplemented', message: procedure }) })
  })
}

function expectHeader(headers: Record<string, string>, name: string, value: string) {
  if (headers[name] !== value) throw new Error(`missing ${name}: ${value} (the API's CSRF check requires it)`)
}

const mcp = 'koinon.mcpserver.v1alpha1.McpServerService/'

/**
 * mcpServerHandlers is an in-memory McpServerService: a healthy server, an unhealthy one and one
 * declared by a Secret, with tools in different tiers and one carrying a review flag. `seen` records
 * the writes so a spec can assert on them. Credentials are never returned.
 */
export function mcpServerHandlers() {
  const at = new Date(Date.now() - 120_000).toISOString()
  const servers: Json[] = [
    { id: 's-docs', name: 'docs', url: 'https://mcp.example.com/mcp', transport: 'streamable-http', hasCredential: true, credentialHeader: 'Authorization', credentialPrefix: 'Bearer ', source: 'ui', health: 'HEALTH_HEALTHY', healthCheckedAt: at, gatewayTarget: 'default-docs' },
    { id: 's-issues', name: 'issues', url: 'https://issues.example.com/mcp', transport: 'sse', source: 'ui', health: 'HEALTH_UNHEALTHY', healthReason: 'connection refused', healthCheckedAt: at, gatewayTarget: 'default-issues' },
    { id: 's-wiki', name: 'wiki', url: 'https://wiki.example.com/mcp', transport: 'streamable-http', hasCredential: true, credentialHeader: 'X-Api-Key', source: 'secret', managedBySecret: 'wiki-secret', health: 'HEALTH_UNKNOWN', gatewayTarget: 'default-wiki' },
  ]
  const tools: Json[] = [
    { serverId: 's-docs', name: 'search', description: 'Search the docs', readOnlyHint: true, tier: 'TIER_ASK', present: true },
    { serverId: 's-docs', name: 'publish', description: 'Publish a page', tier: 'TIER_NEVER', present: true },
    { serverId: 's-docs', name: 'edit', description: 'Edit a page', tier: 'TIER_ASK', present: true, reviewReason: 'the description changed' },
    { serverId: 's-docs', name: 'legacy', tier: 'TIER_AUTO', present: false },
    { serverId: 's-issues', name: 'list_issues', description: 'List issues', tier: 'TIER_AUTO', present: true },
    { serverId: 's-wiki', name: 'read', description: 'Read a page', tier: 'TIER_ASK', present: true },
  ]
  let allowed: Json[] = []
  const seen: { create?: Json; tier?: Json; ack?: Json; allow?: Json } = {}
  const find = (req: Json) => tools.find((t) => t.serverId === req.serverId && t.name === req.tool)!
  const unary: Handlers['unary'] = {
    [mcp + 'ListServers']: () => ({ servers }),
    [mcp + 'ListTools']: () => ({ tools }),
    [mcp + 'CreateServer']: (req) => {
      seen.create = req
      const s = { id: 's-new', name: req.name, url: req.url, transport: req.transport, hasCredential: !!req.credentialValue, source: 'ui', health: 'HEALTH_UNKNOWN', gatewayTarget: `default-${String(req.name)}` }
      servers.push(s)
      return { server: s }
    },
    [mcp + 'SetToolTier']: (req) => {
      seen.tier = req
      const t = find(req)
      t.tier = req.tier
      delete t.reviewReason
      return { tool: t }
    },
    [mcp + 'AcknowledgeToolReview']: (req) => {
      seen.ack = req
      const t = find(req)
      delete t.reviewReason
      return { tool: t }
    },
    [mcp + 'GetAgentServers']: () => ({ servers: allowed }),
    [mcp + 'SetAgentServers']: (req) => {
      seen.allow = req
      allowed = (req.servers as Json[]) ?? []
      return { servers: allowed }
    },
  }
  return { unary, seen }
}
