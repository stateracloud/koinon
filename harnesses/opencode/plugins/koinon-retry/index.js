// SPDX-License-Identifier: Apache-2.0
//
// koinon.retry makes OpenCode 2.2.0 retry a failed model request on Koinon's policy, not its own
// (ADR 0056 amendment 2026-10-31, point 3; harnesses/opencode/README.md, "Model retries"). OpenCode
// is the one layer that retries: koinon-agentd does not repeat the turn for this harness.
//
// OpenCode's own schedule (packages/core/src/model-retry.ts) allows 10 retries, exponential from
// 2 s with a 10 s cap per gap, and honours a Retry-After of up to 15 minutes; no config key sets
// it. Before each retry it asks the session.retry hook (packages/core/src/session/runner/retry.ts),
// once per failed try of one step, with the try about to be made (event.attempt, 2 for the first
// retry), its proposed decision, and the error, which carries the provider's Retry-After as
// retryAfterMs (packages/core/src/session/to-session-error.ts). This hook replaces the decision.
//
// The options come from koinon-opencode's switches through the read-only config
// (/etc/koinon/opencode/opencode.jsonc): tries (the first request included), backoffMs (the first
// wait, doubled per retry up to a minute, with jitter of ±20% as OpenCode's own schedule has) and
// maxRetryAfterMs (a longer Retry-After ends the retries, and the turn fails as throttled).

const MAX_BACKOFF_MS = 60_000

function whole(options, key, min) {
  const raw = options?.[key]
  const n = typeof raw === "string" && /^[0-9]+$/.test(raw) ? Number(raw) : raw
  if (!Number.isSafeInteger(n) || n < min) {
    throw new Error(`koinon.retry: option ${key} must be a whole number of at least ${min}, got ${JSON.stringify(raw)}`)
  }
  return n
}

// A 4xx other than 408 (timeout), 409 (conflict) and 429 (rate limit) is the request's own fault
// (a bad key, no access, a malformed request) and fails the same way again. The gateway's
// x-should-retry: false marks a refusal of any status (internal/gateway DoNotRetry); OpenCode 2.2.0
// reads it before this hook and its session error carries no headers today, so the header check
// only takes effect if a later OpenCode passes them on. A budget refusal the gateway cannot mark (a
// 429 with a Retry-After of hours) ends on maxRetryAfterMs.
const RETRYABLE_4XX = new Set([408, 409, 429])

function refused(error) {
  const status = error?.status
  if (typeof status === "number" && status >= 400 && status < 500 && !RETRYABLE_4XX.has(status)) return true
  const headers = error?.response?.headers ?? error?.headers
  const mark = headers?.["x-should-retry"] ?? headers?.["X-Should-Retry"]
  return typeof mark === "string" && mark.toLowerCase() === "false"
}

export default {
  id: "koinon.retry",
  setup: async (ctx) => {
    const tries = whole(ctx.options, "tries", 1)
    const backoffMs = whole(ctx.options, "backoffMs", 0)
    const maxRetryAfterMs = whole(ctx.options, "maxRetryAfterMs", 0)
    await ctx.session.hook("retry", (event) => {
      // Koinon never retries a refusal, whatever OpenCode would do: this guard keeps the rule here
      // rather than in OpenCode's retryable table.
      if (refused(event.error)) {
        event.decision = { retry: false }
        return
      }
      // OpenCode found the failure not worth a retry (not retryable, x-should-retry: false from the
      // gateway, a fourth timeout): that stands.
      if (!event.decision.retry) return
      if (event.attempt > tries) {
        event.decision = { retry: false }
        return
      }
      const retryAfterMs = event.error?.retryAfterMs
      if (typeof retryAfterMs === "number") {
        event.decision = retryAfterMs > maxRetryAfterMs ? { retry: false } : { retry: true, delay: retryAfterMs }
        return
      }
      const base = Math.min(backoffMs * 2 ** (event.attempt - 2), MAX_BACKOFF_MS)
      event.decision = { retry: true, delay: Math.round(base * (0.8 + 0.4 * Math.random())) }
    })
  },
}
