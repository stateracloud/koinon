// SPDX-License-Identifier: Apache-2.0

// Package a2aedge is the inbound external A2A edge (ADR 0122, issue 1129).
//
// An external A2A client, such as a kagent agent, sends a message to one Koinon agent. The edge
// maps the message to a direct-message thread, which is a task (ADR 0065, ADR 0066), and maps the
// task state back to an A2A task state, with `input-required` for a task that waits on a person.
//
// The edge holds no state of its own. The A2A task ID is the Koinon task ID, and the A2A context ID
// is the same ID. It reuses the direct-message path of the services package, so an agent opts in
// with a `direct_message` trigger, and a client can only reach threads of that agent.
//
// # Authentication
//
// A client sends an org API token as a bearer token, as for the admin API. The token's role must be
// member or higher. The message is written as the user who created the token.
//
// # What it never does
//
// It does not hold or forward a credential for the agent. The sandbox of the agent is not on this
// path. Outbound calls to other A2A agents are a separate slice (ADR 0122).
package a2aedge
