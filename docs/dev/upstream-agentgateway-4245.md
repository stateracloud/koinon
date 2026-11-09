<!-- SPDX-License-Identifier: Apache-2.0 -->

# Draft for upstream: agentgateway#4245 (a human posts it, in their own words)

This is a draft for a human to review, rewrite in their own voice and post on the agentgateway
issue tracker (AGENTS.md, upstream rules). It describes an agentgateway deployment only. Nobody posts it
on a human's behalf, and it carries no tool attribution. Koinon's side of the problem is closed by
ADR 0097 decision 4, step 1 (the interim fix, tracked in #269, removable when the step 2 gateway
refuses these messages itself).

---

**Title:** MCP response hook is not called for error replies, notifications and server-initiated requests

**Who this is for.** I run agentgateway v1.7.0 in front of MCP servers that I do not control, and I
use an external processor (`ext_proc`-style MCP extension) to decide, from every reply, what an agent
may do next (for example, to stop an agent that has read private data from posting it elsewhere). The
extension is only useful if it sees everything the server sends back to the client.

**What happens.** The response hook is called for a successful `tools/call` result. It is not called for:

1. a JSON-RPC error reply to a `tools/call` (the `error` member, with text the server controls);
2. server-to-client notifications on the call's stream;
3. server-initiated requests on the call's stream (`sampling/createMessage`, `elicitation/create` in
   form and URL mode, `roots/list`).

All of them reach the client unchanged. The extension never learns they exist, so it cannot refuse,
redact or even record them.

**Repro (agentgateway only).**

1. Configure an MCP backend with a policy that calls an external MCP extension on responses; make the
   extension log every call it receives.
2. Point the backend at a small MCP server that, on `tools/call`, (a) replies with
   `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"canary-4245"}}`;
   on a second tool it first sends a `sampling/createMessage` request on the stream and then a result.
3. Call both tools through the gateway with any MCP client.
4. The client receives the error text and the sampling request. The extension's log shows no call for
   either; it shows calls only for results.

**Expected.** Every JSON-RPC message the gateway forwards to the client passes through the response
hook, with its method (or `error`) and its JSON-RPC id, or the gateway offers a setting to refuse
what it cannot show the hook. The id matters: without it a hook cannot tell which of several in-flight
calls a reply belongs to.

**Pointer.** In v1.7.0 the hook is applied in the success path of the response handling
(`handler.rs`, around lines 2183-2208); the error and server-initiated branches return before it.

**Why it matters.** Anyone using the hook as a security control (data-loss prevention, taint tracking,
prompt-injection screening) has a silent hole: an attacker-controlled server can put its payload in an
error message or a server-initiated request.
