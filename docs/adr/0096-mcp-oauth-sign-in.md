# 0096. MCP sign-in: OAuth-authorized remote MCP servers

- Status: Accepted (2026-11-06)
- Date: 2026-11-06
- Amends: [0057](0057-mcp-servers.md) (amendment 2026-10-24, items 2 and 5: the OAuth refusal and the
  injection path for servers that need a sign-in)
- Relates to: [0005](0005-agent-identity.md) (agents act as themselves; `delegated-user`),
  [0073](0073-agent-passport.md) (the token service and its delivery spike, decisions 6, 11 and 12),
  [0056](0056-model-providers-pricing-and-budgets-per-org.md) (the KEK and envelopes),
  [0085](0085-upgrade-compatibility-and-recovery-policy.md) (`keys forget`, `keys reencrypt`, decisions 13 and 14),
  [0084](0084-leak-guard.md) and [0091](0091-leak-guard-by-destination.md) (the leak guard and sink classes),
  [0065](0065-work-model-tasks-inbox.md) (parks and the inbox), [0023](0023-observability-and-audit.md) (fixed metric labels),
  ADR 0097 (Koinon's own gateway; step 2 is the MCP path that delivers sign-in tokens; Proposed, on
  branch `docs/258-own-gateway-adr`)
- Issue: #237
- Verification: Pending: the checks Koinon's own MCP path must pass (below), in ADR 0097 step 2 (v0.2)

## Context

The maintainer connected Cloudflare's remote MCP server (`https://mcp.cloudflare.com/mcp`) to Claude
by signing in and picking permissions in the browser, with no token to copy. Koinon should connect
remote MCP servers the same way. Today it cannot: a server whose 401 carries an RFC 9728
`resource_metadata` URL is refused when it is added (`internal/mcpservers/diagnose.go`; ADR 0057
amendment item 2), and the quickstart's step 6b shows that refusal. Only one static credential per
server exists, held as a file in the gateway-only Secret (ADR 0057 item 5).

The MCP authorization specification (version 2026-07-28) asks the client to:

- discover the authorization server (AS) from the 401's `resource_metadata` (RFC 9728), falling back
  to the well-known URIs, then read the AS metadata by RFC 8414 or OIDC discovery, with a matching
  `issuer`;
- identify itself in this order: a pre-registered client, a Client ID Metadata Document (CIMD), Dynamic
  Client Registration (DCR, deprecated), else ask the user; pre-registered and DCR state is kept per
  issuer;
- use the authorization-code flow with PKCE S256 (refuse an AS that does not list it), send the RFC 8707
  `resource` on both the authorize and the token request, and check `iss` on the callback (RFC 9207);
- start from the 401's `scope` and, on `403 insufficient_scope`, re-authorize with the union of scopes,
  a few times at most;
- keep refresh tokens confidential (they may not be issued), send the token only in the
  `Authorization` header, and never pass it to another service.

The specification has no section for a server-side, multi-user, unattended client, which is what
Koinon is. Cloudflare (live, 2026-11-06) answers an unauthenticated POST with 401,
`resource_metadata` and `scope="user:read account:read"`; its AS lists `client_id_metadata_document_supported: true`,
a `registration_endpoint`, S256 only, `authorization_code` and `refresh_token`, token endpoint auth
including `none` and `private_key_jwt`, the `iss` parameter, and `offline_access` among its scopes.

agentgateway (read on main `5f93d44`; Koinon pins v1.7.0) has no upstream consent flow, no persistent
upstream token store and no refresh-token handling. It has a per-backend `oauthTokenExchange` policy
(RFC 8693) that exchanges the inbound credential per request and caches the result by `expires_in`,
and its ExtMcp processor result carries a `header_mutation` and a `metadata` bag. The header mutation
is applied to every backend of the call, and `*/list` methods fan out to every backend.

ADR 0005 has agents act as themselves; on-behalf-of is a per-connector opt-in (`delegated-user`).
ADR 0073 already plans the pieces this needs: a token service in koinon-api that the gateway calls per
MCP call, a spike to prove a different token per target, and per-user OAuth for MCP in v0.2.

Comparable products (Claude connectors, LibreChat, Cursor) hold tokens server-side, bound to a subject
and a server, refresh on use, retry once, then show a clear "sign in again" state. None documents
what an unattended run does with a dead token.

## Decision

### 1. Terms

An **MCP sign-in** is Koinon's stored OAuth authorization to one MCP server: the access token, the
refresh token, the issuer, the resource, the scopes and the expiry, with one owner. It is not a
**grant** (an agent's access to a connection) and not a static credential. The glossary carries the
term.

### 2. Maintainer decisions (procoduck, 2026-11-06)

1. **Owner: org-wide first, with an owner field on every sign-in.** An org admin signs in once, and
   every agent allowed to use that server uses the sign-in (ADR 0005: agents act as themselves). Every
   stored sign-in carries an owner kind, `org` or `user`, and an owner ID from its first migration, so
   per-user sign-in (`delegated-user`, personal agents of [0019](0019-personal-agents-and-teams.md))
   follows with the agent passport without migrating the table.
2. **A dead or missing sign-in parks the run and asks.** Before a run starts (before a sandbox is
   requested), Koinon checks the sign-ins of the servers the agent is allowed to use. A missing,
   lost, revoked or unrefreshable sign-in parks the run with reason `authorization_required` and posts
   an inbox item with "Sign in again" to the sign-in's owner (org admins for an org sign-in). Signing in
   resumes the run; the existing park expiry cancels it. A 401 after a refresh, or a
   `403 insufficient_scope`, during the run parks it on that call the same way. A run is never failed
   outright for this, and the server's tools are never quietly dropped (the target's `failOpen` must
   not hide a missing sign-in).
3. **Milestone: v0.2, with the agent passport.** This ADR is written now. The build lands in v0.2
   together with ADR 0073's token service and Koinon's own MCP path (ADR 0097 step 2). Until then the quickstart keeps refusing
   servers that ask each user to sign in, and `diagnose.go` keeps its refusal.
4. **Testing in three stages.**
   - A: a fake AS and a fake OAuth MCP server in the kind e2e. This is the merge gate, and it replaces
     the quickstart's refusal step.
   - B: a self-hosted OIDC AS (e.g. Pocket ID), a pre-registered public client with PKCE and a real passkey sign-in;
     then CIMD (off by default, since Pocket ID v2.13).
   - C: Cloudflare's MCP server, read-only scopes, the maintainer authorizing in the browser.
   - B and C are tested on a maintainer cluster.
5. **Delivery only through Koinon's own MCP path** (maintainer decision 2026-11-06). Sign-in tokens
   reach MCP servers only through Koinon's MCP path (ADR 0097 step 2), which ships in the same release,
   v0.2. The agentgateway delivery spike is dropped: a token handoff built on agentgateway would be
   built twice, once there and again when Koinon's MCP path replaces agentgateway's MCP configuration
   in the same release.
6. **The gateway renews sign-in tokens** (maintainer decision 2026-11-06, second review). Only
   koinon-gateway can decrypt stored credentials (ADR 0097), so it refreshes sign-ins too. koinon-api
   runs discovery, the browser sign-in start and the callback, and seals the new tokens under the KEK
   without ever decrypting them.

### 3. Koinon is the OAuth client, split between koinon-api and the gateway

agentgateway cannot sign in, store or refresh, so all of that is Koinon's job. koinon-api starts and
finishes the sign-in and seals the tokens; koinon-gateway, the only process that decrypts them,
refreshes and attaches them.

- **Discovery.** The anonymous probe of ADR 0057 item 2 becomes discovery: follow `resource_metadata`
  (RFC 9728), else the well-known protected-resource URIs; read the AS metadata by RFC 8414 or OIDC
  discovery and require `issuer` to match. The probe still dials only addresses the egress policy
  allows and carries no credential. The Servers page shows "needs sign-in" instead of the refusal.
- **Client identity, in spec order, kept per AS issuer.**
  1. An admin-entered client (client ID and, for a confidential client, its secret, sealed under the
     KEK) for an AS that offers neither CIMD nor DCR. Org-scoped.
  2. CIMD: each install serves its client metadata document at
     `https://<public UI URL>/oauth/client-metadata.json`, naming its own callback URI. Only the
     document must be reachable by the AS; the redirect is opened by the user's browser, so a lab-only
     callback works. An install whose URL the AS cannot reach sets a public document URL (a chart
     value; the document holds no secret). Koinon prefers `private_key_jwt` with the install's JWKS
     (the ADR 0073 issuer) where the AS lists it, else `none` with PKCE.
  3. DCR, when the AS has no CIMD or the install cannot publish a document. The registration is kept
     per issuer and sealed.
  - A changed issuer means re-registration (pre-registered and DCR); CIMD IDs are portable.
- **The flow.** Authorization code with PKCE S256 and `resource` (RFC 8707, the server's canonical
  URL) on both requests. A pending flow (state, the person, the server, the recorded issuer, the
  PKCE verifier sealed, the scopes, about 10 minutes of life) is a Postgres row, so the start and the
  callback may land on different koinon-api replicas. The callback checks the state against the
  person's Koinon session and `iss` against the recorded issuer, then exchanges the code. Scopes start
  from the 401's `scope`; step-up re-authorizes with the union, by a person, never inside a run.
  `offline_access` is requested when the AS lists it, because unattended runs need a refresh token.
- **Storage.** Access and refresh tokens are envelopes under the KEK (ADR 0056), in an org-owned
  table keyed by `(org_id, id)` with the owner kind and ID, the server, issuer, resource, scopes,
  expiry, status (`active`, `needs_sign_in`, `lost`, `revoked`), who signed in and when, the last
  refresh and the last error. koinon-api writes the envelopes after the callback (it seals, never
  decrypts); koinon-gateway alone decrypts them and rewrites them on refresh.
- **Refresh by koinon-gateway.** The gateway refreshes when less than 5 minutes remain, single-flight
  per sign-in: a Postgres row lock, so only one of several gateway replicas refreshes. It stores a
  rotated refresh token in the same transaction, before the new access token is used. `invalid_grant`
  makes the sign-in `needs_sign_in` at once; network and 5xx errors retry with backoff while an
  unexpired access token is still served.
- **Delivery by Koinon's MCP path.** Koinon's MCP path (ADR 0097 step 2) reads the target's sign-in
  per upstream call, decrypts the current access token (refreshing it first when due), and attaches
  it to that target's upstream client only. The token exists only between the gateway and the MCP
  server; the sandbox holds only its gateway key placeholder. One gateway target per org and server
  stays (ADR 0057 item 4).
- **Approvals.** An approved call runs through koinon-api with the executor key (ADR 0091 section 5).
  The gateway maps an executor call to the asking run's sign-in: the org sign-in now; for a
  per-user sign-in later, the run's `on_behalf_of` through the passport's `act` chain.
- **Library.** go-sdk `oauthex` (protected-resource and AS metadata, DCR) and `golang.org/x/oauth2`
  (code exchange, refresh), both already in `go.mod`. Koinon owns the persisted state machine; the
  go-sdk `AuthorizationCodeHandler` keeps tokens in one process's memory and does not fit a split
  start and callback across replicas _(inferred)_.

### 4. Traps the design must avoid

- **Never let the token reach the sandbox or the agent.** The token goes only on the upstream
  request to its own server, never into a header, field or result the agent can see, and never into
  anything shared by every server of a call: `tools/list` fans out, so a shared header would send one
  server's token to every server of the org.
- **Never rewrite the gateway Secret on refresh.** Kubelet propagation lag would force early refresh,
  every refresh would churn the Secret and the config, and per-user sign-in would need a target per
  user. Static credentials keep ADR 0057 item 5; sign-ins never go into that Secret.
- **The leak guard strips a server's current token from tool results.** The response phase knows the
  exact access token of that target and redacts it, closing the echo case (a server that reflects
  the `Authorization` header into a result). Sink classes do not change: a server that needs a
  sign-in declares its tools like any other, and its results count as private data (ADR 0091).
- **`keys forget` takes the same "sign in again" path.** Every sign-in envelope, the pending verifier
  and the admin-entered or DCR client secrets are listed in the keys tool (`internal/keyscli`). A
  forgotten key marks the sign-ins `lost`, which parks runs and posts "Sign in again" like
  `needs_sign_in`.
- **Deleting a server or removing a user revokes at the provider.** Deleting a server, removing a user
  from the org (their per-user sign-ins, later) or an admin's "Revoke" calls RFC 7009 revocation at
  the AS when it advertises an endpoint, then deletes the envelopes. An org sign-in whose signer leaves
  the org is flagged on the Servers page, not silently kept.
- **A compromise adds "revoke all sign-ins".** `keys reencrypt` reseals but does not revoke (ADR 0085
  decision 14), so the compromise runbook gains a step that revokes every sign-in at its AS and marks
  them `revoked`, which feeds the same "Sign in again" path.
- **A sign-in is a person's account.** An org sign-in acts with the upstream account of whoever signed
  in (Cloudflare has no bot accounts). The UI names the signer and advises a dedicated service account
  at the upstream.

### 5. Audit and observability

- Audit events: signed in (signer, issuer, scopes), refresh failed, signed in again, revoked, lost.
  Refreshes go to a ledger-style row per sign-in, not one audit event per call (ADR 0073 decision 10).
- A token is never logged, in the access log, the run timeline, traces or audit; only the sign-in ID.
- Metrics use fixed label sets (ADR 0023): for example refresh outcome and AS issuer class, never an
  org, user, agent, run or sign-in ID.

### 6. Rules that stay

- No secrets in sandboxes: tokens, refresh tokens, client secrets and PKCE verifiers stay in Postgres
  envelopes, koinon-api and the gateway. The escape suite checks that no sign-in token is readable in
  the sandbox's environment, files or home.
- No Kyverno: nothing here needs admission policy beyond the native ValidatingAdmissionPolicy in use.
- No CNI-specific features: egress to an AS and an MCP server uses the existing NetworkPolicy and
  `egress.allowedInternalHosts`.
- Metric labels never carry org, agent, user, ticket or run IDs.

## Checks for Koinon's MCP path

The agentgateway delivery spike is dropped (decision 2.5). Koinon's MCP path (ADR 0097 step 2) must
pass these checks instead, as tests with a fake AS and fake MCP servers (one that requires a bearer
token for its audience and records every `Authorization` header it sees, and one with a static
credential), in the gateway behaviour suite and the stage A e2e:

1. **Each server gets only its own token on fan-out.** On a fan-out `tools/list` and `tools/call`,
   each server receives only its own token, and the static server none (ADR 0097 step 2, exit
   criterion 2).
2. **`initialize` carries the token.** The upstream session's `initialize` request already carries
   the target's token.
3. **A dead sign-in parks the run cleanly.** When the target's sign-in is dead, lost, revoked or
   cannot be refreshed, the agent gets a clean error the run parks on with `authorization_required` (decision
   2.2), not a 500.
4. **Refreshed tokens are picked up without reload.** After one gateway replica refreshes a sign-in, the
   next upstream call on any replica uses the new access token, with no config reload and no gateway
   restart; concurrent calls on one sign-in refresh it once.

A failing check goes back to the maintainer before the build continues, not into a workaround.

## Open, unverified points

These are checked in the build stages named in the table; a failed check leads to an amendment of this ADR.

| Point | How it is answered |
|---|---|
| How the gateway's refresh row lock behaves under load (lock wait, a replica that dies mid-refresh) | ADR 0097 step 2 tests: concurrent calls on one sign-in refresh once; a killed refresher leaves the old refresh token usable |
| Cloudflare's access-token lifetime, and whether a refresh token comes without `offline_access` | Stage C: record `expires_in` and the refresh behaviour over a day |
| Cloudflare accepts a redirect URI on a private hostname | Stage C: CIMD with a private callback; fall back to a public callback if refused |
| Pocket ID supports RFC 8707 `resource` (and DCR) | Stage B: a live check of the authorize and token requests with `resource` |

## Alternatives considered

- **Per-user sign-in first.** Matches Claude connectors, but it needs the passport's `on_behalf_of` to
  know whose token a call uses, conflicts with ADR 0005 for shared agents, and leaves scheduled runs
  without a token. Rejected for the first slice; the owner field keeps it open.
- **Org-only sign-in without an owner field.** Cheaper now, but per-user sign-in later would migrate a
  live credential table and rework the token lookup and the audit actor. Rejected.
- **Per-agent sign-ins.** Two upstream accounts for two agents are two servers with two names, and
  per-agent narrowing is already the grant (`mcpAuthorization`). Not needed.
- **Fail the run, or let `failOpen` skip the server, when a sign-in is dead.** Failing loses a scheduled
  job and gives a person a failure instead of a button; skipping lets the agent run without the tools
  and perhaps do the wrong thing. Rejected for park and ask.
- **A refresher that rewrites the gateway Secret.** Reuses ADR 0057 item 5, but see section 4: early
  refresh, Secret churn, and no path to per-user. Rejected.
- **The ExtMcp `header_mutation` as delivery.** Sends one server's token to every server on a fan-out.
  Rejected.
- **Token delivery through agentgateway** (per-target `oauthTokenExchange`, or ExtMcp metadata placed
  by `backendAuth` CEL, tested in a spike). Koinon's MCP path replaces agentgateway's MCP
  configuration in the same release (ADR 0097 step 2), so the handoff would be built twice. Dropped
  (decision 2.5).
- **The sign-in flow in agentgateway.** It has none (no consent flow, store or refresh), and an
  upstream feature would still not hold per-org state under the KEK. Rejected.
- **DCR only.** Works with Cloudflare today, but DCR is deprecated, leaves a registration per install
  at every AS, and many enterprise ASes refuse it. Kept as the third choice.
- **Tokens in the sandbox (an OpenShell provider record per server).** Breaks "no secrets in
  sandboxes" and puts the token in reach of the model. Rejected.

## Consequences

- Positive: servers like Cloudflare's connect with a browser sign-in and no token to copy; unattended
  runs ask for a sign-in instead of failing; per-user sign-in needs no table migration.
- Positive: sign-in tokens are decrypted only in koinon-gateway, the same as every other stored
  credential (ADR 0097); the internet-facing koinon-api only seals them.
- Negative: Koinon owns an OAuth client state machine (discovery, three client-identity methods,
  pending flows, refresh, revocation), split across koinon-api (sign-in) and koinon-gateway
  (refresh), and the gateway needs Postgres and egress to each AS for these servers.
- Negative: an org sign-in acts as one person's upstream account _(inferred)_.
- Follow-ups: the checks for Koinon's MCP path, in ADR 0097's step 2 work; amend ADR 0057 items 2 and 5 when the build lands; the stage A fixtures and
  the quickstart step that replaces 6b; stages B and C tested on a maintainer cluster; the "revoke all
  sign-ins" step in the compromise runbook; the keys tool list.

## Upstream verification

- 2026-11-06: MCP authorization specification 2026-07-28 (authorization, client registration,
  authorization server discovery) read by the research sweeps.
- 2026-11-06: Cloudflare's protected-resource and AS metadata fetched live; 401 probed with curl.
- 2026-11-06: agentgateway main `5f93d44`: `http/auth/oauth/mod.rs`, `oauth/cache.rs`,
  `mcp/guardrails/client.rs` (header mutation for all backends), `mcp/auth.rs`. Not checked at v1.7.0.
- 2026-11-06: go-sdk v1.8.0 module cache (`auth/authorization_code.go`, `oauthex`).
- 2026-11-06: Pocket ID docs and releases v2.13.0 (CIMD) and v2.14.0 (RFC 8414); `resource` unverified.

## Links

- Issue #237 and its "Decisions (maintainer, 2026-11-06)" comment
- https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization
- https://datatracker.ietf.org/doc/html/draft-ietf-oauth-client-id-metadata-document-00
- RFC 9728, RFC 8414, RFC 8707, RFC 9207, RFC 7591, RFC 7009, RFC 8693
- https://claude.com/docs/connectors/building/authentication.md
- https://pocket-id.org/docs/guides/client-id-metadata-documents
- [0005](0005-agent-identity.md), [0057](0057-mcp-servers.md), [0073](0073-agent-passport.md),
  [0056](0056-model-providers-pricing-and-budgets-per-org.md), [0085](0085-upgrade-compatibility-and-recovery-policy.md),
  [0091](0091-leak-guard-by-destination.md), [0065](0065-work-model-tasks-inbox.md)
