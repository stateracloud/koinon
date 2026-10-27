// SPDX-License-Identifier: Apache-2.0

package checkpoint

import "github.com/stateracloud/koinon/internal/db/store"

// Ephemeral reports whether the agent is ephemeral: it has no home that outlives a run, so it is
// not checkpointed and a recreation needs no backup (ADR 0065).
//
// TODO(EP1): ephemeral agents land with plan item EP1 (agentconfig.IsEphemeral). Until then no
// agent is ephemeral; call agentconfig.IsEphemeral here when it exists.
func Ephemeral(store.Agent) bool { return false }
