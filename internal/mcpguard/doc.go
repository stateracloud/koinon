// SPDX-License-Identifier: Apache-2.0

// Package mcpguard is the MCP leak guard of ADR 0084 as ADR 0091 rescoped it: the ExtMcp service
// agentgateway calls through mcpGuardrails (failureMode FailClosed) before every tools/call of an
// org's MCP server and after its result.
//
// # The decision (ADR 0091 decision 5)
//
// Decide is the table. A call's sink class comes from the tool's declaration and the call's
// address arguments (internal/sinkclass, read only through classify.go). none and internal pass.
// external-low passes under the ADR 0084 URL rule and its per-host and per-run budgets, and once
// the session holds both legs under a byte cap per run (DefaultByteCap) counted in raw payload
// bytes; over it the call is refused with text the agent reads. external-high with both legs is an
// ADR 0042 approval of that exact call. The text match (internal/leakguard) runs on the payload of
// external calls only, each string on its own and joined in canonical order (object keys sorted,
// recursively; #256), so the order an encoder wrote the keys in decides nothing; a match is an
// approval, on the same card as the
// both-legs one. A tool on an ask-tier target already gets a card from mcpask, which asks Assess
// for the guard's reasons, so the guard lets that call on.
//
// What counts as payload (ADR 0091 decision 7, SplitArgs): every argument but a derived tool's
// address arguments, which decide the class. A fixed tool's address arguments decide nothing, so
// they are payload. Of a fetched URL the path, query and fragment are payload, and to a host the
// org does not trust the host labels too. An address argument with more than one reading (a
// repeated key in the raw JSON, a dotted literal key, another case, a list) is refused
// (ReasonAmbiguous): the guard and the server must read the same address.
//
// The table never escalates by itself: what it cannot pass or refuse is NeedsDecision, with its
// reasons, destination and payload size (Pending), and goes to Options.Decider. The only Decider
// is PersonDecider, which turns every NeedsDecision into that ADR 0042 approval; the seam is where
// another step between the rules and a person would go (ADR 0092). Only a call whose one reason is
// both legs, with no text match, reaches Options.Decider; a match always goes to a person.
//
// # Legs and taint (ADR 0091 decision 4)
//
// A session is one home generation of one agent. Its legs grow from the results of tools whose
// declarations (or, where a tool has none, its server's) say they carry untrusted input or private
// data, and from agentd's feed of the sandbox's own tool results; they never shrink within the
// generation. A result that carries private data is fingerprinted with the session's key into the
// taint store; the text itself is never stored. agentd computes fingerprints in the sandbox's
// supervisor side and sends those only (State.AddFingerprints).
//
// The guard decides an external call only on legs that hold every tool result the harness finished
// before it (ADR 0091 decision 4, amendment 2026-11-03): agentd counts a finished call when it reads
// it and again when it has reported it (Feed.Read, Feed.Done), and the guard waits for the second
// count to catch up, for at most Options.FeedWait; past it the session is taken as holding both legs.
//
// # Failing closed
//
// Every error inside the guard is a refusal, and a result the guard could not record is
// withheld. A decision is bounded by Deadline, which stays under the gateway's fixed processor
// timeout; if the guard cannot be reached the gateway itself fails the call.
//
// Koinon's executor and prober (gateway.IsInternalAgent) and Koinon's own tool servers
// (Options.OwnTargets) are exempt: the executor runs calls a person approved, and every write the
// own servers offer is already an approval of the exact call.
package mcpguard
