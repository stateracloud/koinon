// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/metrics"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/internal/sandboxres"
)

// fileConfig is the part of the chart's config.yaml (charts/koinon/templates/config.yaml) the
// reconciler reads.
type fileConfig struct {
	Gateway struct {
		Mode      project.GatewayMode `yaml:"mode"`
		ConfigMap string              `yaml:"configMap"`
		// Deployment is the gateway Deployment that owns the ConfigMap the reconciler creates.
		Deployment  string `yaml:"deployment"`
		URL         string `yaml:"url"`
		StatsURL    string `yaml:"statsUrl"`
		Port        int    `yaml:"port"`
		DatabaseURL string `yaml:"databaseUrl"`
		// ProviderKeysSecret is the Secret the reconciler writes provider keys into; the gateway
		// mounts it at ProviderKeysDir.
		ProviderKeysSecret string `yaml:"providerKeysSecret"`
		ProviderKeysDir    string `yaml:"providerKeysDir"`
		// AccessLogOTLP is koinon-api's OTLP/HTTP receiver as host:port (plan item L1).
		AccessLogOTLP string `yaml:"accessLogOtlp"`
		// AccessLogOTLPTokenFile is the file in the gateway pod with the receiver's bearer token
		// (standalone mode); AccessLogOTLPTokenSecret is the Secret that holds it (kubernetes mode,
		// key "token"). Names only: the token is never in this file.
		AccessLogOTLPTokenFile   string `yaml:"accessLogOtlpTokenFile"`
		AccessLogOTLPTokenSecret string `yaml:"accessLogOtlpTokenSecret"`
		// Name and GatewayClassName are Koinon's Gateway and its class in kubernetes mode (plan item G2).
		Name             string `yaml:"name"`
		GatewayClassName string `yaml:"gatewayClassName"`
	} `yaml:"gateway"`
	OpenShell struct {
		Address          string `yaml:"address"`
		TLS              string `yaml:"tls"`
		RuntimeClassName string `yaml:"runtimeClassName"`
		// GatewayID is OpenShell's gateway_id, which names the workspace namespaces.
		GatewayID string `yaml:"gatewayId"`
		// GatewayConfig locates OpenShell's gateway ConfigMap, whose supervisor_image is part of the
		// sandbox spec hash (ADR 0085 decision 9). Empty leaves it out.
		GatewayConfig struct {
			Namespace string `yaml:"namespace"`
			ConfigMap string `yaml:"configMap"`
		} `yaml:"gatewayConfig"`
	} `yaml:"openshell"`
	// SandboxObserver is the chart's sandboxObserver (ADR 0078).
	SandboxObserver struct {
		Enabled bool `yaml:"enabled"`
		// ClusterRole is the observer ClusterRole, and the name of the RoleBinding to it.
		ClusterRole string `yaml:"clusterRole"`
		// ServiceAccount is the reconciler's, the RoleBinding's subject.
		ServiceAccount string `yaml:"serviceAccount"`
	} `yaml:"sandboxObserver"`
	Reconciler struct {
		IdleTimeout    string `yaml:"idleTimeout"`
		ParkWarm       string `yaml:"parkWarm"`
		ArchiveAfter   string `yaml:"archiveAfter"`
		SandboxJobs    int    `yaml:"sandboxJobs"`
		KeepWarmPerOrg int    `yaml:"keepWarmPerOrg"`
		// MaxRecreatesPerPass caps the sandboxes recreated with a changed spec per pass.
		MaxRecreatesPerPass int    `yaml:"maxRecreatesPerPass"`
		ResyncInterval      string `yaml:"resyncInterval"`
		HomeRetention       string `yaml:"homeRetention"`
		Checkpoints         bool   `yaml:"checkpoints"`
	} `yaml:"reconciler"`
	// SandboxResources is the chart's sandbox.resources (ADR 0083). Values left out keep
	// sandboxres.DefaultPolicy's.
	SandboxResources struct {
		Default struct {
			CPU    quantitiesConfig `yaml:"cpu"`
			Memory quantitiesConfig `yaml:"memory"`
		} `yaml:"default"`
		Bounds struct {
			CPU struct {
				Request rangeConfig `yaml:"request"`
				Limit   rangeConfig `yaml:"limit"`
			} `yaml:"cpu"`
			Memory struct {
				Request rangeConfig `yaml:"request"`
				Limit   rangeConfig `yaml:"limit"`
			} `yaml:"memory"`
		} `yaml:"bounds"`
		MemoryRequestEqualsLimit *bool `yaml:"memoryRequestEqualsLimit"`
	} `yaml:"sandboxResources"`
	Flavors map[string]struct {
		Image string `yaml:"image"`
	} `yaml:"flavors"`
	GitHubTools struct {
		URL string `yaml:"url"`
	} `yaml:"githubTools"`
	// KoinonTools is koinon-api's server for Koinon's own agent tools (ask_user, plan item TK3).
	KoinonTools struct {
		URL string `yaml:"url"`
	} `yaml:"koinonTools"`
	// MCPAsk is koinon-api's approval-gated proxy for org MCP servers' ask-tier tools (plan item MC1).
	MCPAsk struct {
		URL string `yaml:"url"`
	} `yaml:"mcpAsk"`
}

type quantitiesConfig struct {
	Request string `yaml:"request"`
	Limit   string `yaml:"limit"`
}

type rangeConfig struct {
	Min string `yaml:"min"`
	Max string `yaml:"max"`
}

// policy lays the file's sandbox resources over sandboxres.DefaultPolicy: a value the file leaves
// empty keeps the default.
func (fc fileConfig) policy() sandboxres.Policy {
	p := sandboxres.DefaultPolicy()
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	sr := fc.SandboxResources
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

// config is the reconciler's effective configuration: the config file, overridden by flags
// and KOINON_* environment variables.
type config struct {
	configFile  string
	databaseURL string
	namespace   string
	kubeconfig  string
	logLevel    string

	openshellAddress string
	openshellTLSDir  string
	runtimeClass     string
	openshellGateway string

	sandboxObserver               bool
	sandboxObserverSet            bool // --sandbox-observer (or KOINON_SANDBOX_OBSERVER) was given: it wins over the config file
	sandboxObserverClusterRole    string
	sandboxObserverServiceAccount string

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
	keySettle          time.Duration
	allowedHosts       string
	egressHosts        string
	healthAddr         string
	metricsAddr        string
	accessLogOTLP      string
	otlpTokenFile      string
	otlpTokenSecret    string

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
	flavorImages                          map[string]string

	// sandboxResources is the platform's sandbox sizing, from the config file's sandboxResources.
	sandboxResources sandboxres.Policy

	githubToolsURL string
	mcpAskURL      string
	koinonToolsURL string
	githubAPIURL   string
}

// parseConfig reads flags (and their KOINON_* environment variables) and fills what they leave
// unset from the config file. Every flag name maps to KOINON_<NAME> with dashes as underscores.
func parseConfig(args []string, getenv func(string) (string, bool)) (config, error) {
	var c config
	fs := flag.NewFlagSet("koinon-reconciler", flag.ContinueOnError)
	fs.StringVar(&c.configFile, "config", "", "Koinon config file (the chart's config.yaml)")
	fs.StringVar(&c.databaseURL, "database-url", "", "Postgres connection string (default $DATABASE_URL)")
	fs.StringVar(&c.namespace, "namespace", "", "Koinon's namespace, for agent key Secrets (default $POD_NAMESPACE)")
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig file; empty uses the in-cluster config")
	fs.StringVar(&c.logLevel, "log-level", "info", "debug, info, warn or error")
	fs.StringVar(&c.openshellAddress, "openshell-address", "", "OpenShell gateway host:port")
	fs.StringVar(&c.openshellTLSDir, "openshell-tls-dir", "", "directory with tls.crt, tls.key and ca.crt for mTLS; empty connects without TLS")
	fs.StringVar(&c.runtimeClass, "runtime-class", "", "sandbox runtime class (ADR 0045: kata)")
	fs.StringVar(&c.openshellGateway, "openshell-gateway-id", "", "OpenShell's gateway_id, which names the workspace namespaces (default openshell)")
	fs.BoolVar(&c.sandboxObserver, "sandbox-observer", false, "read the pods of sandboxes that never became ready, to say why (ADR 0078); needs the chart's observer ClusterRole")
	fs.StringVar(&c.sandboxObserverClusterRole, "sandbox-observer-cluster-role", "", "the observer ClusterRole, and the name of the RoleBinding to it in each workspace namespace")
	fs.StringVar(&c.sandboxObserverServiceAccount, "sandbox-observer-service-account", "", "the reconciler's ServiceAccount, the subject of those RoleBindings")
	fs.Var(&c.gatewayMode, "gateway-mode", "standalone or kubernetes (default standalone)")
	fs.StringVar(&c.gatewayURL, "gateway-url", "", "agentgateway's LLM listener URL as sandboxes reach it; the reconciler also sends its reload probe there")
	fs.StringVar(&c.gatewayConfigMap, "gateway-config-map", "", "ConfigMap holding the standalone agentgateway config; created when missing")
	fs.StringVar(&c.gatewayDeployment, "gateway-deployment", "", "standalone agentgateway Deployment that owns the ConfigMap when the reconciler creates it")
	fs.StringVar(&c.gatewayStatsURL, "gateway-stats-url", "", "agentgateway stats URL, to detect rejected configs")
	fs.StringVar(&c.gatewayName, "gateway-name", "", "name of Koinon's Gateway in kubernetes mode")
	fs.StringVar(&c.gatewayClass, "gateway-class", "", "GatewayClass of agentgateway's controller in kubernetes mode (default agentgateway)")
	fs.IntVar(&c.gatewayPort, "gateway-port", 0, "agentgateway LLM listener port in the rendered config")
	fs.StringVar(&c.gatewayDBURL, "gateway-database-url", "", "agentgateway budget store URL in the rendered config")
	fs.StringVar(&c.providerKeysSecret, "provider-keys-secret", "", "Secret that receives the opened provider keys for agentgateway")
	fs.StringVar(&c.providerKeysDir, "provider-keys-dir", "", "directory where agentgateway mounts the provider keys Secret")
	fs.StringVar(&c.kekDir, "kek-dir", "", "directory with the mounted key-encryption keys Secret (files named by KEK ID plus \"active\")")
	fs.StringVar(&c.accessLogOTLP, "gateway-access-log-otlp", "",
		"host:port of koinon-api's OTLP/HTTP receiver, where agentgateway exports its access log for the spend ledger; empty exports nothing")
	fs.StringVar(&c.otlpTokenFile, "gateway-access-log-otlp-token-file", "",
		"file, as the standalone gateway pod sees it, with the bearer token the OTLP receiver requires; agentgateway reads it and sends it with the access log")
	fs.StringVar(&c.otlpTokenSecret, "gateway-access-log-otlp-token-secret", "",
		"Secret (key token) with the bearer token the OTLP receiver requires, for kubernetes mode: the agentgateway controller reads it and hands it to the proxy")
	fs.StringVar(&c.metricsAddr, "metrics-addr", metrics.DefaultPort, "listen address of /metrics (Prometheus); empty disables it")
	fs.StringVar(&c.healthAddr, "health-addr", ":8082", "listen address of /healthz and /readyz (not ready until the key-encryption keys are loaded and the schema version is supported); empty disables it")
	fs.StringVar(&c.egressHosts, "egress-allowed-hosts", "", "comma-separated hosts (optionally host:port) providers and MCP servers may name although they are internal")
	fs.StringVar(&c.allowedHosts, "provider-allowed-hosts", "", "deprecated alias of --egress-allowed-hosts, kept for one release")
	fs.DurationVar(&c.keySettle, "provider-key-settle", 0,
		"how long to wait after writing a new provider key file before applying a gateway config that names it (default 90s in standalone mode: kubelet updates a mounted Secret within about a minute; 2s in kubernetes mode: the controller reads the Secret through the API)")
	fs.DurationVar(&c.archiveAfter, "archive-after", 0, "archive a sandbox (delete it and its volume, home checkpointed) after this long stopped (default 168h; negative turns it off; needs checkpoints)")
	fs.DurationVar(&c.idleTimeout, "idle-timeout", 0, "stop a sandbox after this long without an active run (default 10m)")
	fs.DurationVar(&c.parkWarm, "park-warm", 0, "keep a sandbox up this long after its run parked on a2a or input_required (default 15m)")
	fs.IntVar(&c.keepWarmPerOrg, "keep-warm-per-org", 0, "keep-warm sandboxes an org may have (default 2; negative grants none)")
	fs.IntVar(&c.maxRecreates, "max-recreates-per-pass", 0, "sandboxes one pass may recreate with a changed spec, such as a new supervisor image (default 4; negative is no cap)")
	fs.StringVar(&c.openshellConfigNS, "openshell-config-namespace", "", "namespace of OpenShell's gateway ConfigMap, to read its supervisor image")
	fs.StringVar(&c.openshellConfigMap, "openshell-config-map", "", "OpenShell's gateway ConfigMap (<fullname>-config); empty leaves the supervisor image out of the sandbox spec hash")
	fs.IntVar(&c.sandboxJobs, "sandbox-jobs", 0, "sandbox start/stop jobs that run at once (default 8)")
	fs.DurationVar(&c.resyncInterval, "resync-interval", 0, "time between passes without a wake-up (default 30s)")
	fs.BoolVar(&c.checkpoints, "checkpoints", false, "stop, recreate and restore sandboxes around home checkpoints (koinon-agentd writes them; ADR 0032)")
	fs.StringVar(&c.homeRetention, "home-retention", "", "home volume on archive: delete or retain (default delete)")
	fs.StringVar(&c.githubAPIURL, "github-api-url", "", "GitHub REST API base URL for minting installation tokens (default https://api.github.com)")
	fs.StringVar(&c.mcpAskURL, "mcp-ask-url", "", "base URL of koinon-api's approval-gated MCP proxy as agentgateway reaches it; empty offers no ask-tier tools of org MCP servers")
	fs.StringVar(&c.koinonToolsURL, "koinon-tools-url", "", "URL of Koinon's own agent tools (ask_user) MCP server as agentgateway reaches it; empty offers no ask_user")
	fs.StringVar(&c.githubToolsURL, "github-tools-url", "", "URL of the GitHub tools MCP server as agentgateway reaches it; empty serves no GitHub tools")

	if err := cli.BindEnv(fs, getenv); err != nil {
		return c, err
	}
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	// BindEnv sets flags without marking them visited, so the variable is looked at too.
	if _, ok := getenv(cli.EnvName("sandbox-observer")); ok {
		c.sandboxObserverSet = true
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "sandbox-observer" {
			c.sandboxObserverSet = true
		}
	})
	if c.databaseURL == "" {
		c.databaseURL, _ = getenv("DATABASE_URL")
	}
	if c.namespace == "" {
		c.namespace, _ = getenv("POD_NAMESPACE")
	}

	var fc fileConfig
	if c.configFile != "" {
		data, err := os.ReadFile(c.configFile)
		if err != nil {
			return c, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal(data, &fc); err != nil {
			return c, fmt.Errorf("parse config %s: %w", c.configFile, err)
		}
	}
	if err := c.fill(fc); err != nil {
		return c, err
	}
	return c, c.validate()
}

func orDefault[T comparable](v *T, def T) {
	var zero T
	if *v == zero {
		*v = def
	}
}

func (c *config) fill(fc fileConfig) error {
	orDefault(&c.openshellAddress, fc.OpenShell.Address)
	orDefault(&c.openshellTLSDir, fc.OpenShell.TLS)
	orDefault(&c.runtimeClass, fc.OpenShell.RuntimeClassName)
	orDefault(&c.openshellGateway, fc.OpenShell.GatewayID)
	orDefault(&c.openshellConfigNS, fc.OpenShell.GatewayConfig.Namespace)
	orDefault(&c.openshellConfigMap, fc.OpenShell.GatewayConfig.ConfigMap)
	orDefault(&c.maxRecreates, fc.Reconciler.MaxRecreatesPerPass)
	orDefault(&c.openshellGateway, execution.DefaultGatewayID)
	// An explicit flag or variable wins over the file in both directions: flag > file > default (off).
	if !c.sandboxObserverSet {
		c.sandboxObserver = fc.SandboxObserver.Enabled
	}
	orDefault(&c.sandboxObserverClusterRole, fc.SandboxObserver.ClusterRole)
	orDefault(&c.sandboxObserverServiceAccount, fc.SandboxObserver.ServiceAccount)
	orDefault(&c.gatewayMode, fc.Gateway.Mode)
	orDefault(&c.gatewayMode, project.GatewayStandalone)
	orDefault(&c.gatewayURL, fc.Gateway.URL)
	orDefault(&c.gatewayName, fc.Gateway.Name)
	orDefault(&c.gatewayClass, fc.Gateway.GatewayClassName)
	orDefault(&c.gatewayClass, "agentgateway")
	if c.gatewayMode == project.GatewayKubernetes {
		orDefault(&c.keySettle, 2*time.Second)
	}
	orDefault(&c.keySettle, 90*time.Second)
	orDefault(&c.gatewayConfigMap, fc.Gateway.ConfigMap)
	orDefault(&c.gatewayDeployment, fc.Gateway.Deployment)
	orDefault(&c.gatewayStatsURL, fc.Gateway.StatsURL)
	orDefault(&c.gatewayPort, fc.Gateway.Port)
	orDefault(&c.gatewayDBURL, fc.Gateway.DatabaseURL)
	orDefault(&c.providerKeysSecret, fc.Gateway.ProviderKeysSecret)
	orDefault(&c.providerKeysDir, fc.Gateway.ProviderKeysDir)
	orDefault(&c.accessLogOTLP, fc.Gateway.AccessLogOTLP)
	orDefault(&c.otlpTokenFile, fc.Gateway.AccessLogOTLPTokenFile)
	orDefault(&c.otlpTokenSecret, fc.Gateway.AccessLogOTLPTokenSecret)
	orDefault(&c.homeRetention, fc.Reconciler.HomeRetention)
	orDefault(&c.checkpoints, fc.Reconciler.Checkpoints)
	if c.sandboxJobs == 0 {
		c.sandboxJobs = fc.Reconciler.SandboxJobs
	}
	if c.keepWarmPerOrg == 0 {
		c.keepWarmPerOrg = fc.Reconciler.KeepWarmPerOrg
	}
	orDefault(&c.githubToolsURL, fc.GitHubTools.URL)
	orDefault(&c.mcpAskURL, fc.MCPAsk.URL)
	orDefault(&c.koinonToolsURL, fc.KoinonTools.URL)
	for _, d := range []struct {
		dst *time.Duration
		src string
	}{{&c.idleTimeout, fc.Reconciler.IdleTimeout}, {&c.archiveAfter, fc.Reconciler.ArchiveAfter}, {&c.parkWarm, fc.Reconciler.ParkWarm}, {&c.resyncInterval, fc.Reconciler.ResyncInterval}} {
		if *d.dst == 0 && d.src != "" {
			v, err := time.ParseDuration(d.src)
			if err != nil {
				return fmt.Errorf("config: %w", err)
			}
			*d.dst = v
		}
	}
	c.sandboxResources = fc.policy()
	c.flavorImages = map[string]string{}
	for name, f := range fc.Flavors {
		c.flavorImages[name] = f.Image
	}

	return nil
}

func (c *config) validate() error {
	var missing []string
	for name, v := range map[string]string{
		"--database-url": c.databaseURL, "--namespace": c.namespace,
		"--openshell-address": c.openshellAddress, "--gateway-url": c.gatewayURL, "--kek-dir": c.kekDir,
		"--provider-keys-secret": c.providerKeysSecret, "--provider-keys-dir": c.providerKeysDir,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required settings: %s", strings.Join(missing, ", "))
	}
	if c.sandboxObserver && (c.sandboxObserverClusterRole == "" || c.sandboxObserverServiceAccount == "") {
		return errors.New("--sandbox-observer-cluster-role and --sandbox-observer-service-account are required with --sandbox-observer")
	}
	if c.gatewayMode == project.GatewayStandalone && c.gatewayConfigMap == "" {
		return errors.New("--gateway-config-map is required in standalone mode")
	}
	if c.gatewayMode == project.GatewayKubernetes && c.gatewayName == "" {
		return errors.New("--gateway-name is required in kubernetes mode")
	}
	if err := c.sandboxResources.Validate(); err != nil {
		return fmt.Errorf("config sandboxResources: %w", err)
	}
	return nil
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
func (c *config) flavors() map[store.AgentFlavor]reconciler.Flavor {
	out := map[store.AgentFlavor]reconciler.Flavor{}
	for name, img := range c.flavorImages {
		f := store.AgentFlavor(name)
		out[f] = reconciler.Flavor{Image: img, Binaries: reconciler.DefaultBinaries[f]}
	}
	return out
}
