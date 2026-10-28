// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/flavor"
)

// Flavor is what the reconciler needs to know about a harness flavor (ADR 0035).
type Flavor struct {
	// Image is the flavor image reference.
	Image string
	// Binaries are the absolute paths of the executables that call agentgateway; only they
	// may use the provider record's credential.
	Binaries []string
}

// DefaultBinaries are the network-calling executables of each flavor Koinon ships
// (flavor.Descriptor.DefaultBinaries).
var DefaultBinaries = func() map[store.AgentFlavor][]string {
	m := map[store.AgentFlavor][]string{}
	for _, d := range flavor.All() {
		m[d.Name] = d.DefaultBinaries
	}
	return m
}()

// GatewayKeyEnvVar holds the credential each sandbox gets: the agent's agentgateway key, as
// the OpenAI-compatible key every flavor reads (flavors/opencode/README.md).
const GatewayKeyEnvVar = "OPENAI_API_KEY"

const (
	keyEnvVar     = GatewayKeyEnvVar
	keyCredential = "api_key"
	profilePrefix = "koinon-agentgateway-"
)

// profile is the workspace-scoped provider profile for one flavor: a bearer credential bound
// to agentgateway's chat completions route. The endpoint is inspected at L7 and enforced
// (ADR 0045), and its stable placeholder lets a rotated key reach running processes without a
// restart (ADR 0048). This profile is also what lets the sandbox reach agentgateway at all:
// OpenShell adds a provider profile's endpoints to the sandbox's network allowlist (spike
// steps 5 and 7), so the sandbox policy itself needs no rule for it.
func (r *Reconciler) profile(flavor store.AgentFlavor, f Flavor) execution.ProviderProfile {
	return GatewayProfile(flavor, f, r.cfg.Gateway, len(r.cfg.ToolServers) > 0)
}

// GatewayProfile is the profile the reconciler renders for flavor, with agentgateway at gw and
// the /mcp route allowed when tools is set. Exported for the escape suite, which attaches the
// same profile to a sandbox on a real gateway.
func GatewayProfile(flavor store.AgentFlavor, f Flavor, gw Endpoint, tools bool) execution.ProviderProfile {
	// One route per model format (ADR 0069 decision 6): chat completions and Messages, the same host,
	// credential and enforcement. The profile is per flavor, so adding a model changes nothing here.
	// /v1/messages/count_tokens stays closed, and so does everything else.
	allow := []execution.L7Rule{
		{Method: "POST", Path: "/v1/chat/completions"},
		{Method: "POST", Path: "/v1/messages"},
	}
	if tools {
		// MCP Streamable HTTP on agentgateway's /mcp route (agentd passes <gateway>/mcp to
		// the harness): POST for messages, GET for the server stream, DELETE to end the
		// session. Which tools are callable is agentgateway's CEL allowlist.
		allow = append(allow,
			execution.L7Rule{Method: "POST", Path: "/mcp"},
			execution.L7Rule{Method: "GET", Path: "/mcp"},
			execution.L7Rule{Method: "DELETE", Path: "/mcp"})
	}
	return execution.ProviderProfile{
		ID:          profilePrefix + string(flavor),
		DisplayName: "agentgateway (" + string(flavor) + ")",
		Description: "Per-agent agentgateway key, rendered by koinon-reconciler",
		Category:    execution.CategoryInference,
		Credentials: []execution.ProfileCredential{{
			Name:       keyCredential,
			EnvVars:    []string{keyEnvVar},
			Required:   true,
			AuthStyle:  "bearer",
			HeaderName: "authorization",
		}},
		Endpoints: []execution.Endpoint{{
			Host:     gw.Host,
			Port:     gw.Port,
			Protocol: "rest",
			Allow:    allow,
		}},
		Binaries: slices.Clone(f.Binaries),
	}
}

// policy is the sandbox policy. The filesystem rules are the ones the harnesses ran under in
// spike step 7 (policies/harness.yaml); /sandbox, the home volume, is the working directory.
// Network access is deny by default: agentgateway is reachable through the provider profile,
// and network holds the grant-derived rules (plan item C2, github.go), enforced like every L7
// endpoint.
func policy(network map[string]execution.NetworkRule) execution.Policy { return SandboxPolicy(network) }

// SandboxPolicy is the policy every agent sandbox gets, with network as its grant-derived
// entries. Exported for the escape suite, which renders the same policy against a real gateway.
func SandboxPolicy(network map[string]execution.NetworkRule) execution.Policy {
	if network == nil {
		network = map[string]execution.NetworkRule{}
	}
	return execution.Policy{
		Filesystem: execution.FilesystemPolicy{
			IncludeWorkdir: true,
			ReadOnly:       []string{"/bin", "/usr", "/lib", "/lib64", "/opt", "/proc", "/dev/urandom", "/etc", "/var/log"},
			ReadWrite:      []string{"/tmp", "/dev/null"},
		},
		Network: network,
	}
}

// sandboxSpec renders the agent's sandbox and the hash of everything in it that cannot change
// in place. It carries no environment and no credential: the key reaches the sandbox only as
// the provider record's placeholder (ADR 0045).
//
// Home volume: OpenShell 0.1.5's Kubernetes driver gives every sandbox a workspace PVC mounted
// at /sandbox, kept while the sandbox is stopped and deleted with it (spike 1.4, 1.5). The
// SandboxTemplate field for custom volume claim templates was removed upstream
// (volume_claim_templates is reserved), so the driver's PVC is the agent's home.
//
// providers are the provider records the sandbox attaches; they are part of the hash, so adding
// or removing the GitHub record of a read grant recreates the sandbox (OpenShell 0.1.5 can attach
// providers at runtime, but Koinon keeps the attached set fixed per sandbox). network are the
// grant-derived policy entries, updated in place.
func (r *Reconciler) sandboxSpec(org, agent pgtype.UUID, names execution.AgentNames, f Flavor, providers []string, network map[string]execution.NetworkRule) (execution.SandboxSpec, string) {
	spec := execution.SandboxSpec{
		Name:             names.Sandbox,
		Image:            f.Image,
		RuntimeClassName: r.cfg.RuntimeClassName,
		Providers:        providers,
		Policy:           policy(network),
		Labels:           ownerLabels(org, agent),
	}
	hash := specHash(spec)
	spec.Labels[LabelSpecHash] = hash
	return spec, hash
}

// specHash covers the fields OpenShell cannot change on an existing sandbox. Network rules are
// left out: they are updated in place.
func specHash(s execution.SandboxSpec) string {
	b, err := json.Marshal(struct {
		Image, RuntimeClassName, CPU, Memory string
		Command, Providers                   []string
		Filesystem                           execution.FilesystemPolicy
		Labels                               map[string]string
	}{
		s.Image, s.RuntimeClassName, s.CPU, s.Memory,
		s.Command, s.Providers, s.Policy.Filesystem, maps.Clone(s.Labels),
	})
	if err != nil {
		panic(err) // plain strings and slices always marshal
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// sandboxState maps an OpenShell phase to the state agentd reads.
func sandboxState(p execution.Phase) store.SandboxState {
	switch p {
	case execution.PhaseProvisioning, execution.PhaseStarting:
		return store.SandboxStateStarting
	case execution.PhaseReady:
		return store.SandboxStateReady
	case execution.PhaseStopping:
		return store.SandboxStateStopping
	case execution.PhaseStopped:
		return store.SandboxStateStopped
	case execution.PhaseDeleting:
		return store.SandboxStateDeleting
	default: // Error, Completed (the main process exited) and Unknown
		return store.SandboxStateError
	}
}
