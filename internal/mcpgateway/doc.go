// SPDX-License-Identifier: Apache-2.0

// Package mcpgateway is the MCP path of Koinon's own gateway (ADR 0097 decision 5, issue 278).
//
// It is an aggregating MCP server on the official Go SDK. An agent connects to it with its gateway
// key. The gateway answers tools/list from the tools of every target the agent may use, and sends
// tools/call to the target that owns the tool. It holds one SDK client per upstream target and
// agent.
//
// # The guard sees every message
//
// Each upstream client connects through [guardTransport]. It wraps the SDK [mcp.Connection], the
// pattern of the SDK's LoggingTransport, so every raw JSON-RPC message passes the [Guard] before the
// SDK dispatches it: results, errors, notifications and server-initiated requests, and the requests
// the gateway sends. agentgateway's guard hook sees only the successful replies of tools/call (issue
// 254).
//
// # What the gateway refuses
//
//   - A method that is not on the allowlist ([AllowedMethods]) is refused, in both directions.
//   - The gateway advertises no sampling and no elicitation to an upstream server. If one arrives,
//     the gateway refuses it with a JSON-RPC error and reports it to the guard as a violation, so
//     the guard can taint the run. roots/list is answered by the gateway with one fixed folder.
//   - A message the guard refuses never reaches the agent: a result or error becomes a JSON-RPC
//     error with the same ID; a notification is dropped.
//
// # Stateless
//
// The server runs the SDK's stateless Streamable HTTP mode: no session affinity, and a replica
// restart drops nothing. A changed tool list shows on the next call.
//
// # What it never records
//
// A span and an audit row for each call name the target, the tool, the guard verdict and the kind of
// credential. They never carry arguments, results or token values, and a metric label never names
// an org, an agent, a user or a run (ADR 0023).
package mcpgateway
