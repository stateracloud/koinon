# Step 7: harnesses over ACP

Run 2026-10-21, 14:55–15:30Z, after step 8 (see `8-escape.md`). Full transcripts (JSON lines, one
per ACP message) and harness stderr are in `raw/7/`; the exec prerequisite is in
`raw/7-exec-stdio.md`.

Setup:

- Flavor images (`hack/spike/flavors/`): `koinon/spike-opencode:2` (OpenCode 1.19.3, glibc binary
  from npm on the probe base), `koinon/spike-hermes:3` (official `nousresearch/hermes-agent:v0.22.3`),
  `koinon/spike-pi:3` (pi 1.3.0 + pi-acp 0.0.34 on `node:24-bookworm-slim`). Each has read-only
  config in `/etc/koinon/` pointing at agentgateway for chat completions and MCP, with the
  agent's key referenced as `$OPENAI_API_KEY` (an OpenShell placeholder), and a `koinon-*` wrapper
  that sets the environment and starts the ACP agent.
- Sandboxes `h-opencode`, `h-hermes`, `h-pi` with provider `agentgateway-key` (agent A) and
  `policies/harness.yaml` (Landlock `hard_requirement`, `/opt` readable, no network beyond the
  provider profile, which now names the three harness executables).
- Driver `hack/spike/acpdrive` (Go, `github.com/coder/acp-go-sdk` v0.13.5) runs
  `openshell sandbox exec --no-tty -- koinon-<harness>` and speaks ACP over its stdio; it grants
  harness-local permission requests (the sandbox is the boundary; risky actions go through the
  approval tool).
- Task for 7.5: call `create_pull_request` on the koinon MCP server with a branch, title and
  `hello.md`. The approver (now image `:2`) writes the branch and file and opens the PR only after
  approval. No direct write path to Gitea exists for the agent.

| # | OpenCode | Hermes | pi (pi-acp) |
| --- | --- | --- | --- |
| 7.1 | pass: `loadSession`, `mcpCapabilities {http, sse}`, sessions close/fork/list/resume (init 4–10 s) | pass: `loadSession`, sessions fork/list/resume, **no `mcpCapabilities`** (init 3–11 s) | pass: `loadSession`, sessions delete/list, **no resume**, no `mcpCapabilities` (init 2 s) |
| 7.2 | pass: session/new, prompt, streamed updates, `end_turn` in 1 m 49 s | pass: `end_turn` in 49 s, reply `done` | pass: `end_turn` in 10 s, reply `done` |
| 7.3 | pass: after `sandbox stop`/`start`, `session/load` replays the history and the agent answers `agent/opencode-hello` | pass: load replays 13 updates; the model recalls `agent/hermes-hello` in its reasoning but answers only "agent" (model ability, not plumbing) | pass: load after stop/start, answers `agent/pi-hello` |
| 7.4 | pass: `write` tool call, `notes.txt` = `koinon` | pass: `write_file`, preceded by an ACP permission request (`Approve edit`) | pass: `write` tool call |
| 7.5 | **pass, attempt 1**: request `15d955b99082815a` → PR #2 | **pass, attempt 1**: request `ceaca76ba6e420fb` → PR #3 | **pass, attempt 2**: request `fcfde3179b2e2d44` → PR #4 |
| 7.6 | "load needs the original cwd": **refuted** (loaded with `cwd=/tmp`, recalled the PR title). Unknown id → error, not a new session | unknown id on load → error `not_found` (the source reading predicted a silent null result: refuted). Resume not exercised. Substitutes `${OPENAI_API_KEY}` in MCP headers (confirmed) | confirmed: no `session/resume`; does not take client MCP servers (`mcpCapabilities` empty), so MCP comes from pi's own `mcp.json`. Unknown id → `Invalid params … Unknown sessionId` |

All three harnesses pass 7.1–7.5.

## Findings

1. **OpenShell does not pass the image's `ENV` to the processes it starts**, `PATH` included. Only
   `HOME`, the provider placeholders and OpenShell's own variables arrive. Flavor images must set
   their environment in an entry script (Koinon's flavor contract, ADR 0048), and call binaries by
   absolute path or set `PATH` there.
2. **ACP over `exec --no-tty` works for all three harnesses** with no relay inside the sandbox;
   `agentd` needs the same non-TTY stdin path through the gateway API.
3. **pi exposes MCP tools through `codemode` by default** (tools not declared to the model, only
   callable from scripts); the 4B model never found them (attempt 1 explored pi's example files for
   4 minutes). `"exposure": "direct"` in `mcp.json` fixed it. Koinon's pi flavor sets direct
   exposure for the approval tools.
4. **Harnesses phone home and are refused, harmlessly**: OpenCode (`opencode.ai`,
   `models.opencode.ai`, `registry.npmjs.org` for its plugin install), Hermes (model catalog on
   `nousresearch.com` and GitHub), pi (`pi.dev`). None blocked the run; flavor configs should
   still switch them off where a switch exists.
5. **Hermes and pi-acp don't advertise MCP capabilities**, so per-session MCP servers from ACP clients
   don't work for them; Koinon configures MCP in each flavor's own config, which all three honour.
6. Hermes asks for edit permission over ACP; OpenCode and pi did not for these tool calls. Koinon's
   `agentd` answers permission requests from the agent's tier policy.
7. agentgateway passed two upstream `503`s from Ollama through as-is; pi retried them, which is
   the harness's job, not the gateway's.
8. The 4B model is the limiting factor (empty or truncated replies, needing explicit tool
   instructions); plumbing worked in every run.
