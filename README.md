# Koinon

[![CI](https://img.shields.io/github/actions/workflow/status/stateracloud/koinon/ci.yml?branch=main&label=ci)](https://github.com/stateracloud/koinon/actions/workflows/ci.yml?query=branch%3Amain)
[![e2e](https://img.shields.io/github/actions/workflow/status/stateracloud/koinon/e2e-kind.yml?branch=main&label=e2e)](https://github.com/stateracloud/koinon/actions/workflows/e2e-kind.yml?query=branch%3Amain)
[![Version](https://img.shields.io/github/v/tag/stateracloud/koinon?include_prereleases&sort=semver&label=version)](https://github.com/stateracloud/koinon/tags)
[![License](https://img.shields.io/github/license/stateracloud/koinon)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/stateracloud/koinon)](go.mod)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/stateracloud/koinon/badge)](https://scorecard.dev/viewer/?uri=github.com/stateracloud/koinon)

Koinon is an open source org-and-work layer for AI agents on Kubernetes. Teams and people get
their own orgs with agents, connectors and secrets; agents pick up work from a ticket board, talk to
each other over A2A, and ask a human before risky actions. Execution, isolation and credential
brokering are reused from [NVIDIA OpenShell](https://github.com/NVIDIA/OpenShell) on
[Kubernetes Agent Sandbox](https://agent-sandbox.sigs.k8s.io), so real credentials and model keys
never enter the agent's sandbox.

Pronounced **KOY-non**, after the leagues of independent Greek city-states.

**Status: pre-release (v0.1 release candidates, currently rc.9).** The quickstart works on kind;
expect rough edges and changes between candidates. The design lives in
[`docs/design.md`](docs/design.md) and the decisions in [`docs/adr/`](docs/adr/).

## Layout

| Path | Contents |
| --- | --- |
| `api/` | Protobuf API definitions (connect-rpc) |
| `cmd/` | `koinon-api` and `koinon-reconciler` binaries |
| `internal/` | Go packages, database migrations and generated code |
| `web/` | React + Vite single-page app |
| `harnesses/` | Harness images and contracts (OpenCode) |
| `connectors/` | Connector types, starting with GitHub |
| `templates/` | Agent templates |
| `charts/koinon/` | Helm chart |
| `test/` | End-to-end and sandbox escape tests |
| `docs/` | Design, ADRs, spikes and developer documents (the docs site is in [stateracloud/koinon-docs](https://github.com/stateracloud/koinon-docs), live at <https://koinon.stateracloud.com>) |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Koinon is licensed under [Apache-2.0](LICENSE); contributions
need a DCO sign-off, no CLA.
