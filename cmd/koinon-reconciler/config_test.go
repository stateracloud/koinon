// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/reconciler"
)

// chartConfig is config.yaml as charts/koinon renders it (standalone gateway).
const chartConfig = `
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
openshell:
  address: openshell.openshell.svc:8080
  tls: /etc/koinon/openshell-tls
  runtimeClassName: kata
reconciler:
  idleTimeout: 10m
  resyncInterval: 30s
  homeRetention: delete
koinonTools:
  url: http://koinon-api-tools.koinon.svc:8081/mcp-koinon
githubTools:
  url: http://koinon-api-tools.koinon.svc:8081/mcp
flavors:
  opencode:
    image: ghcr.io/stateracloud/flavor-opencode:0.1.0
bootstrapAdmin:
  enabled: true
`

func TestReconcilerCommand(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "koinon-reconciler Suite")
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
		c, err := parseConfig(nil, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
		Expect(err).NotTo(HaveOccurred())
		Expect(c.idleTimeout).To(Equal(15 * time.Minute))
		Expect(c.resyncInterval).To(Equal(30 * time.Second))
		Expect(c.homeRetention).To(Equal("delete"))
		Expect(c.runtimeClass).To(Equal("kata"))
		Expect(c.openshellTLSDir).To(Equal("/etc/koinon/openshell-tls"))
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

		f := c.flavors()[store.AgentFlavorOpencode]
		Expect(f.Image).To(Equal("ghcr.io/stateracloud/flavor-opencode:0.1.0"))
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
  providerKeysSecret: koinon-gateway-providers
  providerKeysDir: /secrets/providers
openshell:
  address: openshell.openshell.svc:8080
`
		Expect(os.WriteFile(path, []byte(cfg), 0o600)).To(Succeed())
		env := map[string]string{
			"KOINON_CONFIG": path, "DATABASE_URL": "postgres://x@db/koinon", "POD_NAMESPACE": "koinon", "KOINON_KEK_DIR": "/k",
		}
		c, err := parseConfig(nil, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
		Expect(err).NotTo(HaveOccurred())
		Expect(c.gatewayMode).To(Equal(project.GatewayKubernetes))
		Expect(c.gatewayName).To(Equal("koinon-gateway"))
		Expect(c.gatewayClass).To(Equal("agentgateway"))
		Expect(c.gatewayPort).To(Equal(4000))
		Expect(c.keySettle).To(Equal(2 * time.Second))

		delete(env, "KOINON_CONFIG")
		env["KOINON_GATEWAY_MODE"], env["KOINON_GATEWAY_URL"] = "kubernetes", "http://g:4000"
		env["KOINON_OPENSHELL_ADDRESS"], env["KOINON_PROVIDER_KEYS_SECRET"], env["KOINON_PROVIDER_KEYS_DIR"] = "o:1", "s", "/d"
		_, err = parseConfig(nil, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
		Expect(err).To(MatchError(ContainSubstring("--gateway-name is required in kubernetes mode")))
	})

	It("reads archiveAfter: a negative file value turns the tier off, a flag overrides it, empty leaves the default to the reconciler", func() {
		base := map[string]string{"DATABASE_URL": "postgres://x@db/koinon", "POD_NAMESPACE": "koinon", "KOINON_KEK_DIR": "/k"}
		write := func(archiveAfter string) map[string]string {
			path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
			cfg := "gateway:\n  mode: kubernetes\n  name: g\n  url: http://g:8080\n  port: 8080\n  providerKeysSecret: s\n  providerKeysDir: /d\nopenshell:\n  address: o:1\n"
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
			c, err := parseConfig(args, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
			Expect(err).NotTo(HaveOccurred())
			return c
		}
		Expect(parse(write("-1h")).archiveAfter).To(BeNumerically("<", 0))
		Expect(parse(write("-1h"), "--archive-after=48h").archiveAfter).To(Equal(48 * time.Hour))
		Expect(parse(write("")).archiveAfter).To(BeZero())
	})

	It("requires the database, namespace, OpenShell and gateway settings", func() {
		_, err := parseConfig(nil, func(string) (string, bool) { return "", false })
		Expect(err).To(HaveOccurred())
	})
})
