# 0026. Home lab for real isolation testing; Apache-2.0 with DCO

- Status: Accepted; amended 2026-11-02 (below)
- Date: 2026-10-21
- Decision: [design.md#26](../design.md)

## Context

Kata Containers needs hardware virtualization, which GitHub-hosted runners do not offer reliably,
and cloud clusters with nested virtualization cost money. A maintainer has a Talos cluster with the
Kata runtime installed. Public CI must never run untrusted pull-request code on that cluster.

For licensing, Koinon reuses Apache-2.0 projects (OpenShell, Agent Sandbox, agentgateway) and wants
a low barrier for contributors and adopters, including companies.

## Decision

- The maintainer's Talos+Kata cluster is for development and local testing only. It is never
  connected to public CI.
- License: Apache-2.0.
- Contributions are signed off under the Developer Certificate of Origin (`git commit -s`). No CLA.

## Alternatives considered

- Self-hosted GitHub runner on the home lab _(reconstructed)_. Rejected: PRs from forks would run on
  a private network.
- Paid cloud CI with nested virtualization _(reconstructed)_. Rejected for now: cost; may revisit for
  a release gate.
- MIT license _(reconstructed)_. Rejected: Apache-2.0 has an explicit patent grant and matches the
  reused upstreams.
- AGPL or source-available license _(reconstructed)_. Rejected: hinders adoption and contribution;
  note that Multica (prior art) uses Apache-2.0 with hosting restrictions, which is the kind of
  license Koinon avoids.
- CLA _(reconstructed)_. Rejected: friction for contributors; DCO is enough for Apache-2.0.

## Consequences

- Positive: real VM isolation testing at no cloud cost; simple, well-known license.
- Negative: Kata-specific behaviour is only tested where procoduck runs it; regressions may be found
  late _(inferred)_.
- Negative: no relicensing flexibility without a CLA (accepted trade-off).
- Follow-ups: DCO check on PRs (DCO app with `.github/dco.yml`, or an Actions-based check); document
  home-lab test steps in CONTRIBUTING.

## Open questions

- DCO enforcement mechanism: the DCO GitHub app or an Actions job.
- Whether a release gate should run the escape suite on Kata in paid infrastructure later.

## Upstream verification

Checked 2026-10-21:
- Talos Kata system extension registers `kata` (Cloud Hypervisor) and `kata-qemu` handlers; example
  RuntimeClass `kata` with pod overhead 130Mi / 250m.
  https://github.com/siderolabs/extensions/blob/main/container-runtime/kata-containers/README.md
- Agent Sandbox Kata examples cover AKS and GKE only; Talos is untested upstream.
  https://github.com/kubernetes-sigs/agent-sandbox
- DCO app configuration: https://github.com/dcoapp/app

## Amendment (2026-11-02, koinon-private#547): a maintainer's agent may submit lab runs

The lab is still not a CI runner and is still not connected to public CI: no GitHub workflow, and no
pull request's code, reaches it by itself. A maintainer's own agent submits a run of the maintainer's
own checkout (`hack/e2e-remote.sh --argo`) and posts the result as the commit status `e2e-lab`.
`e2e-lab` is not a check of the repository ruleset: contributors without a lab use GitHub's `e2e-kind`.
The merge rule and the exit codes are in ADR [0025](0025-testing-strategy.md) (amendment).

## Links

- [design.md](../design.md), decision 26
- ADR [0025](0025-testing-strategy.md), [0038](0038-governance.md),
  [0039](0039-development-setup.md)

## Note (2026-11-07, issue #300)

The maintainer lab tooling left the repository: the remote-run client, its pod-side scripts, its
configuration and the document that described the setup are no longer tracked. The repository's e2e path
is kind (`make dev-kind-up`, `make test-e2e-kind`, the PR's own `quickstart` run). What the text above
says about the lab's scripts, `--argo`, `--gate` or `hack/lab/` describes the history. The repository
still reads two commit statuses, `e2e-lab` and `go-test-lab`, as evidence that an external run passed
on a commit. Private infrastructure detail was removed in the same change (maintainer decision
2026-11-07). The domain lint that guarded this left the repository too.
