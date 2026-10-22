# flavors

Harness images and contracts. A flavor only selects the harness image (ADR 0009, 0035).
v0.1: [`opencode/`](opencode/README.md), [`hermes/`](hermes/README.md),
[`pi/`](pi/README.md) (via pi-acp), all driven over ACP by agentd.

Every flavor image follows the contract in ADR 0048: an entry script sets the environment and
execs the ACP agent by absolute path, configuration is read-only under `/etc/koinon/`, state lives
under `/sandbox`, and MCP is configured in the harness's own config. Per-agent values (model
endpoint, model, MCP URL) arrive as arguments to the entry script, the same for every flavor:

```
/usr/local/bin/koinon-<flavor> --model-base-url URL --model NAME --mcp-url URL
```

Every image also ships what `common/` holds, through the `common` build context (`make
flavor-<name>` passes it): `koinon-git-credential`, git's credential helper for
`https://github.com`, configured in the read-only `/etc/koinon/git/config` and included from
`/etc/gitconfig`. It prints the GitHub provider's OpenShell placeholder from `$GITHUB_TOKEN`,
which OpenShell swaps for the installation token on the wire, and stores nothing
(`connectors/README.md`).

`internal/agentd` maps each flavor to its entry script. `test/flavors` checks each flavor with
the same specs and per-flavor expectations (`make test-flavors`; `make flavor-<name>` builds
one image).

| | OpenCode | Hermes | pi |
| --- | --- | --- | --- |
| Harness | OpenCode 1.19.3 | Hermes Agent 0.22.3 | pi 1.3.0 + pi-acp 0.0.34 |
| `session/resume` advertised | yes | yes | no |
| Asks permission for | paths outside the session directory | every file write or patch | nothing |
| Unknown session on load | `-32603` service failure | `-32603` `not_found` | `-32602` `Unknown sessionId` |
| MCP tool names | `koinon_<tool>` | `mcp__koinon__<tool>` | `mcp__koinon__<tool>` (direct exposure) |
| Config | read-only, used in place | read-only, linked into `HERMES_HOME` | read-only templates rendered into the home at start |
