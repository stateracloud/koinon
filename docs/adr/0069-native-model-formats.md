# 0069. Models speak their family's native API format

- Status: Accepted
- Date: 2026-10-28
- Relates to: [#148](https://github.com/stateracloud/koinon/issues/148), [0041](0041-koinon-spend-ledger-and-budget-stops.md), [0045](0045-openshell-hardening-defaults.md), [0048](0048-openshell-spike-results.md), [0056](0056-model-providers-pricing-and-budgets-per-org.md), [0058](0058-gateway-modes.md), [0068](0068-prompt-caching-always-on.md)

## Context

ADR 0068 turns prompt caching on for every model with a `cache_read` price. Its amendment maps
direct `anthropic` providers to `CacheUnsupported` until a native Messages route exists. Every
provider is an OpenAI-compatible upstream today: `gateway.Provider` is "one OpenAI-compatible
upstream of an org", and both drivers render each model as an agentgateway `custom` provider with
`formats: [completions]` (`internal/gateway/standalone/render.go`, `internal/gateway/kubernetes/render.go`).
OpenCode talks chat completions to the gateway through `aisdk:@ai-sdk/openai-compatible`
(`flavors/opencode/etc/koinon/opencode/opencode.jsonc`).

That is the wrong format for Claude:

- Anthropic's OpenAI-compatible endpoint says "Prompt caching is not supported" and that it "is not
  considered a long-term or production-ready solution for most use cases"
  ([Anthropic: OpenAI SDK compatibility](https://platform.claude.com/docs/en/api/openai-sdk)).
- agentgateway v1.6.0 cannot translate the problem away. Its completions-to-Messages conversion
  rebuilds content blocks from typed fields, so a `cache_control` on a content part is dropped
  (`crates/llm/src/conversion/messages.rs`).
- On OpenRouter, Claude caches through chat completions only with `cache_control` markers
  (`compatibility.cacheFormat`, ADR 0068). OpenRouter also serves Anthropic Messages at
  `/api/v1/messages`, with native `cache_control`, `session_id` and `provider`
  ([OpenRouter: create a message](https://openrouter.ai/docs/api/api-reference/anthropic-messages/create-a-message.md)).
- OpenRouter serves several families, so a provider has no single right format. Its Claude models
  want Messages and its other models want chat completions.

What the components already do (agentgateway v1.6.0, OpenCode v2.2.0, Anthropic's API docs as of
2026-10-28):

- **agentgateway** serves `/v1/messages` on the same LLM listener and model router as
  `/v1/chat/completions`. The router classifies the request by path
  (`crates/agentgateway/src/llm/model_router.rs`, `classify_route`) and picks the model from the
  body. A Messages request to a provider that supports Messages is passed through: the chat table
  pairs `Messages` with `AnthropicMessages` (`llm/mod.rs`, `CHAT_TRANSLATIONS`), and the body is
  re-serialised from a type that keeps every unknown field in a `#[serde(flatten)] rest` map
  (`llm/mod.rs`, `render_anthropic_messages`; `crates/llm/src/types/messages.rs`). `cache_control`
  survives.
- A `custom` provider with `formats: [{type: messages}]` posts to `<baseUrl path>/messages`
  (`llm/mod.rs`, `set_default_path`). For `https://api.anthropic.com/v1` that is `/v1/messages`.
  It is the same base URL as the OpenAI-compatible endpoint, so existing provider URLs stay valid.
  For `https://openrouter.ai/api/v1` it is `/api/v1/messages`. The custom provider leaves auth
  headers alone (`set_required_fields` has no `Custom` arm), and its catalog and telemetry name is
  its `providerOverride` (`AIProvider::provider`).
- **Usage.** The Messages passthrough stream reads `input_tokens`, `cache_read_input_tokens` and
  `cache_creation_input_tokens` from `message_start` and `message_delta`
  (`conversion/messages.rs`, `passthrough_stream`). A custom provider in Messages format counts as
  `InputExcludesCache` (`llm/mod.rs`, `cache_convention_for`). CEL `llm.inputTokens` is therefore
  normalised to include cache reads and writes (`crates/llm/src/lib.rs`, `include_cache_tokens`),
  which is what `koinon.in`, `koinon.cache_read` and `koinon.cache_write` log today.
- **The agent's gateway key** is removed from the request once the key policy has verified it
  (`crates/agentgateway/src/http/apikey.rs`, `location.remove`), so it never reaches an upstream.
- **OpenCode** rewrites `aisdk:@ai-sdk/anthropic` to its own `@opencode/ai/providers/anthropic`
  (`packages/core/src/aisdk-native.ts`). Its `anthropic-messages` route posts to `<baseURL>/messages`
  and is in `RESPECTS_INLINE_HINTS`. The default `AUTO` policy places up to four breakpoints with no
  TTL, which means the 5-minute cache: the last tool, the first and last system part, and the
  conversation tail (`packages/ai/src/cache-policy.ts`). With `authToken` instead of `apiKey` the
  provider sends `Authorization: Bearer` (`packages/ai/src/providers/anthropic.ts`). Setting both is a
  configuration error.
- **Anthropic** accepts an API key as `Authorization: Bearer`. `x-api-key` is the "legacy fallback,
  still supported". `anthropic-version` is required, and so is `anthropic-workspace-id` with "an API
  key that isn't scoped to a workspace"
  ([Anthropic: API overview](https://platform.claude.com/docs/en/api/overview)). `input_tokens`
  excludes cache reads and writes ([Anthropic: prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)).
- **The sandbox** reaches the gateway only through its OpenShell provider profile. The profile's
  L7 allowlist is `POST /v1/chat/completions` plus the MCP routes, with a bearer credential in
  `authorization` (`internal/reconciler/render.go`, `GatewayProfile`). Under ADR 0045 enforcement a
  request to `/v1/messages` would be denied.

## Decision

1. **Every model family speaks its native API, whoever serves it.** Claude uses Anthropic Messages,
   both from Anthropic directly and through OpenRouter. OpenAI models use chat completions; Responses
   becomes a format value when it is needed. Other families use chat completions until they get a
   native format. Prompt-cache markers on chat completions (`compatibility.cacheFormat`, ADR 0068)
   stay only for families without a native route here, Qwen today, and for the staged OpenRouter
   rollout (decision 4).

2. **The wire format belongs to the model, not the provider.**
   - `models.format` holds the model's wire format: `openai` (chat completions) or `anthropic`
     (Messages). The value set is open: Gemini `generateContent`, Bedrock Converse or OpenAI
     Responses would each be a new value, a gateway renderer, a harness entry and tests.
   - One table in Go (`internal/modelformat`) holds the defaults, and it is the only place that knows
     them:
     - **Provider capabilities:** the formats each provider type offers. `anthropic` offers
       `anthropic`. `openrouter` offers `openai` and `anthropic` (Responses joins when it is a
       value). Every other type offers `openai`.
     - **Family preference:** `claude-*` and `anthropic/*` prefer `anthropic`. Everything else
       prefers `openai`.
     - **Default:** the family's preference when the provider offers it, otherwise the provider's
       first offered format.
   - An admin can override a model's format, within what its provider offers. Saving a format the
     provider does not offer is refused.
   - `models.format_overridden` records whether an admin set the format. At startup `koinon-api`
     re-derives every non-overridden model from the table, and audits each change with the actor
     `koinon-api`. This is how the table changes existing models when a default flips, as the
     startup fill in ADR 0056's W4 amendment does for price settings.
   - Migration `00031_model_formats.sql` adds `models.format text NOT NULL DEFAULT 'openai'` (a CHECK
     on the known values) and `models.format_overridden boolean NOT NULL DEFAULT false`. It sets
     `format = 'anthropic'` for models of `type = 'anthropic'` providers. In the same migration,
     providers get `anthropic_workspace_id` (decision 5).
   - API and UI: `Model.format` and `Model.format_overridden` in `provider.proto` and the OpenAPI
     `Model` schema. The model form offers the provider's formats, with the default marked. The
     providers page lists "Anthropic" as a type. Declared provider Secrets take an optional
     per-model `format` in the `models` list, validated by the same rule; leaving it out means the
     default.

3. **The gateway renders one entry per model, in that model's format, in both modes.**
   - Every model stays a `custom` provider with `providerOverride: <org>.<type>`, so the gateway's
     budget catalog stays per org and type (ADR 0056). agentgateway's native `anthropic` provider is
     not used: it has no `providerOverride`, so its catalog key would be the bare `anthropic` and
     shared by every org.
   - A format is a value in one rendering table, used by both drivers. The table gives the
     agentgateway format (`completions`/`Completions` or `messages`/`Messages`), the request headers
     to set, and the body transformations:
     - `openai`: as today, including the `stream_options.include_usage` transformation.
     - `anthropic`: no `stream_options`, since Messages streams carry usage in `message_start` and
       `message_delta`. It sets the header `anthropic-version: 2023-06-01` (standalone
       `llm.models[].requestHeaders.set`, Kubernetes `policies.headers.request.set`), the version
       OpenCode itself sends.
   - The listener and paths do not change. Agents post to `<gateway>/v1/messages` and the router
     picks the model by its plain name, scoped to the org exactly as for chat completions (the
     `x-koinon-org` match, `allowedModels`, and the authorization rule in Kubernetes mode). Model IDs
     stay `<org>/<model>`. A chat-completions client can still reach an `anthropic` model, because
     agentgateway translates completions to Messages; that path is uncached.
   - The provider key is sent as `Authorization: Bearer`, with the same `params.apiKey` and
     `auth.credentials` rendering as every other provider. If Anthropic ever refuses that,
     `x-api-key` is a one-line change of the credential location.
   - Per-key budgets and limits apply unchanged. The key policy and the budgets sit on the single LLM
     route (`types/local.rs`, `llm:request`), and requests are priced through the `<org>.<type>`
     catalog entry, which already carries cache read and write rates.
   - The routing guard keeps working for OpenRouter models in either format. Its `provider` overrides
     are fields that OpenRouter's Messages endpoint accepts.
   - `gateway.Model` gains `Format`, and `State.Validate` refuses unknown values.

4. **Rollout is staged.**
   - Direct `anthropic` models switch to Messages through the migration. `CacheUnsupported` and its
     `anthropic` reason (ADR 0068 amendment) are removed.
   - OpenRouter `anthropic/*` models keep chat completions with `cacheFormat: "anthropic"` as their
     default until the live check (decision 7) passes. In the table, the family preference for
     OpenRouter is pinned to `openai` until then. **Follow-up step:** after the check passes, flip
     that one entry so OpenRouter `anthropic/*` models default to `anthropic`, and update the catalog
     test's expectation. The startup re-derivation moves existing non-overridden models. Admins can
     opt a model in earlier by overriding its format.
   - `cacheFormat` markers stay for Qwen and for any family whose provider offers no native route.

5. **Anthropic headers.** The gateway pins `anthropic-version: 2023-06-01`. Providers get an optional
   `anthropic_workspace_id` (allowed for type `anthropic` only, matching `^wrkspc_[A-Za-z0-9]+$`). It
   is a provider field in the API and UI and an `anthropicWorkspaceId` key in declared Secrets. When
   it is set, the gateway sends it as the `anthropic-workspace-id` request header on that provider's
   models. It is an identifier, not a credential, so it is stored in plain text.

6. **The harness gets one provider entry per format.**
   - `prices` gains the cache mode `CacheNative`: the harness places the provider's native cache
     markers itself. An `anthropic`-format model is `CacheNative`, its marker is `""`, and
     `--cache-anthropic` is not passed (`cache-policy.ts` selects `AUTO` for the
     `anthropic-messages` route). Chat-completions models keep the ADR 0068 modes.
   - `opencode.jsonc` gets a second static provider beside `koinon`: `koinon-anthropic`, with
     `"package": "aisdk:@ai-sdk/anthropic"` and `"settings": {"baseURL": "{env:KOINON_MODEL_BASE_URL}",
     "authToken": "{env:KOINON_MODEL_KEY}"}`. The token goes out as `Authorization: Bearer`, which is
     where OpenShell rewrites the placeholder and where the gateway's key policy reads it. The base
     URL stays `<gateway>/v1`.
   - The flavor descriptor maps each format to its entry (`openai` to `koinon/`, `anthropic` to
     `koinon-anthropic/`). The entry script takes `--model-format`, and agentd passes the run's model
     format and selects `<entry>/<model>` as the model option. A new format adds a row here.
   - An org with models in both formats needs nothing more: a run uses one model, so it uses one
     entry.
   - The OpenRouter `session_id` body overlay (ADR 0068 amendment) goes on whichever entry the run
     uses, for OpenRouter models only. The tool-output caps and compaction settings are flavor-wide
     and do not change.
   - The sandbox profile adds `POST /v1/messages` for every flavor. It is the same host, credential
     and enforcement as the completions route. It is per flavor, so adding a model does not change a
     profile. `/v1/messages/count_tokens` stays closed, and Koinon sends no 1-hour TTLs.

7. **Live check in the rc.6 home-lab window.** Haiku runs once through a direct Anthropic provider,
   with a key procoduck supplies (about $0.10), and the ledger must show `cache_read_tokens > 0` priced at
   the cache-read rate. In the same window, one run of an OpenRouter `anthropic/*` model with its
   format overridden to `anthropic` (cents, logged against the OpenRouter test budget) must show
   cache reads. Together the two runs gate the OpenRouter flip in decision 4.

8. **Tests.** CI uses only the fake model server.
   - **Fake model server:** `Usage` gains `CacheWriteTokens`, reported as
     `cache_creation_input_tokens` (fixed at 0 today), and `Request` records headers. `/v1/messages`
     with streaming already exists.
   - **`gatewaytest`** (both drivers, real binary), a Messages spec for an `anthropic` model:
     - `cache_control` on system, tool and message blocks arrives unchanged;
     - the upstream sees the provider key, `anthropic-version`, and `anthropic-workspace-id` when
       set, and never the agent's key;
     - there is no `stream_options`;
     - another org's key gets 404, and a key over budget gets 429;
     - the access log carries `koinon.cache_read` and `koinon.cache_write`, with `koinon.in`
       including both;
     - the same org serves an `openai` and an `anthropic` model side by side.
   - **`internal/modelformat`:** table tests of default derivation, offered-format validation and
     re-derivation of non-overridden models only.
   - **`internal/prices` and `internal/agentd`:** the ADR 0068 catalog test accepts `CacheNative`.
     Direct Anthropic Claude models in `pricing/default.yaml` are `CacheNative` and launch with
     `koinon-anthropic`.
   - **`test/flavors`:** with `--model-format anthropic`, OpenCode posts to `/v1/messages` with up to
     four `cache_control` breakpoints and `Authorization: Bearer`. A session saved under `koinon/` and
     resumed under `koinon-anthropic/` keeps its history (amendment PR 3).
   - **Reconciler and escape suite:** the profile allows `POST /v1/messages`. A sandbox can post there
     and still reach nothing else.
   - **e2e (local kind):** one org with an `anthropic` provider on the fake model server. A two-turn
     run posts to `/v1/messages`, and its ledger entry has `cache_read_tokens > 0`.

9. **Delivery: four PRs.**
   1. **Formats foundation** (about 1.5 days): `internal/modelformat`, migration `00031`, sqlc,
      validation, startup re-derivation with audit, proto and OpenAPI (`make generate`), the model
      form's format and the provider's workspace ID in the UI, declared Secret keys, and
      `guides/models.md`.
   2. **Gateway** (about 1.5 days): `Model.Format`, the rendering table in both drivers, the workspace
      header, `POST /v1/messages` in the profile, the fake server changes, the `gatewaytest` spec, and
      Kubernetes mode on kind.
   3. **Harness** (about 1.5 days): `CacheNative`, `--model-format`, `koinon-anthropic`, the model
      prefix per format, `session_id` per entry, flavor and catalog tests, the session-resume test,
      and the e2e path.
   4. **Follow-up after the live check** (an hour): flip OpenRouter `anthropic/*` to `anthropic` in
      the table.

   That is about 4.5 to 5 agent-days, plus the live check.

## Consequences

- Claude models are cached with OpenCode's native breakpoints on direct Anthropic now, and on
  OpenRouter once the flip lands. Cache hits are billed through the existing ledger fields; the
  ledger does not change.
- Koinon maintains two wire formats in the gateway and the harness. Because a format is a value in
  one table with one renderer per driver and one harness entry, a fix to one format is visible next
  to the other. The `gatewaytest` spec runs both on the real binary. Adding Gemini native or Bedrock
  later is the same kind of change.
- Direct `anthropic` models switch format at their next harness start, and a resumed OpenCode session
  changes provider entry. The test in decision 8 covers that (amendment PR 3).
- An admin who overrides a format keeps it through later default changes. Everyone else follows the
  table.
- Keys that are not scoped to a workspace work once the provider's workspace ID is set. The models
  guide says so.
- Cache writes are priced at the 5-minute rate, which is right because Koinon sends no TTL.
  agentgateway v1.6.0 reads only `cache_creation_input_tokens` and does not split 5-minute from
  1-hour writes, so adding 1-hour TTLs would need ledger work first.
- The sandbox gets one more allowed route on the same host, with the same credential.

## Alternatives

- **A format per provider.** It is simpler, but OpenRouter serves several families through one
  provider, so one format would be wrong for some of its models. Rejected.
- **Chat completions in, translated to Messages by the gateway.** agentgateway's translation drops
  `cache_control` on content parts, so OpenCode's breakpoints would be lost. Only a top-level
  `cache_control` set through `finalTransformation` might survive, and that is not yet tested.
  Rejected.
- **Staying on Anthropic's OpenAI-compatible endpoint, or sending Claude through OpenRouter only.**
  Anthropic calls that endpoint not production-ready and it does not cache. Sending everything through
  OpenRouter adds a processor that direct-Anthropic orgs chose not to have. Rejected.
- **agentgateway's native `anthropic` provider.** It moves the key to `x-api-key` and adds
  `anthropic-version` itself, but it has no `providerOverride`, so its catalog entry would be shared
  across orgs. Rejected.
- **One OpenCode provider whose package and settings are rendered per run in
  `OPENCODE_CONFIG_CONTENT`.** That moves provider configuration out of the read-only image file, and
  merging a static `apiKey` with an `authToken` is a configuration error. Rejected.

## Amendment (2026-10-28): agentgateway v1.7.0

- v1.7.0 forwards the client's `anthropic-version` instead of replacing it. Koinon does not set the
  header yet; when it does (decision 3), a `requestHeaders.set` pin must win. The spec in
  `internal/gateway/standalone` shows it does on v1.7.0: a client sends `2099-01-01`; a model with
  the set reaches the upstream with `2023-06-01`, and one without it with `2099-01-01`.
- v1.7.0 also reads a separate 1-hour cache-write rate (`cacheWrite1h`) and
  `cache_creation_1h_input_tokens`, which lifts the limit in the last consequence above. Koinon's
  price catalog has no 1-hour price yet (ADR 0070 plans `Cost.CacheWrite1h`), so nothing changes
  until it does; cache writes stay priced at the 5-minute rate.

## Amendment (2026-10-28): what the gateway spec showed (PR 2)

- The rendering table is `gateway.Rendering` in `internal/gateway/formats.go`. Both drivers read it,
  and `gateway.Provider` gained `AnthropicWorkspaceID` (checked against `^wrkspc_[A-Za-z0-9]+$` in
  `State.Validate`, since it becomes a header value).
- agentgateway v1.7.0 re-serialises a Messages body: the values of `system`, `tools` and `messages`
  arrive unchanged, `cache_control` included, but typed fields come first and unknown fields such as
  `cache_control` last, so the bytes are not identical. The spec compares JSON values.
- A key of another org is refused with 403 in both modes (`allowedModels` in standalone,
  the org authorization rule on the Kubernetes proxy); the specs accept 403 or 404, as the chat-completions
  specs do.
- **Threat: an agent is untrusted, and the Messages API reaches beyond the gateway.** A client's
  `anthropic-workspace-id` would pick the workspace an unscoped key acts in, possibly another
  tenant's. `anthropic-beta` enables the MCP connector (Anthropic itself calls the agent's URL, which
  bypasses the sandbox's egress rules), code execution, web fetch and long context. `mcp_servers` and
  the server tools use those features. **Controls**, all in the rendering table: the gateway removes
  `anthropic-beta` and, for a provider without a workspace ID, `anthropic-workspace-id`
  (`requestHeaders.remove` / `policies.headers.request.remove`), and sets the version and the
  provider's workspace. It filters `tools` to those without a type or of type `custom`, so every
  server tool, present or future, is dropped. It refuses a request that has `mcp_servers` with 403,
  because dropping an untyped field is not possible: a `null` transformation would add
  `"mcp_servers":null` to every request. The refusal is a CEL condition in the authorization rule
  (`!has(llmRequest.mcp_servers)`). The refusal is the generic 403 of the authorization rule, with no
  message naming the field (Koinon's harnesses never send `mcp_servers`). The tools filter applies to
  Messages requests only (`request.path == "/v1/messages"`), so a chat-completions request sent to an
  anthropic-format model keeps its `function` tools. If OpenCode ever needs a beta flag, PR 3 captures what it sends and
  pins only that with `set` from the table. The gatewaytest spec sends the attack in both drivers.
- The sandbox profile's routes come from the same table (`Rendering.Path`), so a new format needs no
  profile edit.
- **Cut streams.** A Messages response cut mid-stream is logged with status 200, because the status was
  sent before the cut. The access log carries what lets the ledger tell: `koinon.format` (from the
  request path, `anthropic` or `openai`), `koinon.stream` (`llm.streaming`) and `koinon.error`
  (`proxy.error.reason`, set only when the proxy failed the response; `Upstream` for a dropped upstream
  connection). A cut stream's record has the usage of `message_start` alone (the spec pins
  `koinon.out` 1 against 10 for the finished response). The ledger side (settling a streamed
  anthropic-format record as cut when its run was canceled at or before the log time) is in the spend
  settlement work, not here.
- An empty `Model.Format` is an error in `State.Validate`; the reconciler always fills it.
- The workspace-ID rule (`modelformat.WorkspaceIDPattern`, `MaxWorkspaceIDLength`, `ValidWorkspaceID`)
  has one home, used by the catalog and the gateway; a test compares it with the migration's CHECK.

## Amendment (2026-10-28): the harness (PR 3)

- `prices.CacheModeFor(providerType, model, format, price)` is the one decision point: an
  `anthropic`-format model is `CacheNative` (no marker, no `--cache-*`), whoever serves it, and chat
  completions keep the ADR 0068 modes by provider (an OpenRouter `anthropic/*` model on `openai`
  stays `CacheAnthropic` until the flip). `CacheUnsupported` and its reason are gone; nothing maps to
  it. `prices.Model` carries the model's `Format` (from `models.format`).
- `flavor.Descriptor.ModelEntries` maps each format to the OpenCode provider entry and `FormatArg` is
  the entry script's switch. agentd always passes `--model-format` and selects `<entry>/<model>`. The
  script sets the default model and the OpenRouter `session_id` overlay on the entry in use.
- **Beta flags:** on the `koinon-anthropic` entry OpenCode 2.2.0 sends `anthropic-beta:
  interleaved-thinking-2025-05-14` only. The model declares no reasoning, so no `thinking` block is
  sent and the beta is a no-op; the gateway keeps removing the header and the rendering table pins
  nothing. A flavor spec fails if OpenCode's set changes.
- **Resume across the switch:** a session created under `koinon/` loads under `koinon-anthropic/` and
  the other way round, with its history (a tool-call turn included) in the next request and the next
  request in the new format. No new session is needed. OpenCode rewrites every character outside
  `[a-zA-Z0-9_-]` in a tool-call ID to `_` when it builds a Messages request
  (`scrubToolCallID`, `anthropic-messages.ts`), which Anthropic's ID pattern needs. A spec resumes a
  session whose tool call is `functions.write:0` and sees `functions_write_0`.
- **The OpenRouter flip** deletes the two pin rows in `internal/modelformat/modelformat.go`
  (`anthropic/` and `claude-` for OpenRouter); the startup re-derivation moves every model that is
  not overridden. Nothing else changes: a flipped model becomes `CacheNative` through
  `CacheModeFor`.
- **Sandbox auth:** both entries send `Authorization: Bearer <placeholder>`, the header the profile's
  `authStyle bearer` credential names, and the gateway's key policy reads (OpenShell v0.1.5 rewrites the placeholder in any header, so
  the header is chosen for the key policy). The flavor spec asserts
  one `Authorization` value and no `x-api-key` on every Messages request.
- **e2e (kind):** the quickstart driver declares a second, direct `anthropic` provider on the fake
  model server (`test/e2e/kind/model-provider-anthropic.yaml`, script `claude-e2e.yaml`). A run on its
  model posts only to `/v1/messages` (the fake server's `GET /_fake/requests`), and its ledger entries
  show a cache write and then `cache_read_tokens > 0`, each priced at its own rate. The ledger's
  `input_tokens` includes cache reads and writes.
