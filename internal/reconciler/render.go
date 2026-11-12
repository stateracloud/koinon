// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/agentresources"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/sandboxres"
	"github.com/stateracloud/koinon/templates"
)

// Harness is what the reconciler needs to know about a harness (ADR 0035).
type Harness struct {
	// Image is the harness image reference.
	Image string
	// Binaries are the absolute paths of the executables that call agentgateway; only they
	// may use the provider record's credential.
	Binaries []string
}

// rendered reports whether the reconciler renders the harness: it has an image and binaries. Only a
// rendered harness gets a profile (koinon-agentgateway-<harness>), so only its profile is claimed.
func (f Harness) rendered() bool { return f.Image != "" && len(f.Binaries) > 0 }

// DefaultBinaries are the network-calling executables of each harness Koinon ships
// (harness.Descriptor.DefaultBinaries).
var DefaultBinaries = func() map[string][]string {
	m := map[string][]string{}
	for _, d := range harness.All() {
		m[d.Name] = d.DefaultBinaries
	}
	return m
}()

// GatewayKeyEnvVar holds the credential each sandbox gets: the agent's agentgateway key, as
// the OpenAI-compatible key every harness reads (harnesses/opencode/README.md).
const GatewayKeyEnvVar = "OPENAI_API_KEY"

const (
	keyEnvVar     = GatewayKeyEnvVar
	keyCredential = "api_key"
	profilePrefix = "koinon-agentgateway-"
)

// profile is the workspace-scoped provider profile for one harness: a bearer credential bound
// to agentgateway's chat completions route. The endpoint is inspected at L7 and enforced
// (ADR 0045), and its stable placeholder lets a rotated key reach running processes without a
// restart (ADR 0048). This profile is also what lets the sandbox reach agentgateway at all:
// OpenShell adds a provider profile's endpoints to the sandbox's network allowlist (spike
// steps 5 and 7), so the sandbox policy itself needs no rule for it.
func (r *Reconciler) profile(harness string, f Harness) execution.ProviderProfile {
	return GatewayProfile(harness, f, r.cfg.Gateway, len(r.cfg.ToolServers) > 0)
}

// GatewayProfile is the profile the reconciler renders for harness, with agentgateway at gw and
// the /mcp route allowed when tools is set. Exported for the escape suite, which attaches the
// same profile to a sandbox on a real gateway.
func GatewayProfile(harness string, f Harness, gw Endpoint, tools bool) execution.ProviderProfile {
	// One route per model format (ADR 0069 decision 6), from the same table the gateway renders:
	// the same host, credential and enforcement. The profile is per harness, so adding a model changes
	// nothing here. Anything else on the gateway (/v1/messages/count_tokens included) stays closed.
	var allow []execution.L7Rule
	for _, p := range gateway.RoutePaths() {
		allow = append(allow, execution.L7Rule{Method: "POST", Path: p})
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
		ID:          profilePrefix + harness,
		DisplayName: "agentgateway (" + harness + ")",
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
// grant-derived policy entries, updated in place. res are the effective resources (sandboxResources);
// they are part of the hash, so a changed template or platform setting recreates the sandbox, as a
// changed image does (ADR 0083).
func (r *Reconciler) sandboxSpec(org, agent pgtype.UUID, names execution.AgentNames, f Harness, providers []string, network map[string]execution.NetworkRule, res templates.Resources) (execution.SandboxSpec, string) {
	spec := execution.SandboxSpec{
		Name:             names.Sandbox,
		Image:            f.Image,
		RuntimeClassName: r.cfg.RuntimeClassName,
		Providers:        providers,
		Policy:           policy(network),
		Labels:           ownerLabels(org, agent),
		CPURequest:       res.CPU.Request,
		CPULimit:         res.CPU.Limit,
		MemoryRequest:    res.Memory.Request,
		MemoryLimit:      res.Memory.Limit,
	}
	hash := specHash(spec)
	spec.Labels[LabelSpecHash] = hash
	// The supervisor image is not in the hash: an unknown or unreadable image must not stop the
	// other spec changes from recreating a sandbox. It has its own label, compared separately.
	if img, known := r.supervisorImage(); known {
		spec.Labels[LabelSupervisorImage] = supervisorDigest(img)
	}
	return spec, hash
}

// specHash covers the fields OpenShell cannot change on an existing sandbox. Network rules are
// left out: they are updated in place. The OpenShell supervisor image (ADR 0085 decision 9) is
// left out too: it has its own label (LabelSupervisorImage), so an image that cannot be read never
// hides a change in the fields above, and the hashes of existing sandboxes stay valid.
func specHash(s execution.SandboxSpec) string {
	b, err := json.Marshal(struct {
		Image, RuntimeClassName    string
		CPURequest, CPULimit       string
		MemoryRequest, MemoryLimit string
		Command, Providers         []string
		Filesystem                 execution.FilesystemPolicy
		Labels                     map[string]string
	}{
		s.Image, s.RuntimeClassName,
		s.CPURequest, s.CPULimit, s.MemoryRequest, s.MemoryLimit,
		s.Command, s.Providers, s.Policy.Filesystem, maps.Clone(s.Labels),
	})
	if err != nil {
		panic(err) // plain strings and slices always marshal
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// sandboxResources is the agent's effective sandbox size: the agent's own override, else its
// version's template values, else the platform default, clamped to the platform bounds (sandboxres;
// ADR 0083). The bounds are applied again here although the API checked the override, because the
// bounds can change after it was stored. A clamp is logged once per change, not once per pass.
func (r *Reconciler) sandboxResources(w *work) templates.Resources {
	res, clamps := r.cfg.SandboxResources.Effective(sandboxres.Overlay(w.cfg.Resources, agentresources.OverrideOf(w.agent)))
	id := execution.UUIDString(w.agent.ID.Bytes)
	var msg string
	for _, c := range clamps {
		msg += c.String() + "; "
	}
	r.clampMu.Lock()
	defer r.clampMu.Unlock()
	if r.clampLogged == nil {
		r.clampLogged = map[string]string{}
	}
	if r.clampLogged[id] != msg {
		if msg == "" {
			delete(r.clampLogged, id)
		} else {
			r.clampLogged[id] = msg
			r.log.Warn("the sandbox resources were clamped to the platform bounds", "org", w.org.Slug, "agent", id, "clamped", msg)
		}
	}
	return res
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
