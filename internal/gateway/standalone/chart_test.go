// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	"bytes"
	"os/exec"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.yaml.in/yaml/v3"
)

// The chart does not render agentgateway's config ConfigMap: a render without cluster access
// (helm template, Argo CD, Flux) cannot read the live one, so rendering it would reset the
// reconciler's config on every sync. The reconciler creates it (ConfigMapDeliverer), so the first
// config is Render's document and nothing else has to agree with it.
var _ = Describe("the chart and the gateway config", func() {
	chart := filepath.Join("..", "..", "..", "charts", "koinon")

	render := func(args ...string) []map[string]any {
		GinkgoHelper()
		helm, err := exec.LookPath("helm")
		if err != nil {
			Skip("helm is not installed")
		}
		cmd := exec.Command(helm, append([]string{
			"template", "koinon", chart, "--namespace", "koinon",
			"--set", "gateway.mode=standalone", "--set", "openshell.insecure=true",
		}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		Expect(err).NotTo(HaveOccurred(), stderr.String())

		var objs []map[string]any
		dec := yaml.NewDecoder(bytes.NewReader(out))
		for {
			var obj map[string]any
			if dec.Decode(&obj) != nil {
				return objs
			}
			if obj != nil {
				objs = append(objs, obj)
			}
		}
	}

	It("renders no gateway config ConfigMap, but the gateway Deployment that mounts it", func() {
		var mounted bool
		for _, obj := range render() {
			name := obj["metadata"].(map[string]any)["name"]
			Expect(obj["kind"] == "ConfigMap" && name == "koinon-gateway-config").To(BeFalse(), "the chart renders the gateway config")
			if obj["kind"] == "Deployment" && name == "koinon-gateway" {
				mounted = bytes.Contains(mustYAML(obj), []byte("name: koinon-gateway-config"))
			}
		}
		Expect(mounted).To(BeTrue(), "the gateway Deployment mounts koinon-gateway-config")
	})
})

func mustYAML(v any) []byte {
	b, err := yaml.Marshal(v)
	Expect(err).NotTo(HaveOccurred())
	return b
}
