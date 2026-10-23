// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/procoduck/koinon/connectors"
	"github.com/procoduck/koinon/internal/cli"
	"github.com/procoduck/koinon/internal/db/store"
	"github.com/procoduck/koinon/internal/gateway"
	"github.com/procoduck/koinon/internal/project"
	"github.com/procoduck/koinon/internal/reconciler"
)

// fileConfig is the part of the chart's config.yaml (charts/koinon/templates/config.yaml) the
// reconciler reads.
type fileConfig struct {
	Gateway struct {
		Mode         project.GatewayMode `yaml:"mode"`
		ConfigMap    string              `yaml:"configMap"`
		URL          string              `yaml:"url"`
		StatsURL     string              `yaml:"statsUrl"`
		Port         int                 `yaml:"port"`
		DatabaseURL  string              `yaml:"databaseUrl"`
		ModelKeyFile string              `yaml:"modelKeyFile"`
		// AccessLogOTLP is koinon-api's OTLP/HTTP receiver as host:port (plan item L1).
		AccessLogOTLP string `yaml:"accessLogOtlp"`
	} `yaml:"gateway"`
	Model struct {
		BaseURL  string `yaml:"baseUrl"`
		Provider string `yaml:"provider"`
		Models   []struct {
			Name  string `yaml:"name"`
			Rates struct {
				Input     string `yaml:"input"`
				Output    string `yaml:"output"`
				CacheRead string `yaml:"cacheRead"`
			} `yaml:"rates"`
			Overrides map[string]any `yaml:"overrides"`
		} `yaml:"models"`
	} `yaml:"model"`
	OpenShell struct {
		Address          string `yaml:"address"`
		TLS              string `yaml:"tls"`
		RuntimeClassName string `yaml:"runtimeClassName"`
	} `yaml:"openshell"`
	Reconciler struct {
		IdleTimeout    string `yaml:"idleTimeout"`
		ResyncInterval string `yaml:"resyncInterval"`
		HomeRetention  string `yaml:"homeRetention"`
	} `yaml:"reconciler"`
	Flavors map[string]struct {
		Image string `yaml:"image"`
	} `yaml:"flavors"`
	GitHubTools struct {
		URL string `yaml:"url"`
	} `yaml:"githubTools"`
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

	gatewayMode      project.GatewayMode
	gatewayURL       string
	gatewayConfigMap string
	gatewayStatsURL  string
	gatewayPort      int
	gatewayDBURL     string
	modelKeyFile     string
	accessLogOTLP    string

	idleTimeout    time.Duration
	resyncInterval time.Duration
	homeRetention  string
	flavorImages   map[string]string

	githubToolsURL string
	githubAPIURL   string

	models gateway.ModelUpstream
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
	fs.Var(&c.gatewayMode, "gateway-mode", "standalone or kubernetes (default standalone)")
	fs.StringVar(&c.gatewayURL, "gateway-url", "", "agentgateway's LLM listener URL as sandboxes reach it; the reconciler also sends its reload probe there")
	fs.StringVar(&c.gatewayConfigMap, "gateway-config-map", "", "ConfigMap holding the standalone agentgateway config")
	fs.StringVar(&c.gatewayStatsURL, "gateway-stats-url", "", "agentgateway stats URL, to detect rejected configs")
	fs.IntVar(&c.gatewayPort, "gateway-port", 0, "agentgateway LLM listener port in the rendered config")
	fs.StringVar(&c.gatewayDBURL, "gateway-database-url", "", "agentgateway budget store URL in the rendered config")
	fs.StringVar(&c.modelKeyFile, "model-key-file", "", "path of the upstream model key inside agentgateway")
	fs.StringVar(&c.accessLogOTLP, "gateway-access-log-otlp", "",
		"host:port of koinon-api's OTLP/HTTP receiver, where agentgateway exports its access log for the spend ledger; empty exports nothing")
	fs.DurationVar(&c.idleTimeout, "idle-timeout", 0, "stop a sandbox after this long without an active run (default 10m)")
	fs.DurationVar(&c.resyncInterval, "resync-interval", 0, "time between passes without a wake-up (default 30s)")
	fs.StringVar(&c.homeRetention, "home-retention", "", "home volume on archive: delete or retain (default delete)")
	fs.StringVar(&c.githubAPIURL, "github-api-url", "", "GitHub REST API base URL for minting installation tokens (default https://api.github.com)")
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
	orDefault(&c.gatewayMode, fc.Gateway.Mode)
	orDefault(&c.gatewayMode, project.GatewayStandalone)
	orDefault(&c.gatewayURL, fc.Gateway.URL)
	orDefault(&c.gatewayConfigMap, fc.Gateway.ConfigMap)
	orDefault(&c.gatewayStatsURL, fc.Gateway.StatsURL)
	orDefault(&c.gatewayPort, fc.Gateway.Port)
	orDefault(&c.gatewayDBURL, fc.Gateway.DatabaseURL)
	orDefault(&c.modelKeyFile, fc.Gateway.ModelKeyFile)
	orDefault(&c.accessLogOTLP, fc.Gateway.AccessLogOTLP)
	orDefault(&c.homeRetention, fc.Reconciler.HomeRetention)
	orDefault(&c.githubToolsURL, fc.GitHubTools.URL)
	for _, d := range []struct {
		dst *time.Duration
		src string
	}{{&c.idleTimeout, fc.Reconciler.IdleTimeout}, {&c.resyncInterval, fc.Reconciler.ResyncInterval}} {
		if *d.dst == 0 && d.src != "" {
			v, err := time.ParseDuration(d.src)
			if err != nil {
				return fmt.Errorf("config: %w", err)
			}
			*d.dst = v
		}
	}
	c.flavorImages = map[string]string{}
	for name, f := range fc.Flavors {
		c.flavorImages[name] = f.Image
	}

	c.models = gateway.ModelUpstream{
		Provider:   fc.Model.Provider,
		BaseURL:    fc.Model.BaseURL,
		APIKeyFile: c.modelKeyFile,
	}
	for _, m := range fc.Model.Models {
		gm := gateway.Model{Name: m.Name}
		if m.Rates.Input != "" || m.Rates.Output != "" || m.Rates.CacheRead != "" {
			gm.Price = &gateway.Price{Input: m.Rates.Input, Output: m.Rates.Output, CacheRead: m.Rates.CacheRead}
		}
		c.models.Models = append(c.models.Models, gm)
		// ModelUpstream has one set of overrides for the upstream; the chart lists them per
		// model, so they must agree.
		if len(m.Overrides) > 0 {
			if c.models.Overrides != nil && !reflect.DeepEqual(c.models.Overrides, m.Overrides) {
				return errors.New("config: models with different overrides are not supported yet; use the same overrides for every model")
			}
			c.models.Overrides = m.Overrides
		}
	}
	return nil
}

func (c *config) validate() error {
	var missing []string
	for name, v := range map[string]string{
		"--database-url": c.databaseURL, "--namespace": c.namespace,
		"--openshell-address": c.openshellAddress, "--gateway-url": c.gatewayURL,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required settings: %s", strings.Join(missing, ", "))
	}
	if c.gatewayMode == project.GatewayStandalone && c.gatewayConfigMap == "" {
		return errors.New("--gateway-config-map is required in standalone mode")
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

// toolServers maps connector type IDs to their MCP tool servers.
func (c *config) toolServers() map[string]string {
	if c.githubToolsURL == "" {
		return nil
	}
	return map[string]string{connectors.GitHubID: c.githubToolsURL}
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
