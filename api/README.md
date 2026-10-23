# api

Protobuf definitions for the Koinon API, served with connect-rpc (ADR 0022, 0047).
Packages are `koinon.<area>.v1alpha1` under `api/proto/koinon/`. Run `make generate` after changes;
`buf breaking` runs in CI.

| Package | Services | Implementation |
| --- | --- | --- |
| `koinon.auth.v1alpha1` | `AuthService` | `internal/auth` |
| `koinon.agent.v1alpha1` | `AgentService` | `internal/services` |
| `koinon.ticket.v1alpha1` | `TicketService` | `internal/services` |
| `koinon.run.v1alpha1` | `RunService` (`WatchRun` streams) | `internal/services`, `internal/runwatch` |
| `koinon.template.v1alpha1` | `TemplateService` | `internal/services` |

Every org-owned request names its `org_id`; IDs from another org are not found. Every RPC must be
listed in the org isolation suite (`internal/api/isolation_test.go`), or the registry check fails.
