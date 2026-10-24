// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.yaml.in/yaml/v3"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
)

// The chart writes agentgateway's first config file itself
// (charts/koinon/templates/gateway-standalone.yaml) because the reconciler
// may not have run yet. That file must be the document Render produces for an
// empty State (no providers, no keys: the org's providers come from Postgres and
// the reconciler renders them), so agentgateway's static `config` section (read
// only at startup) does not depend on who wrote the file last.
var _ = Describe("the chart's bootstrap config", func() {
	chart := filepath.Join("..", "..", "..", "charts", "koinon")

	// The Options the reconciler builds from the chart's config.yaml.
	options := standalone.Options{
		Port:          8080,
		DatabaseURL:   "sqlite:///data/agentgateway.db?mode=rwc",
		AccessLogOTLP: &standalone.OTLPExport{HostPort: "koinon-api.koinon.svc:4318"},
	}

	renderChart := func(args ...string) map[string]any {
		GinkgoHelper()
		helm, err := exec.LookPath("helm")
		if err != nil {
			Skip("helm is not installed")
		}
		cmd := exec.Command(helm, append([]string{ //nolint:gosec // fixed test arguments
			"template", "koinon", chart, "--namespace", "koinon",
			"--show-only", "templates/gateway-standalone.yaml",
		}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		Expect(err).NotTo(HaveOccurred(), stderr.String())

		dec := yaml.NewDecoder(bytes.NewReader(out))
		for {
			var obj struct {
				Kind     string                `yaml:"kind"`
				Metadata struct{ Name string } `yaml:"metadata"`
				Data     map[string]string     `yaml:"data"`
			}
			Expect(dec.Decode(&obj)).To(Succeed(), "no gateway config ConfigMap rendered")
			if obj.Kind == "ConfigMap" && obj.Metadata.Name == "koinon-gateway-config" {
				var doc map[string]any
				Expect(yaml.Unmarshal([]byte(obj.Data["config.yaml"]), &doc)).To(Succeed())
				return doc
			}
		}
	}

	renderGo := func(s gateway.State) map[string]any {
		GinkgoHelper()
		body, rev, err := standalone.Render(s, options)
		Expect(err).NotTo(HaveOccurred())
		var doc map[string]any
		Expect(yaml.Unmarshal(body, &doc)).To(Succeed())
		// The revision is the one intended difference: the chart cannot compute
		// Render's content hash, so its file is for standalone.BootstrapRevision.
		// Swap in that revision's marker and probe key hash.
		add := doc["frontendPolicies"].(map[string]any)["accessLog"].(map[string]any)["add"].(map[string]any)
		Expect(add).To(HaveKeyWithValue(standalone.MarkerField, strconv.Quote(string(rev))))
		add[standalone.MarkerField] = strconv.Quote(string(standalone.BootstrapRevision))
		keys := doc["llm"].(map[string]any)["policies"].(map[string]any)["apiKey"].(map[string]any)["keys"].([]any)
		probe := keys[len(keys)-1].(map[string]any)
		Expect(probe).To(HaveKeyWithValue("keyHash", gateway.HashKey(standalone.ReloadProbeKey(rev))))
		probe["keyHash"] = gateway.HashKey(standalone.ReloadProbeKey(standalone.BootstrapRevision))
		return doc
	}

	It("matches Render of an empty State", func() {
		Expect(renderChart()).To(Equal(renderGo(gateway.State{})))
	})
})
