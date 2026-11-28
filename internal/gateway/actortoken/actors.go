// SPDX-License-Identifier: Apache-2.0

package actortoken

// SubstrateActor is the identity of one Agent Substrate actor: its workspace (atespace), its name and the
// UID that Substrate gave it. Koinon's State maps it to the Actor (org, agent, run) the actor works for
// (issue #1386). The UID makes a key unique when a name comes back for a new actor.
type SubstrateActor struct {
	Atespace, Name, UID string
}

// Refusals of the gateway's identity check that come after or before the verifier. Each matches
// ErrInvalid with errors.Is, and Reason gives its fixed text.
var (
	// ErrUnknownActor means the token verified but Koinon's record (the State) has no actor with its
	// workspace, name and UID: an unknown actor, a UID that is not the recorded one, or an actor whose
	// sandbox is deleted or archived (ADR 0120, amendment for issue #1386, item 5).
	ErrUnknownActor error = &reasonError{"unknown_actor"}
	// ErrMultipleTokens means the request carried more than one Koinon-Actor-Token value (item 4).
	ErrMultipleTokens error = &reasonError{"multiple_tokens"}
)

// Key returns the identity that a checked Substrate token names.
func (c SubstrateClaims) Key() SubstrateActor {
	return SubstrateActor{Atespace: c.Atespace, Name: c.ActorName, UID: c.ActorUID}
}
