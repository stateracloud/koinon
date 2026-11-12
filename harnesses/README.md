# harnesses

Harness images and contracts. A harness only selects the harness image (ADR 0009, 0035).
v0.1 ships one harness, [`opencode/`](opencode/README.md), driven over ACP by agentd (ADR 0059).
Hermes, pi and other harnesses are a later release; the harness seam below stays so that adding one
is a table entry, an image and a contract run.

Every harness image follows the contract in ADR 0048: an entry script sets the environment and
execs the ACP agent by absolute path, configuration is read-only under `/etc/koinon/`, state lives
under `/sandbox`, and MCP is configured in the harness's own config. Per-agent values (model
endpoint, model, MCP URL) arrive as arguments to the entry script, the same for every harness:

```
/usr/local/bin/koinon-<harness> --model-base-url URL --model NAME --mcp-url URL
```

Exactly one layer retries a failed model request, with Koinon's policy (ADR 0056 amendment
2026-10-31, point 3). A harness whose harness retries model requests itself sets
`RetriesModelRequests` in its descriptor: koinon-agentd then repeats no turn for it and adds
`--model-tries N --model-retry-backoff-ms MS --model-max-retry-after-ms MS` (parsed by the shared
library, with koinon-agentd's defaults 3, 1000 and 60000), and the entry script makes the harness
follow them instead of its own schedule (OpenCode: [`opencode/README.md`](opencode/README.md#model-retries)).
For any other harness koinon-agentd repeats the turn with the same policy.

Every image also ships what `common/` holds, through the `common` build context (`make
harness-<name>` passes it): `koinon-git-credential`, git's credential helper for
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

`internal/harness` holds one descriptor per harness (entry script, ready command, instruction
link (empty for OpenCode), default binaries, plugin option); `internal/agentd` and the reconciler read it. `test/harnesses` checks each harness with
the same specs and per-harness expectations (`make test-harnesses`; `make harness-<name>` builds
one image). Today the suite runs against OpenCode only.

| | OpenCode |
| --- | --- |
| Harness | OpenCode 2.2.6 |
| `session/resume` advertised | yes |
| Asks permission for | paths outside the session directory |
| Unknown session on load | `session not found` |
| MCP readiness | `koinon:github_koinon-ready` command, waited for by agentd |
| MCP tool names | `koinon_<target>_<tool>`, for example `koinon_github_create_pull_request` |
| Config | read-only, used in place |
| Tool switches | read-only config |
| Run instructions (`/sandbox/.koinon/AGENTS.md`, written by agentd) | system prompt, through the `instructions` key of the read-only config (OpenCode) |

Instruction delivery is the same for every harness: agentd writes the file before the harness
starts, the entry script refuses to start (exit 64) when it is missing, empty or a symbolic
link, and before every prompt agentd checks the file's hash and restores it (event
`run.instructions_restored`). OpenCode needs no link: its config names the file. For a harness
whose descriptor sets `InstructionLink`, agentd also checks that link
(`internal/agentd`, `instructionLink`) and restores it.

The OpenCode image also ships data tools for large inputs: jq, yq, fd, ast-grep, DuckDB (behind
a wrapper that limits its memory and turns extension downloads off), sqlite3, poppler-utils
(`pdftotext`, `pdfinfo`), file, tree, unzip, zstd, xz-utils and a standard-library-only
`python3`, with no pip and no formatter. Versions, licences (poppler is GPL, shipped unmodified
from Ubuntu, like git and bash), pins and the size cost are in
[`opencode/README.md`](opencode/README.md#tools-in-the-image); koinon-agentd's instruction file
tells the agent how to use them. The contract suite runs each tool without a network
(`test/harnesses/tools_test.go`) and fails when an image outgrows its size budget.

Before agentd starts a harness, it ends every process the sandbox user runs except PID 1 and
the sandbox's main process (`internal/agentd/sweep.sh`), so a run starts with nothing left of an
earlier one: no harness daemon, no background job of the agent's that could change the home
between the entry script's checks and the harness reading it. It sweeps again once the run's
harness has ended, before the run is finished or parked and so while it still holds the agent;
a fenced worker (taken over, canceled, given up) skips that sweep, and the next run's sweep
catches what is left. Under OpenShell, PID 1 is the supervisor and the main process its first
child; the contract suite's Docker stand-in marks its PID 1 as the main process
(`KOINON_SANDBOX_STANDIN=1`). Under any other PID 1 the sweep ends nothing and the run fails.
The script needs `/bin/sh`, `grep`, `sleep` and `/proc`, which every harness image has.
