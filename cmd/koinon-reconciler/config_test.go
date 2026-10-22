// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/db/store"
	"github.com/procoduck/koinon/internal/reconciler"
)

// chartConfig is config.yaml as charts/koinon renders it (standalone gateway, one model).
const chartConfig = `
gateway:
  mode: standalone
  configMap: koinon-gateway-config
  url: http://koinon-gateway.koinon.svc:8080
  adminUrl: http://koinon-gateway.koinon.svc:15000
  statsUrl: http://koinon-gateway.koinon.svc:15020
  port: 8080
  databaseUrl: sqlite:///data/agentgateway.db?mode=rwc
  modelKeyFile: /secrets/model/api_key
model:
  apiKeySecret: {key: api_key, name: upstream}
  baseUrl: https://openrouter.ai/api/v1
  models:
    - name: deepseek/deepseek-v4.1-flash
      rates: {input: "0.30", output: "1.20"}
      overrides: {provider: {zdr: true}}
  provider: openrouter
openshell:
  address: openshell.openshell.svc:8080
  tls: /etc/koinon/openshell-tls
  runtimeClassName: kata
reconciler:
  idleTimeout: 10m
  resyncInterval: 30s
  homeRetention: delete
flavors:
  opencode:
    image: ghcr.io/procoduck/flavor-opencode:0.1.0
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
		}
		c, err := parseConfig(nil, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
		Expect(err).NotTo(HaveOccurred())
		Expect(c.idleTimeout).To(Equal(15 * time.Minute))
		Expect(c.resyncInterval).To(Equal(30 * time.Second))
		Expect(c.homeRetention).To(Equal("delete"))
		Expect(c.runtimeClass).To(Equal("kata"))
		Expect(c.openshellTLSDir).To(Equal("/etc/koinon/openshell-tls"))

		ep, err := c.gatewayEndpoint()
		Expect(err).NotTo(HaveOccurred())
		Expect(ep).To(Equal(reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080}))

		Expect(c.models.Provider).To(Equal("openrouter"))
		Expect(c.models.APIKeyFile).To(Equal("/secrets/model/api_key"))
		Expect(c.models.Models).To(HaveLen(1))
		Expect(c.models.Models[0].Price.Output).To(Equal("1.20"))
		Expect(c.models.Overrides).NotTo(BeNil())

		f := c.flavors()[store.AgentFlavorOpencode]
		Expect(f.Image).To(Equal("ghcr.io/procoduck/flavor-opencode:0.1.0"))
		Expect(f.Binaries).To(HaveLen(1))
	})

	It("requires the database, namespace, OpenShell and gateway settings", func() {
		_, err := parseConfig(nil, func(string) (string, bool) { return "", false })
		Expect(err).To(HaveOccurred())
	})
})
