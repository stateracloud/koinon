// SPDX-License-Identifier: Apache-2.0

package preflight_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stateracloud/koinon/internal/preflight"
)

// openshellConfig is OpenShell 0.1.5's gateway ConfigMap with the given [openshell.drivers.kubernetes] lines.
func openshellConfig(lines string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "openshell-config", Namespace: "openshell"},
		Data: map[string]string{"gateway.toml": `# image_pull_secrets = ["commented-out"]
[openshell.gateway]
bind_address = "0.0.0.0:8080"

[openshell.drivers.kubernetes]
namespace = "openshell-sandboxes"
` + lines},
	}
}

const (
	supervisor = "ghcr.io/nvidia/openshell/supervisor:0.1.5"
	mirrored   = "art.example.com/ghcr-remote/nvidia/openshell/supervisor:0.1.5"
)

var _ = Describe("Sandbox image checks", func() {
	withConfig := func(cm *corev1.ConfigMap, mutate func(*preflight.Config)) *cluster {
		c := healthyCluster()
		c.config.OpenShellConfigNamespace, c.config.OpenShellConfigMap = "openshell", "openshell-config"
		mutate(&c.config)
		if cm != nil {
			c.add(cm)
		}
		return c
	}

	It("does nothing when no pull secret, mirror or supervisor pin is configured", func(ctx SpecContext) {
		c := withConfig(openshellConfig(""), func(*preflight.Config) {})
		Expect(checks(c.run(ctx))).NotTo(ContainElements("Sandbox image pull secrets", "Sandbox supervisor image"))
	})

	It("passes when the gateway has pull secrets and its supervisor image is admitted", func(ctx SpecContext) {
		c := withConfig(openshellConfig("image_pull_secrets = [\"regcred\"]\nsupervisor_image = \""+mirrored+"\"\n"), func(c *preflight.Config) {
			c.ExpectSandboxPullSecrets = true
			c.SupervisorImages = []string{mirrored}
		})
		results := c.run(ctx)
		Expect(find(results, "Sandbox image pull secrets").Status).To(Equal(preflight.Pass))
		Expect(find(results, "Sandbox image pull secrets").Message).To(ContainSubstring("regcred"))
		Expect(find(results, "Sandbox supervisor image").Status).To(Equal(preflight.Pass))
	})

	It("warns, naming server.sandboxImagePullSecrets, when the gateway has none", func(ctx SpecContext) {
		// The only image_pull_secrets in the file is a comment.
		c := withConfig(openshellConfig(""), func(c *preflight.Config) { c.ExpectSandboxPullSecrets = true })
		results := c.run(ctx)
		r := find(results, "Sandbox image pull secrets")
		Expect(r.Status).To(Equal(preflight.Warn))
		Expect(r.Message).To(ContainSubstring("no image_pull_secrets"))
		Expect(r.Fix).To(ContainSubstring("server.sandboxImagePullSecrets"))
		Expect(preflight.Write(&discard{}, results)).To(BeZero(), "a warning does not fail the preflight")
	})

	It("warns when the gateway's supervisor image is not admitted, or unknown", func(ctx SpecContext) {
		c := withConfig(openshellConfig("supervisor_image = \""+supervisor+"\"\n"), func(c *preflight.Config) {
			c.SupervisorImages = []string{mirrored}
		})
		r := find(c.run(ctx), "Sandbox supervisor image")
		Expect(r.Status).To(Equal(preflight.Warn))
		Expect(r.Message).To(ContainSubstring(supervisor))
		Expect(r.Fix).To(ContainSubstring("sandboxPolicy.supervisorImages"))

		c = withConfig(openshellConfig(""), func(c *preflight.Config) { c.SupervisorImages = []string{mirrored} })
		r = find(c.run(ctx), "Sandbox supervisor image")
		Expect(r.Status).To(Equal(preflight.Warn))
		Expect(r.Message).To(ContainSubstring("does not set supervisor_image"))
	})

	DescribeTable("warns once, without failing, when the gateway ConfigMap cannot be read",
		func(ctx SpecContext, cm *corev1.ConfigMap, forbid string) {
			c := withConfig(cm, func(c *preflight.Config) {
				c.ExpectSandboxPullSecrets = true
				c.SupervisorImages = []string{supervisor}
			})
			c.forbid = forbid
			results := c.run(ctx)
			r := find(results, "OpenShell gateway configuration")
			Expect(r.Status).To(Equal(preflight.Warn))
			Expect(r.Fix).To(ContainSubstring("preflight.openshell"))
			Expect(checks(results)).NotTo(ContainElements("Sandbox image pull secrets", "Sandbox supervisor image"))
			Expect(preflight.Write(&discard{}, results)).To(BeZero())
		},
		Entry("not found", nil, ""),
		Entry("forbidden", openshellConfig(""), "configmaps"),
		Entry("no gateway.toml", &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "openshell-config", Namespace: "openshell"}}, ""),
	)

	DescribeTable("reads the settings out of gateway.toml",
		func(toml string, secrets []string, image string) {
			gotSecrets, gotImage := preflight.ParseGatewayConfig(toml)
			Expect(gotSecrets).To(Equal(secrets))
			Expect(gotImage).To(Equal(image))
		},
		Entry("both", "image_pull_secrets = [\"a\", \"b\"]\nsupervisor_image = \"r/s:1\"", []string{"a", "b"}, "r/s:1"),
		Entry("multi-line array", "image_pull_secrets = [\n  \"a\",\n]\n", []string{"a"}, ""),
		Entry("empty array", "image_pull_secrets = []", nil, ""),
		Entry("comments only", "# image_pull_secrets = [\"a\"]\n  # supervisor_image = \"x\"", nil, ""),
		Entry("neither", "[openshell.gateway]\nbind = \"x\"", nil, ""),
	)
})

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
