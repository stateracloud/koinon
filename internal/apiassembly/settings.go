// SPDX-License-Identifier: Apache-2.0

package apiassembly

import (
	"time"

	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/sandboxres"
)

// Settings is what Assemble reads from the flags and nothing else. The flag set owns its parsing;
// each field here is a value already split and typed.
type Settings struct {
	// SandboxResources is the platform's sandbox sizing policy (ADR 0083); a per-agent override
	// must stay inside its bounds.
	SandboxResources sandboxres.Policy
	// EgressAllowedHosts are the internal hosts that providers and MCP servers may name.
	EgressAllowedHosts []string
	// Namespace is the namespace of the secrets Koinon reads and writes.
	Namespace string
	// GatewayMCPURL is agentgateway's MCP endpoint as koinon-api reaches it, for approved MCP calls.
	GatewayMCPURL string
	// GatewayURL is agentgateway's LLM listener, for the provider echo check.
	GatewayURL string
	// GitHubAPIURL is the base URL of the GitHub API.
	GitHubAPIURL string
	// WatchPoll is the backstop poll interval of WatchRun.
	WatchPoll time.Duration
	// WatchHeartbeat is how often an idle WatchRun stream sends a heartbeat; 0 keeps the default.
	WatchHeartbeat time.Duration
	// WatchCancelGrace is how long WatchRun waits for late events of a canceled run; 0 keeps the
	// default, a negative value turns the grace off.
	WatchCancelGrace time.Duration
	// TrustedOrigins are extra origins that may make state-changing requests.
	TrustedOrigins []string
	// RestoreHold starts the install held (issue #462): nothing acts until the operator releases it.
	RestoreHold bool

	// GuardByteCap bounds what the MCP leak guard reads of one call (ADR 0091).
	GuardByteCap int
	// The restore status reads these three: where the install is configured to be reached, and
	// whether the declarative removals a restore brought in are accepted (ADR 0110).
	PublicURL, OIDCIssuer, OIDCRedirectURL string
	AcceptImporterDeletions                bool
}

// InputError is a Settings value that no start can use. Setting names it; a caller shows it
// to the operator as a configuration error, not as a failure of the platform.
type InputError struct {
	Setting string
	Err     error
}

// Error is the text of the wrapped error as it is, so the message an operator sees does not change.
func (e *InputError) Error() string { return e.Err.Error() }
func (e *InputError) Unwrap() error { return e.Err }

// Validate reports the first setting that no start can use, as an *InputError.
func (s Settings) Validate() error {
	if err := catalog.ValidateAllowedHosts(s.EgressAllowedHosts); err != nil {
		return &InputError{Setting: "egress allowed hosts", Err: err}
	}
	return nil
}
