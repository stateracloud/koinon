// SPDX-License-Identifier: Apache-2.0

package preflight_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
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

// gateway is OpenShell's gateway StatefulSet with the given environment.
func gateway(env ...corev1.EnvVar) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "openshell", Namespace: "openshell"},
		Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "openshell", Env: env}},
		}}},
	}
}

const (
	supervisor = "ghcr.io/nvidia/openshell/supervisor:0.1.7"
	mirrored   = "art.example.com/ghcr-remote/nvidia/openshell/supervisor:0.1.7"
)

var _ = Describe("Sandbox image checks", func() {
	withConfig := func(cm *corev1.ConfigMap, mutate func(*preflight.Config)) *cluster {
		c := healthyCluster()
		c.config.OpenShellConfigNamespace, c.config.OpenShellConfigMap = "openshell", "openshell-config"
		c.config.OpenShellWorkload = "openshell"
		mutate(&c.config)
		c.add(gateway())
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

	DescribeTable("fails once when the gateway ConfigMap cannot be read and a supervisor image is configured (old issue 489)",
		func(ctx SpecContext, cm *corev1.ConfigMap, forbid string) {
			c := withConfig(cm, func(c *preflight.Config) {
				c.ExpectSandboxPullSecrets = true
				c.SupervisorImages = []string{supervisor}
			})
			c.forbid = forbid
			results := c.run(ctx)
			r := find(results, "OpenShell gateway configuration")
			Expect(r.Status).To(Equal(preflight.Fail))
			Expect(r.Fix).To(ContainSubstring("preflight.openshell"))
			Expect(checks(results)).NotTo(ContainElements("Sandbox image pull secrets", "Sandbox supervisor image"))
			Expect(preflight.Write(&discard{}, results)).NotTo(BeZero())
		},
		Entry("not found", nil, ""),
		Entry("forbidden", openshellConfig(""), "configmaps"),
		Entry("no gateway.toml", &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "openshell-config", Namespace: "openshell"}}, ""),
	)

	DescribeTable("warns once, without failing, when the gateway ConfigMap cannot be read and no supervisor image is configured",
		func(ctx SpecContext, cm *corev1.ConfigMap, forbid string) {
			c := withConfig(cm, func(c *preflight.Config) { c.ExpectSandboxPullSecrets = true })
			c.forbid = forbid
			results := c.run(ctx)
			r := find(results, "OpenShell gateway configuration")
			Expect(r.Status).To(Equal(preflight.Warn))
			Expect(r.Fix).To(ContainSubstring("preflight.openshell"))
			Expect(preflight.Write(&discard{}, results)).To(BeZero())
		},
		Entry("not found", nil, ""),
		Entry("forbidden", openshellConfig(""), "configmaps"),
		Entry("no gateway.toml", &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "openshell-config", Namespace: "openshell"}}, ""),
	)

	Describe("the supervisor token lifetime (old issue 419)", func() {
		bound := func(n int) func(*preflight.Config) {
			return func(c *preflight.Config) { c.SupervisorTokenMaxSeconds = n }
		}
		const name = "Sandbox supervisor token lifetime"

		It("passes on OpenShell's default of 3600 seconds when the bound is the default", func(ctx SpecContext) {
			r := find(withConfig(openshellConfig(""), bound(3600)).run(ctx), name)
			Expect(r.Status).To(Equal(preflight.Pass))
		})

		It("fails when sa_token_ttl_secs is above the bound, naming both values", func(ctx SpecContext) {
			results := withConfig(openshellConfig("sa_token_ttl_secs = 7200\n"), bound(3600)).run(ctx)
			r := find(results, name)
			Expect(r.Status).To(Equal(preflight.Fail))
			Expect(r.Message).To(ContainSubstring("7200"))
			Expect(r.Message).To(ContainSubstring("supervisorTokenMaxSeconds=3600"))
			Expect(r.Fix).To(ContainSubstring("at least 7200"))
			Expect(preflight.Write(&discard{}, results)).NotTo(BeZero())
		})

		It("passes when the bound is raised to match", func(ctx SpecContext) {
			r := find(withConfig(openshellConfig("sa_token_ttl_secs = 7200\n"), bound(7200)).run(ctx), name)
			Expect(r.Status).To(Equal(preflight.Pass))
		})

		It("applies OpenShell's clamp to 600-86400", func(ctx SpecContext) {
			Expect(find(withConfig(openshellConfig("sa_token_ttl_secs = 5\n"), bound(600)).run(ctx), name).Status).To(Equal(preflight.Pass))
			r := find(withConfig(openshellConfig("sa_token_ttl_secs = 999999\n"), bound(86399)).run(ctx), name)
			Expect(r.Status).To(Equal(preflight.Fail))
			Expect(r.Message).To(ContainSubstring("86400"))
		})

		It("reads only the Kubernetes driver's table", func(ctx SpecContext) {
			cm := openshellConfig("")
			cm.Data["gateway.toml"] += "\n[other]\nsa_token_ttl_secs = 9000\n"
			Expect(find(withConfig(cm, bound(3600)).run(ctx), name).Status).To(Equal(preflight.Pass))
		})

		It("fails when only OPENSHELL_K8S_SA_TOKEN_TTL_SECS is above the bound", func(ctx SpecContext) {
			c := withConfig(openshellConfig(""), bound(3600))
			c.with(gateway(corev1.EnvVar{Name: "OPENSHELL_K8S_SA_TOKEN_TTL_SECS", Value: "7200"}))
			r := find(c.run(ctx), name)
			Expect(r.Status).To(Equal(preflight.Fail))
			Expect(r.Message).To(ContainSubstring("7200"))
		})

		It("lets the environment variable win over the file", func(ctx SpecContext) {
			c := withConfig(openshellConfig("sa_token_ttl_secs = 7200\n"), bound(3600))
			c.with(gateway(corev1.EnvVar{Name: "OPENSHELL_K8S_SA_TOKEN_TTL_SECS", Value: "1800"}))
			Expect(find(c.run(ctx), name).Status).To(Equal(preflight.Pass))
		})

		It("warns, never passes, when the gateway workload cannot be read", func(ctx SpecContext) {
			c := withConfig(openshellConfig(""), bound(3600))
			c.without("openshell")
			r := find(c.run(ctx), name)
			Expect(r.Status).To(Equal(preflight.Warn))
			Expect(r.Message).To(ContainSubstring("OPENSHELL_K8S_SA_TOKEN_TTL_SECS"))
			Expect(r.Message).To(ContainSubstring("not checked"))
		})

		It("warns when the variable comes from a reference", func(ctx SpecContext) {
			c := withConfig(openshellConfig(""), bound(3600))
			c.with(gateway(corev1.EnvVar{Name: "OPENSHELL_K8S_SA_TOKEN_TTL_SECS", ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "x"}, Key: "ttl"},
			}}))
			Expect(find(c.run(ctx), name).Status).To(Equal(preflight.Warn))
		})

		It("is not checked when the policy is off (bound 0)", func(ctx SpecContext) {
			Expect(checks(withConfig(openshellConfig("sa_token_ttl_secs = 7200\n"), bound(0)).run(ctx))).NotTo(ContainElement(name))
		})
	})

	Describe("the gateway ID (ADR 0078)", func() {
		// OpenShell 0.1.5 on kind: gateway_id appears twice, under the Kubernetes driver and under
		// gateway_jwt, and only the first is the one that names the namespaces.
		gatewayToml := func(driverID, jwtID, mode string) *corev1.ConfigMap {
			return &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "openshell-config", Namespace: "openshell"},
				Data: map[string]string{"gateway.toml": `[openshell]
version = 2
[openshell.drivers.kubernetes]
allow_driver_config = false
gateway_id = "` + driverID + `"
namespace = "openshell"
workspace_mode = "` + mode + `"
[openshell.drivers.kubernetes.managed_ssh_ingress]
enabled = true
[openshell.gateway]
name = "openshell"
[openshell.gateway.gateway_jwt]
gateway_id = "` + jwtID + `"
kid_path = "/etc/openshell-jwt/kid"
`},
			}
		}
		withID := func(cm *corev1.ConfigMap, id string) *cluster {
			return withConfig(cm, func(c *preflight.Config) { c.OpenShellGatewayID = id })
		}

		It("passes when openshell.gatewayId matches the Kubernetes driver's gateway_id", func(ctx SpecContext) {
			results := withID(gatewayToml("openshell", "other", "managed"), "openshell").run(ctx)
			Expect(find(results, "OpenShell gateway ID").Status).To(Equal(preflight.Pass))
			Expect(checks(results)).NotTo(ContainElement("OpenShell workspace mode"))
		})

		It("reads the driver's gateway_id and not the one under gateway_jwt", func(ctx SpecContext) {
			r := find(withID(gatewayToml("prod", "openshell", "managed"), "openshell").run(ctx), "OpenShell gateway ID")
			Expect(r.Status).To(Equal(preflight.Warn))
			Expect(r.Message).To(And(ContainSubstring(`"prod"`), ContainSubstring(`"openshell"`)))
			Expect(r.Fix).To(ContainSubstring("openshell.gatewayId=prod"))
		})

		It("fails on a mismatch when the admission policies are scoped to the gateway", func(ctx SpecContext) {
			cm := gatewayToml("prod", "prod", "managed")
			results := withConfig(cm, func(c *preflight.Config) {
				c.OpenShellGatewayID = "openshell"
				c.SandboxPolicyScopedToGateway = true
			}).run(ctx)
			r := find(results, "OpenShell gateway ID")
			Expect(r.Status).To(Equal(preflight.Fail))
			Expect(r.Message).To(ContainSubstring("admission policies"))
			Expect(r.Message).To(ContainSubstring("fail open"))
			Expect(preflight.Write(&discard{}, results)).NotTo(BeZero())
		})

		It("warns, without failing, when gateway_id is missing from the driver table", func(ctx SpecContext) {
			cm := openshellConfig("")
			cm.Data["gateway.toml"] += "[openshell.gateway.gateway_jwt]\ngateway_id = \"openshell\"\n"
			results := withID(cm, "openshell").run(ctx)
			Expect(find(results, "OpenShell gateway ID").Status).To(Equal(preflight.Warn))
			Expect(find(results, "OpenShell gateway ID").Message).To(ContainSubstring("does not set gateway_id"))
			Expect(preflight.Write(&discard{}, results)).To(BeZero())
		})

		It("warns on an unset gateway_id when the policies are scoped and openshell.gatewayId is OpenShell's default", func(ctx SpecContext) {
			cm := openshellConfig("")
			results := withConfig(cm, func(c *preflight.Config) {
				c.OpenShellGatewayID = "openshell"
				c.SandboxPolicyScopedToGateway = true
			}).run(ctx)
			r := find(results, "OpenShell gateway ID")
			Expect(r.Status).To(Equal(preflight.Warn))
			Expect(r.Message).To(ContainSubstring("default"))
			Expect(preflight.Write(&discard{}, results)).To(BeZero())
		})

		It("fails on an unset gateway_id when the policies are scoped and openshell.gatewayId is not OpenShell's default", func(ctx SpecContext) {
			cm := openshellConfig("")
			results := withConfig(cm, func(c *preflight.Config) {
				c.OpenShellGatewayID = "prod"
				c.SandboxPolicyScopedToGateway = true
			}).run(ctx)
			r := find(results, "OpenShell gateway ID")
			Expect(r.Status).To(Equal(preflight.Fail))
			Expect(r.Message).To(And(ContainSubstring("default"), ContainSubstring("fail open")))
			Expect(r.Fix).To(ContainSubstring("openshell.gatewayId"))
			Expect(preflight.Write(&discard{}, results)).NotTo(BeZero())
		})

		It("warns when the workspace mode is not managed", func(ctx SpecContext) {
			r := find(withID(gatewayToml("openshell", "openshell", "operator"), "openshell").run(ctx), "OpenShell workspace mode")
			Expect(r.Status).To(Equal(preflight.Warn))
			Expect(r.Message).To(ContainSubstring(`"operator"`))
		})

		It("is not checked without openshell.gatewayId", func(ctx SpecContext) {
			Expect(checks(withID(gatewayToml("prod", "prod", "managed"), "").run(ctx))).NotTo(ContainElement("OpenShell gateway ID"))
		})
	})

	Describe("the sandbox observer", func() {
		It("warns, without failing, when it is off", func(ctx SpecContext) {
			c := withConfig(nil, func(c *preflight.Config) { c.SandboxObserverDisabled = true })
			results := c.run(ctx)
			r := find(results, "Sandbox observer")
			Expect(r.Status).To(Equal(preflight.Warn))
			Expect(r.Message).To(And(ContainSubstring("sandboxObserver.enabled is false"), ContainSubstring("unknown")))
			Expect(r.Fix).To(ContainSubstring("sandboxObserver.enabled=true"))
			Expect(preflight.Write(&discard{}, results)).To(BeZero())
		})

		It("says nothing when it is on", func(ctx SpecContext) {
			Expect(checks(withConfig(nil, func(*preflight.Config) {}).run(ctx))).NotTo(ContainElement("Sandbox observer"))
		})
	})

	DescribeTable("reads the gateway ID and workspace mode out of gateway.toml, by section",
		func(toml, id, mode string) {
			gotID, gotMode := preflight.ParseGatewayKubernetesDriver(toml)
			Expect([]string{gotID, gotMode}).To(Equal([]string{id, mode}))
		},
		Entry("the driver table", "[openshell.drivers.kubernetes]\ngateway_id = \"a\"\nworkspace_mode = \"managed\"", "a", "managed"),
		Entry("the same key in another table is ignored", "[openshell.gateway.gateway_jwt]\ngateway_id = \"jwt\"\n[openshell.drivers.kubernetes]\ngateway_id = \"a\"", "a", ""),
		Entry("only the other table has it", "[openshell.gateway.gateway_jwt]\ngateway_id = \"jwt\"", "", ""),
		Entry("a sub-table of the driver does not count", "[openshell.drivers.kubernetes.resource_admission]\ngateway_id = \"sub\"", "", ""),
		Entry("after a sub-table the driver's keys are not read again", "[openshell.drivers.kubernetes]\ngateway_id = \"a\"\n[openshell.drivers.kubernetes.managed_ssh_ingress]\ngateway_id = \"sub\"", "a", ""),
		Entry("comments and single quotes", "[openshell.drivers.kubernetes] # driver\n  gateway_id = 'a' # id\n# workspace_mode = \"operator\"", "a", ""),
		Entry("a key that merely ends in the name", "[openshell.drivers.kubernetes]\nother_gateway_id = \"x\"", "", ""),
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
		Entry("single-quoted strings", "image_pull_secrets = ['a']\nsupervisor_image = 'r/s:1'", []string{"a"}, "r/s:1"),
		Entry("comments inside the array and after values", "image_pull_secrets = [\n  \"a\", # first\n  # \"gone\",\n  \"b#c\",\n] # done\nsupervisor_image = \"r/s:1\" # pinned", []string{"a", "b#c"}, "r/s:1"),
		Entry("keys are anchored: other keys that end in the name, and text in values", "other_image_pull_secrets = [\"x\"]\nnote = \"supervisor_image = 'y'\"\nold_supervisor_image = \"z\"", nil, ""),
		Entry("indented keys", "[openshell.drivers.kubernetes]\n  image_pull_secrets = [\"a\"]\n  supervisor_image = \"r/s:1\"", []string{"a"}, "r/s:1"),
		Entry("neither", "[openshell.gateway]\nbind = \"x\"", nil, ""),
	)
})

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
