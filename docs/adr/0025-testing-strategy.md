# 0025. Deterministic, free CI with a continuous sandbox escape suite

- Status: Accepted
- Date: 2026-10-21
- Decision: [design.md#25](../design.md)
- Verification: escape suite design depends on the OpenShell spike ([#3](https://github.com/procoduck/koinon/issues/3))

## Context

Koinon's core promise is security: agents never hold real credentials and cannot escape their
sandbox. That promise must be tested continuously, not once. CI must also be free (GitHub-hosted
runners, open source) and deterministic, so it can never call paid models. Components span Go
controllers, a Postgres-backed API, a React UI and harnesses driven over ACP.

Model traffic goes through agentgateway (ADR [0040](0040-agentgateway-model-and-mcp-gateway.md)),
so a fake model server can sit behind agentgateway exactly where a real provider would, and the
same test also exercises usage parsing and the spend ledger (ADR
[0041](0041-koinon-spend-ledger-and-budget-stops.md)).

## Decision

| Layer | Tooling |
| --- | --- |
| Go unit and integration | Ginkgo v2 / Gomega |
| Controllers | envtest (etcd + API server) |
| Database | testcontainers-go Postgres module |
| Web unit | Vitest |
| Web e2e | Playwright |
| Models | fake OpenAI- and Anthropic-compatible server with scripted responses, including SSE usage events |
| Flavors | contract tests per flavor: ACP initialize, session/new, session/load, prompt, permission, cancel |
| End to end | kind cluster in CI |
| Security | sandbox escape suite against OpenShell sandboxes on every PR |
| Quality | capped nightly evals against real models, with a hard budget |

- CI never holds paid model keys; only the nightly eval job does, with a capped budget.
- The fake model server is registered as an agentgateway backend in e2e, so the model path in CI
  matches production.
- The escape suite asserts: no direct egress, no cloud metadata, no service account token, no
  secrets on disk, placeholders only in the agent environment.

## Alternatives considered

- Record-and-replay of real model traffic _(reconstructed)_. Rejected as the main approach:
  recordings go stale and still need real keys to refresh; scripted fakes are deterministic.
- Escape suite only before releases _(reconstructed)_. Rejected: the security promise must hold on
  every change.
- Self-hosted runners with Kata for CI _(reconstructed)_. Rejected: public CI must not run on the
  home lab (ADR [0026](0026-home-lab-and-license.md)); kind cannot run Kata, so CI checks policy and
  credential isolation while Kata isolation is tested on the home lab _(inferred)_.

## Consequences

- Positive: deterministic, free CI; the security promise is a test, not a claim.
- Negative: kind on GitHub runners cannot run Kata VMs; CI runs the escape suite with the default
  runtime, and VM-boundary checks only run on the home lab _(inferred)_.
- Negative: harness contract tests must track three upstream harnesses and the community ACP Go SDK
  (pre-1.0) _(inferred)_.
- Negative: envtest has no garbage collection or controller-manager; namespace deletion and owner
  references must be tested in kind e2e.
- Follow-ups: fake model server in `test/`; escape suite skeleton; CI layout (still open).

## Open questions

- Which checks block merges (listed as still open in BOOTSTRAP.md).
- Whether OpenShell's Kubernetes driver runs on kind in GitHub runners within time limits (spike).
- Eval budget and the eval set for nightly runs.

## Upstream verification

Checked 2026-10-21:
- controller-runtime v0.25.2; envtest runs etcd and API server only (no GC).
  https://book.kubebuilder.io/reference/envtest.html
- Ginkgo v2.33.1, Gomega v1.44.0 (MIT).
- testcontainers-go v0.44.0, `modules/postgres` with Snapshot/Restore; set a wait strategy.
  https://golang.testcontainers.org/modules/postgres/
- ACP methods and optional `session/load`: https://agentclientprotocol.com/protocol/session-setup

## Links

- [design.md](../design.md), decision 25
- ADR [0026](0026-home-lab-and-license.md), [0035](0035-v01-flavors.md),
  [0040](0040-agentgateway-model-and-mcp-gateway.md)
