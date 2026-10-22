// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/procoduck/koinon/internal/db/store"
	"github.com/procoduck/koinon/internal/execution"
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
// (flavors/<flavor>/README.md). OpenShell matches the real path of the executable that opens a
// connection, or of one of its ancestors, never a symbolic link or a script: a script runs as
// its interpreter. Hermes is Python scripts on the image's own Python, whose path names the
// architecture (flavors/hermes/Dockerfile checks it); pi and pi-acp are scripts on Node.
var DefaultBinaries = map[store.AgentFlavor][]string{
	store.AgentFlavorOpencode: {"/usr/local/bin/opencode"},
	store.AgentFlavorHermes: {
		"/opt/hermes/tools/python-3.14.7+20260901-linux-x64/bin/python3.14",
		"/opt/hermes/tools/python-3.14.7+20260901-linux-arm64/bin/python3.14",
	},
	store.AgentFlavorPi: {"/opt/node/bin/node"},
}

// The credential each sandbox gets: the agent's agentgateway key, as the OpenAI-compatible
// key every flavor reads (flavors/opencode/README.md).
const (
	keyEnvVar     = "OPENAI_API_KEY"
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
			Host:     r.cfg.Gateway.Host,
			Port:     r.cfg.Gateway.Port,
			Protocol: "rest",
			Allow:    []execution.L7Rule{{Method: "POST", Path: "/v1/chat/completions"}},
		}},
		Binaries: slices.Clone(f.Binaries),
	}
}

// policy is the sandbox policy. The filesystem rules are the ones the harnesses ran under in
// spike step 7 (policies/harness.yaml); /sandbox, the home volume, is the working directory.
// Network access is deny by default: agentgateway is reachable through the provider profile,
// and grant-derived rules (plan item C2) will be added here, enforced like every L7 endpoint.
func policy() execution.Policy {
	return execution.Policy{
		Filesystem: execution.FilesystemPolicy{
			IncludeWorkdir: true,
			ReadOnly:       []string{"/bin", "/usr", "/lib", "/lib64", "/opt", "/proc", "/dev/urandom", "/etc", "/var/log"},
			ReadWrite:      []string{"/tmp", "/dev/null"},
		},
		Network: map[string]execution.NetworkRule{},
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
func (r *Reconciler) sandboxSpec(org, agent pgtype.UUID, names execution.AgentNames, f Flavor) (execution.SandboxSpec, string) {
	spec := execution.SandboxSpec{
		Name:             names.Sandbox,
		Image:            f.Image,
		RuntimeClassName: r.cfg.RuntimeClassName,
		Providers:        []string{names.Provider},
		Policy:           policy(),
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
