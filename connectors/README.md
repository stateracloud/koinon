# connectors

Connector types as data: auth, hosts, scopes, risk tiers and setup guides (ADR 0007, 0021).
v0.1 ships `github/` (ADR 0036, 0049) and `gitea/` (ADR 0062, 0063).

- `<id>/connector.yaml`: one connector type per directory, named after `metadata.id`,
  `apiVersion: connectors.koinon.dev/v1alpha1`, `kind: ConnectorType`.
- `schema/connector.schema.json`: the JSON Schema (2020-12) every connector type must pass.
- `connectors.go`: embeds the types into the binary, validates them (schema plus the rules a schema
  cannot express) and answers what a grant allows: `Decide` (preset × scope tier → allow, ask or
  deny), `Tools` (the MCP tool allowlist agentgateway enforces) and `Permissions` (what a minted
  credential is narrowed to).

## Format

- `spec.auth`: the auth type (`app-credential` for GitHub, `static-token` for Gitea, whose
  `tokenWarnAfter` is the age after which Koinon warns about a token), the provider permissions the app must
  have (and no more), the permissions it must not have, and the credential lifetime.
- `spec.site.selfHosted: true` (Gitea): the connector has no host of its own. `hosts` and `site` hold
  a placeholder on a reserved `.invalid` name, each connection names its base URL, which may carry a
  sub-path (`https://host[:port][/path]`), and the network rules, written for the root of the host, are
  rendered under that path. GitHub's base URL is a host and a port only.
- `spec.site.mcpHosts`: hosts of MCP servers known to act on the default host besides its web and
  API hosts (GitHub: `api.githubcopilot.com`). An MCP server may be linked to a code-host connection
  (ADR 0091) only when its URL names the connection's web or API host or, for a connection on the
  default host, one of these; a connection on its own host (GitHub Enterprise Server, Gitea) accepts
  only that host. Not allowed with `selfHosted`. An org admin can add further hosts to one connection
  (#240; `connections.mcp_hosts`, not part of the connector).
- `spec.hosts`: every host the connector talks to; scope network rules may only name these.
- `spec.targets`: what grants are narrowed to (GitHub: `owner/name` repositories).
- `spec.scopes`: each with a risk tier (ADR 0008), the provider permissions a credential for it is
  narrowed to, and either `network` rules (read tier only: direct access from the sandbox, compiled
  to OpenShell policy by plan item C2) or `tools` (MCP tools behind agentgateway; every write goes
  through tools, ADR 0042).
- `spec.presets`: for `read-only`, `standard` and `trusted`, the decision per tier. `forbidden` is
  always `deny`.
- `spec.setupGuide`: the one source for the connection page and the agent builder (ADR 0021).
  `fields` are the inputs; a field with `secret: true` is a secure input, sealed straight into
  the encrypted secret store (ADR 0056) and never stored in the clear, returned or logged. `steps` are ordered, each
  with Markdown, console links, values to copy, the fields it collects and a `check`: the name of
  a built-in Go check (`internal/connections`) that validates the step. `TestConnection` runs the
  checks in step order and reports each against its step.

Bump `metadata.version` whenever you change a type's content.

## Network rules (plan item C2)

A read-tier scope's `network` rules are what the sandbox may send straight to the connector's
hosts. `{owner}` and `{repo}` are filled from the grant's bound targets; `query` requires query
parameters (every value must match the glob); `deny: true` makes a rule an OpenShell deny rule,
which wins over every allow rule and may not name a target. `Network` renders them per host.

For GitHub `repo.read`, `internal/reconciler/github.go` renders, per active agent whose grant is
bound to repositories:

- a provider record `kg-<hash>` holding an installation token narrowed to the bound repositories
  with `contents`, `metadata`, `pull_requests` and `issues` read, behind a stable placeholder in
  `GITHUB_TOKEN` and `GH_TOKEN`. The token is cached until 10 minutes before its 1-hour expiry;
  the next pass mints a new one and pushes it into the same record, so running processes keep
  their placeholder and no sandbox restarts (spike 2.7);
- a per-agent provider profile `koinon-github-<hash>` whose endpoints are the rendered rules.
  OpenShell sends a static credential only to its profile's endpoints, so the profile cannot be
  shared between agents bound to different repositories;
- the same endpoints as the sandbox policy entry `github_read`, `protocol: rest`,
  `enforcement: enforce`, for `/usr/bin/git` and the flavor's binaries.

Allowed: `GET /{owner}/{repo}[.git]/info/refs?service=git-upload-pack` and
`POST /{owner}/{repo}[.git]/git-upload-pack` on `github.com` (clone and fetch over smart HTTP),
and `GET /repos/{owner}/{repo}` and `/repos/{owner}/{repo}/**` on `api.github.com`. Denied
explicitly: `git-receive-pack` (push, including its `info/refs` discovery) and `POST`, `PUT`,
`PATCH`, `DELETE` on the REST API. Everything else is denied by default.

### What OpenShell 0.1.7 cannot express

Checked against the vendored protos and the policy schema reference at tag v0.1.7
(`docs/how-it-works/policies/` is unchanged since v0.1.5):

- **Case.** REST path globs are case-sensitive; GitHub owner and repository names are not. Only
  the spelling stored in the binding is allowed (other spellings are denied, which fails closed).
  The binding stores GitHub's spelling: at binding time Koinon reads `full_name` from
  `GET /repos/{owner}/{repo}` with the metadata-read token, so a binding entered as
  `acme/WIDGETS` grants `Acme/Widgets`, the spelling git's remote URLs then need.
- **Binary ancestry.** A binary matches its own connections and those of its children. Listing
  the harness binary (needed for REST reads from its tools) therefore admits every process the
  harness starts; the method and path rules, not the binary list, are the boundary.
- **No GraphQL, search or redirects.** OpenShell's GraphQL rules match operation types, names and
  root fields, not arguments, so `api.github.com/graphql` cannot be limited to the bound
  repositories and is not allowed. `GET /search/*` takes the repository in free text (`q`) and is
  not allowed. Redirects to other paths or hosts are not followed by policy: renamed repositories
  (`/repositories/{id}`), tarball and zipball downloads (`codeload.github.com`),
  `raw.githubusercontent.com` and Git LFS fail.
- **Absent query parameters.** The schema does not say whether an allow rule's query matcher
  matches a request without the parameter; the explicit deny rule for
  `service=git-receive-pack` keeps push denied either way.
- **Responses.** OpenShell inspects requests, not responses; GitHub's own token scoping is what
  keeps a `GET` under `/repos/{owner}/{repo}/**` to data the token may read.
- **Credential placement.** OpenShell 0.1.7 injects static credentials only by rewriting the
  placeholder where the client puts it (headers, including Basic and Bearer, and URL path and
  query values; `crates/openshell-core/src/secrets.rs`, unchanged since 0.1.5). Every flavor image
  therefore ships `koinon-git-credential` (`flavors/common`), configured for
  `https://github.com` in the read-only `/etc/koinon/git/config` (included from
  `/etc/gitconfig`): it answers `x-access-token` and the placeholder from `$GITHUB_TOKEN`, git
  sends it as Basic auth and OpenShell substitutes the token on the wire. It stores nothing.
- **Hosts per connection (ADR 0062).** A connection stores a `baseUrl` and an optional `caBundle`
  in its non-secret config (both empty for github.com). The type's network rules name the default
  hosts; `Type.Locate` and `Type.NetworkAt` render them at the connection's host, with the REST API
  under `spec.site.apiPath` (`/api/v3` for GitHub Enterprise Server) on that host. koinon-agentd
  renders `/sandbox/.koinon/git/config` before every run (the credential helper scoped to the
  host, `http.sslCAInfo` for the CA bundle, which is public) and `/sandbox/.koinon/git/ca.pem` (the
  sandbox's system roots plus the bundle); `/etc/gitconfig` includes the file. OpenShell's proxy
  must itself trust a custom CA to reach the host; Koinon does not configure that.
- **Capabilities and CI paths are data.** `spec.capabilities` (display name, credential class,
  narrowing, repository limit, CI denial, exact modes), `auth.tokenLifetime` and `spec.ciPaths` (and `spec.ciPathsFoldCase`)
  describe the provider; `codehost.CapabilitiesOf` reads them.
- **Profile changes.** Changing the bound repositories updates the profile in place; OpenShell
  revokes placeholders whose endpoint boundary changed, so a running harness needs a new
  placeholder (a new session) after such a change.
- **Expiry metadata.** Providers can carry `credential_expiration_times`; Koinon does not set them
  yet (not verified on a gateway), so an unrefreshed token is refused by GitHub, not OpenShell,
  after its hour.

The escape suite (`test/escape`) checks push, `POST` and other repositories against a real
gateway.
