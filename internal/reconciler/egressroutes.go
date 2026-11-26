// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/stateracloud/koinon/connectors"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/egress"
	"github.com/stateracloud/koinon/internal/gateway/route"
	harnessdesc "github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/reconciler/gatewayplan"
)

// The gateway's org egress routes (#1086 part B, ADR 0097 step 2) come from the same rules as the
// sandbox's OpenShell network rules: the agent's code-host grant and its npm plugin rule. The
// OpenShell rules stay in force as a second layer.

// EgressRoutes returns the org egress routes of one active agent: ref names it at the gateway
// (gateway.AgentRef, Name is the agent ID; the actor token's agent claim must carry the same value).
// forge are the enforced endpoints of its code-host grant (GitHubRead.Endpoints, nil without a
// grant), and registry is set when its version turns plugin installs on.
//
// The forge routes name the agent's code-host credential. Until the gateway can attach it (#1086
// part C), a request on such a route is refused with 503 credential_unavailable: the routes are
// enforced, never open without the rule. A rule that the route model cannot express exactly is an
// error, and then the agent gets no route (fail closed, ADR 0120 2.2).
func EgressRoutes(ref gateway.AgentRef, forge []execution.Endpoint, registry bool) ([]route.Route, error) {
	if ref.Org == "" || ref.Name == "" {
		return nil, fmt.Errorf("egress routes: the agent ref %q/%q is incomplete", ref.Org, ref.Name)
	}
	prefix := "org/" + ref.Org + "/agent/" + ref.Name + "/"
	var out []route.Route
	add := func(kind string, class route.Class, credential string, eps []execution.Endpoint) error {
		for i, ep := range eps {
			rs, err := endpointRoutes(ep, class)
			if err != nil {
				return fmt.Errorf("egress routes: %s endpoint %d (%s): %w", kind, i, ep.Host, err)
			}
			for j := range rs {
				rs[j].Name = fmt.Sprintf("%s%s-%d-%d", prefix, kind, i, j)
				rs[j].Org, rs[j].Agent = ref.Org, ref.Name
				if !rs[j].Deny {
					rs[j].Credential = credential
				}
			}
			out = append(out, rs...)
		}
		return nil
	}
	if err := add("forge", route.ClassForge, CodeHostCredentialName(ref), forge); err != nil {
		return nil, err
	}
	if registry {
		if err := add("registry", route.ClassRegistry, "", npmEndpoints()); err != nil {
			return nil, err
		}
	}
	// The set must compile on its own, so one agent's fault never refuses the other agents' routes
	// at the gateway (egress.Merge refuses the whole set).
	if _, err := route.Compile(out); err != nil {
		return nil, fmt.Errorf("egress routes: %w", err)
	}
	return out, nil
}

// CodeHostCredentialName is the name of the credential the gateway attaches on an agent's forge
// routes. It is a name, never a value; #1086 part C resolves it in the gateway.
func CodeHostCredentialName(ref gateway.AgentRef) string {
	return "org/" + ref.Org + "/agent/" + ref.Name + "/codehost"
}

// PluginInstallsOn reports whether cfg turns plugin installs on for a harness that has the option.
// It is PluginInstallRule's condition without the harness binaries, which only the sandbox rule
// needs.
func PluginInstallsOn(cfg agentconfig.Config, harness string) bool {
	return cfg.Harness.PluginInstalls && harnessdesc.SupportsPluginInstalls(harness)
}

func npmEndpoints() []execution.Endpoint {
	return []execution.Endpoint{{
		Host: NPMRegistryHost, Port: 443, Protocol: "rest",
		Allow: []execution.L7Rule{{Method: "GET", Path: "/**"}, {Method: "HEAD", Path: "/**"}},
	}}
}

// endpointRoutes translates one enforced endpoint: one route per allow rule and one deny route per
// deny rule. Only the forms the route model matches exactly are taken.
func endpointRoutes(ep execution.Endpoint, class route.Class) ([]route.Route, error) {
	switch {
	case ep.Protocol != "rest":
		return nil, fmt.Errorf("protocol %q is not an enforced REST endpoint", ep.Protocol)
	case ep.Access != execution.AccessNone:
		return nil, fmt.Errorf("the access preset %q has no exact route form", ep.Access)
	case ep.Path != "":
		return nil, fmt.Errorf("an endpoint path %q has no exact route form", ep.Path)
	case len(ep.Allow) == 0:
		return nil, fmt.Errorf("no allow rule")
	}
	port := int(ep.Port)
	if port == 0 {
		port = 443
	}
	var out []route.Route
	for _, deny := range []bool{false, true} {
		rules := ep.Allow
		if deny {
			rules = ep.Deny
		}
		for _, r := range rules {
			q, err := exactQuery(r.Query)
			if err != nil {
				return nil, err
			}
			rt := route.Route{
				Class: class, Host: ep.Host, Port: port, Methods: []string{strings.ToUpper(r.Method)},
				Paths: []string{r.Path}, QueryValues: q, Deny: deny,
			}
			// OpenShell lets any query through an allow rule that names none; the route model says so
			// explicitly. A deny route names no query beyond the values it denies on.
			rt.AnyQuery = !deny
			out = append(out, rt)
		}
	}
	return out, nil
}

// exactQuery returns the query values of a rule, refusing a glob: the route model compares values
// exactly, so a glob would narrow an allow rule or widen past a deny rule.
func exactQuery(q map[string]string) (map[string]string, error) {
	if len(q) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(q))
	for k, v := range q {
		if v == "" || strings.ContainsAny(v, "*?[]{}") {
			return nil, fmt.Errorf("query %s=%q is not an exact value", k, v)
		}
		out[k] = v
	}
	return out, nil
}

// agentEgress returns the org egress routes of the active agent w, and the sealed code-host credential
// its forge routes name (#1086 part C). A fault leaves the agent with no route, which closes its gateway
// egress and opens nothing; it is logged, and the sandbox still gets its OpenShell rules from
// ensureSandbox. A credential that cannot be sealed leaves the routes without it: they fail closed.
// forgeErr is set only for an agent that expects a forge credential (codehost.Registry.ReadGrant, the
// rule koinon-agentd also reads): it says why that agent has no credential, and its run start fails with
// it (recordForge). So every agent that expects a credential gets a row, served or with its cause.
func (r *Reconciler) agentEgress(ctx context.Context, w *work) (routes []route.Route, cred *gatewayplan.ForgeCredential, file []byte, forgeErr error) {
	ref := agentRef(w.org, w.agent.ID)
	// The agent's git goes through the egress listener only when the install has one (Egress, the same
	// chart setting that turns on koinon-agentd's egress prefix); without it the driver's own rule serves
	// git, and no run waits for the credential.
	_, grant, grantErr := providers.Default().ReadGrant(w.cfg)
	expects := r.cfg.Egress != nil && (grant || grantErr != nil)
	w.forgeExpects = expects
	// fail logs err and returns it as the forge cause when the agent expects a credential.
	fail := func(what string, err error) error {
		r.log.Error("the agent has no egress routes", "org", w.org.Slug, "agent", ref.Name, "err", err)
		if expects {
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	}
	baseURL, err := r.baseURLOf(ctx, w)
	if err != nil {
		return nil, nil, nil, fail("the agent's code-host connection", err)
	}
	read, hasRead, err := GitHubReadAccessAt(w.cfg, baseURL)
	if err != nil {
		return nil, nil, nil, fail("the agent's code-host grant", err)
	}
	if expects && !hasRead {
		return nil, nil, nil, fail("the agent's code-host grant", errors.New("the grant has no network rules at the connection's host"))
	}
	var forge []execution.Endpoint
	if hasRead {
		forge = read.Endpoints
	}
	routes, err = EgressRoutes(ref, forge, PluginInstallsOn(w.cfg, w.agent.Harness))
	if err != nil {
		return nil, nil, nil, fail("the agent's forge routes", err)
	}
	if !hasRead {
		return routes, nil, nil, nil
	}
	cred, file, err = r.forgeCredential(ctx, w, read, routes)
	if err == nil && cred == nil && expects {
		err = errors.New("koinon-reconciler has no key source for gateway credentials, so koinon-gateway refuses the agent's forge routes")
	}
	if err != nil {
		r.m.PassErrors.WithLabelValues(errKindAgentPrepare).Inc()
		r.log.Error("the agent's forge routes have no credential; they fail closed", "org", w.org.Slug, "agent", ref.Name, "err", err)
		return routes, nil, nil, err
	}
	return routes, cred, file, nil
}

// egressRule names the sandbox policy entry of the gateway's egress listener.
const egressRule = "koinon_egress"

// EgressRule is the sandbox policy entry that lets git reach the agent's forge routes at the gateway's
// egress listener gw (#1086 part C2). koinon-agentd rewrites the code host to
// <gw>/egress/<authority>/, so each rule of the code-host grant is the same rule under that prefix:
// the same methods, paths and queries, enforced at L7, for the same binaries. Nothing else on the
// listener is reachable. The gateway enforces the same rules again (EgressRoutes) and checks the
// actor token; the sandbox holds no credential for it.
func EgressRule(gw Endpoint, read GitHubRead, binaries []string) execution.NetworkRule {
	var allow, deny []execution.L7Rule
	for _, ep := range read.Endpoints {
		port := int(ep.Port)
		if port == 0 {
			port = 443
		}
		prefix := egress.PathPrefix + connectors.HostPort{Host: ep.Host, Port: port}.Authority()
		for _, r := range ep.Allow {
			allow = append(allow, execution.L7Rule{Method: r.Method, Path: prefix + r.Path, Query: maps.Clone(r.Query)})
		}
		for _, r := range ep.Deny {
			deny = append(deny, execution.L7Rule{Method: r.Method, Path: prefix + r.Path, Query: maps.Clone(r.Query)})
		}
	}
	return execution.NetworkRule{
		Endpoints: []execution.Endpoint{{Host: gw.Host, Port: gw.Port, Protocol: "rest", Allow: allow, Deny: deny}},
		Binaries:  slices.Clone(binaries),
	}
}
