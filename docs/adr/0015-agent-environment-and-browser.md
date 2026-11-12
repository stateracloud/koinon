# 0015. Agent environment and browser

- Status: Accepted; amended by [0044](0044-in-sandbox-credential-exception.md) (and its amendment of 2026-11-11), [0113](0113-agent-package-access.md) and [0114](0114-model-credentials-by-type.md)
- Date: 2026-10-21
- Area: runtime-sandbox
- Tags: sandbox, agent-home, egress, credentials
- Decision: [design.md#15](../design.md)

## Context

Agents find gaps in their tooling at runtime ("I need `jq`", "I need the Terraform CLI"). Baking
every tool into images does not scale, and unrestricted installs from the internet break the egress
model. Browsers are useful for agents, but cookies are bearer credentials that would bypass the
secret model.

## Decision

- Flavor images are based on OpenShell sandbox images.
- Agents self-install into `$HOME` (with mise/aqua, uv, npm) from allowlisted sources through a
  pull-through cache. _(Changed by [0113](0113-agent-package-access.md): Koinon runs no pull-through cache; see the amendment of 2026-11-11.)_
- Installs are recorded in `environment.lock` (part of the home, [0032](0032-agent-home-checkpoints.md)).
- Org admins can promote commonly installed tools (into the org's image or default lock).
- A browser runs in the sandbox with a per-agent profile:
  - credentials are entered only through human takeover (live view, [0022](0022-frontend-architecture.md));
  - cookies are treated as secrets (an encrypted profile, [0032](0032-agent-home-checkpoints.md)).

**Amendment ([0044](0044-in-sandbox-credential-exception.md)).** Human takeover is generalised into
the *in-sandbox credential exception*. Platform-allowlisted auth types (for example a Copilot
device-flow login) follow the same rules as the browser profile
_(Changed 2026-11-11: no auth type is allowlisted today and `copilot-device-flow` does not qualify; GitHub Copilot is a gateway credential. See the amendment of 0044 and [0114](0114-model-credentials-by-type.md).)_:
- obtained by a human;
- stored encrypted on the home volume;
- treated as a secret in checkpoints and excluded from export;
- egress limited to the provider's hosts;
- revocable.

## Alternatives considered

- **Fat images with every tool** _(reconstructed)_. Large images that are never complete.
- **Unrestricted installs** _(reconstructed)_. Breaks the egress model and is a supply-chain risk.
- **Browser outside the sandbox (remote browser service)** _(reconstructed)_. Cleaner isolation of
  cookies, but more infrastructure. It could come back later.

## Consequences

- Positive: agents fix their own tool gaps within guardrails, and environments are reproducible
  from `environment.lock`.
- Negative: the pull-through cache and the allowlist are extra infrastructure _(inferred)_.
- Negative: OpenShell publishes no harness images. Its default is plain
  `nvcr.io/nvidia/base/ubuntu:24.04`, so Koinon builds and maintains its flavor images
  ([0035](0035-v01-flavors.md)).
- Negative: OpenShell does not rewrite cookies, so browser sessions are an explicit, accepted
  in-sandbox credential.
- Follow-ups: browser and self-installed tools ship in v0.5 ([0024](0024-roadmap.md)).

## Open questions

- The pull-through cache: which product (a generic OCI/npm/PyPI proxy, Artifactory OSS, Nexus OSS,
  Zot plus Verdaccio plus devpi)? To be decided before v0.5.
- How OpenShell network policy expresses "allowlisted sources", given that installs go through
  many binaries (curl, npm, uv, mise).

## Upstream verification

- 2026-10-21: the OpenShell default sandbox image is `nvcr.io/nvidia/base/ubuntu:24.04` with no
  agent installed. NVIDIA publishes no harness images. OpenCode is shown through the upstream
  `ghcr.io/anomalyco/opencode`. https://github.com/NVIDIA/OpenShell
- 2026-10-21: OpenShell placeholder rewriting does not cover cookies.
  https://github.com/NVIDIA/OpenShell (docs/how-it-works/providers/overview.mdx)

## Amendment (2026-11-11)

[0113](0113-agent-package-access.md) changes how self-installs reach packages. Agents install straight from
allow-listed registries, read-only, with any registry login injected by OpenShell; Koinon runs no
pull-through cache. A proxy behind the registry connection is optional. Access comes in four levels per
agent. The first open question above (which cache product) is closed; the second (how the network policy
names the many installing programs) is that ADR's pending spike. `environment.lock` and the promotion of
common tools are not changed here.

## Links

- [0032](0032-agent-home-checkpoints.md), [0035](0035-v01-flavors.md),
  [0044](0044-in-sandbox-credential-exception.md)
