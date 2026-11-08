---
title: Models
description: Add model providers and models per org, in the UI, with Kubernetes Secrets or the admin API.
---

Agents never hold a model key. A sandbox gets a placeholder key and the address of agentgateway.
agentgateway holds the real key, forwards the request to your provider and counts the cost. Koinon
reads the cost from agentgateway's access log and keeps a ledger.

Each org has its own **providers** (an OpenAI-compatible endpoint and, usually, a key) and **models**
(a name, an API format and an optional routing guard). Provider keys are encrypted in Postgres
([ADR 0056](https://github.com/stateracloud/koinon/blob/main/docs/adr/0056-model-providers-pricing-and-budgets-per-org.md));
the encryption keys live in a Secret that only `koinon-api` and `koinon-reconciler` mount
([Secret keys](https://github.com/stateracloud/koinon/blob/main/docs/ops/secret-keys.md)).
agentgateway serves each model as `<org>/<model>` to that org's agents only. An agent still asks for
the plain name.

There are two ways to add them, and the admin API is a third door to the UI's source. A provider or model has one source:

| Source | How | Changeable in the UI and admin API |
| --- | --- | --- |
| `ui` | The **Providers** page (Settings in the sidebar, for org admins and owners), or the admin API. | Yes. |
| `secret` | A labelled Kubernetes Secret, below. | No. They are read-only and say which Secret manages them. |

## Add a provider in the UI

Org admins and owners open **Providers** under Settings in the sidebar. Members do not see it.

- **Add provider** takes a name, a provider type (OpenRouter, Anthropic, Ollama, an OpenAI-compatible API, or
  another agentgateway provider name), the URL and the API key. The key is write-only: Koinon stores
  it encrypted and never shows it again, not even its last characters. To change it, type a new one.
  To remove it, tick the box on the edit form.
- If the URL is refused (a private, loopback or cluster-internal address), the form shows the reason.
  Ask a platform admin to allow the host ([Upstream hosts](#upstream-hosts)).
- For an Anthropic provider the form also takes an optional **workspace ID**
  ([below](#anthropic)). Anthropic and OpenAI providers take an optional **cost API key** and **cost
  scope** for the daily comparison with the provider's bill ([Budgets](/guides/budgets/#comparing-a-direct-provider-with-its-bill)).
- **Add model** on a provider takes the model name, its [API format](#api-formats) and, for OpenRouter
  providers only, the routing guard. The page shows the name the gateway serves, `<org>/<model>`. A model has no price of its own:
  see [Prices](/guides/prices/).
- A provider declared by a Secret shows "Managed by Secret <name>" and has no edit buttons. If its
  Secret was deleted it shows as removed, with the reason, and you may delete it.

## Declare a provider with a Secret

This works like Argo CD's
[declarative setup](https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/).
Create a Secret in Koinon's namespace with two labels and the provider in `stringData`:

```yaml model-provider
apiVersion: v1
kind: Secret
metadata:
  name: openrouter
  namespace: koinon
  labels:
    koinon.dev/secret-type: model-provider
    koinon.dev/org: default
stringData:
  name: openrouter
  provider: openrouter
  url: https://openrouter.ai/api/v1
  apiKey: sk-or-replace-me
  models: |
    - name: deepseek/deepseek-v4.1-flash
      guard: true
```

- `koinon.dev/org` is the org's slug. The bootstrap install has the org `default`.
- `name` is the provider's name, unique in the org. `provider` is agentgateway's `providerOverride`
  (`openai`, `openrouter`, `ollama` and so on). `url` is the OpenAI-compatible base URL. `apiKey` is
  optional; leave it out for Ollama. A provider of type `anthropic` also takes an optional
  `anthropicWorkspaceId` ([Anthropic](#anthropic)). A provider of type `anthropic` or `openai` at its
  own API URL also takes `costApiKey` and `costScope`, together
  ([Budgets](/guides/budgets/#comparing-a-direct-provider-with-its-bill)).
- `models` is a YAML list. Each model has a `name`, an optional `guard` and an optional `format`
  ([API formats](#api-formats)). Prices are not declared
  here: Koinon prices a model from [its default price list or a price you add](/guides/prices/). **Upgrading:** an
  older Secret that still has `rates` stays valid, but Koinon ignores the rates and says so in a Warning Event
  on every sync. A model that only had `rates` is not served unless the default list prices it; add a price
  for it on the **Prices** page.

`koinon-api` imports the Secret within seconds. What you need to know:

- A declared provider or model is read-only in the UI and admin API. Both name the Secret that
  manages it. Change the Secret instead.
- A declared entry wins a name clash. If someone already added a provider or model of the same name
  in the UI, that entry is **disabled** and shows why, until the Secret is deleted.
- Deleting the Secret does not delete what it declared: the provider, its models or the connection are
  marked **removed** and kept. They are not served or usable, agents bound to a removed connection
  report why, and runs that need it fail with that reason. Creating the Secret again restores them. An
  org admin can delete a removed entry for good.
- A Secret that does not parse is ignored. The previous state stays, and a Warning Event on the
  Secret and a line in the `koinon-api` log say why. Run `kubectl -n koinon describe secret <name>`.
- A Secret applied before its org exists is retried on a backoff, from 5 seconds and doubling up to the
  resync interval (5 minutes), and imported once the org exists. A `koinon.dev/org` slug that never
  resolves gets one `names no org` Warning Event per Secret version and is retried every resync interval.
- Every apply and removal is written to the audit log with the actor `secret <name>`.
- The same labels and mechanism declare a GitHub connection: `koinon.dev/secret-type: connection`
  with `name`, `connector: github`, `appId`, `installationId` and `privateKey`, plus optional `baseUrl` and `caBundle` for a GitHub Enterprise Server, and optional `ciDirs`
  (extra CI directories Koinon refuses changes under: one per line or comma-separated, at most 20, relative, no `..`).

`apiKey` and the other values stay in the Secret's `stringData`, as in Argo CD. Turn on etcd encryption
at rest, and generate the Secret with External Secrets Operator or Sealed Secrets instead of keeping it
in git in clear text.

**Upstream hosts.** A provider URL may not point at loopback, link-local, private (including Kubernetes
Services' ClusterIPs), shared or cloud metadata addresses, at a name without a dot, or at a Service in
Koinon's own namespace; odd spellings such as `127.1` or `0x7f.0.0.1` are read as the addresses they are.
A platform admin allows an internal upstream in the chart value `egress.allowedInternalHosts` (`providers.allowedInternalHosts` is its deprecated old name; both are read for one more release; names,
IPs or CIDRs, for example `ollama.lab.example.com:11434`) and, when the chart's gateway egress policy is
on, in `networkPolicy.gatewayEgressTo`. The runbook has the details and limits. The same list and the same egress policy cover the
[remote MCP servers](/guides/mcp-servers/) an org adds.

**Trust boundary.** Write access to Koinon's namespace is platform-admin level. Anyone who can create
a labelled Secret there can define providers and credentials for any org. Grant it as you would
grant cluster administration.

## API formats

A model speaks one wire format, whichever provider serves it
([ADR 0069](https://github.com/stateracloud/koinon/blob/main/docs/adr/0069-native-model-formats.md)):

| Format | API | Used for |
| --- | --- | --- |
| `openai` | OpenAI chat completions | Most models and most providers. |
| `anthropic` | Anthropic Messages | Claude, which caches prompts through this API. |

The format belongs to the model because one provider can serve several families. OpenRouter serves
Claude and other models, and each family wants its own API. What a provider offers:

| Provider type | Formats it offers |
| --- | --- |
| `anthropic` | `anthropic` |
| `openrouter` | `openai`, `anthropic` |
| Every other type | `openai` |

**The default.** Koinon prefers `anthropic` for `claude-*` and `anthropic/*` model names and `openai`
for everything else. If the provider does not offer the preferred format, the model gets the first
format the provider offers. That includes OpenRouter's `anthropic/*` models: Koinon checked the Messages
path against OpenRouter live on 2026-10-29 and moved them to `anthropic`. The model form marks the default.

**Overriding it.** Choose another format on the model form, in the admin API (`format` on `POST
/admin/v1/models` and `PATCH /admin/v1/models/{id}`) or per model in a declared Secret. The format
must be one the provider offers, or the save is refused. A model you set keeps its format when Koinon
changes its defaults. Choosing the default again, or an empty `format` in the API, puts the model
back on the default. The model list shows a model's format, and "(set)" when someone chose it.

**Defaults follow the table.** At every start `koinon-api` applies the table to each model whose
format nobody set, and records each change in the audit log with the actor `system koinon-api:model-formats`. Existing
models of an Anthropic provider moved to `anthropic` when Koinon was upgraded.

**What a run does with it.** A run uses the harness entry for its effective model's format (the
agent's model, else its version's, else the organisation default). An `anthropic` model is sent
to `/v1/messages`, and OpenCode places the prompt-cache breakpoints itself, so direct Anthropic
models cache. An `openai` model stays on chat completions with the ADR 0068 cache markers.

## Thinking

Claude models think before they answer, and Koinon decides whether they do, not the harness
([ADR 0069](https://github.com/stateracloud/koinon/blob/main/docs/adr/0069-native-model-formats.md),
amendment of 2026-10-29). **Thinking is on unless you turn it off.**

- **Which models can.** Koinon keeps a table of the models that can think (today the Claude 5.x
  families, on the `anthropic` format, direct or through OpenRouter). A model that is not in it runs
  without thinking, and the agent page says "Not supported by this model".
- **Turning it off for an agent.** The agent page has a **Thinking** choice under the model. It is a
  setting of the agent, not a new version: it applies to the next run and is recorded in the audit
  log. In the admin API it is `PUT /admin/v1/agents/{id}/thinking` with `{"thinking": "off"}` (`on`, or
  `default` to clear it), and `GET /admin/v1/agents/{id}/model` returns its state.
- **Templates.** A template can start its agents with thinking off: `spec.model.thinking: off`. The
  agent's own setting wins over the template's.
- **Cost.** Thinking tokens are billed as output tokens at the model's output price. The spend report
  shows the part providers report separately as `reasoningTokens`, inside `outputTokens`; it is `null`
  for Claude models, because Anthropic counts thinking inside the output tokens and reports no split.
  Expect a
  thinking run to cost more than one without; turn thinking off for agents where it does not help.
- **The gateway.** It sets the `anthropic-beta` value the model needs itself and never passes an
  agent's header through, so an agent cannot enable provider features.

## Anthropic

Choose the **Anthropic** provider type for Claude through Anthropic's own API, with the URL
`https://api.anthropic.com/v1` and an Anthropic API key. Its models use the `anthropic` format.

Anthropic needs a **workspace ID** for an API key that is not scoped to one workspace. Set it on the
provider (`anthropicWorkspaceId` in the admin API and in a declared Secret): `wrkspc_` followed by
letters and digits, for Anthropic providers only. It identifies the workspace and is not a secret, so
Koinon stores it in plain text and shows it.

```yaml model-provider
apiVersion: v1
kind: Secret
metadata:
  name: anthropic
  namespace: koinon
  labels:
    koinon.dev/secret-type: model-provider
    koinon.dev/org: default
stringData:
  name: anthropic
  provider: anthropic
  url: https://api.anthropic.com/v1
  apiKey: sk-ant-replace-me
  anthropicWorkspaceId: wrkspc_replaceme
  models: |
    - name: claude-haiku-4-5
```

## When a new provider is served

A new provider is not served the moment you save it. `koinon-reconciler` writes its key into the
gateway's provider key Secret and waits for kubelet to update the gateway pod's copy (about 90 seconds in
standalone mode, `--provider-key-settle`) before it applies a gateway config that names the key; a new
model of a provider already served needs only that apply. Once agentgateway serves the model, the
reconciler records it.

A run whose model is not served yet waits before it starts the harness. The task's timeline shows
"Run … is waiting for the model provider *name* to reach the gateway", and the run stays *starting*
meanwhile; its sandbox starts in parallel. When the model is served, the run goes on by itself.

A model without a usable price is never served, so its run fails at once and says so: add a price on
the **Prices** page.

If the model is still not served after five minutes (`koinon-agentd --model-serve-timeout`), the run
fails with "the gateway did not serve the model … its provider … has not reached the gateway". That
usually means the provider is not served at all: its key cannot be opened, its URL is refused, or
the gateway rejected the config. The `koinon-reconciler` log says which (a
rejected config also shows on the agent's status). Fix it and run the task again.

Changing a served provider's key does not make runs wait: the gateway keeps using the previous key
until the new one has arrived, also when the key changed while `koinon-reconciler` was down.

Keep `--model-serve-timeout` well above the reconciler's `--provider-key-settle` plus one gateway
apply (agentgateway's reload takes up to a minute). The two are flags of different components, so
Koinon cannot check this for you; if you raise the settle time, raise the timeout too.

## OpenRouter, with the routing guard

OpenRouter forwards a request to one of many hosts. Some keep prompts. The routing guard makes sure
a request reaches only hosts that do not. It works in two places, and you should use both.

**In your OpenRouter account** (settings, Privacy): turn on zero data retention for every model
group, turn training off, turn prompt logging off, and allow only hosts you have audited. Put a hard
credit limit on the key.

**In Koinon**, `guard: true` on a model. agentgateway then adds this `provider` object to every
request for the model, and it wins over anything the client sends:

```json
{"provider": {"zdr": true, "data_collection": "deny"}}
```

- `zdr: true` allows only endpoints with zero data retention.
- `data_collection: deny` allows only endpoints that do not store or train on data.

The guard exists for OpenRouter providers only. Koinon refuses `guard: true` on any other provider
type. Add a price for the model that is the highest among your allowed hosts, so the ledger never
counts too little.

## Ollama

Ollama needs no key and has no cost per token. Koinon's default price list prices every Ollama model at
zero, so there is nothing to add:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: ollama
  namespace: koinon
  labels:
    koinon.dev/secret-type: model-provider
    koinon.dev/org: default
stringData:
  name: ollama
  provider: ollama
  url: http://ollama.example.internal:11434/v1
  models: |
    - name: "qwen3.5:4b"
```

Sandboxes cannot reach the Ollama server directly. Only agentgateway can. Small models struggle
with the repo-helper template. In the spike, a 4B model finished every task but needed explicit
instructions, and sometimes returned empty or cut-off replies. Use a larger model for real work.

## The default model

An agent whose template names no model (or `default`) uses the org's default model. It is stored:
the first model an org gets becomes its default, and an admin changes it with `PUT /admin/v1/default-model`
(a model the org serves and has a price for; leaving `model` out clears it). Adding a provider, a model or a
price never moves it, and each change is audited as `org.default_model.set`. An org with no default model
fails runs of agents that name no model, with a message that says so.

## The admin API

REST under `/admin/v1`, described by an
[OpenAPI document](https://github.com/stateracloud/koinon/blob/main/api/admin/v1/openapi.yaml) that
the server also serves at `/admin/v1/openapi.yaml`. It manages providers, models, prices, budgets and API tokens.

It takes bearer tokens only. A token belongs to one org and has the role `admin` or `owner`, never
more than the role of whoever created it. It expires: after 30 days by default, after at most 90.
The token is shown once, when you create it, and Koinon keeps only a hash. You can revoke it, and
the list shows when it was last used. Every call is written to the audit log with the token's ID.

A token can create tokens, but only with the same or a lower role and an expiry no later than its own.
Revoking a token revokes every token it created. A token works only while the user whose session began
its chain still holds at least its role in the org. Bad tokens slow a client down (HTTP 429).

An org admin or owner creates the first token on the **API tokens** page (Settings in the sidebar):
name, role (never above your own) and expiry (30 days by default, 90 at most). The page shows the
token once, with a copy button, and lists each token's prefix, role, dates, last use and who created
it. Revoking says that it revokes the tokens it created. A token can create further tokens with
`POST /admin/v1/tokens`.

```sh
curl -sS -H "Authorization: Bearer $KOINON_TOKEN" -H 'Content-Type: application/json' \
  https://koinon.example.com/admin/v1/providers \
  -d '{"name":"ollama","type":"ollama","url":"http://ollama.example.internal:11434/v1"}'
```

## Prices and budgets

Koinon prices every request from the tokens the provider reports. See [Prices](/guides/prices/) for
the default list and your own prices, and [Budgets](/guides/budgets/) for the org's monthly cap and the
agents' daily limits.

See [ADR 0056](https://github.com/stateracloud/koinon/blob/main/docs/adr/0056-model-providers-pricing-and-budgets-per-org.md).
