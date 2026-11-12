// SPDX-License-Identifier: Apache-2.0

package koinonreconciler

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/metrics"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/restorehold"
	"github.com/stateracloud/koinon/internal/sandboxres"
)

// NewRootCommand is koinon-reconciler's command tree (ADR 0088). A setting comes from the flag,
// then KOINON_<FLAG>, then the chart's config file (--config, KOINON_CONFIG), then the default
// (internal/cli); the chart's contract test (test/chartsmoke) builds this tree to check what the
// rendered Deployment passes, including the settings that come from the file.
func NewRootCommand() *cobra.Command { return newRootCommand(serve) }

// newRootCommand builds the tree with run as the work of serve, so a test can read the settings
// the command resolved.
func newRootCommand(run func(context.Context, config) error) *cobra.Command {
	return newRootCommandWith(run, listUnmarked)
}

// newRootCommandWith also takes the work of "unmarked list".
func newRootCommandWith(run func(context.Context, config) error, list func(context.Context, unmarkedConfig, io.Writer) error) *cobra.Command {
	root := &cobra.Command{
		Use:   "koinon-reconciler",
		Short: "Koinon's reconciler",
		Long: "koinon-reconciler renders agents into OpenShell sandboxes, provider records and agentgateway keys (serve, " +
			"also what runs without a subcommand). unmarked lists the OpenShell objects that carry no install mark.\n\n" +
			"Settings come from the chart's config file (--config, KOINON_CONFIG) and can be overridden by flags; every flag " +
			"can also be set by an environment variable: KOINON_ plus the flag name in upper case with dashes as underscores " +
			"(--idle-timeout is KOINON_IDLE_TIMEOUT). Secrets are never flags: the database password comes from PGPASSWORD, " +
			"the OpenShell client certificate from files.",
	}
	v := cli.NewViper()
	cli.ConfigFile(root, v)
	cli.AllowEnv(root, "KOINON_MEMORY_LIMIT_BYTES")
	cli.DefaultTo(root, "serve")
	root.AddCommand(newServeCommand(v, run))
	root.AddCommand(newUnmarkedCommand(v, list))
	return root
}

// configKeys maps a flag to its key in the chart's config.yaml (charts/koinon/templates/config.yaml).
// A flag that is not here is not in the file.
var configKeys = map[string]string{
	"openshell-address":                    "openshell.address",
	"openshell-tls-dir":                    "openshell.tls",
	"runtime-class":                        "openshell.runtimeClassName",
	"openshell-gateway-id":                 "openshell.gatewayId",
	"openshell-config-namespace":           "openshell.gatewayConfig.namespace",
	"openshell-config-map":                 "openshell.gatewayConfig.configMap",
	"sandbox-observer":                     "sandboxObserver.enabled",
	"sandbox-observer-cluster-role":        "sandboxObserver.clusterRole",
	"sandbox-observer-service-account":     "sandboxObserver.serviceAccount",
	"sandbox-network-policy":               "sandboxNetworkPolicy.enabled",
	"sandbox-network-policy-name":          "sandboxNetworkPolicy.name",
	"sandbox-limit-range":                  "sandboxLimitRange.enabled",
	"sandbox-limit-range-name":             "sandboxLimitRange.name",
	"gateway-mode":                         "gateway.mode",
	"gateway-url":                          "gateway.url",
	"gateway-config-map":                   "gateway.configMap",
	"gateway-deployment":                   "gateway.deployment",
	"gateway-stats-url":                    "gateway.statsUrl",
	"gateway-name":                         "gateway.name",
	"gateway-class":                        "gateway.gatewayClassName",
	"gateway-port":                         "gateway.port",
	"gateway-database-url":                 "gateway.databaseUrl",
	"provider-keys-secret":                 "gateway.providerKeysSecret",
	"provider-keys-dir":                    "gateway.providerKeysDir",
	"gateway-access-log-otlp":              "gateway.accessLogOtlp",
	"gateway-access-log-otlp-token-file":   "gateway.accessLogOtlpTokenFile",
	"gateway-access-log-otlp-token-secret": "gateway.accessLogOtlpTokenSecret",
	"idle-timeout":                         "reconciler.idleTimeout",
	"park-warm":                            "reconciler.parkWarm",
	"archive-after":                        "reconciler.archiveAfter",
	"sandbox-jobs":                         "reconciler.sandboxJobs",
	"keep-warm-per-org":                    "reconciler.keepWarmPerOrg",
	"max-recreates-per-pass":               "reconciler.maxRecreatesPerPass",
	"resync-interval":                      "reconciler.resyncInterval",
	"home-retention":                       "reconciler.homeRetention",
	"checkpoints":                          "reconciler.checkpoints",
	"github-tools-url":                     "githubTools.url",
	"mcp-ask-url":                          "mcpAsk.url",
	"mcp-guard-url":                        "mcpGuard.url",
	"koinon-tools-url":                     "koinonTools.url",
}

// requiredFlags are the settings the reconciler cannot start without, from any source.
var requiredFlags = []string{
	"database-url", "namespace", "openshell-address", "gateway-url", "kek-dir", "provider-keys-secret", "provider-keys-dir",
}

func newServeCommand(v *viper.Viper, run func(context.Context, config) error) *cobra.Command {
	var c config
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Reconcile agents into sandboxes, provider records and gateway keys",
		Args:  cobra.NoArgs,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			// Missing required settings come first; the rules between settings assume them.
			if err := cmd.ValidateRequiredFlags(); err != nil {
				return err
			}
			return c.finish(v)
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return run(cmd.Context(), c) },
	}
	c.flags(cmd.Flags())
	cli.EnvAlias(cmd, "database-url", "DATABASE_URL")
	cli.EnvAlias(cmd, "namespace", "POD_NAMESPACE")
	for flag, key := range configKeys {
		cli.ConfigKey(cmd, flag, key)
	}
	for _, n := range requiredFlags {
		if err := cmd.MarkFlagRequired(n); err != nil {
			panic(fmt.Sprintf("%s: %v", cmd.Name(), err))
		}
	}
	return cmd
}

// finish applies the defaults that depend on other settings, reads the config file's sections that
// are not flags, and checks the rules between settings. The flags are resolved already.
func (c *config) finish(v *viper.Viper) error {
	// An empty value (the chart renders an unset one that way) takes the default.
	orDefault(&c.openshellGateway, execution.DefaultGatewayID)
	orDefault(&c.gatewayClass, "agentgateway")
	orDefault(&c.gatewayMode, project.GatewayStandalone)
	if c.gatewayMode == project.GatewayKubernetes {
		orDefault(&c.keySettle, 2*time.Second)
	}
	orDefault(&c.keySettle, 90*time.Second)

	if c.sandboxObserver && (c.sandboxObserverClusterRole == "" || c.sandboxObserverServiceAccount == "") {
		return fmt.Errorf("--sandbox-observer-cluster-role and --sandbox-observer-service-account are required with --sandbox-observer")
	}
	if c.sandboxNetworkPolicy && c.sandboxNetworkPolicyName == "" {
		return fmt.Errorf("--sandbox-network-policy-name is required with --sandbox-network-policy")
	}
	if c.sandboxLimitRange && c.sandboxLimitRangeName == "" {
		return fmt.Errorf("--sandbox-limit-range-name is required with --sandbox-limit-range")
	}
	if c.gatewayMode == project.GatewayStandalone && c.gatewayConfigMap == "" {
		return fmt.Errorf("--gateway-config-map is required in standalone mode")
	}
	if c.gatewayMode == project.GatewayKubernetes && c.gatewayName == "" {
		return fmt.Errorf("--gateway-name is required in kubernetes mode")
	}

	policy, err := sandboxres.FromConfig(func(key string, out any) error { return v.UnmarshalKey(key, out) })
	if err != nil {
		return err
	}
	c.sandboxResources = policy
	images, err := harnessImages(v)
	if err != nil {
		return err
	}
	c.harnessImages = images
	return nil
}

// harnessImages reads the config file's harnesses section: each harness's image.
func harnessImages(v *viper.Viper) (map[string]string, error) {
	var harnesses map[string]struct {
		Image string `mapstructure:"image"`
	}
	if err := v.UnmarshalKey("harnesses", &harnesses); err != nil {
		return nil, fmt.Errorf("config harnesses: %w", err)
	}
	images := map[string]string{}
	for name, f := range harnesses {
		images[name] = f.Image
	}
	return images, nil
}

func orDefault[T comparable](v *T, def T) {
	var zero T
	if *v == zero {
		*v = def
	}
}

// flags registers the flags of "koinon-reconciler serve".
func (c *config) flags(fs *pflag.FlagSet) {
	fs.StringVar(&c.databaseURL, "database-url", "", "Postgres connection string (default $DATABASE_URL)")
	fs.StringVar(&c.namespace, "namespace", "", "Koinon's namespace, for agent key Secrets (default $POD_NAMESPACE)")
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig file; empty uses the in-cluster config")
	fs.StringVar(&c.logLevel, "log-level", "info", "debug, info, warn or error")
	fs.StringVar(&c.openshellAddress, "openshell-address", "", "OpenShell gateway host:port")
	fs.StringVar(&c.openshellTLSDir, "openshell-tls-dir", "", "directory with tls.crt, tls.key and ca.crt for mTLS; empty connects without TLS")
	fs.StringVar(&c.installMark, "install-mark", "", "this install's random mark (the chart's koinon-install-mark Secret), which labels the OpenShell objects it makes; empty turns the ownership checks off")
	fs.StringVar(&c.runtimeClass, "runtime-class", "", "sandbox runtime class (ADR 0045: kata)")
	fs.StringVar(&c.openshellGateway, "openshell-gateway-id", execution.DefaultGatewayID, "OpenShell's gateway_id, which names the workspace namespaces")
	fs.BoolVar(&c.sandboxObserver, "sandbox-observer", false, "read the pods of sandboxes that never became ready, to say why (ADR 0078); needs the chart's observer ClusterRole")
	fs.StringVar(&c.sandboxObserverClusterRole, "sandbox-observer-cluster-role", "", "the observer ClusterRole, and the name of the RoleBinding to it in each workspace namespace")
	fs.StringVar(&c.sandboxObserverServiceAccount, "sandbox-observer-service-account", "", "the reconciler's ServiceAccount, the subject of those RoleBindings")
	fs.BoolVar(&c.sandboxNetworkPolicy, "sandbox-network-policy", false, "apply a default-deny NetworkPolicy in each OpenShell workspace namespace (spike 8.15, ADR 0045)")
	fs.StringVar(&c.sandboxNetworkPolicyName, "sandbox-network-policy-name", "", "name of the default-deny NetworkPolicy in each workspace namespace")
	fs.BoolVar(&c.sandboxLimitRange, "sandbox-limit-range", false, "apply a LimitRange in each OpenShell workspace namespace so the supervisor and init containers are sized (#76)")
	fs.StringVar(&c.sandboxLimitRangeName, "sandbox-limit-range-name", "", "name of the LimitRange in each workspace namespace")
	c.gatewayMode = project.GatewayStandalone
	fs.Var(&c.gatewayMode, "gateway-mode", "standalone or kubernetes")
	fs.StringVar(&c.gatewayURL, "gateway-url", "", "agentgateway's LLM listener URL as sandboxes reach it; the reconciler also sends its reload probe there")
	fs.StringVar(&c.gatewayConfigMap, "gateway-config-map", "", "ConfigMap holding the standalone agentgateway config; created when missing")
	fs.StringVar(&c.gatewayDeployment, "gateway-deployment", "", "standalone agentgateway Deployment that owns the ConfigMap when the reconciler creates it")
	fs.StringVar(&c.gatewayStatsURL, "gateway-stats-url", "", "agentgateway stats URL, to detect rejected configs")
	fs.StringVar(&c.gatewayName, "gateway-name", "", "name of Koinon's Gateway in kubernetes mode")
	fs.StringVar(&c.gatewayClass, "gateway-class", "agentgateway", "GatewayClass of agentgateway's controller in kubernetes mode")
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
	c.kekWrapping.Flags(fs)
	fs.StringVar(&c.healthAddr, "health-addr", ":8082", "listen address of /healthz and /readyz (not ready until the key-encryption keys are loaded and the schema version is supported); empty disables it")
	fs.StringVar(&c.egressHosts, "egress-allowed-hosts", "", "comma-separated hosts (optionally host:port) providers and MCP servers may name although they are internal")
	fs.StringVar(&c.allowedHosts, "provider-allowed-hosts", "", "deprecated alias of --egress-allowed-hosts, kept for one release")
	fs.DurationVar(&c.keySettle, "provider-key-settle", 0,
		"how long to wait after writing a new provider key file before applying a gateway config that names it (default 90s in standalone mode: kubelet updates a mounted Secret within about a minute; 2s in kubernetes mode: the controller reads the Secret through the API)")
	fs.DurationVar(&c.archiveAfter, "archive-after", 0, "archive a sandbox (delete it and its volume, home checkpointed) after this long stopped (default 168h; negative turns it off; needs checkpoints)")
	fs.DurationVar(&c.idleTimeout, "idle-timeout", 0, "stop a sandbox after this long without an active run (default 10m)")
	fs.DurationVar(&c.parkWarm, "park-warm", 0, "keep a sandbox up this long after its run parked on a2a or input_required (default 15m)")
	fs.IntVar(&c.keepWarmPerOrg, "keep-warm-per-org", 0, "keep-warm sandboxes an org may have (default 2; negative grants none)")
	fs.IntVar(&c.maxRecreates, "max-recreates-per-pass", 0, "sandboxes that may be recreated at once with a changed spec, such as a new supervisor image; the allowance comes back at this many per resync interval (default 4; negative is no cap)")
	fs.StringVar(&c.openshellConfigNS, "openshell-config-namespace", "", "namespace of OpenShell's gateway ConfigMap, to read its supervisor image")
	fs.StringVar(&c.openshellConfigMap, "openshell-config-map", "", "OpenShell's gateway ConfigMap (<fullname>-config); empty leaves the supervisor image out of the sandbox spec hash")
	fs.IntVar(&c.sandboxJobs, "sandbox-jobs", 0, "sandbox start/stop jobs that run at once (default 8)")
	fs.DurationVar(&c.resyncInterval, "resync-interval", 0, "time between passes without a wake-up (default 30s)")
	restorehold.Flag(fs, &c.restoreHold)
	fs.BoolVar(&c.checkpoints, "checkpoints", false, "stop, recreate and restore sandboxes around home checkpoints (koinon-agentd writes them; ADR 0032)")
	fs.StringVar(&c.homeRetention, "home-retention", "", "home volume on archive: delete or retain (default delete)")
	fs.StringVar(&c.githubAPIURL, "github-api-url", "", "GitHub REST API base URL for minting installation tokens (default https://api.github.com)")
	fs.StringVar(&c.mcpGuardURL, "mcp-guard-url", "", "koinon-api's MCP leak guard as agentgateway reaches it, http://host:port (ADR 0091); empty makes the gateway refuse every call to an org's MCP server")
	fs.StringVar(&c.mcpAskURL, "mcp-ask-url", "", "base URL of koinon-api's approval-gated MCP proxy as agentgateway reaches it; empty offers no ask-tier tools of org MCP servers")
	fs.StringVar(&c.koinonToolsURL, "koinon-tools-url", "", "URL of Koinon's own agent tools (ask_user) MCP server as agentgateway reaches it; empty offers no ask_user")
	fs.StringVar(&c.githubToolsURL, "github-tools-url", "", "URL of the GitHub tools MCP server as agentgateway reaches it; empty serves no GitHub tools")
}
