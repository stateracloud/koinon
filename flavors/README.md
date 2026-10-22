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
| Tool switches | read-only config | managed scope `/etc/hermes/config.yaml`, over the user config | rendered config |
| Run instructions (`/sandbox/.koinon/AGENTS.md`, written by agentd) | system prompt, through the global `AGENTS.md` link in the read-only image | system prompt, through `$HERMES_HOME/SOUL.md`, linked at start; a loaded session keeps its old prompt | appended to the system prompt, through `$PI_CODING_AGENT_DIR/APPEND_SYSTEM.md`, linked at start |

Instruction delivery is the same for all three: agentd writes the file before the harness
starts, the entry script refuses to start (exit 64) when it is missing, empty or a symbolic
link, and before every prompt agentd checks the file's hash and the flavor's link
(`internal/agentd`, `instructionLink`) and restores either (event `run.instructions_restored`).

Before agentd starts a harness, and again once the run's harness has ended, it ends every
process the sandbox user runs except the sandbox's main process (`internal/agentd/sweep.sh`),
so a run starts with nothing left of an earlier one: no harness daemon, no background job of the
agent's that could change the home between the entry script's checks and the harness reading
it. The script needs `/bin/sh`, `sleep` and `/proc`, which every flavor image has.
