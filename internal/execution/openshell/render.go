// SPDX-License-Identifier: Apache-2.0

package openshell

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/stateracloud/koinon/internal/execution"
	sandboxv1 "github.com/stateracloud/koinon/internal/gen/openshell/sandbox/v1"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
)

// Hardening defaults rendered into every policy (ADR 0045, ADR 0048).
const (
	policyVersion         = 1
	landlockHard          = "hard_requirement"
	reservedAnnotationKey = "internal.openshell.ai/"
)

func renderPolicy(p execution.Policy) (*sandboxv1.SandboxPolicy, error) {
	out := &sandboxv1.SandboxPolicy{
		Version: policyVersion,
		Filesystem: &sandboxv1.FilesystemPolicy{
			IncludeWorkdir: p.Filesystem.IncludeWorkdir,
			ReadOnly:       slices.Clone(p.Filesystem.ReadOnly),
			ReadWrite:      slices.Clone(p.Filesystem.ReadWrite),
		},
		Landlock: &sandboxv1.LandlockPolicy{Compatibility: landlockHard},
	}
	if len(p.Network) > 0 {
		out.NetworkPolicies = make(map[string]*sandboxv1.NetworkPolicyRule, len(p.Network))
	}
	for _, name := range slices.Sorted(maps.Keys(p.Network)) {
		rule := p.Network[name]
		if name == "" {
			return nil, errors.New("network rule without a name")
		}
		endpoints, err := renderEndpoints(rule.Endpoints)
		if err != nil {
			return nil, fmt.Errorf("network rule %q: %w", name, err)
		}
		out.NetworkPolicies[name] = &sandboxv1.NetworkPolicyRule{
			Name:      name,
			Endpoints: endpoints,
			Binaries:  renderBinaries(rule.Binaries),
		}
	}
	return out, nil
}

func renderEndpoints(in []execution.Endpoint) ([]*sandboxv1.NetworkEndpoint, error) {
	out := make([]*sandboxv1.NetworkEndpoint, 0, len(in))
	for _, e := range in {
		if e.Host == "" || e.Port == 0 {
			return nil, errors.New("endpoint needs a host and a port")
		}
		// Defence in depth: the API refuses wildcard hosts where they enter, and nothing
		// below it may render one (ADR 0075, decision 13).
		if err := execution.CheckEgressHost(e.Host); err != nil {
			return nil, err
		}
		access, err := renderAccess(e.Access)
		if err != nil {
			return nil, err
		}
		if e.Protocol == "" && (access != sandboxv1.NetworkAccessPreset_NETWORK_ACCESS_PRESET_UNSPECIFIED || len(e.Allow) > 0 || len(e.Deny) > 0 || e.Path != "") {
			return nil, fmt.Errorf("endpoint %s:%d: L7 rules need a protocol", e.Host, e.Port)
		}
		ep := &sandboxv1.NetworkEndpoint{
			Host:     e.Host,
			Port:     e.Port,
			Protocol: e.Protocol,
			Path:     e.Path,
			Access:   access,
			// TLS stays on auto detection and credentials are never sent uninspected:
			// those fields keep their safe zero values on purpose (ADR 0045).
		}
		if e.Protocol != "" {
			// Inspected endpoints are enforced, never audited (ADR 0045). OpenShell
			// rejects an enforcement mode on an L4-only endpoint.
			ep.Enforcement = sandboxv1.NetworkEnforcementMode_NETWORK_ENFORCEMENT_MODE_ENFORCE
		}
		for _, r := range e.Allow {
			ep.Rules = append(ep.Rules, &sandboxv1.L7Rule{Allow: &sandboxv1.L7Allow{Method: r.Method, Path: r.Path, Query: renderQuery(r.Query)}})
		}
		for _, r := range e.Deny {
			ep.DenyRules = append(ep.DenyRules, &sandboxv1.L7DenyRule{Method: r.Method, Path: r.Path, Query: renderQuery(r.Query)})
		}
		out = append(out, ep)
	}
	return out, nil
}

func renderQuery(q map[string]string) map[string]*sandboxv1.L7QueryMatcher {
	if len(q) == 0 {
		return nil
	}
	out := make(map[string]*sandboxv1.L7QueryMatcher, len(q))
	for k, v := range q {
		out[k] = &sandboxv1.L7QueryMatcher{Glob: v}
	}
	return out
}

func renderAccess(a execution.Access) (sandboxv1.NetworkAccessPreset, error) {
	switch a {
	case execution.AccessNone:
		return sandboxv1.NetworkAccessPreset_NETWORK_ACCESS_PRESET_UNSPECIFIED, nil
	case execution.AccessReadOnly:
		return sandboxv1.NetworkAccessPreset_NETWORK_ACCESS_PRESET_READ_ONLY, nil
	case execution.AccessReadWrite:
		return sandboxv1.NetworkAccessPreset_NETWORK_ACCESS_PRESET_READ_WRITE, nil
	case execution.AccessFull:
		return sandboxv1.NetworkAccessPreset_NETWORK_ACCESS_PRESET_FULL, nil
	}
	return 0, fmt.Errorf("unknown access preset %q", a)
}

func renderBinaries(paths []string) []*sandboxv1.NetworkBinary {
	out := make([]*sandboxv1.NetworkBinary, 0, len(paths))
	for _, p := range paths {
		out = append(out, &sandboxv1.NetworkBinary{Path: p})
	}
	return out
}

func renderProfile(p execution.ProviderProfile) (*openshellv1.ProviderProfile, error) {
	if p.ID == "" {
		return nil, errors.New("provider profile needs an ID")
	}
	category, err := renderCategory(p.Category)
	if err != nil {
		return nil, err
	}
	endpoints, err := renderEndpoints(p.Endpoints)
	if err != nil {
		return nil, fmt.Errorf("profile %q: %w", p.ID, err)
	}
	out := &openshellv1.ProviderProfile{
		Id:          p.ID,
		DisplayName: p.DisplayName,
		Description: p.Description,
		Category:    category,
		Endpoints:   endpoints,
		Binaries:    renderBinaries(p.Binaries),
	}
	for _, c := range p.Credentials {
		if c.Name == "" || len(c.EnvVars) == 0 {
			return nil, fmt.Errorf("profile %q: credential needs a name and an environment variable", p.ID)
		}
		out.Credentials = append(out.Credentials, &openshellv1.ProviderProfileCredential{
			Name:       c.Name,
			EnvVars:    slices.Clone(c.EnvVars),
			Required:   c.Required,
			AuthStyle:  c.AuthStyle,
			HeaderName: c.HeaderName,
			// Rotation without a sandbox restart needs a stable placeholder (ADR 0048, 2.7).
			StablePlaceholder: true,
		})
	}
	return out, nil
}

func renderCategory(c execution.ProfileCategory) (openshellv1.ProviderProfileCategory, error) {
	switch c {
	case "", execution.CategoryOther:
		return openshellv1.ProviderProfileCategory_PROVIDER_PROFILE_CATEGORY_OTHER, nil
	case execution.CategoryInference:
		return openshellv1.ProviderProfileCategory_PROVIDER_PROFILE_CATEGORY_INFERENCE, nil
	case execution.CategorySourceControl:
		return openshellv1.ProviderProfileCategory_PROVIDER_PROFILE_CATEGORY_SOURCE_CONTROL, nil
	}
	return 0, fmt.Errorf("unknown profile category %q", c)
}

func renderSandbox(workspace string, s execution.SandboxSpec) (*openshellv1.CreateSandboxRequest, error) {
	if s.Name == "" || s.Image == "" {
		return nil, errors.New("sandbox needs a name and an image")
	}
	for k := range s.Labels {
		if strings.HasPrefix(k, reservedAnnotationKey) {
			return nil, fmt.Errorf("label %q uses a prefix reserved by OpenShell", k)
		}
	}
	policy, err := renderPolicy(s.Policy)
	if err != nil {
		return nil, err
	}
	tmpl := &openshellv1.SandboxTemplate{Image: s.Image, RuntimeClassName: s.RuntimeClassName}
	if res := resourcesStruct(s); len(res) > 0 {
		st, err := structpb.NewStruct(res)
		if err != nil {
			return nil, err
		}
		tmpl.Resources = st
	}
	return &openshellv1.CreateSandboxRequest{
		WorkspaceScope: scope(workspace),
		Name:           s.Name,
		Labels:         maps.Clone(s.Labels),
		Spec: &openshellv1.SandboxSpec{
			Template:  tmpl,
			Policy:    policy,
			Providers: slices.Clone(s.Providers),
			Command:   slices.Clone(s.Command),
		},
	}, nil
}

var phases = map[openshellv1.SandboxPhase]execution.Phase{
	openshellv1.SandboxPhase_SANDBOX_PHASE_PROVISIONING: execution.PhaseProvisioning,
	openshellv1.SandboxPhase_SANDBOX_PHASE_READY:        execution.PhaseReady,
	openshellv1.SandboxPhase_SANDBOX_PHASE_ERROR:        execution.PhaseError,
	openshellv1.SandboxPhase_SANDBOX_PHASE_DELETING:     execution.PhaseDeleting,
	openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPING:     execution.PhaseStopping,
	openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED:      execution.PhaseStopped,
	openshellv1.SandboxPhase_SANDBOX_PHASE_STARTING:     execution.PhaseStarting,
	openshellv1.SandboxPhase_SANDBOX_PHASE_COMPLETED:    execution.PhaseCompleted,
}

func sandboxFromProto(s *openshellv1.Sandbox) execution.Sandbox {
	phase, ok := phases[s.GetStatus().GetPhase()]
	if !ok {
		phase = execution.PhaseUnknown
	}
	out := execution.Sandbox{
		Name:          s.GetMetadata().GetName(),
		ID:            s.GetMetadata().GetId(),
		Workspace:     s.GetMetadata().GetWorkspace(),
		Labels:        s.GetMetadata().GetLabels(),
		Phase:         phase,
		PolicyVersion: s.GetStatus().GetCurrentPolicyVersion(),
		Enforcement:   enforcementOf(s.GetSpec().GetPolicy()),
	}
	if s.GetStatus().ExitCode != nil {
		code := s.GetStatus().GetExitCode()
		out.ExitCode = &code
	}
	for _, c := range s.GetStatus().GetConditions() {
		if c.GetStatus() != "True" {
			out.Reason, out.Message = c.GetReason(), c.GetMessage()
			break
		}
	}
	return out
}

// resourcesStruct is SandboxTemplate.resources: {requests: {cpu, memory}, limits: {cpu, memory}},
// with the empty values and the empty maps left out (ADR 0083).
func resourcesStruct(s execution.SandboxSpec) map[string]any {
	out := map[string]any{}
	for key, v := range map[string]map[string]string{
		"requests": {"cpu": s.CPURequest, "memory": s.MemoryRequest},
		"limits":   {"cpu": s.CPULimit, "memory": s.MemoryLimit},
	} {
		m := map[string]any{}
		for name, q := range v {
			if q != "" {
				m[name] = q
			}
		}
		if len(m) > 0 {
			out[key] = m
		}
	}
	return out
}

// enforcementOf reads the enforcement mode of the inspected endpoints of a sandbox policy as the
// gateway reports it (execution.Enforcement*): enforce when every endpoint with a protocol is
// enforced (L4-only endpoints are allow-lists with no audit mode, so they count as enforced), audit
// when any is not, unknown without a policy.
func enforcementOf(p *sandboxv1.SandboxPolicy) string {
	if p == nil {
		return execution.EnforcementUnknown
	}
	for _, rule := range p.GetNetworkPolicies() {
		for _, ep := range rule.GetEndpoints() {
			if ep.GetProtocol() == "" {
				continue // L4 only: an allow-list OpenShell always enforces; it has no audit mode
			}
			if ep.GetEnforcement() != sandboxv1.NetworkEnforcementMode_NETWORK_ENFORCEMENT_MODE_ENFORCE {
				return execution.EnforcementAudit
			}
		}
	}
	return execution.EnforcementEnforce
}
