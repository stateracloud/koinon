# 0025. Deterministic, free CI with a continuous sandbox escape suite

- Status: Accepted; amended 2026-11-02 (below)
- Date: 2026-10-21
- Decision: [design.md#25](../design.md)
- Verification: escape suite design depends on the OpenShell spike ([koinon-private#3](https://github.com/stateracloud/koinon-private/issues/3))

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

## Amendment (2026-11-02, koinon-private#512 and koinon-private#547): the optional lab lane and where the kind e2e runs

CI stays free and public (above). This amendment records the lab lane a maintainer may add and the
routing of the kind e2e. It does not change what CI requires.

**What koinon-private#512 built.** `hack/e2e-remote.sh --gate <set>` runs one gate set (go-test,
agentd-ginkgo, ...) on an optional external queue a maintainer operates. A run that is still Pending after
120 s is given up (no capacity, a quota or an unschedulable pod). It is deleted, never stopped (a stopped run
turns Failed and the queue pushes a false "Failed"), and the script exits 75: the caller runs the gate locally.

**Decision (procoduck, 2026-11-02, koinon-private#547): a green lab run is enough to merge a needs-e2e pull request.**
The kind e2e of a pull request is tested like this, in this order:

1. With a lab: `hack/e2e-remote.sh --argo` runs it on the lab. A followed run posts the commit status
   `e2e-lab` (success or failure) on the tested commit; `hack/pr-wait.sh <PR> --checks --require e2e`
   accepts a green `e2e-lab` on the head, or any green `quickstart` check run on the head.
2. GitHub's `e2e-kind` runs on a pull request only when there is no green lab run: no lab (exit 78), the
   lab cannot run it now (exit 75: unreachable, no slot, Pending fallback, no verdict), the change touches
   `flavors/` (exit 79: the external run cannot build a flavor image; refused up front unless
   `--allow-flavors`), or the pull request
   changes the e2e's own files (already forced) or carries the `e2e` label.
3. Main stays batched on GitHub, plus the nightly run, always.

Requiring `quickstart` alone would pay for GitHub on every needs-e2e PR and make the lab pointless, so
`e2e-lab` is accepted instead. It is **not** a required check of the ruleset: contributors without a
lab cannot produce it. The merge rule is the agents' (`AGENTS.md`), checked with `hack/pr-wait.sh`;
the required check stays `e2e result`. `e2e-lab` is a plain commit status, trusted as far as the
maintainer's lab credentials are; it asserts only that a lab run passed on that exact commit.

Note (2026-11-05, issue #148): point 2 above no longer holds for pull requests. `hack/e2e-decide.sh` now
runs the quickstart in the pull request's own run whenever the full rule (`hack/needs-e2e.sh`) says the
change is e2e-relevant, not only for the e2e's own files or the `e2e` label. The earlier "required but not
run here" state could never go green for a fork pull request (it cannot label, dispatch or post `e2e-lab`),
which blocked Dependabot's. The `e2e` label (now: force a run for a change the rule thinks irrelevant) and
the lab's `e2e-lab` status remain accelerators and still satisfy `e2e result` (the result job waits for the pull request's own quickstart; if that fails or is cancelled, `hack/e2e-gate.sh` falls through to `e2e-lab` or another run's quickstart); the lab-first order is a
way to get the verdict sooner, not a precondition. Cost: a needs-e2e pull request pays for a GitHub run
even when the lab also passes.

Exit codes of `hack/e2e-remote.sh --argo` separate the cases a caller acts on differently: 0 passed,
1 the e2e failed, 2 usage, 75 the lab cannot run it now (fall back), 76 not run, 78 no lab configured,
79 refused for flavors.

Note (2026-11-06, issue #200): exit 79 and `--allow-flavors` are gone. The external run builds the flavor image
through the build input `FLAVOR_BASE`, which must carry the digest the Dockerfile pins
(`hack/flavor-base.sh`); the external runner sets it from its own configuration. A change under `flavors/` is tested externally like any other (point 2's flavors case no longer
applies).

## Amendment (2026-11-03, koinon-private#610): CI's go-test on the lab gate first

The same lab lane serves CI's longest Blacksmith job. A followed `hack/e2e-remote.sh --argo --gate go-test`
posts the commit status `go-test-lab` (named like `e2e-lab`) on the tested commit; `hack/ship.sh --ready
--lab-go-test` runs it on the pushed draft head before marking the pull request ready. In `ci.yml` the
`changes` job reads the status for the pull request's exact head SHA (`hack/ci-skip-heavy.sh lab`);
`success` skips `go-test`, and the required check `go` accepts that one skip, nothing else. No status,
a draft, a push or an API error: `go-test` runs on Blacksmith, so a contributor without a lab is unaffected.
CI does not wait for a status that arrives later (a waiting Blacksmith job is billed), so a push to a ready
pull request runs `go-test` on Blacksmith. `hack/ci-skip-heavy.sh decide` (push to main) and
`hack/release-gate.sh` treat a skipped `go-test` as green only with a green `go-test-lab` on the pull
request head or a change with no Go in it. The required check name (`go`), the ruleset and the merge rule do
not change. Trust limit as for `e2e-lab` (koinon-private#568): a status anyone with write access can post, behind the
required review on `.github/**`. Details: the maintainer note (left the repository, 2026-11-07).

## Note (2026-11-05, public repository)

The repository is public. Statements above that tie CI choices to the private period (for example
that a waiting Blacksmith job is billed, or that Actions minutes are scarce) describe that period:
the workflows now run on GitHub-hosted runners, which cost nothing on a public repository. The
decisions stand; only the cost reasoning changed. What runs where: CI runs the admission half of the
escape suite (`escape (admission)`); the OpenShell cases run on kind (`make test-escape-kind`) and
on the maintainers' lab, where the Kata tier can be tested.

## Links

- [design.md](../design.md), decision 25
- ADR [0026](0026-home-lab-and-license.md), [0035](0035-v01-flavors.md),
  [0040](0040-agentgateway-model-and-mcp-gateway.md)

## Note (2026-11-07, issue #300)

The maintainer lab tooling left the repository: the remote-run client, its pod-side scripts, its
configuration and the document that described the setup are no longer tracked. The repository's e2e path
is kind (`make dev-kind-up`, `make test-e2e-kind`, the PR's own `quickstart` run). What the text above
says about the lab's scripts, `--argo`, `--gate` or `hack/lab/` describes the history. The repository
still reads two commit statuses, `e2e-lab` and `go-test-lab`, as evidence that an external run passed
on a commit.
