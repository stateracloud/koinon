// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	"bytes"
	"context"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.yaml.in/yaml/v3"

	"github.com/stateracloud/koinon/internal/testing/agentgatewayimage"
	"github.com/stateracloud/koinon/internal/testing/cfgdeliver"
	"github.com/stateracloud/koinon/internal/testing/containerhost"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// The chart does not render agentgateway's config ConfigMap: a render without cluster access
// (helm template, Argo CD, Flux) cannot read the live one, so rendering it would reset the
// reconciler's config on every sync. The reconciler creates it (ConfigMapDeliverer), so the first
// config is Render's document and nothing else has to agree with it.
var _ = Describe("the chart and the gateway config", func() {
	chart := filepath.Join("..", "..", "..", "charts", "koinon")

	render := func(args ...string) []map[string]any {
		GinkgoHelper()
		testenv.Require(testenv.Binary("helm"))
		helm, err := exec.LookPath("helm")
		Expect(err).NotTo(HaveOccurred())
		cmd := exec.Command(helm, append([]string{
			"template", "koinon", chart, "--namespace", "koinon",
			"--set", "gateway.mode=standalone", "--set", "openshell.insecure=true",
			"--set", "openshell.address=openshell.openshell.svc:8080",
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

// A held install (restoreHold, ADR 0107 amendment, issue #530) has no reconciler config, so the chart
// renders a fixed deny-all one. agentgateway must accept it, report ready (helm --wait and GitOps
// health depend on it) and refuse every request, a model request with a plausible key included.
var _ = Describe("the held gateway config", func() {
	heldConfig := func() []byte {
		GinkgoHelper()
		testenv.Require(testenv.Binary("helm"))
		helm, err := exec.LookPath("helm")
		Expect(err).NotTo(HaveOccurred())
		var stderr bytes.Buffer
		cmd := exec.Command(helm, "template", "koinon", filepath.Join("..", "..", "..", "charts", "koinon"),
			"--namespace", "koinon", "--set", "gateway.mode=standalone", "--set", "openshell.insecure=true",
			"--set", "openshell.address=openshell.openshell.svc:8080", "--set", "restoreHold=true",
			"--show-only", "templates/gateway-standalone.yaml")
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		Expect(err).NotTo(HaveOccurred(), stderr.String())
		dec := yaml.NewDecoder(bytes.NewReader(out))
		for {
			var obj struct {
				Kind     string `yaml:"kind"`
				Metadata struct {
					Name string `yaml:"name"`
				} `yaml:"metadata"`
				Data map[string]string `yaml:"data"`
			}
			if dec.Decode(&obj) != nil {
				Fail("the held render has no koinon-gateway-config-held ConfigMap")
			}
			if obj.Kind == "ConfigMap" && obj.Metadata.Name == "koinon-gateway-config-held" {
				return []byte(obj.Data["config.yaml"])
			}
		}
	}

	It("is ready and refuses every request", func(ctx SpecContext) {
		testenv.Require(testenv.Docker)
		cfg := heldConfig()
		ctr, err := testcontainers.Run(ctx, agentgatewayimage.Ref(), append(cfgdeliver.ContainerOptions(cfg),
			testcontainers.WithExposedPorts("8080/tcp", "15021/tcp"),
			testcontainers.WithWaitStrategyAndDeadline(time.Minute, wait.ForHTTP("/healthz/ready").WithPort("15021/tcp")),
		)...)
		DeferCleanup(func(ctx context.Context) error { return testcontainers.TerminateContainer(ctr) })
		Expect(err).NotTo(HaveOccurred())
		url, err := containerhost.Endpoint(ctx, ctr, "8080/tcp", "http")
		Expect(err).NotTo(HaveOccurred())
		for _, key := range []string{"", "koinon_test_not-a-real-token"} {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions",
				strings.NewReader(`{"model":"koinon/any","messages":[{"role":"user","content":"hi"}]}`))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/json")
			if key != "" {
				req.Header.Set("Authorization", "Bearer "+key)
			}
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			_ = resp.Body.Close()
			Expect(resp.StatusCode).To(BeElementOf(http.StatusUnauthorized, http.StatusForbidden), "key %q", key)
		}
	}, NodeTimeout(3*time.Minute))
})
