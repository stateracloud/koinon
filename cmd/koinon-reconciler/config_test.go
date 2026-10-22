// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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
  accessLogOtlp: koinon-api.koinon.svc:4318
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

func TestParseConfigFromChart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(chartConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"KOINON_CONFIG":       path,
		"DATABASE_URL":        "postgres://koinon_reconciler@db/koinon",
		"POD_NAMESPACE":       "koinon",
		"KOINON_IDLE_TIMEOUT": "15m", // flags and env override the file
	}
	c, err := parseConfig(nil, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	if c.idleTimeout != 15*time.Minute || c.resyncInterval != 30*time.Second {
		t.Errorf("durations = %v, %v", c.idleTimeout, c.resyncInterval)
	}
	if c.homeRetention != "delete" || c.runtimeClass != "kata" || c.openshellTLSDir != "/etc/koinon/openshell-tls" ||
		c.accessLogOTLP != "koinon-api.koinon.svc:4318" {
		t.Errorf("unexpected %+v", c)
	}
	ep, err := c.gatewayEndpoint()
	if err != nil || ep != (reconciler.Endpoint{Host: "koinon-gateway.koinon.svc", Port: 8080}) {
		t.Errorf("gateway endpoint = %+v, %v", ep, err)
	}
	if c.models.Provider != "openrouter" || c.models.APIKeyFile != "/secrets/model/api_key" ||
		len(c.models.Models) != 1 || c.models.Models[0].Price.Output != "1.20" || c.models.Overrides == nil {
		t.Errorf("models = %+v", c.models)
	}
	f := c.flavors()[store.AgentFlavorOpencode]
	if f.Image != "ghcr.io/procoduck/flavor-opencode:0.1.0" || len(f.Binaries) != 1 {
		t.Errorf("flavor = %+v", f)
	}
}

func TestParseConfigRequiresSettings(t *testing.T) {
	_, err := parseConfig(nil, func(string) (string, bool) { return "", false })
	if err == nil {
		t.Fatal("want an error without database, namespace, OpenShell and gateway settings")
	}
}
