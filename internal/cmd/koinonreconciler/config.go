// SPDX-License-Identifier: Apache-2.0

package koinonreconciler

import (
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/internal/sandboxres"
	"github.com/stateracloud/koinon/internal/secretstore/wrapping"
)

// The sections of the chart's config.yaml (charts/koinon/templates/config.yaml) that are not
// flags. Every other key of the file sets a flag (see commands.go, configKeys), at the key
// internal/cli.ConfigKey names.

// fileSandboxResources is the chart's sandbox.resources (ADR 0083). Values left out keep
// sandboxres.DefaultPolicy's.
type fileSandboxResources struct {
	Default struct {
		CPU    quantitiesConfig `mapstructure:"cpu"`
		Memory quantitiesConfig `mapstructure:"memory"`
	} `mapstructure:"default"`
	Bounds struct {
		CPU struct {
			Request rangeConfig `mapstructure:"request"`
			Limit   rangeConfig `mapstructure:"limit"`
		} `mapstructure:"cpu"`
		Memory struct {
			Request rangeConfig `mapstructure:"request"`
			Limit   rangeConfig `mapstructure:"limit"`
		} `mapstructure:"memory"`
	} `mapstructure:"bounds"`
	MemoryRequestEqualsLimit *bool `mapstructure:"memoryRequestEqualsLimit"`
}

type quantitiesConfig struct {
	Request string `mapstructure:"request"`
	Limit   string `mapstructure:"limit"`
}

type rangeConfig struct {
	Min string `mapstructure:"min"`
	Max string `mapstructure:"max"`
}

// policy lays the file's sandbox resources over sandboxres.DefaultPolicy: a value the file leaves
// empty keeps the default.
func (sr fileSandboxResources) policy() sandboxres.Policy {
	p := sandboxres.DefaultPolicy()
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&p.Default.CPU.Request, sr.Default.CPU.Request)
	set(&p.Default.CPU.Limit, sr.Default.CPU.Limit)
	set(&p.Default.Memory.Request, sr.Default.Memory.Request)
	set(&p.Default.Memory.Limit, sr.Default.Memory.Limit)
	for _, r := range []struct {
		dst *sandboxres.Range
		src rangeConfig
	}{
		{&p.Bounds.CPURequest, sr.Bounds.CPU.Request},
		{&p.Bounds.CPULimit, sr.Bounds.CPU.Limit},
		{&p.Bounds.MemoryRequest, sr.Bounds.Memory.Request},
		{&p.Bounds.MemoryLimit, sr.Bounds.Memory.Limit},
	} {
		set(&r.dst.Min, r.src.Min)
		set(&r.dst.Max, r.src.Max)
	}
	if sr.MemoryRequestEqualsLimit != nil {
		p.MemoryRequestEqualsLimit = *sr.MemoryRequestEqualsLimit
	}
	return p
}

// config is the reconciler's effective configuration, filled by internal/cli from the flags, the
// KOINON_* environment variables and the config file, in that order.
type config struct {
	databaseURL string
	namespace   string
	kubeconfig  string
	logLevel    string

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

	gatewayMode        project.GatewayMode
	gatewayURL         string
	gatewayConfigMap   string
	gatewayDeployment  string
	gatewayStatsURL    string
	gatewayName        string
	gatewayClass       string
	gatewayPort        int
	gatewayDBURL       string
	providerKeysSecret string
	providerKeysDir    string
	kekDir             string
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
	// maxRecreates is the cap on spec-change recreates per pass; openshellConfigNS and
	// openshellConfigMap locate the gateway ConfigMap that names the supervisor image.
	maxRecreates                          int
	openshellConfigNS, openshellConfigMap string
	resyncInterval                        time.Duration
	homeRetention                         string
	checkpoints                           bool
	// restoreHold starts the process without converging anything (issue #462).
	restoreHold  bool
	flavorImages map[string]string

	// sandboxResources is the platform's sandbox sizing, from the config file's sandboxResources.
	sandboxResources sandboxres.Policy

	githubToolsURL string
	mcpAskURL      string
	mcpGuardURL    string
	koinonToolsURL string
	githubAPIURL   string
}

// gatewayEndpoint parses the agentgateway URL into the host and port the provider profile
// binds the key to.
func (c *config) gatewayEndpoint() (reconciler.Endpoint, error) {
	u, err := url.Parse(c.gatewayURL)
	if err != nil {
		return reconciler.Endpoint{}, fmt.Errorf("--gateway-url: %w", err)
	}
	port := u.Port()
	switch {
	case port != "":
	case u.Scheme == "https":
		port = "443"
	case u.Scheme == "http":
		port = "80"
	default:
		return reconciler.Endpoint{}, fmt.Errorf("--gateway-url %q: want http(s)://host[:port]", c.gatewayURL)
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil || u.Hostname() == "" {
		return reconciler.Endpoint{}, fmt.Errorf("--gateway-url %q: want http(s)://host[:port]", c.gatewayURL)
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

// flavors combines the configured images with each flavor's binaries.
func (c *config) flavors() map[store.AgentFlavor]reconciler.Flavor { return flavorsOf(c.flavorImages) }

// flavorsOf combines flavor images with each flavor's binaries.
func flavorsOf(images map[string]string) map[store.AgentFlavor]reconciler.Flavor {
	out := map[store.AgentFlavor]reconciler.Flavor{}
	for name, img := range images {
		f := store.AgentFlavor(name)
		out[f] = reconciler.Flavor{Image: img, Binaries: reconciler.DefaultBinaries[f]}
	}
	return out
}
