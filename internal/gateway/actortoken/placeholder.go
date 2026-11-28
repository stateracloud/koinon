// SPDX-License-Identifier: Apache-2.0

package actortoken

// Placeholder is the value a Substrate guest sends in the Koinon-Actor-Token header (ADR 0120,
// amendment for issue #1386), and as its bearer token to the model and MCP listeners (issue #1489:
// the OpenCode harness sets KOINON_MODEL_KEY to it). It is public and the same in every sandbox: it is not a secret and
// it names no actor. atenet-egress, outside the guest, replaces it with the actor's Substrate JWT
// before the request reaches the gateway. The gateway never accepts it as a token:
// SubstrateVerifier.Verify refuses it with ErrPlaceholder, and the Ed25519 Verifier cannot parse it.
const Placeholder = "koinon-actor-placeholder"
