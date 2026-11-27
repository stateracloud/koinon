// SPDX-License-Identifier: Apache-2.0

package store

// Written by hand, next to the generated model.

// ActorRef is the name that every execution driver call uses for the sandbox: the actor name when one is
// recorded (Agent Substrate, issue #1386), else the sandbox name (OpenShell, and a Substrate sandbox whose
// actor is not named yet).
func (s AgentSandbox) ActorRef() string {
	if s.ActorName.Valid && s.ActorName.String != "" {
		return s.ActorName.String
	}
	return s.SandboxName
}
