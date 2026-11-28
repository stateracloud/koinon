// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"crypto/rand"
	"encoding/hex"
)

// MaxActorNameLength is the longest actor name that Substrate admits: a resource name is a DNS-1123 label (63
// characters), format k8s-short-name in api/ateapi/v1/ateapi.proto (ResourceMetadata.name, Substrate commit
// 20778a6d).
const MaxActorNameLength = 63

// NewActorName implements [execution.ActorNamer]: "<sandbox>-<16 lowercase hex from crypto/rand>" (issues #1386, #1485).
// Substrate signs the JWT of an actor with the actor name, so a name is new for every actor and never reused. The
// suffix has 64 bits, so that a name does not collide with a recently deleted one: the atenet-egress token cache
// is keyed by actor name, and a reused name got the UID of a deleted actor (lab, 2026-11-27). The
// result is longer than [MaxActorNameLength] when the sandbox name is longer than 46 characters; the driver does not
// shorten it, and Substrate then refuses the create with InvalidArgument (the Koinon sandbox names are 19 characters
// or fewer).
func (d *Driver) NewActorName(sandbox string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("substrate: no random bytes: " + err.Error()) // crypto/rand does not fail on a supported platform
	}
	return sandbox + "-" + hex.EncodeToString(b[:])
}
