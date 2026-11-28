// SPDX-License-Identifier: Apache-2.0

package koinonreconciler

import (
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/internal/sandboxres"
	"github.com/stateracloud/koinon/internal/secretstore/wrapping"
)

// The sections of the chart's config.yaml (charts/koinon/templates/config.yaml) that are not
// flags. Every other key of the file sets a flag (see commands.go, configKeys), at the key
// internal/cli.ConfigKey names.

// config is the reconciler's effective configuration, filled by internal/cli from the flags, the
// KOINON_* environment variables and the config file, in that order.
type config struct {
	databaseURL string
	namespace   string
	kubeconfig  string
	logLevel    string

	// execution selects the driver (execution.driver) and reaches Substrate; the OpenShell address
	// and TLS directory below fill its OpenShell half.
	execution        cli.Execution
	openshellAddress string
	openshellTLSDir  string
	installMark      string
	runtimeClass     string
	openshellGateway string

	sandboxObserver               bool
	sandboxObserverClusterRole    string
	sandboxObserverServiceAccount string

	sandboxNetworkPolicy     bool
	sandboxNetworkPolicyName string

	sandboxLimitRange     bool
	sandboxLimitRangeName string

	gatewayMode project.GatewayMode
	gatewayURL  string
	// gatewayMCPURL is koinon-gateway's MCP listener as sandboxes reach it (gateway.mcp.driver=koinon):
	// the sandbox profile allows /mcp there and not on agentgateway. Empty: agentgateway serves /mcp.
	gatewayMCPURL string
	// gatewayModelURL is koinon-gateway's model listener as sandboxes reach it (gateway.model.driver=koinon).
	// On Agent Substrate the guest's model calls go there and not to agentgateway, so the listener checks the
	// actor JWT (issue #1489, ADR 0097). Required with --execution-driver=substrate.
	gatewayModelURL    string
	gatewayConfigMap   string
	gatewayDeployment  string
	gatewayStatsURL    string
	gatewayName        string
	gatewayClass       string
	gatewayPort        int
	gatewayDBURL       string
	providerKeysSecret string
	providerKeysDir    string
	// mcpKeysSecret and mcpKeysDir: the MCP credential files and CA bundles live in a Secret of their own,
	// so koinon-gateway mounts no LLM provider key (ADR 0097 decision 5). Empty: they share the provider keys Secret.
	mcpKeysSecret string
	mcpKeysDir    string
	// mcpSealed: koinon-gateway serves every MCP path, so the reconciler may serve sealed credentials.
	mcpSealed bool
	// egressURL is koinon-gateway's egress listener as sandboxes reach it (#1086 part C2). With it the
	// sandbox of an agent with a code-host grant may reach the forge routes there; empty: no such rule.
	egressURL string
	kekDir    string
	// kekWrapping is how the KEKs are wrapped (wrapping.New); zero is the default local KEK.
	kekWrapping     wrapping.Config
	keySettle       time.Duration
	allowedHosts    string
	egressHosts     string
	healthAddr      string
	metricsAddr     string
	accessLogOTLP   string
	otlpTokenFile   string
	otlpTokenSecret string

	idleTimeout    time.Duration
	archiveAfter   time.Duration
	parkWarm       time.Duration
	sandboxJobs    int
	keepWarmPerOrg int
	// maxRecreates is the cap on spec-change recreates (a token bucket: this many at once, this many per resync interval); openshellConfigNS and
	// openshellConfigMap locate the gateway ConfigMap that names the supervisor image.
	maxRecreates                          int
	openshellConfigNS, openshellConfigMap string
	resyncInterval                        time.Duration
	homeRetention                         string
	checkpoints                           bool
	checkpointsReadOnly                   bool
	// restoreHold starts the process without converging anything (issue #462).
	restoreHold bool
	// recoveryPolicy and recoveryBinding name this release's ValidatingAdmissionPolicy and binding for
	// PlatformAdminRecovery requests (issue #512, ADR 0110 decision 6); both empty turns the handler off.
	recoveryPolicy, recoveryBinding string
	harnessImages                   map[string]string

	// sandboxResources is the platform's sandbox sizing, from the config file's sandboxResources.
	sandboxResources sandboxres.Policy

	githubToolsURL string
	mcpAskURL      string
	mcpGuardURL    string
	koinonToolsURL string
	githubAPIURL   string
	clusterDomain  string
}

// gatewayEndpoint parses the agentgateway URL into the host and port the provider profile
// binds the key to.
func (c *config) gatewayEndpoint() (reconciler.Endpoint, error) {
	return endpointOf("--gateway-url", c.gatewayURL)
}

// mcpGatewayEndpoint parses koinon-gateway's MCP URL (gateway.mcp.driver=koinon) into the host and port
// the provider profile allows /mcp on. Empty: the zero Endpoint, and agentgateway serves /mcp.
func (c *config) mcpGatewayEndpoint() (reconciler.Endpoint, error) {
	if c.gatewayMCPURL == "" {
		return reconciler.Endpoint{}, nil
	}
	return endpointOf("--gateway-mcp-url", c.gatewayMCPURL)
}

// egressEndpoint is the gateway's egress listener, nil without --gateway-egress-url.
func (c *config) egressEndpoint() (*reconciler.Endpoint, error) {
	if c.egressURL == "" {
		return nil, nil
	}
	e, err := endpointOf("--gateway-egress-url", c.egressURL)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// substrateGatewayRoutes are the gateway listeners of the Substrate actor egress policy (issues #1444 and #1489):
// routes for every actor (koinon-gateway's model and MCP listeners), and egress, the egress listener, which only an
// actor of an agent with a code-host grant gets (nil without --gateway-egress-url). Each listener checks the actor
// JWT of its own audience, so the guest calls only koinon-gateway: agentgateway (--gateway-url) is not a route, and
// --gateway-model-url and --gateway-mcp-url are required. The guest calls them over cleartext HTTP (ADR 0120), and
// the policy has HTTP rules only, so an https URL is refused.
func (c *config) substrateGatewayRoutes() (routes []substrate.GatewayRoute, egress *substrate.GatewayRoute, err error) {
	routeOf := func(flag, raw string, mk func(string, uint32) substrate.GatewayRoute) (substrate.GatewayRoute, error) {
		if raw == "" {
			return substrate.GatewayRoute{}, fmt.Errorf("%s is required with --execution-driver=substrate: the guest calls koinon-gateway's listeners, which check the actor JWT (issue #1489)", flag)
		}
		if u, err := url.Parse(raw); err == nil && u.Scheme != "http" {
			return substrate.GatewayRoute{}, fmt.Errorf("%s %q: the Substrate egress policy allows cleartext HTTP to the gateway only; use an http:// URL", flag, raw)
		}
		e, err := endpointOf(flag, raw)
		if err != nil {
			return substrate.GatewayRoute{}, err
		}
		r := mk(e.Host, e.Port)
		if err := substrate.ValidateGatewayRoutes([]substrate.GatewayRoute{r}, nil); err != nil {
			return substrate.GatewayRoute{}, fmt.Errorf("%s %q: the Substrate egress policy cannot allow it: %w", flag, raw, err)
		}
		return r, nil
	}
	for _, f := range []struct {
		flag, raw string
		mk        func(string, uint32) substrate.GatewayRoute
	}{
		{"--gateway-model-url", c.gatewayModelURL, substrate.ModelRoute},
		{"--gateway-mcp-url", c.gatewayMCPURL, substrate.MCPRoute},
	} {
		r, err := routeOf(f.flag, f.raw, f.mk)
		if err != nil {
			return nil, nil, err
		}
		routes = append(routes, r)
	}
	if c.egressURL != "" {
		r, err := routeOf("--gateway-egress-url", c.egressURL, substrate.EgressRoute)
		if err != nil {
			return nil, nil, err
		}
		egress = &r
	}
	if err := substrate.ValidateGatewayRoutes(routes, egress); err != nil {
		return nil, nil, fmt.Errorf("substrate egress policy: %w", err)
	}
	return routes, egress, nil
}

// endpointOf parses an http(s)://host[:port] URL, named flag in errors, into a host and a port.
func endpointOf(flag, raw string) (reconciler.Endpoint, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return reconciler.Endpoint{}, fmt.Errorf("%s: %w", flag, err)
	}
	port := u.Port()
	switch {
	case port != "":
	case u.Scheme == "https":
		port = "443"
	case u.Scheme == "http":
		port = "80"
	default:
		return reconciler.Endpoint{}, fmt.Errorf("%s %q: want http(s)://host[:port]", flag, raw)
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil || u.Hostname() == "" {
		return reconciler.Endpoint{}, fmt.Errorf("%s %q: want http(s)://host[:port]", flag, raw)
	}
	return reconciler.Endpoint{Host: u.Hostname(), Port: uint32(p)}, nil
}

// toolServers maps connector type IDs (and gateway.KoinonTarget, Koinon's own tools) to their MCP tool servers.
func (c *config) toolServers() map[string]string {
	out := map[string]string{}
	if c.githubToolsURL != "" {
		// One code-host tool server serves every code-host connector; it picks the connector
		// from the grant (hosts.Grant).
		for _, id := range providers.Default().IDs() {
			out[id] = c.githubToolsURL
		}
	}
	if c.koinonToolsURL != "" {
		out[gateway.KoinonTarget] = c.koinonToolsURL
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// harnesses combines the configured images with each harness's binaries.
func (c *config) harnesses() map[string]reconciler.Harness { return harnessesOf(c.harnessImages) }

// harnessesOf combines harness images with each harness's binaries.
func harnessesOf(images map[string]string) map[string]reconciler.Harness {
	out := map[string]reconciler.Harness{}
	for name, img := range images {
		f := name
		out[f] = reconciler.Harness{Image: img, Binaries: reconciler.DefaultBinaries[f]}
	}
	return out
}
