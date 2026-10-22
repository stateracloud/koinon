# connectors

Connector types as data: auth, hosts, scopes, risk tiers and setup guides (ADR 0007, 0021).
v0.1 ships `github/` (ADR 0036, 0049).

- `<id>/connector.yaml`: one connector type per directory, named after `metadata.id`,
  `apiVersion: connectors.koinon.dev/v1alpha1`, `kind: ConnectorType`.
- `schema/connector.schema.json`: the JSON Schema (2020-12) every connector type must pass.
- `connectors.go`: embeds the types into the binary, validates them (schema plus the rules a schema
  cannot express) and answers what a grant allows: `Decide` (preset × scope tier → allow, ask or
  deny), `Tools` (the MCP tool allowlist agentgateway enforces) and `Permissions` (what a minted
  credential is narrowed to).

## Format

- `spec.auth`: the auth type (`app-credential` for GitHub), the provider permissions the app must
  have (and no more), the permissions it must not have, and the credential lifetime.
- `spec.hosts`: every host the connector talks to; scope network rules may only name these.
- `spec.targets`: what grants are narrowed to (GitHub: `owner/name` repositories).
- `spec.scopes`: each with a risk tier (ADR 0008), the provider permissions a credential for it is
  narrowed to, and either `network` rules (read tier only: direct access from the sandbox, compiled
  to OpenShell policy by plan item C2) or `tools` (MCP tools behind agentgateway; every write goes
  through tools, ADR 0042).
- `spec.presets`: for `read-only`, `standard` and `trusted`, the decision per tier. `forbidden` is
  always `deny`.
- `spec.setupGuide`: the one source for the connection page and the agent builder (ADR 0021).
  `fields` are the inputs; a field with `secret: true` is a secure input, written straight to a
  Kubernetes Secret and never stored in Postgres, returned or logged. `steps` are ordered, each
  with Markdown, console links, values to copy, the fields it collects and a `check`: the name of
  a built-in Go check (`internal/connections`) that validates the step. `TestConnection` runs the
  checks in step order and reports each against its step.

Bump `metadata.version` whenever you change a type's content.
