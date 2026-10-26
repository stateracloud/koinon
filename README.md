# Koinon

Koinon is an open source org-and-work layer for AI agents on Kubernetes. Teams and people get
their own orgs with agents, connectors and secrets; agents pick up work from a ticket board, talk to
each other over A2A, and ask a human before risky actions. Execution, isolation and credential
brokering are reused from [NVIDIA OpenShell](https://github.com/NVIDIA/OpenShell) on
[Kubernetes Agent Sandbox](https://agent-sandbox.sigs.k8s.io), so real credentials and model keys
never enter the agent's sandbox.

Pronounced **KOY-non**, after the leagues of independent Greek city-states.

**Status: pre-alpha.** Nothing here is usable yet. The design lives in
[`docs/design.md`](docs/design.md) and the decisions in [`docs/adr/`](docs/adr/).

## Layout

| Path | Contents |
| --- | --- |
| `api/` | Protobuf API definitions (connect-rpc) |
| `cmd/` | `koinon-api` and `koinon-reconciler` binaries |
| `internal/` | Go packages, database migrations and generated code |
| `web/` | React + Vite single-page app |
| `flavors/` | Harness images and contracts (OpenCode) |
| `connectors/` | Connector types, starting with GitHub |
| `templates/` | Agent templates |
| `charts/koinon/` | Helm chart |
| `test/` | End-to-end and sandbox escape tests |
| `docs/` | Design, ADRs, spikes and the docs site |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Koinon is licensed under [Apache-2.0](LICENSE); contributions
need a DCO sign-off, no CLA.
