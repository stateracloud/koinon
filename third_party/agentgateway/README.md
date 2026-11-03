# agentgateway ExtMcp proto (vendored)

The wire protocol of agentgateway's MCP guardrails (`mcpGuardrails`, a remote processor), used to
generate the gRPC server of koinon-api's MCP guard in `internal/gen/agentgateway/extmcp`
(`internal/mcpguard`, ADR 0084 and ADR 0091).

- Upstream: https://github.com/agentgateway/agentgateway
- Tag: `v1.7.0`, commit `26f5aebe99a36a037fe54f8080a608aca5547e15` (2026-10-27), the gateway
  version the chart deploys
- Licence: Apache-2.0; see [LICENSE](LICENSE). The file has no SPDX header upstream and is kept
  unmodified.
- File: `proto/ext_mcp.proto`, copied unmodified from upstream `crates/protos/proto/`. It is the
  same protocol the spike in `hack/spike/extmcp` ran against.

The upstream `go_package` is replaced with an `M` option in `buf.gen.agentgateway.yaml`. The file
is not part of the buf workspace in `buf.yaml`, so `buf lint` and `buf breaking` check only
Koinon's own protos (the package keeps agentgateway's name, `agentgateway.dev.ext_mcp`, which the
gateway calls).

Upstream facts the guard relies on (read in `crates/agentgateway/src/mcp/guardrails/client.rs` at
the tag): the processor's request timeout is fixed at 10 s, and a remote processor that fails or
times out with `failureMode: FailClosed` fails the tool call with JSON-RPC error -32603.

## Updating

1. Copy the file from the new tag, unmodified, and update the tag and commit above.
2. Run `make generate`, then `go test ./internal/mcpguard/...`.
3. Check the processor timeout in `client.rs` still exceeds `mcpguard.Deadline` (a spec asserts
   the constant this README names).
