# Step 5: inference through agentgateway

Run 2026-10-21. agentgateway 1.6.0 in standalone mode (`hack/spike/agentgateway.sh`, config
`hack/spike/agentgateway/config.yaml`): one replica in `agentgateway-system`, SQLite on a PVC, a
custom OpenAI-compatible provider (`providerOverride: ollama`) pointing at
`http://ollama.lab.example.invalid:11434/v1`, model `qwen3.5:4b` priced nominally at $1 / $2 per 1M
input / output tokens in an inline model catalog. Two per-agent API keys (`agent-a`: $10/24h,
`agent-b`: $0.0005/24h, both `Block`) appear in the config only as `keyHash: sha256:…`; the values
are in OpenBao (`secret/koinon-spike/agentgateway`) and reach sandbox `p2` through the OpenShell
provider `agentgateway-key` (profile from step 4.3, `stable_placeholder`), swapped between keys
with `provider update` without restarting `p2`.

| # | Result | Evidence |
| --- | --- | --- |
| 5.1 | pass | from `p2`: `POST /v1/chat/completions` with `Authorization: Bearer $OPENAI_API_KEY` (placeholder) → `200` in 2.2 s, usage 18 / 40 tokens; a wrong key → `401` |
| 5.2 | pass | `OPENAI_API_KEY=openshell:resolve:env:s4534…`; Ollama direct from `p2` → `000` (no policy entry), as in 3.3 |
| 5.3 | pass | JSON access log per request: `gen_ai.usage.input_tokens`, `…output_tokens`, `agw.ai.usage.cost.total` (`0.000098` = 18×$1/1M + 40×$2/1M), plus `koinon.key` / `koinon.agent` from CEL `apiKey.name` / `apiKey.agent`. A streaming request with `stream_options.include_usage` is charged like a non-streaming one (`0.000139`) |
| 5.4 | pass | `agent-b`: stream + 3 requests at $0.000139 each, then `429 {"code":"budget_exceeded"}`. Charged total $0.000556 vs limit $0.0005: overshoot $0.000056, less than one response |
| 5.5 | pass (logs) | the JSON access log carries key, agent, tokens and USD per request, which is enough for the ADR 0041 ledger (ship stdout or use OTLP export). Budget usage itself sits in an internal `budget_usage` table; there is no budget read API in the admin schema |
| 5.6 | standalone | Kubernetes mode (`AgentgatewayPolicy`) has API-key auth from Secrets/ConfigMaps and token-bucket rate limits (local or global) but **no per-key USD budgets**. Standalone mode has per-key `budgets` (USD or tokens, rolling window, `Block`/`Audit`) and hot-reloads its config: raising `agent-b`'s limit through the ConfigMap took effect after 11 s with no restart |

## Findings

1. **Koinon pushes hashes, not keys.** `keyHash: sha256:<hex>` lets Koinon write the gateway config
   without the key values; the value goes only to OpenShell's provider store.
2. **Budget-carrying keys need `metadata.name`** (startup fails otherwise), and budgets need a
   configured database.
3. **Budget state is per database.** SQLite on a PVC limits agentgateway to one replica; HA needs
   Postgres (the upstream `agentgateway-standalone` chart has a `database` mode for this).
4. **Windows are epoch-aligned** (`24h` = UTC midnight); Koinon's ledger windows should match or
   Koinon keeps never-reset totals itself and pushes remaining amounts.
5. `config.logging.fields` is deprecated in 1.6 in favour of `frontendPolicies.accessLog`; the
   product config should use the new form.
6. Reasoning models spend `max_tokens` on thinking; harness defaults need headroom (the first
   reply was empty at 40 tokens).
