// SPDX-License-Identifier: Apache-2.0

package koinonreconciler

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/internal/sandboxres"
	"github.com/stateracloud/koinon/templates"
)

// chartConfig is config.yaml as charts/koinon renders it for an OpenShell install (standalone gateway,
// execution.driver=openshell: the default driver is substrate since #1093).
const chartConfig = `
execution:
  driver: openshell
gateway:
  mode: standalone
  configMap: koinon-gateway-config
  url: http://koinon-gateway.koinon.svc:8080
  statsUrl: http://koinon-gateway.koinon.svc:15020
  port: 8080
  databaseUrl: sqlite:///data/agentgateway.db?mode=rwc
  providerKeysSecret: koinon-gateway-providers
  providerKeysDir: /secrets/providers
  accessLogOtlp: koinon-api.koinon.svc:4318
  accessLogOtlpTokenFile: /secrets/otlp/token
openshell:
  address: openshell.openshell.svc:8080
  tls: /etc/koinon/openshell-tls
  runtimeClassName: kata
  gatewayId: prod
sandboxObserver:
  enabled: true
  clusterRole: koinon-koinon-sandbox-observer
  serviceAccount: koinon-reconciler
sandboxNetworkPolicy:
  enabled: true
  name: koinon-koinon-sandbox-default-deny
sandboxLimitRange:
  enabled: true
  name: koinon-koinon-sandbox-limitrange
reconciler:
  idleTimeout: 10m
  resyncInterval: 30s
  homeRetention: delete
koinonTools:
  url: http://koinon-api-tools.koinon.svc:8081/mcp-koinon
githubTools:
  url: http://koinon-api-tools.koinon.svc:8081/mcp
harnesses:
  opencode:
    image: ghcr.io/stateracloud/koinon/sandbox:0.1.0
bootstrapAdmin:
  enabled: true
`

// parseConfig runs koinon-reconciler's real startup (flags, KOINON_* environment, config file, defaults
// and rules) and returns the settings serve would get. The process environment is cleared of every
// variable the command reads, then set to env.
func parseConfig(args []string, env map[string]string) (config, error) {
	var got config
	root := newRootCommand(func(_ context.Context, c config) error { got = c; return nil })
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, cli.EnvPrefix) || name == "DATABASE_URL" || name == "POD_NAMESPACE" {
			old := os.Getenv(name)
			Expect(os.Unsetenv(name)).To(Succeed())
			DeferCleanup(func() { _ = os.Setenv(name, old) })
		}
	}
	for k, v := range env {
		GinkgoT().Setenv(k, v)
	}
	root.SetArgs(cli.DefaultSubcommand(root, args))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return got, root.Execute()
}

func TestReconcilerCommand(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "koinon-reconciler Suite")
}

var _ = Describe("parseConfig for the sandbox observer", func() {
	parse := func(file string, args ...string) (config, error) {
		path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(path, []byte(file), 0o600)).To(Succeed())
		env := map[string]string{
			"KOINON_CONFIG": path, "DATABASE_URL": "postgres://x@db/koinon", "POD_NAMESPACE": "koinon", "KOINON_KEK_DIR": "/k",
		}
		return parseConfig(args, env)
	}

	It("is off and uses OpenShell's default gateway ID when the chart does not set them", func() {
		c, err := parse(stripObserver(chartConfig))
		Expect(err).NotTo(HaveOccurred())
		Expect(c.sandboxObserver).To(BeFalse())
		Expect(c.openshellGateway).To(Equal("openshell"))
		Expect(c.otlpTokenFile).To(Equal("/secrets/otlp/token"))
	})

	It("reads the cluster domain from the flag, and refuses a value that is not a DNS name (#1054)", func() {
		c, err := parse(stripObserver(chartConfig))
		Expect(err).NotTo(HaveOccurred())
		Expect(c.clusterDomain).To(BeEmpty(), "empty means the default, applied where the policy is built")
		c, err = parse(stripObserver(chartConfig), "--cluster-domain=corp.example")
		Expect(err).NotTo(HaveOccurred())
		Expect(c.clusterDomain).To(Equal("corp.example"))
		_, err = parse(stripObserver(chartConfig), "--cluster-domain=Corp.Example.")
		Expect(err).To(MatchError(ContainSubstring("--cluster-domain")))
	})

	It("is off when the chart says enabled: false", func() {
		c, err := parse(stripObserver(chartConfig) + "sandboxObserver:\n  enabled: false\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(c.sandboxObserver).To(BeFalse())
	})

	It("lets an explicit flag or variable override the file in both directions", func() {
		c, err := parse(chartConfig, "--sandbox-observer=false")
		Expect(err).NotTo(HaveOccurred())
		Expect(c.sandboxObserver).To(BeFalse(), "the flag turns off what the file turns on")

		c, err = parse(chartConfig)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.sandboxObserver).To(BeTrue(), "no flag: the file")

		c, err = parse(stripObserver(chartConfig)+"sandboxObserver:\n  enabled: false\n  clusterRole: r\n  serviceAccount: s\n", "--sandbox-observer")
		Expect(err).NotTo(HaveOccurred())
		Expect(c.sandboxObserver).To(BeTrue(), "the flag turns on what the file turns off")
	})

	It("lets KOINON_SANDBOX_OBSERVER override the file", func() {
		path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(path, []byte(chartConfig), 0o600)).To(Succeed())
		env := map[string]string{
			"KOINON_CONFIG": path, "DATABASE_URL": "postgres://x@db/koinon", "POD_NAMESPACE": "koinon", "KOINON_KEK_DIR": "/k",
			"KOINON_SANDBOX_OBSERVER": "false",
		}
		c, err := parseConfig(nil, env)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.sandboxObserver).To(BeFalse())
	})

	It("needs its ClusterRole and ServiceAccount", func() {
		_, err := parse(stripObserver(chartConfig), "--sandbox-observer")
		Expect(err).To(MatchError(ContainSubstring("--sandbox-observer-cluster-role")))
	})
})

// stripObserver removes the sandboxObserver block and the gateway ID from chartConfig.
func stripObserver(cfg string) string {
	out := strings.Replace(cfg, "  gatewayId: prod\n", "", 1)
	return strings.Replace(out, "sandboxObserver:\n  enabled: true\n  clusterRole: koinon-koinon-sandbox-observer\n  serviceAccount: koinon-reconciler\n", "", 1)
}

var _ = Describe("parseConfig", func() {
	It("reads the config file as the chart renders it, with env overriding the file", func() {
		path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(path, []byte(chartConfig), 0o600)).To(Succeed())
		env := map[string]string{
			"KOINON_CONFIG":       path,
			"DATABASE_URL":        "postgres://koinon_reconciler@db/koinon",
			"POD_NAMESPACE":       "koinon",
			"KOINON_IDLE_TIMEOUT": "15m", // flags and env override the file
			"KOINON_KEK_DIR":      "/secrets/kek",
		}
		c, err := parseConfig(nil, env)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.idleTimeout).To(Equal(15 * time.Minute))
		Expect(c.resyncInterval).To(Equal(30 * time.Second))
		Expect(c.homeRetention).To(Equal("delete"))
		Expect(c.runtimeClass).To(Equal("kata"))
		Expect(c.openshellTLSDir).To(Equal("/etc/koinon/openshell-tls"))
		Expect(c.openshellGateway).To(Equal("prod"))
		Expect(c.sandboxObserver).To(BeTrue())
		Expect(c.sandboxObserverClusterRole).To(Equal("koinon-koinon-sandbox-observer"))
		Expect(c.sandboxObserverServiceAccount).To(Equal("koinon-reconciler"))
		Expect(c.sandboxNetworkPolicy).To(BeTrue())
		Expect(c.sandboxNetworkPolicyName).To(Equal("koinon-koinon-sandbox-default-deny"))
		Expect(c.sandboxLimitRange).To(BeTrue())
		Expect(c.sandboxLimitRangeName).To(Equal("koinon-koinon-sandbox-limitrange"))
		Expect(c.accessLogOTLP).To(Equal("koinon-api.koinon.svc:4318"))
		Expect(c.gatewayURL).To(Equal("http://koinon-gateway.koinon.svc:8080"))
		Expect(c.gatewayStatsURL).To(Equal("http://koinon-gateway.koinon.svc:15020"))

		ep, err := c.gatewayEndpoint()
		Expect(err).NotTo(HaveOccurred())
		Expect(ep).To(Equal(reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080}))

		Expect(c.providerKeysSecret).To(Equal("koinon-gateway-providers"))
		Expect(c.providerKeysDir).To(Equal("/secrets/providers"))
		Expect(c.kekDir).To(Equal("/secrets/kek"))

		Expect(c.toolServers()).To(Equal(map[string]string{
			"github": "http://koinon-api-tools.koinon.svc:8081/mcp",
			"gitea":  "http://koinon-api-tools.koinon.svc:8081/mcp",
			"koinon": "http://koinon-api-tools.koinon.svc:8081/mcp-koinon", // ask_user (TK3)
		}))

		f := c.harnesses()[harness.OpenCode]
		Expect(f.Image).To(Equal("ghcr.io/stateracloud/koinon/sandbox:0.1.0"))
		Expect(f.Binaries).To(HaveLen(1))
	})

	It("reads the Kubernetes-mode config and defaults the key settle time to seconds, not the kubelet's minute", func() {
		path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		cfg := `
gateway:
  mode: kubernetes
  name: koinon-gateway
  gatewayClassName: agentgateway
  url: http://koinon-gateway.koinon.svc.cluster.local:4000
  port: 4000
  accessLogOtlp: koinon-api.koinon.svc:4318
  accessLogOtlpTokenSecret: koinon-otlp-token
  providerKeysSecret: koinon-gateway-providers
  providerKeysDir: /secrets/providers
execution:
  driver: openshell
openshell:
  address: openshell.openshell.svc:8080
`
		Expect(os.WriteFile(path, []byte(cfg), 0o600)).To(Succeed())
		env := map[string]string{
			"KOINON_CONFIG": path, "DATABASE_URL": "postgres://x@db/koinon", "POD_NAMESPACE": "koinon", "KOINON_KEK_DIR": "/k",
		}
		c, err := parseConfig(nil, env)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.gatewayMode).To(Equal(project.GatewayKubernetes))
		Expect(c.gatewayName).To(Equal("koinon-gateway"))
		Expect(c.gatewayClass).To(Equal("agentgateway"))
		Expect(c.gatewayPort).To(Equal(4000))
		Expect(c.otlpTokenSecret).To(Equal("koinon-otlp-token"))
		Expect(c.keySettle).To(Equal(2 * time.Second))

		delete(env, "KOINON_CONFIG")
		env["KOINON_GATEWAY_MODE"], env["KOINON_GATEWAY_URL"] = "kubernetes", "http://g:4000"
		env["KOINON_OPENSHELL_ADDRESS"], env["KOINON_PROVIDER_KEYS_SECRET"], env["KOINON_PROVIDER_KEYS_DIR"] = "o:1", "s", "/d"
		env["KOINON_EXECUTION_DRIVER"] = "openshell"
		_, err = parseConfig(nil, env)
		Expect(err).To(MatchError(ContainSubstring("--gateway-name is required in kubernetes mode")))
	})

	// gateway.mcp.driver=koinon (#1239): the sandbox profile allows /mcp on koinon-gateway's MCP listener.
	It("reads koinon-gateway's MCP URL, needs standalone mode for it and leaves it empty by default", func() {
		path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		withMCP := strings.Replace(chartConfig, "  port: 8080\n", "  port: 8080\n  mcpUrl: http://koinon-mcp-gateway.koinon.svc:8080\n", 1)
		Expect(os.WriteFile(path, []byte(withMCP), 0o600)).To(Succeed())
		env := map[string]string{
			"KOINON_CONFIG": path, "DATABASE_URL": "postgres://x@db/koinon", "POD_NAMESPACE": "koinon", "KOINON_KEK_DIR": "/k",
		}
		c, err := parseConfig(nil, env)
		Expect(err).NotTo(HaveOccurred())
		ep, err := c.mcpGatewayEndpoint()
		Expect(err).NotTo(HaveOccurred())
		Expect(ep).To(Equal(reconciler.Endpoint{Host: "koinon-mcp-gateway.koinon.svc", Port: 8080}))

		env["KOINON_GATEWAY_MCP_URL"] = "koinon-mcp-gateway:8080"
		_, err = parseConfig(nil, env)
		Expect(err).To(MatchError(ContainSubstring("--gateway-mcp-url")))

		env["KOINON_GATEWAY_MCP_URL"] = "http://m:8080"
		env["KOINON_GATEWAY_MODE"], env["KOINON_GATEWAY_NAME"] = "kubernetes", "koinon-gateway"
		_, err = parseConfig(nil, env)
		Expect(err).To(MatchError(ContainSubstring("--gateway-mcp-url needs standalone mode")))

		Expect(os.WriteFile(path, []byte(chartConfig), 0o600)).To(Succeed())
		delete(env, "KOINON_GATEWAY_MCP_URL")
		delete(env, "KOINON_GATEWAY_MODE")
		delete(env, "KOINON_GATEWAY_NAME")
		c, err = parseConfig(nil, env)
		Expect(err).NotTo(HaveOccurred())
		ep, err = c.mcpGatewayEndpoint()
		Expect(err).NotTo(HaveOccurred())
		Expect(ep).To(Equal(reconciler.Endpoint{}), "agentgateway serves /mcp")
	})

	It("reads archiveAfter: a negative file value turns the tier off, a flag overrides it, empty leaves the default to the reconciler", func() {
		base := map[string]string{"DATABASE_URL": "postgres://x@db/koinon", "POD_NAMESPACE": "koinon", "KOINON_KEK_DIR": "/k"}
		write := func(archiveAfter string) map[string]string {
			path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
			cfg := "gateway:\n  mode: kubernetes\n  name: g\n  url: http://g:8080\n  port: 8080\n  providerKeysSecret: s\n  providerKeysDir: /d\nexecution:\n  driver: openshell\nopenshell:\n  address: o:1\n"
			if archiveAfter != "" {
				cfg += "reconciler:\n  archiveAfter: " + archiveAfter + "\n"
			}
			Expect(os.WriteFile(path, []byte(cfg), 0o600)).To(Succeed())
			env := map[string]string{"KOINON_CONFIG": path}
			for k, v := range base {
				env[k] = v
			}
			return env
		}
		parse := func(env map[string]string, args ...string) config {
			c, err := parseConfig(args, env)
			Expect(err).NotTo(HaveOccurred())
			return c
		}
		Expect(parse(write("-1h")).archiveAfter).To(BeNumerically("<", 0))
		Expect(parse(write("-1h"), "--archive-after=48h").archiveAfter).To(Equal(48 * time.Hour))
		Expect(parse(write("")).archiveAfter).To(BeZero())
	})

	It("requires the database, namespace, OpenShell and gateway settings", func() {
		_, err := parseConfig(nil, nil)
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("parseConfig for the sandbox resources", func() {
	parse := func(file string) (config, error) {
		path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(path, []byte(file), 0o600)).To(Succeed())
		env := map[string]string{
			"KOINON_CONFIG": path, "DATABASE_URL": "postgres://x@db/koinon", "POD_NAMESPACE": "koinon", "KOINON_KEK_DIR": "/k",
		}
		return parseConfig(nil, env)
	}

	It("uses the built-in policy when the file has no sandboxResources", func() {
		c, err := parse(chartConfig)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.sandboxResources).To(Equal(sandboxres.DefaultPolicy()))
	})

	It("lays the file's values over the defaults", func() {
		c, err := parse(chartConfig + `
sandboxResources:
  default:
    cpu: {request: 500m}
    memory: {limit: 4Gi}
  bounds:
    cpu:
      limit: {max: "8"}
    memory:
      request: {min: 256Mi}
      limit: {min: 256Mi}
  memoryRequestEqualsLimit: false
`)
		Expect(err).NotTo(HaveOccurred())
		p := c.sandboxResources
		Expect(p.Default.CPU).To(Equal(templates.Quantities{Request: "500m", Limit: "2"}))
		Expect(p.Default.Memory).To(Equal(templates.Quantities{Request: "1Gi", Limit: "4Gi"}))
		Expect(p.Bounds.CPULimit).To(Equal(sandboxres.Range{Min: "100m", Max: "8"}))
		Expect(p.Bounds.MemoryRequest).To(Equal(sandboxres.Range{Min: "256Mi", Max: "8Gi"}))
		Expect(p.MemoryRequestEqualsLimit).To(BeFalse())
	})

	DescribeTable("refuses to start on",
		func(section, want string) {
			_, err := parse(chartConfig + section)
			Expect(err).To(MatchError(ContainSubstring(want)))
		},
		Entry("a bad quantity", "sandboxResources:\n  default:\n    memory: {limit: 2 gigs}\n", "memory.limit"),
		Entry("a bad bound", "sandboxResources:\n  bounds:\n    cpu:\n      limit: {max: lots}\n", `bounds.cpuLimit.max "lots"`),
		Entry("a minimum above the maximum", "sandboxResources:\n  bounds:\n    memory:\n      limit: {min: 16Gi}\n", "bounds.memoryLimit: min 16Gi is greater than max 8Gi"),
		Entry("a cpu limit range below the request minimum", "sandboxResources:\n  bounds:\n    cpu:\n      limit: {min: 50m}\n", "bounds.cpu: the limit range"),
		Entry("a default request above its limit", "sandboxResources:\n  default:\n    cpu: {request: 4}\n", "cpu.request 4 is greater than cpu.limit 2"),
	)
})

var _ = Describe("koinon-reconciler's settings", func() {
	write := func(file string) string {
		path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(path, []byte(file), 0o600)).To(Succeed())
		return path
	}
	base := func(path string) map[string]string {
		return map[string]string{"KOINON_CONFIG": path, "DATABASE_URL": "postgres://x@db/koinon", "POD_NAMESPACE": "koinon", "KOINON_KEK_DIR": "/k"}
	}

	It("takes a setting from the flag, then the environment, then the file, then the default", func() {
		env := base(write(chartConfig))
		c, err := parseConfig(nil, env)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.idleTimeout).To(Equal(10*time.Minute), "the file")
		Expect(c.healthAddr).To(Equal(":8082"), "the default")

		env["KOINON_IDLE_TIMEOUT"] = "15m"
		c, err = parseConfig(nil, env)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.idleTimeout).To(Equal(15*time.Minute), "the environment beats the file")

		c, err = parseConfig([]string{"--idle-timeout=20m"}, env)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.idleTimeout).To(Equal(20*time.Minute), "the flag beats the environment")
	})

	It("takes the config file from --config as well as KOINON_CONFIG", func() {
		env := base("")
		delete(env, "KOINON_CONFIG")
		c, err := parseConfig([]string{"--config", write(chartConfig)}, env)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.openshellAddress).To(Equal("openshell.openshell.svc:8080"))
	})

	It("leaves a flag's default where the file has an empty value, as the chart renders an unset one", func() {
		env := base(write(strings.Replace(chartConfig, "  gatewayId: prod\n", "  gatewayId: \"\"\n", 1)))
		c, err := parseConfig(nil, env)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.openshellGateway).To(Equal("openshell"))
	})

	It("names the unreadable config file and the key whose value does not parse", func() {
		env := base(filepath.Join(GinkgoT().TempDir(), "missing.yaml"))
		_, err := parseConfig(nil, env)
		Expect(err).To(MatchError(ContainSubstring("read config")))

		_, err = parseConfig(nil, base(write(strings.Replace(chartConfig, "idleTimeout: 10m", "idleTimeout: soon", 1))))
		Expect(err).To(MatchError(ContainSubstring("config key reconciler.idleTimeout")))
	})

	DescribeTable("refuses to start without",
		func(drop string) {
			env := base(write(chartConfig))
			delete(env, drop)
			if drop == "KOINON_CONFIG" {
				env["KOINON_KEK_DIR"] = "/k"
			}
			_, err := parseConfig(nil, env)
			Expect(err).To(HaveOccurred())
		},
		Entry("the database URL", "DATABASE_URL"),
		Entry("the namespace", "POD_NAMESPACE"),
		Entry("the KEK directory (3151301a)", "KOINON_KEK_DIR"),
		Entry("the config file, which holds the OpenShell and gateway settings", "KOINON_CONFIG"),
	)

	It("names every missing required setting", func() {
		_, err := parseConfig(nil, nil)
		Expect(err).To(MatchError(And(ContainSubstring("database-url"), ContainSubstring("kek-dir"), ContainSubstring("gateway-url"))))
	})

	It("takes the gateway's egress URL with sealed MCP credentials, and refuses it alone or malformed (#1086 part C2)", func() {
		env := base(write(chartConfig))
		c, err := parseConfig(nil, env)
		Expect(err).NotTo(HaveOccurred())
		ep, err := c.egressEndpoint()
		Expect(err).NotTo(HaveOccurred())
		Expect(ep).To(BeNil(), "off by default")

		c, err = parseConfig([]string{"--gateway-mcp-sealed", "--gateway-egress-url=http://koinon-mcp-gateway.koinon.svc:8081"}, env)
		Expect(err).NotTo(HaveOccurred())
		ep, err = c.egressEndpoint()
		Expect(err).NotTo(HaveOccurred())
		Expect(ep.Host).To(Equal("koinon-mcp-gateway.koinon.svc"))
		Expect(ep.Port).To(BeEquivalentTo(8081))

		_, err = parseConfig([]string{"--gateway-egress-url=http://koinon-mcp-gateway.koinon.svc:8081"}, env)
		Expect(err).To(MatchError(ContainSubstring("--gateway-mcp-sealed")))
		_, err = parseConfig([]string{"--gateway-mcp-sealed", "--gateway-egress-url=::not a url"}, env)
		Expect(err).To(MatchError(ContainSubstring("--gateway-egress-url")))
	})

	It("refuses an unknown flag and an environment variable nothing reads", func() {
		_, err := parseConfig([]string{"--no-such-flag"}, base(write(chartConfig)))
		Expect(err).To(MatchError(ContainSubstring("unknown flag")))
		err = cli.Check(NewRootCommand(), nil, map[string]string{"KOINON_NO_SUCH": "1"})
		Expect(err).To(MatchError(ContainSubstring("KOINON_NO_SUCH")))
	})
})

var _ = Describe("parseConfig with the substrate driver (#1192)", func() {
	// The fake references use a reserved test domain: the dependency age check skips it (ADR 0079).
	const pinned = "registry.example.com/koinon/sandbox:0.1.0@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// substrateConfig is chartConfig without the OpenShell-only features, with the harness image given.
	substrateConfig := func(image string) string {
		out := strings.ReplaceAll(stripObserver(chartConfig), "  enabled: true\n  name:", "  enabled: false\n  name:")
		return strings.Replace(out, "image: ghcr.io/stateracloud/koinon/sandbox:0.1.0\n", "image: "+image+"\n", 1)
	}
	parse := func(file, driver string) (config, error) {
		path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(path, []byte(file), 0o600)).To(Succeed())
		// The guest API signing key (#1309), an obviously fake seed.
		key := filepath.Join(GinkgoT().TempDir(), "guest-key")
		Expect(os.WriteFile(key, []byte(strings.Repeat("11", 32)), 0o600)).To(Succeed())
		env := map[string]string{
			"KOINON_CONFIG": path, "DATABASE_URL": "postgres://x@db/koinon", "POD_NAMESPACE": "koinon", "KOINON_KEK_DIR": "/k",
		}
		return parseConfig([]string{
			"--execution-driver=" + driver,
			"--substrate-control-address=api.ate-system.svc:443", "--substrate-router-address=atenet-router.ate-system.svc:443",
			"--substrate-allow-unhardened", "--substrate-default-tier=dev", "--substrate-guest-key-file=" + key,
			"--gateway-model-url=http://koinon-model-gateway.koinon.svc:8080", "--gateway-mcp-url=http://koinon-mcp-gateway.koinon.svc:8080",
		}, env)
	}

	DescribeTable("needs every harness image pinned by digest",
		func(image string, ok bool) {
			c, err := parse(substrateConfig(image), cli.DriverSubstrate)
			if ok {
				Expect(err).NotTo(HaveOccurred())
				Expect(c.harnesses()[harness.OpenCode].Image).To(Equal(image))
				return
			}
			Expect(err).To(MatchError(And(
				ContainSubstring("harnesses.opencode.image"),
				ContainSubstring("not pinned by digest"),
			)))
		},
		Entry("tag only is refused", "registry.example.com/koinon/sandbox:0.1.0", false),
		Entry("digest only is accepted", "registry.example.com/koinon/sandbox@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", true),
		Entry("tag and digest is accepted", pinned, true),
	)

	It("does not apply to the OpenShell driver", func() {
		_, err := parse(chartConfig, cli.DriverOpenShell)
		Expect(err).NotTo(HaveOccurred())
	})

	It("allows koinon-gateway's listeners in the actor egress policy, each with its audience, keeps the egress listener apart, and refuses a URL it cannot carry (#1444, #1489)", func() {
		c, err := parse(substrateConfig(pinned), cli.DriverSubstrate)
		Expect(err).NotTo(HaveOccurred())
		routes, egress, err := c.substrateGatewayRoutes()
		Expect(err).NotTo(HaveOccurred())
		// agentgateway (--gateway-url) is not a route: the guest calls only koinon-gateway, which checks the actor JWT.
		Expect(routes).To(Equal([]substrate.GatewayRoute{
			substrate.ModelRoute("koinon-model-gateway.koinon.svc", 8080),
			substrate.MCPRoute("koinon-mcp-gateway.koinon.svc", 8080),
		}))
		Expect(egress).To(BeNil())

		c.egressURL = "http://koinon-mcp-gateway.koinon.svc:8081"
		_, egress, err = c.substrateGatewayRoutes()
		Expect(err).NotTo(HaveOccurred())
		// The egress listener is not a route of every actor: only an agent with a code-host grant gets it.
		Expect(egress).To(Equal(ptrTo(substrate.EgressRoute("koinon-mcp-gateway.koinon.svc", 8081))))

		// An egress listener on the port of a route of every actor cannot be gated, so it is refused.
		c.egressURL = "http://koinon-mcp-gateway.koinon.svc:8080"
		_, _, err = c.substrateGatewayRoutes()
		Expect(err).To(MatchError(ContainSubstring("cannot gate it")))

		for _, bad := range []string{"https://koinon-model-gateway.koinon.svc:8443", "http://10.96.0.10:8080", "http://Gateway.koinon.svc:8080", ""} {
			c.gatewayModelURL, c.egressURL = bad, ""
			_, _, err = c.substrateGatewayRoutes()
			Expect(err).To(MatchError(ContainSubstring("--gateway-model-url")), bad)
		}
		c.gatewayModelURL, c.gatewayMCPURL = "http://koinon-model-gateway.koinon.svc:8080", ""
		_, _, err = c.substrateGatewayRoutes()
		Expect(err).To(MatchError(ContainSubstring("--gateway-mcp-url is required")))
		c.gatewayMCPURL, c.egressURL = "http://koinon-mcp-gateway.koinon.svc:8080", "https://koinon-mcp-gateway.koinon.svc:8443"
		_, _, err = c.substrateGatewayRoutes()
		Expect(err).To(MatchError(ContainSubstring("--gateway-egress-url")))
	})

	It("refuses to start without koinon-gateway's model listener (#1489)", func() {
		path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(path, []byte(substrateConfig(pinned)), 0o600)).To(Succeed())
		key := filepath.Join(GinkgoT().TempDir(), "guest-key")
		Expect(os.WriteFile(key, []byte(strings.Repeat("11", 32)), 0o600)).To(Succeed())
		_, err := parseConfig([]string{
			"--execution-driver=" + cli.DriverSubstrate,
			"--substrate-control-address=api.ate-system.svc:443", "--substrate-router-address=atenet-router.ate-system.svc:443",
			"--substrate-allow-unhardened", "--substrate-default-tier=dev", "--substrate-guest-key-file=" + key,
			"--gateway-mcp-url=http://koinon-mcp-gateway.koinon.svc:8080",
		}, map[string]string{"KOINON_CONFIG": path, "DATABASE_URL": "postgres://x@db/koinon", "POD_NAMESPACE": "koinon", "KOINON_KEK_DIR": "/k"})
		Expect(err).To(MatchError(ContainSubstring("--gateway-model-url is required with --execution-driver=substrate")))
	})
})

func ptrTo[T any](v T) *T { return &v }
