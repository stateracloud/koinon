---
title: Models
description: Point Koinon at OpenRouter, with the routing guard, or at Ollama.
---

Agents never hold a model key. A sandbox gets a placeholder key and the address of agentgateway.
agentgateway holds the real key, forwards the request to your upstream and counts the cost. Koinon
reads the cost from agentgateway's access log and keeps a ledger.

Today you configure one OpenAI-compatible upstream in Helm values, under `model`
([ADR 0049](https://github.com/stateracloud/koinon/blob/main/docs/adr/0049-v01-scope-decisions.md)).

| Value | Meaning |
| --- | --- |
| `model.baseUrl` | The upstream's OpenAI-compatible base URL. |
| `model.provider` | agentgateway's `providerOverride`: `openai`, `openrouter`, `ollama` and so on. |
| `model.apiKeySecret.name`, `.key` | A Secret that holds the upstream key. Leave `name` empty for a keyless upstream. Only agentgateway mounts it. |
| `model.models[].name` | The model name the upstream expects. The first entry is the default for all agents. |
| `model.models[].rates` | US dollars per million tokens: `input`, `output`, and optionally `cacheRead`. Koinon does not serve a model without a rate. |
| `model.models[].overrides` | Request fields that agentgateway forces on every request. Every model must have the same overrides. |

## OpenRouter, with the routing guard

OpenRouter forwards a request to one of many hosts. Some keep prompts. The routing guard makes sure
a request reaches only hosts that do not. It works in two places, and you should use both.

**In your OpenRouter account** (settings, Privacy): turn on zero data retention for every model
group, turn training off, turn prompt logging off, and allow only hosts you have audited. Put a hard
credit limit on the key.

**In Koinon**, as `overrides` on each model. agentgateway adds this `provider` object to every
request, and it wins over anything the client sends:

```yaml chart-values
model:
  baseUrl: https://openrouter.ai/api/v1
  provider: openrouter
  apiKeySecret:
    name: koinon-openrouter
    key: api_key
  models:
    - name: deepseek/deepseek-v4.1-flash
      rates: {input: "0.30", output: "1.20"}
      overrides:
        provider:
          zdr: true
          data_collection: deny
          only: [fireworks, together, deepinfra, amazon-bedrock, google-vertex]
          allow_fallbacks: false
```

Each field closes one gap:

- `zdr: true` allows only endpoints with zero data retention.
- `data_collection: deny` allows only endpoints that do not store or train on data.
- `only` names the hosts that may serve the request. This list is the one the spike audited
  ([ADR 0048](https://github.com/stateracloud/koinon/blob/main/docs/adr/0048-openshell-spike-results.md)).
  Replace it with your own audited hosts.
- `allow_fallbacks: false` stops OpenRouter from sending the request elsewhere when those hosts fail.
  The run fails instead.

Create the key Secret before you install. The quickstart's `model-key` step does this:

```sh
kubectl -n koinon create secret generic koinon-openrouter --from-literal=api_key="$OPENROUTER_API_KEY" --dry-run=client -o yaml | kubectl apply -f -
```

Set `rates` to the highest price among your allowed hosts, so the ledger never counts too little.

## Ollama

Ollama needs no key and has no cost per token. Koinon still needs a rate, so set it to zero:

```yaml chart-values
model:
  baseUrl: http://ollama.example.internal:11434/v1
  provider: ollama
  models:
    - name: "qwen3.5:4b"
      rates: {input: "0", output: "0"}
```

Sandboxes cannot reach the Ollama server directly. Only agentgateway can. Small models struggle
with the repo-helper template. In the spike, a 4B model finished every task but needed explicit
instructions, and sometimes returned empty or cut-off replies. Use a larger model for real work.

## Budgets

The agent template sets `spendUSD`, a limit per agent per UTC calendar day. Koinon pushes it to
agentgateway as a key limit. When the agent reaches it, Koinon cancels the run with the reason
`spend_limit` and says so on the ticket. A response in flight can overshoot the limit by a little.

## Changing in v0.1

These are decided and **not built yet**. Today there is one upstream in Helm values and one limit
from the template.

- Providers and models per org, with provider keys encrypted in Postgres, managed in the UI and in an
  admin API under `/admin/v1`. Model names become `<org>/<model>`. The Helm upstream then seeds the
  default org's first provider. The routing guard becomes a per-model setting.
- A price schema with a default price list, and raw usage kept beside the cost.
- An org monthly cap and per-agent daily limits that admins set. At 80% Koinon warns. At 100% it blocks
  the run.

See [ADR 0056](https://github.com/stateracloud/koinon/blob/main/docs/adr/0056-model-providers-pricing-and-budgets-per-org.md).
