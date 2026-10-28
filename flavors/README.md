# flavors

Harness images and contracts. A flavor only selects the harness image (ADR 0009, 0035).
v0.1 ships one flavor, [`opencode/`](opencode/README.md), driven over ACP by agentd (ADR 0059).
Hermes, pi and other harnesses are a later release; the flavor seam below stays so that adding one
is a table entry, an image and a contract run.

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
(`connectors/README.md`). For a connection on another host (GitHub Enterprise Server),
koinon-agentd renders the same sections for that host, and `http.sslCAInfo` for its CA bundle,
into `/sandbox/.koinon/git/config` before every run, which `/etc/gitconfig` also includes; the
helper then takes `--host HOST[:PORT]`.

The entry scripts source `common/koinon-entry-lib.sh` (installed as
`/usr/local/lib/koinon/koinon-entry-lib.sh`): `koinon_die` (exit 64), argument parsing, URL
checks, the instruction-file check and the `KOINON_*` exports. Each script
keeps what differs: its value check, PATH, the harness's variables, home clean-up and the exec.

`internal/flavor` holds one descriptor per flavor (entry script, ready command, instruction
link (empty for OpenCode), default binaries, plugin option); `internal/agentd` and the reconciler read it. `test/flavors` checks each flavor with
the same specs and per-flavor expectations (`make test-flavors`; `make flavor-<name>` builds
one image). Today the suite runs against OpenCode only.

| | OpenCode |
| --- | --- |
| Harness | OpenCode 2.2.0 |
| `session/resume` advertised | yes |
| Asks permission for | paths outside the session directory |
| Unknown session on load | `session not found` |
| MCP readiness | `koinon:github_koinon-ready` command, waited for by agentd |
| MCP tool names | `koinon_<target>_<tool>`, for example `koinon_github_create_pull_request` |
| Config | read-only, used in place |
| Tool switches | read-only config |
| Run instructions (`/sandbox/.koinon/AGENTS.md`, written by agentd) | system prompt, through the global `AGENTS.md` link in the read-only image |

Instruction delivery is the same for every flavor: agentd writes the file before the harness
starts, the entry script refuses to start (exit 64) when it is missing, empty or a symbolic
link, and before every prompt agentd checks the file's hash and the flavor's link
(`internal/agentd`, `instructionLink`) and restores either (event `run.instructions_restored`).

Before agentd starts a harness, it ends every process the sandbox user runs except PID 1 and
the sandbox's main process (`internal/agentd/sweep.sh`), so a run starts with nothing left of an
earlier one: no harness daemon, no background job of the agent's that could change the home
between the entry script's checks and the harness reading it. It sweeps again once the run's
harness has ended, before the run is finished or parked and so while it still holds the agent;
a fenced worker (taken over, canceled, given up) skips that sweep, and the next run's sweep
catches what is left. Under OpenShell, PID 1 is the supervisor and the main process its first
child; the contract suite's Docker stand-in marks its PID 1 as the main process
(`KOINON_SANDBOX_STANDIN=1`). Under any other PID 1 the sweep ends nothing and the run fails.
The script needs `/bin/sh`, `grep`, `sleep` and `/proc`, which every flavor image has.
