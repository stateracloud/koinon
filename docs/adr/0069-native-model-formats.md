# 0069. Models speak their family's native API format

- Status: Accepted. The OpenRouter live check (decision 7) passed in the rc.7 home-lab re-run on 2026-10-29 (`anthropic/claude-haiku-5.5` in the `anthropic` format, 5 requests, charged 31% of the uncached list price so cache reads were priced, no `run.uncached` events), and the OpenRouter pin rows are deleted (old issue 242): OpenRouter `anthropic/*` models default to `anthropic` like every other Claude model. Thinking: see the amendments of 2026-10-29 and 2026-10-31.
- Date: 2026-10-28
- Relates to: [#22](https://github.com/stateracloud/koinon/issues/22), [0041](0041-koinon-spend-ledger-and-budget-stops.md), [0045](0045-openshell-hardening-defaults.md), [0048](0048-openshell-spike-results.md), [0056](0056-model-providers-pricing-and-budgets-per-org.md), [0058](0058-gateway-modes.md), [0068](0068-prompt-caching-always-on.md)

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
   - The harness's format is the format of the run's **effective model** (the agent's setting, then
     the version's model, then the org default; ADR 0072), looked up by name, never of the
     version's model alone. An agent set to a Messages model runs on the `koinon-anthropic` entry
     even when its version names a chat-completions model, and the reverse.
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
  The catalog and the ledger now have the rate and the column (ADR 0070, "Landed", #270); what is
  still missing before Koinon may send 1-hour TTLs is a gateway log field with the 1-hour token count.

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
  nothing. A flavor spec fails if OpenCode's set changes. **Wrong for a Claude 5.1+ ID, found in
  rc.7: superseded by the amendment of 2026-10-29 below.**
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

## Amendment (2026-10-29, rc.8): thinking is on unless the agent turns it off (old issue 240, old issue 242)

### Context

rc.7 failed on direct Anthropic with `thinking.adaptive.block_binding: Extra inputs are not permitted`
on every turn. OpenCode 2.2.0 decides from the model ID, not from the config: for an ID that parses as
Claude 5.1 or later it gives every Messages request `thinking: {"type": "adaptive", "block_binding":
...}` and the beta `thinking-binding-controls-2026-08-01` that goes with the field
(`anthropic-messages.ts` `applyThinkingBindingDefault`, `claude-model.ts`). The gateway removes
`anthropic-beta` (PR 2), so Anthropic saw a beta-only field without its beta. PR 3's "no thinking is
sent" was true only for the fake model's ID, which does not parse as Claude. The OpenRouter runs very
likely carried the same block, and OpenRouter accepted it: those runs were probably on adaptive thinking.
The fault was Koinon not deciding whether a model thinks, which is a cost and behaviour choice (thinking
tokens are billed as output).

### Decision (procoduck, 2026-10-29)

1. **Koinon decides; no harness does.** Thinking is **on by default** with a per-agent opt-out.
   Turning it off is a setting, not a code path, so the next model family changes a table row.
2. **Catalog capability per model.** `internal/thinking` is the one table, in the manner of
   `internal/modelformat`: for a provider type, a model name and the model's format, a mode and the
   `anthropic-beta` values that mode needs. Today the modes are `none` and `adaptive` (Anthropic's
   adaptive thinking on Messages). A model is `adaptive` when its family is in the table and it speaks
   `anthropic`; a Claude ID not in the table (or on chat completions) is `none`, because a field or a
   beta the model does not know is a refusal. The families are the four the price list ships
   (`claude-fable-5-`, `claude-opus-5-`, `claude-sonnet-5-`, `claude-haiku-5-`), matched on the name
   with the vendor prefix and dots normalized, so `anthropic/claude-haiku-5.5` and `claude-haiku-5-5`
   are one model. A new family or mode is a row; the model-metadata work of ADR 0070 may later fill the
   table from models.dev.
   - **Not built: reasoning effort for OpenAI-style models.** The decision names it as the second mode.
     It is not in the table because nothing renders it yet: no flavor switch would turn it on or off,
     and a mode the UI offers but no harness honours would be a lie. It joins as a mode with a row and
     a case in the flavor's entry script, and until then those models are `none` ("not supported by this
     model"). Their hidden-thinking flag in the price list is unchanged. **Superseded by the
     amendment of 2026-10-31: every catalog model thinks, and chat-completions models have rows
     (`effort-none` off form for Luna and DeepSeek on OpenRouter).**
3. **The gateway pins the beta per model, from the catalog.** The Messages rendering sets
   `anthropic-beta` to the model's mode betas (`gateway.Model.Thinking`, set by the reconciler from
   `thinking.Of`) and removes the client's header only for a model that needs none. It never passes the
   agent's header through, so the PR 2 threat model (MCP connector, code execution, web fetch, long
   context) stands. The pin is per model, not per request: a thinking-capable model always carries its
   beta, whether or not this agent has thinking on, because a beta no field uses changes nothing and
   the gateway cannot see the agent. For direct Anthropic the pinned value is
   `thinking-binding-controls-2026-08-01` only. `interleaved-thinking-2025-05-14`, which OpenCode also
   sends, is not pinned: Anthropic's documentation of adaptive thinking lists no beta for it, so the
   adaptive request Koinon sends should not need one. No spec in the suite shows that: the flavor
   spec runs with OpenCode's own header (it asserts the header is present) and the strict fake has
   no rule for interleaved thinking. Whether direct Anthropic accepts an adaptive request with tool
   use and no interleaved beta is an open item for the home-lab window, a live check before the
   claim is relied on. For OpenRouter models no beta is pinned: the live check of
   2026-10-29 passed through OpenRouter's Messages route with none. If a later check shows OpenRouter
   needs one, it is a field in the row.
4. **The agent's setting.** `agents.thinking` (migration 00039) is `NULL`, `on` or `off`, a mutable
   setting of the agent like `agents.model` (ADR 0072): not part of a version, applied to the next
   run, audited as `agent.thinking.set` with the old and new value in the same transaction, a repeat
   writes nothing. Precedence: the agent's setting, then the version's value (a template's
   `spec.model.thinking`, so a template sets the starting value), then the default, **on**
   (`thinking.Choose`). It is on `AgentService.SetAgentThinking` and `PUT /admin/v1/agents/{id}/thinking`;
   `GetAgentModel` returns the thinking state (setting, version value, what it resolves to, whether the
   model supports it, the mode a run uses). The agent page shows a Thinking select under the model; a model whose capability is
   `none` shows "Not supported by this model" and no choice, and keeps the setting for when the model
   changes. The setting is not validated against the model on save, because the model can change
   later.
5. **The flavor renders Koinon's choice.** agentd computes `thinking.Of(provider type, model,
   format).Effective(setting)` for the run's effective model and passes it as `--thinking none|adaptive`
   (`flavor.Descriptor.ThinkingArg`). The OpenCode entry script writes the model's `compatibility`
   into `OPENCODE_CONFIG_CONTENT`: `adaptive` is `supportsThinkingBlockBinding: true`, which makes
   OpenCode send adaptive thinking with the binding the gateway's beta covers; `none`, or no switch,
   is `supportsThinkingBlockBinding: false`, which sends no `thinking` field. `supportsEffortUpdates`
   is `false` in both, since per-turn effort adds `output_config` and a beta of its own. OpenCode's ID
   regex never decides. `flavors/opencode/README.md` is corrected. **Amended 2026-10-31: on and off
   are both sent explicitly, per row; see below.**
6. **Cost.** Thinking tokens are billed as output: agentgateway and Anthropic report them inside
   `output_tokens`, and `pricing.Split` already bills the reasoning part at the output rate unless the
   price names a reasoning rate (ADR 0056). Nothing about the ledger changes except visibility: the spend
   report line gains `reasoningTokens` (the part providers report separately, inside `outputTokens`).
   Anthropic Messages reports no such split, so for direct and OpenRouter Claude models the line is
   `null` ("not reported"), never 0: a 0 would read as "no thinking happened" (old issue 240 comment 2), and the
   thinking is in `outputTokens`. **Corrected 2026-10-31: Anthropic does report the split; agentgateway
   v1.7.0 does not read it. See below.** It is a number once any request of the model reported a split. The
   price list marks every thinking-capable model `hidden_thinking` (Haiku 5.5 lacked it; list version
   `2026-10-29.1`), so a cut stream's output is flagged `thinking_unmeasured`, and a test keeps the
   price list and the catalog in step. The budget reserve of a cut stream is the request's `max_tokens`
   capped at the entry's `max_output`; Anthropic counts thinking inside `max_tokens`, so the reserve
   already covers it. 2026-11-07: since #302 (ADR 0097 decision 3) the driver splits reasoning out of
   the output when it maps its log; the ledger prices the disjoint buckets and pricing.Split is gone.
7. **old issue 242: the OpenRouter pin rows are deleted** after the live check (decision 7) passed on 2026-10-29,
   as decision 4 said. `anthropic/*` models on OpenRouter default to `anthropic` through the family
   preference; the startup re-derivation moves every model whose format no admin set and writes
   `model.format_rederived`. The per-model overrides the home lab applied stay (they are overrides,
   not re-derived). They are no longer needed and an admin may reset them. The migration `00031` comment
   about "stay on openai for now" is history.

### Tests

- `internal/thinking`: the table over real IDs on two providers, the precedence, and the price-list
  consistency check.
- The fake model server's **strict Messages mode** (`SetStrictMessages`, `koinon-fakemodel
  --strict-messages`) refuses unknown top-level and `thinking` fields, beta-only fields without their
  beta and a missing `anthropic-version`, with Anthropic's 400 shape.
- The flavor spec (`test/flavors/thinking_test.go`) runs `claude-haiku-5-5`,
  `anthropic/claude-haiku-5.5`, `claude-opus-5-5`, `claude-sonnet-4-5` and the contract ID with
  thinking on and off, against the strict fake.
- The gateway spec (`gatewaytest`, both drivers, real agentgateway v1.7.0) sends adaptive thinking
  with `block_binding` and no client beta through a Claude-shaped model ID to the strict fake: it
  arrives with the pinned beta and the client's own beta replaced; the same body to a model the catalog
  says cannot think is refused with Anthropic's message, reproducing the rc.7 failure. A second case
  covers the OpenRouter route: an adaptive model with no pinned beta (`Supported()` with empty
  `Betas`) gets the client's `anthropic-beta` removed, so OpenCode's own betas never reach the
  upstream and cannot hide a request that works only because of them.
- The kind e2e runs the fake model server with `--strict-messages`, so every e2e Messages request is
  checked the way Anthropic checks it. Step 8 (after the quickstart's time check) sets the agent to
  `claude-haiku-5-5`, a Claude 5.x ID on the anthropic provider (script `claude-haiku-5-5.yaml`),
  turns thinking on and then off, and asserts on `GET /_fake/requests` (which now reports each
  request's thinking type and `anthropic-beta` values): OpenCode's real body crosses the real
  agentgateway to the strict fake, with adaptive thinking and only the pinned beta when on, and no
  thinking block when off. Step 6d keeps `claude-e2e`, a model that does not think.
- The ledger prices reasoning tokens as output; the agent API audits and resolves the setting; agentd
  passes the mode.

### Consequences

- Every Claude 5.x model now thinks by default, on the direct and the OpenRouter route. That raises
  output tokens and cost per run; it is the point of the decision, and the follow-up in the next
  home-lab window compares FinOps and research with thinking on and off and records it in the release
  notes. An org that wants the old behaviour sets `thinking: off` in a template or on the agent.
- Open questions are listed in the pull request: the OpenAI-style effort mode, no CLI in this
  repository to carry the setting, and the beta pin for OpenRouter.

## Amendment (2026-10-31): one row per model and route, on and off sent explicitly (old issue 316)

### Context

The rc.9 home-lab window (finding F-3) showed two faults in the 2026-10-29 table. (a) Direct-Anthropic
`claude-haiku-5-5` with thinking on streamed no thinking text in any run: Claude 5.x on the Anthropic
API defaults `display` to `omitted`, so the thinking was billed in full and never shown or traced,
while OpenRouter's Messages route summarizes by default. (b) GPT-6 Luna and DeepSeek v4.1 flash were
catalogued "cannot think" (decision 2 put every chat-completions model at `none`), yet reported 5k to
49k reasoning tokens per run: on OpenRouter they think by default. Deep dive G4 (research store,
`g4-thinking-controls`) found that every catalog model thinks, and that thinking can really be turned
off for only three model and route pairs. Relying on "send nothing" leaves the decision to provider
defaults that differ per route.

### Decision (procoduck, 2026-10-31)

1. **The table has one row per catalog model and route** (`internal/thinking`), with: whether the model
   thinks (`never`, `optional`, `always`), the on form, the off form or none ("cannot"), how the
   thinking comes back (display), the betas, and where the provider reports the thinking split. A test
   fails when an entry of `pricing/default.yaml` has no row. Ollama stays "not supported" (`never`).
   This supersedes the "not built" note of decision 2 above.

   | Model and route | Thinks | On form | Off form | Display | Split reported in |
   | --- | --- | --- | --- | --- | --- |
   | Claude Haiku 5.x, Anthropic and OpenRouter Messages | optional | `adaptive-summarized` | `disabled` | summarized | `thinking_tokens` |
   | Claude Sonnet 5.x, both routes | always (`between_tools` is not off) | `adaptive-summarized` | cannot | summarized | `thinking_tokens` |
   | Claude Opus 5.x and Fable 5.x, both routes | always | `adaptive-summarized` | cannot | summarized | `thinking_tokens` |
   | GPT-6 Luna, DeepSeek v4.1 flash, OpenRouter chat completions | optional | `default` | `effort-none` | visible | `reasoning_tokens` |
   | GPT-6 Astra, OpenRouter chat completions | always | `default` | cannot | visible | `reasoning_tokens` |
   | GPT-6 Astra and GPT-6 Luna, OpenAI chat completions | always | `default` | cannot | hidden | `reasoning_tokens` |
   | Ollama, any model | never | `none` | none | | |

   GPT-6 Luna on OpenAI's own API documents `reasoning_effort: "none"`, but no live check has covered
   it and no flavor sends that field, so its row offers no off until one does.
2. **Decision 5 amended: on and off are both sent explicitly, per row.** The forms are
   `adaptive-summarized` (`thinking: {"type": "adaptive", "display": "summarized"}`), `disabled`
   (`thinking: {"type": "disabled"}`), `effort-none` (`reasoning: {"effort": "none"}`), `default` (no
   field: a chat-completions model thinks by default) and `none` (no field: the model does not think).
   agentd passes the form as `--thinking`; the OpenCode entry script keeps OpenCode's own block off
   (`supportsThinkingBlockBinding: false`, `supportsEffortUpdates: false`) and puts the field in the
   provider-level `body` overlay, the mechanism that carries OpenRouter's `session_id`. No form has a
   beta-only field (`block_binding` was OpenCode's choice, not Koinon's), so no row pins a beta: the
   gateway drops the client's `anthropic-beta`. The pin of decision 3 stays for a row that needs one.
3. **Off exists only where the model can stop.** The API refuses `off` for an agent whose next-run
   model always thinks, with "this model always thinks", and the agent page shows that text instead of
   an off choice. A stored `off` (the agent's or its version's) on a model that later turns out to
   always think runs with thinking on: the run never fails for it, a model change is never refused for
   it, and the stored value is kept, so a later model that can stop gets it back. The thinking state
   reports it (`alwaysThinks`, `offOverridden`, and the notice "this model always thinks; your off
   setting doesn't apply").
4. **Decision 6 corrected.** Anthropic does report the thinking split, as
   `usage.output_tokens_details.thinking_tokens` (on the last `message_delta` when streaming).
   agentgateway v1.7.0's Messages parser does not read it, so the ledger still has no split for Claude
   models; reading it is #78. Until then the trace exporter leaves `gen_ai.usage.reasoning.output_tokens`
   out of a model-call span and a turn root when no call reported a split, instead of writing 0. The
   ledger column stays `NOT NULL DEFAULT 0`, so 0 and "not reported" are one value there; the spend
   report already reads a split as reported only when it is above 0. **Amended 2026-11-06: the ledger
   now tells them apart with `reasoning_known`, see below.**

### Tests

- `internal/thinking`: the truth table above over real IDs on every route, the explicit forms, the
  stored-off override, and that every price-list entry has a row.
- The strict fake model knows the per-model rules: of the Claude 5.x families only Haiku takes
  `disabled`, Opus and Fable take only `adaptive`; `display` is `summarized` or `omitted`; on chat
  completions it checks the `reasoning` object and refuses `effort: none` for a model whose reasoning
  is mandatory (GPT-6 Astra, Claude Sonnet, Opus and Fable on OpenRouter). It lists each request's
  thinking display and reasoning effort.
- The flavor spec asserts the exact on and off body for Haiku, Sonnet, Opus and Fable on both Claude
  routes and Luna and DeepSeek on chat completions, driven by `thinking.Of`.
- The admin API refuses `off` for a model that always thinks and reports a stored `off` overridden
  after a model change; agentd launches with the explicit forms; the trace exporter omits the
  reasoning attribute when unreported; the agent page shows the always-thinks text and the notice.
- The kind e2e (step 8) asserts `adaptive` with display `summarized` and no beta when on, and
  `disabled` when off, through the real gateway to the strict fake.

### Consequences

- Direct-Anthropic thinking is now summarized, so it reaches the run trace at the redacted+ level
  (ADR 0074); summaries are not billed.
- Turning thinking off now costs less where it can be turned off (Haiku, Luna, DeepSeek), and is
  refused where it never could: the setting no longer promises what the model does not do.
- An agent whose template says `thinking: off` on a model that always thinks keeps running with
  thinking on; the agent page says so.
- A live check in the next home-lab window confirms the forms: OpenRouter Messages Haiku with
  `disabled`, direct Haiku with `adaptive` and `display: summarized`, Luna and DeepSeek with
  `effort: none` reporting no reasoning tokens.

## Amendment (2026-11-06): the ledger records whether the split is known (#47, #78)

### Context

Decision 4 of 2026-10-31 left 0 and "not reported" as one value in `spend_entries.reasoning_tokens`.
A chat-completions call that reported a split of 0 and a Claude call through agentgateway v1.7.0's
Messages parser, which logs no split, were indistinguishable. Koinon cannot read the split itself
today: the gateway's access log carries no response body, and gateway log and tracer fields never
read `response.body` (AGENTS.md, working rules).

### Decision (procoduck, 2026-11-06)

1. **`spend_entries.reasoning_known boolean`** (migration 00062, nullable, expand only per ADR 0085)
   says whether `reasoning_tokens` is the provider's split. The OTLP decoder sets it when the gateway
   logged `koinon.reasoning`, a 0 included; an absent field writes `false` and 0 tokens.
2. **Legacy rows** (NULL) read as `coalesce(reasoning_known, reasoning_tokens > 0)`: a stored split
   above 0 was reported, a stored 0 is taken as not reported, as the spend report read it before.
3. **OpenRouter settlement** makes the split known only from `native_tokens_reasoning` above 0. A 0 or
   a missing field has not been shown to mean "no thinking" on every route (the false "no thinking"
   that old issue 316 removed), so it keeps the gateway's split and flag. A positive count
   replaces them.
4. The spend report's `reasoning_reported` and the ledger's trace spans use the flag, so a reported 0
   is written as 0 and an unknown split is left out.

### Consequences

- Direct Claude calls still record the split as unknown until agentgateway reads
  `usage.output_tokens_details.thinking_tokens`, or Koinon's own gateway (ADR 0097) does; #78 stays
  open for that. Thinking is billed either way: it is inside the output count.
- If a live check shows OpenRouter's 0 is reliable on a route, decision 3 can accept it there.
