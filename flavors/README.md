# flavors

Harness images and contracts. A flavor only selects the harness image (ADR 0009, 0035).
v0.1: `opencode/`, `hermes/`, `pi/` (via pi-acp), all driven over ACP by agentd.

Every flavor image follows the contract in ADR 0048: an entry script sets the environment and
execs the ACP agent by absolute path, configuration is read-only under `/etc/koinon/`, state lives
under `/sandbox`, and MCP is configured in the harness's own config. Per-agent values (model
endpoint, model, MCP URL) arrive as arguments to the entry script. See `opencode/README.md`;
`test/flavors` checks each flavor (`make test-flavors`).
