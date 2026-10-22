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
