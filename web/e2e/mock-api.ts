// SPDX-License-Identifier: Apache-2.0

// A fake Koinon API inside the browser: page.route answers the /auth/*
// endpoints and speaks the Connect protocol (JSON) for unary and server-
// streaming RPCs. The real API path (koinon-api, Postgres, fake model) is
// covered by the E1 end-to-end suite.

import type { Page, Route } from '@playwright/test'

type Json = Record<string, unknown>
export type Stream = { messages: Json[]; error?: { code: string; message: string } }

export type Handlers = {
  unary: Record<string, (req: Json) => Json | { error: { code: string; message: string; details?: { type: string; value: string }[] } }>
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
        const err = res.error as { code: string; message: string; details?: unknown }
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
    { serverId: 's-docs', name: 'edit', description: 'Edit a page', tier: 'TIER_ASK', present: true, reviewReason: 'the description changed; moved to Ask until reviewed' },
    { serverId: 's-docs', name: 'legacy', tier: 'TIER_AUTO', present: false },
    { serverId: 's-docs', name: 'a_very_long_tool_name', description: 'Too long', tier: 'TIER_ASK', present: true, unavailable: 'unavailable: name too long for the gateway' },
    { serverId: 's-issues', name: 'list_issues', description: 'List issues', tier: 'TIER_AUTO', present: true },
    { serverId: 's-wiki', name: 'read', description: 'Read a page', tier: 'TIER_ASK', present: true },
  ]
  let allowed: Json[] = []
  const seen: { create?: Json; tier?: Json; two?: Json; ack?: Json; allow?: Json } = {}
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
    [mcp + 'SetToolTwoApprovers']: (req) => {
      seen.two = req
      const t = find(req)
      t.requiresTwoApprovers = req.required
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

/**
 * askableMembersHandlers answers QuestionService.ListAskableMembers with two members, for the
 * trigger editor's "Who is asked" picker.
 */
export function askableMembersHandlers(): Handlers['unary'] {
  return {
    'koinon.question.v1alpha1.QuestionService/ListAskableMembers': () => ({
      members: [
        { userId: 'u-ana', email: 'ana@example.test', displayName: 'Ana Example', role: 'ORG_ROLE_MEMBER' },
        { userId: 'u-sam', email: 'sam@example.test', displayName: 'Sam Example', role: 'ORG_ROLE_ADMIN' },
      ],
    }),
  }
}

const trace = 'koinon.trace.v1alpha1.TraceDestinationService/'

/**
 * traceDestinationHandlers is an in-memory TraceDestinationService: a healthy Langfuse destination, one
 * whose last test failed and a generic one, plus the org's settings and the platform's destination and
 * ceiling. A write whose endpoint contains "refuse" is refused as a failed test would be. `seen`
 * records the writes. Credentials are never returned.
 */
export function traceDestinationHandlers(opts: { level?: string; ceiling?: string; platform?: boolean } = {}) {
  const at = new Date(Date.now() - 120_000).toISOString()
  const destinations: Json[] = [
    {
      id: 'd-lf', name: 'langfuse', backend: 'BACKEND_LANGFUSE', endpointUrl: 'https://cloud.langfuse.com/api/public/otel/v1/traces', secretFields: ['public_key', 'secret_key'], enabled: true,
      lastTest: { ok: true, testedAt: at }, health: { lastSuccessAt: at, spansDropped: '0' },
    },
    {
      id: 'd-dd', name: 'datadog', backend: 'BACKEND_DATADOG', endpointUrl: 'https://api.datadoghq.com/api/intake/otlp/v1/traces', secretFields: ['api_key'], enabled: true,
      lastTest: { ok: false, message: 'the backend answered HTTP 403 (the credentials were refused)', testedAt: at }, health: { lastSuccessAt: at, lastErrorAt: at, lastError: 'the backend answered HTTP 403 (the credentials were refused)', spansDropped: '1204' },
    },
    { id: 'd-g', name: 'collector', backend: 'BACKEND_GENERIC', endpointUrl: 'https://collector.example.com/v1/traces', secretFields: ['X-Api-Key'], enabled: false },
  ]
  let settings: Json = { contentLevel: opts.level ?? 'CONTENT_LEVEL_NONE', sampleRate: 0.5, platformCopyLevel: 'CONTENT_LEVEL_NONE', ceiling: opts.ceiling ?? 'CONTENT_LEVEL_FULL', hasPlatformDestination: opts.platform ?? true }
  let platform: Json | undefined = opts.platform === false ? undefined : {
    backend: 'BACKEND_DATADOG', endpointUrl: 'https://api.datadoghq.com/api/intake/otlp/v1/traces', secretFields: ['api_key'], enabled: true,
    lastTest: { ok: true, testedAt: at }, health: { lastSuccessAt: at, spansDropped: '3' },
  }
  const seen: { create?: Json; update?: Json; test?: Json; settings?: Json; platformSet?: Json; ceiling?: Json } = {}
  const refused = (input: Json | undefined) => String(input?.endpointUrl ?? '').includes('refuse')
  // Length-delimited string fields by hand: the wire form of google.rpc.ErrorInfo and of TestResult.message.
  const str = (field: number, v: string) => Buffer.concat([Buffer.from([(field << 3) | 2, Buffer.byteLength(v)]), Buffer.from(v)])
  const outcome = 'the backend answered HTTP 401 (the credentials were refused)'
  const refusal = {
    error: {
      code: 'failed_precondition',
      message: `the destination was not saved: the test trace failed: ${outcome}`,
      details: [
        { type: 'google.rpc.ErrorInfo', value: Buffer.concat([str(1, 'TEST_FAILED'), str(2, 'koinon.dev')]).toString('base64') },
        { type: 'koinon.trace.v1alpha1.TestResult', value: str(2, outcome).toString('base64') },
      ],
    },
  }
  const unary: Handlers['unary'] = {
    [trace + 'ListDestinations']: () => ({ destinations }),
    [trace + 'GetSettings']: () => ({ settings }),
    [trace + 'CreateDestination']: (req) => {
      seen.create = req
      const input = req.destination as Json
      if (refused(input)) return refusal
      const d = { id: 'd-new', name: input.name, backend: input.backend, endpointUrl: input.endpointUrl, secretFields: Object.keys((input.secrets as Json) ?? {}), enabled: input.enabled ?? false, lastTest: { ok: true, testedAt: new Date().toISOString() } }
      destinations.push(d)
      return { destination: d }
    },
    [trace + 'UpdateDestination']: (req) => {
      seen.update = req
      const input = req.destination as Json
      if (refused(input)) return refusal
      const d = destinations.find((x) => x.id === req.destinationId)!
      Object.assign(d, { endpointUrl: input.endpointUrl, enabled: input.enabled ?? false, lastTest: { ok: true, testedAt: new Date().toISOString() } })
      return { destination: d }
    },
    [trace + 'DeleteDestination']: (req) => {
      destinations.splice(destinations.findIndex((x) => x.id === req.destinationId), 1)
      return {}
    },
    [trace + 'TestDestination']: (req) => {
      seen.test = req
      const d = destinations.find((x) => x.id === req.destinationId)!
      d.lastTest = { ok: true, testedAt: new Date().toISOString() }
      return { destination: d, result: d.lastTest }
    },
    [trace + 'UpdateSettings']: (req) => {
      seen.settings = req
      settings = { ...settings, contentLevel: req.contentLevel, sampleRate: req.sampleRate, platformCopyLevel: req.platformCopyLevel }
      return { settings }
    },
    [trace + 'GetPlatform']: () => ({ destination: platform, ceiling: opts.ceiling ?? 'CONTENT_LEVEL_FULL' }),
    [trace + 'SetPlatformDestination']: (req) => {
      seen.platformSet = req
      const input = req.destination as Json
      if (refused(input)) return refusal
      platform = { backend: input.backend, endpointUrl: input.endpointUrl, secretFields: Object.keys((input.secrets as Json) ?? {}), enabled: true, lastTest: { ok: true, testedAt: new Date().toISOString() } }
      return { destination: platform }
    },
    [trace + 'SetPlatformCeiling']: (req) => {
      seen.ceiling = req
      opts.ceiling = req.ceiling as string
      return { ceiling: req.ceiling, orgsLowered: 2 }
    },
  }
  return { unary, seen }
}
