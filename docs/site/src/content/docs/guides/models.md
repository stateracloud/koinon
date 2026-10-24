---
title: Models
description: Add model providers and models per org, with Kubernetes Secrets or the admin API.
---

Agents never hold a model key. A sandbox gets a placeholder key and the address of agentgateway.
agentgateway holds the real key, forwards the request to your provider and counts the cost. Koinon
reads the cost from agentgateway's access log and keeps a ledger.

Each org has its own **providers** (an OpenAI-compatible endpoint and, usually, a key) and **models**
(a name, an optional routing guard and a price). Provider keys are encrypted in Postgres
([ADR 0056](https://github.com/stateracloud/koinon/blob/main/docs/adr/0056-model-providers-pricing-and-budgets-per-org.md));
the encryption keys live in a Secret that only `koinon-api` and `koinon-reconciler` mount
([Secret keys](https://github.com/stateracloud/koinon/blob/main/docs/ops/secret-keys.md)).
agentgateway serves each model as `<org>/<model>` to that org's agents only. An agent still asks for
the plain name.

There are two ways to add them. A provider or model has one source:

| Source | How | Changeable in the UI and admin API |
| --- | --- | --- |
| `ui` | The admin API now; the UI pages come in the next pull request. | Yes. |
| `secret` | A labelled Kubernetes Secret, below. | No. They are read-only and say which Secret manages them. |

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
      rates: {input: "0.30", output: "1.20"}
```

- `koinon.dev/org` is the org's slug. The bootstrap install has the org `default`.
- `name` is the provider's name, unique in the org. `provider` is agentgateway's `providerOverride`
  (`openai`, `openrouter`, `ollama` and so on). `url` is the OpenAI-compatible base URL. `apiKey` is
  optional; leave it out for Ollama.
- `models` is a YAML list. Each model has a `name`, an optional `guard` and `rates`: US dollars per
  million tokens, `input` and `output`, and optionally `cacheRead`. Koinon does not serve a model
  without rates; give a free model the rate `"0"`.

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
- Every apply and removal is written to the audit log with the actor `secret <name>`.
- The same labels and mechanism declare a GitHub connection: `koinon.dev/secret-type: connection`
  with `name`, `connector: github`, `appId`, `installationId` and `privateKey`.

`apiKey` and the other values stay in the Secret's `stringData`, as in Argo CD. Turn on etcd encryption
at rest, and generate the Secret with External Secrets Operator or Sealed Secrets instead of keeping it
in git in clear text.

**Upstream hosts.** A provider URL may not point at loopback, link-local, private (including Kubernetes
Services' ClusterIPs), shared or cloud metadata addresses, at a name without a dot, or at a Service in
Koinon's own namespace; odd spellings such as `127.1` or `0x7f.0.0.1` are read as the addresses they are.
A platform admin allows an internal upstream in the chart value `providers.allowedInternalHosts` (names,
IPs or CIDRs, for example `ollama.lab.example.com:11434`) and, when the chart's gateway egress policy is
on, in `networkPolicy.gatewayEgressTo`. The runbook has the details and limits. Any other target the gateway must reach on a private address,
such as a remote MCP server, needs a `networkPolicy.gatewayEgressTo` entry as well.

**Trust boundary.** Write access to Koinon's namespace is platform-admin level. Anyone who can create
a labelled Secret there can define providers and credentials for any org. Grant it as you would
grant cluster administration.

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
type. Set `rates` to the highest price among your allowed hosts, so the ledger never counts too
little.

## Ollama

Ollama needs no key and has no cost per token. Koinon still needs a rate, so set it to zero:

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
      rates: {input: "0", output: "0"}
```

Sandboxes cannot reach the Ollama server directly. Only agentgateway can. Small models struggle
with the repo-helper template. In the spike, a 4B model finished every task but needed explicit
instructions, and sometimes returned empty or cut-off replies. Use a larger model for real work.

## The default model

An agent whose template names no model (or `default`) uses the first model its org serves: providers
by name, then models by name, skipping models without rates.

## The admin API

REST under `/admin/v1`, described by an
[OpenAPI document](https://github.com/stateracloud/koinon/blob/main/api/admin/v1/openapi.yaml) that
the server also serves at `/admin/v1/openapi.yaml`. It manages providers, models and API tokens.

It takes bearer tokens only. A token belongs to one org and has the role `admin` or `owner`, never
more than the role of whoever created it. It expires: after 30 days by default, after at most 90.
The token is shown once, when you create it, and Koinon keeps only a hash. You can revoke it, and
the list shows when it was last used. Every call is written to the audit log with the token's ID.

A token can create tokens, but only with the same or a lower role and an expiry no later than its own.
Revoking a token revokes every token it created. A token works only while the user whose session began
its chain still holds at least its role in the org. Bad tokens slow a client down (HTTP 429).

Until the UI page exists, an org admin creates the first token through the UI's connect-rpc API
(`koinon.token.v1alpha1.TokenService/CreateToken`, signed in as an admin or owner). A token can then
create further tokens with `POST /admin/v1/tokens`.

```sh
curl -sS -H "Authorization: Bearer $KOINON_TOKEN" -H 'Content-Type: application/json' \
  https://koinon.example.com/admin/v1/providers \
  -d '{"name":"ollama","type":"ollama","url":"http://ollama.example.internal:11434/v1"}'
```

## Budgets

The agent template sets `spendUSD`, a limit per agent per UTC calendar day. Koinon pushes it to
agentgateway as a key limit. When the agent reaches it, Koinon cancels the run with the reason
`spend_limit` and says so on the ticket. A response in flight can overshoot the limit by a little.

## Changing in v0.1

These are decided and **not built yet**:

- The providers and models pages in the UI (the next pull request, plan item W4), and a page for
  API tokens.
- A price schema with a default price list, and raw usage kept beside the cost (plan item PV2). Until
  then a model carries its own `rates`.
- An org monthly cap and per-agent daily limits that admins set. At 80% Koinon warns. At 100% it blocks
  the run (plan item BG1).

See [ADR 0056](https://github.com/stateracloud/koinon/blob/main/docs/adr/0056-model-providers-pricing-and-budgets-per-org.md).
