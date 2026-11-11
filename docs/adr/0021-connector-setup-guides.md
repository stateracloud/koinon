# 0021. Machine-readable connector setup guides and secure inputs

- Status: Accepted
- Date: 2026-10-21
- Amended by: [0056](0056-model-providers-pricing-and-budgets-per-org.md) (where the secure inputs
  land: an encrypted column in Postgres, no longer a Kubernetes Secret per connection)
- Decision: [design.md#21](../design.md)

## Context

Connector setup is where self-service platforms fail: creating a GitHub App, an Entra app with
federated credentials or a Slack app takes many steps in third-party consoles, and errors surface
later as opaque 401s. Connector types are platform-curated (ADR [0007](0007-connector-model.md)) and
the agent builder must be able to guide setup too (ADR [0020](0020-agent-builder-and-templates.md)).
Secrets must never pass through chat transcripts or the model, because the builder is a model.

Research on 2026-10-21 showed how much setup varies: GitHub App installation tokens last one hour and
can be narrowed per repository; Entra federated credentials need exact issuer/subject/audience
matches and take minutes to propagate; Azure DevOps needs each identity added to every org with a
license (see ADR [0043](0043-workload-identity-adapters-and-koinon-issuer.md)).

## Decision

- Each connector type ships a machine-readable setup guide: fields, ordered steps, links to the
  third-party console, and a validation check per step.
- The same guide drives the UI connection card and the agent builder; there is one source.
- The builder generates setup-specific values (callback URLs, issuer, subject, scopes) for the
  user's org and deployment.
- Secrets are entered through inline secure inputs that post directly to the secret store
  (Vault/OpenBao); they never enter the chat, the model context or Postgres.
  _(Replaced by [0056](0056-model-providers-pricing-and-budgets-per-org.md): a secure input is written to an envelope-encrypted column in Postgres. The rule that a secret never enters the chat or the model context stays.)_
- Each step is validated as it completes (e.g. "app installed on repo", "token exchange works"), so
  errors appear at the step that caused them.
- Setup guides are data, versioned with the connector type in `connectors/`.

## Alternatives considered

- Static documentation pages per connector _(reconstructed)_. Rejected: no validation, drifts from
  the UI, and the builder cannot use it.
- Pasting secrets into the builder chat with redaction _(reconstructed)_. Rejected: the model and
  transcripts would see secrets before redaction.
- Automated provisioning via cloud admin APIs only _(reconstructed)_. Partly possible later (e.g.
  creating federated credentials), but needs high-privilege credentials users will not hand over.

## Consequences

- Positive: help arrives at the moment of failure; the builder and UI never disagree.
- Positive: the secret path is short and auditable: browser to secret store.
- Negative: each connector type carries a guide that must track third-party console changes; guides
  need periodic manual verification _(inferred)_.
- Negative: validation checks need network access from the control plane to third-party APIs
  _(inferred)_.
- Follow-ups: guide schema; GitHub connector guide for v0.1 (ADR [0036](0036-v01-demo.md)); secure
  input component in the web UI.

## Open questions

- Guide schema format (JSON Schema plus step list, or a small DSL) and how validation checks are
  implemented (built-in Go checks referenced by name is the likely answer).
- ~~Secret store choice for v0.1 (Vault vs OpenBao, or Kubernetes Secrets as a dev fallback).~~
  Closed: the encrypted store in Postgres, see [0056](0056-model-providers-pricing-and-budgets-per-org.md).

## Upstream verification

- GitHub App installation tokens: 1 hour, narrowable by repositories and permissions,
  https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app
  (checked 2026-10-21).
- Entra federated credential constraints:
  https://learn.microsoft.com/en-us/entra/workload-id/workload-identity-federation-considerations
  (checked 2026-10-21).

## Links

- [design.md](../design.md), decision 21
- ADR [0007](0007-connector-model.md), [0020](0020-agent-builder-and-templates.md),
  [0043](0043-workload-identity-adapters-and-koinon-issuer.md)
