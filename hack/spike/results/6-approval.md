# Step 6: approval routing (ask tier)

Run 2026-10-21. `hack/spike/approver` (Go, `github.com/modelcontextprotocol/go-sdk` v1.8.0, image
`koinon/spike-approver:1`) runs in `koinon-spike`: MCP on 8080 (NetworkPolicy: ingress only from
`agentgateway-system`), admin on 9090 (no in-cluster ingress; used through `kubectl port-forward`).
It exposes `create_pull_request` (parks: records the request, returns at once),
`get_approval_status`, and `direct_create_pull_request`, a stand-in for an upstream tool that
would write without approval. agentgateway serves it on `:3000/mcp` with the same per-agent key
hashes and `mcpAuthorization` CEL rules allowing only the first two tools. The OpenShell profile
`hack/spike/profiles/agentgateway-spike.yaml` adds a `protocol: mcp` endpoint for `:3000/mcp` that
allows only those two tool names. The agent in `p2` uses `hack/spike/probe/mcp.sh` (curl, MCP
Streamable HTTP) with its placeholder key.

| # | Result | Evidence |
| --- | --- | --- |
| 6.1 | pass | `tools/list` through agentgateway returns `[create_pull_request, get_approval_status]`. Calling `direct_create_pull_request` from the sandbox: OpenShell `403 policy_denied` ("MCP tool call has no matching allow rule"); from the Mac straight to agentgateway (no OpenShell): `-32602 Unknown tool`. The stub never logged `BYPASS TOOL REACHED`. No key: `401`. Direct Gitea PR creation stays denied (3.2) |
| 6.2 | pass | the call returned `approval requested: id=8ef58da115b2502a status=pending` in 118 ms; stub log: tool, full arguments, `args_sha256`, target `gitea:koinon-spike/hello` |
| 6.3 | pass | approving `{id, args_sha256}` created exactly PR #1 (`spike-approval → main`, author `koinon-spike-bot`); replaying the approval returned the same PR with `executions: 1`; Gitea lists one PR. The agent then reads `status=executed pr=…/pulls/1` through `get_approval_status` (the resume path) |
| 6.4 | pass | approving with the hash of changed arguments (base `release`) → `409 approval does not match the requested arguments`. The agent asking again with base `release` creates a separate pending request (`a6f1c34a5d2c32b8`), which nobody approved |
| 6.5 | CEL: works; ExtMCP: not tried | CEL `mcpAuthorization` both filters `tools/list` and rejects calls. ExtMCP (`mcp.policies.mcpGuardrails`, an external gRPC processor) was not built within the time box; the park-and-execute design (ADR 0042) does not hold MCP calls open, so call duration limits were not measured |

## Findings

1. **Two independent tool gates.** OpenShell's `protocol: mcp` endpoint allowlists tool names
   inside the sandbox boundary; agentgateway's CEL hides and rejects tools at the gateway. Either
   alone blocks the bypass tool. Neither matches tool *arguments* (OpenShell says so explicitly),
   which is why the approval binds to an argument hash in Koinon, not in policy.
2. **OpenShell pins MCP revisions.** With no `mcp.versions`, only the newest revision
   (`2025-11-25`) is accepted; a `2025-06-18` client gets `403 mcp_protocol_version_not_allowed`
   after `initialize`. Harness MCP clients must speak a permitted revision, or Koinon lists the
   revisions it allows.
3. **Approval binding works as designed** with a content hash over tool name plus canonical
   arguments as the request id; idempotency comes from the request id plus a pre-create lookup
   (Gitea has no idempotency-key header).
4. The bot token for execution lives only in the approver's Secret (from OpenBao) and never in the
   sandbox; the agent's only credential is its agentgateway key.
