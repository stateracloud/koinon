<!-- SPDX-License-Identifier: Apache-2.0 -->

# hack/spike: historical

These are the scripts, manifests, probes and raw results of the early spikes (October 2026) that
settled the OpenShell, Agent Sandbox, agentgateway and harness decisions in the ADRs and in
`docs/spikes/`. They are kept as evidence, not as supported tooling:

- They were written against the maintainer's own cluster (a private registry, a shared OpenShell) and are not
  maintained; several no longer match the current charts or APIs.
- Nothing in the build or the tests depends on them, apart from `probe/`, the source of the
  `spike-probe` image used by the escape suite (see `test/escape` and `make test-escape-kind`).
  (`kind/` was run by a `spike-kind` workflow, deleted in old issue 561; the scripts stay as evidence.)
- Do not start new work here. Reproducible checks belong in `test/`, `hack/` or the chart.
