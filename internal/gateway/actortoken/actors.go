// SPDX-License-Identifier: Apache-2.0

package actortoken

// SubstrateActor is the identity of one Agent Substrate actor: its workspace (atespace), its name and the
// UID that Substrate gave it. Koinon's State maps it to the Actor (org, agent, run) the actor works for
// (issue #1386). The UID makes a key unique when a name comes back for a new actor.
type SubstrateActor struct {
	Atespace, Name, UID string
}

// Key returns the identity that a checked Substrate token names.
func (c SubstrateClaims) Key() SubstrateActor {
	return SubstrateActor{Atespace: c.Atespace, Name: c.ActorName, UID: c.ActorUID}
}
