// SPDX-License-Identifier: Apache-2.0

// Package a2aout is the outbound half of Koinon's external A2A edge (ADR 0123 decisions 7 and 8,
// issue 1129). It lets a Koinon agent call an external A2A agent, for example a kagent agent.
//
// The client lives in the gateway, never in the sandbox. It follows the rules of the other
// outbound routes (ADR 0120):
//
//   - It dials through the platform URL policy, checked at dial time (the same transport rule as
//     the code-host clients, issue 657), with no proxy and no redirect.
//   - The credential is read on each request from a [mcpgateway.ScrubbedSource] and set by the
//     HTTP client. Only the credential scrub makes that type, so each value is recorded first. It goes to the host of the target only and only over https. No sandbox, image,
//     home volume, log line or error holds it.
//   - The agent card must offer an interface on the host of the target.
//   - A response is bounded in size, and the text it holds is cut and treated as quoted data.
//
// The client follows A2A 1.0.0 as the inbound edge does: a task that has ended (completed, failed,
// canceled, rejected) never takes a new message. A follow-up is a new task in the same context
// with referenceTaskIds. The client refuses to send to an ended task and to accept a reused task
// ID. It does not stream: it reads the task ([Client.Await]).
package a2aout
