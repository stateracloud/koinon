// SPDX-License-Identifier: Apache-2.0

package kind_test

import (
	"net/netip"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"
)

// The kind cluster of the e2e and of `make dev-kind-up` shares one config. Its pod and Service
// subnets must not be kind's defaults, which collide with common host clusters (#73).
var _ = Describe("kind-config.yaml", func() {
	var svc, pod netip.Prefix

	BeforeEach(func() {
		raw, err := os.ReadFile("kind-config.yaml")
		Expect(err).NotTo(HaveOccurred())
		var cfg struct {
			Networking struct {
				ServiceSubnet string `json:"serviceSubnet"`
				PodSubnet     string `json:"podSubnet"`
			} `json:"networking"`
		}
		Expect(yaml.Unmarshal(raw, &cfg)).To(Succeed())
		svc, err = netip.ParsePrefix(cfg.Networking.ServiceSubnet)
		Expect(err).NotTo(HaveOccurred(), "networking.serviceSubnet must be set")
		pod, err = netip.ParsePrefix(cfg.Networking.PodSubnet)
		Expect(err).NotTo(HaveOccurred(), "networking.podSubnet must be set")
	})

	It("does not use kind's default subnets", func() {
		for _, def := range []string{"10.96.0.0/12", "10.244.0.0/16"} {
			d := netip.MustParsePrefix(def)
			Expect(svc.Overlaps(d)).To(BeFalse(), "serviceSubnet %s overlaps %s", svc, d)
			Expect(pod.Overlaps(d)).To(BeFalse(), "podSubnet %s overlaps %s", pod, d)
		}
	})

	It("keeps the two subnets apart", func() {
		Expect(svc.Overlaps(pod)).To(BeFalse())
	})

	It("is not copied into the escape suite or its runner as a fixed Service IP", func() {
		// The escape suite reads the cluster's kube-dns and kubernetes Service IPs from the cluster it
		// targets (test/escape/kind.sh), so a fixed IP would silently probe nothing elsewhere.
		for _, f := range []string{"../../escape/sandbox_test.go", "../../escape/kind.sh"} {
			src, err := os.ReadFile(f)
			Expect(err).NotTo(HaveOccurred())
			for _, p := range []string{"10.96.", svc.Masked().Addr().String()[:strings.LastIndex(svc.Masked().Addr().String(), ".")+1]} {
				Expect(string(src)).NotTo(ContainSubstring(p), "%s hard-codes a Service IP", f)
			}
		}
	})
})

// The kind cluster pulls through the local registry and its caches (registry.sh, #45): containerd must
// read certs.d, and the legacy mirrors form, which turns that default off, must not come back.
var _ = Describe("kind-config.yaml registry wiring", func() {
	var raw string

	BeforeEach(func() {
		b, err := os.ReadFile("kind-config.yaml")
		Expect(err).NotTo(HaveOccurred())
		raw = string(b)
	})

	It("points containerd at /etc/containerd/certs.d", func() {
		var cfg struct {
			Patches []string `json:"containerdConfigPatches"`
		}
		Expect(yaml.Unmarshal([]byte(raw), &cfg)).To(Succeed())
		Expect(strings.Join(cfg.Patches, "\n")).To(ContainSubstring(`config_path = "/etc/containerd/certs.d"`))
	})

	It("never uses the legacy registry.mirrors patch", func() {
		for _, line := range strings.Split(raw, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			Expect(line).NotTo(ContainSubstring("registry.mirrors"))
			Expect(line).NotTo(ContainSubstring(".mirrors."))
		}
	})

	It("is the registry that registry.sh and the values files name", func() {
		sh, err := os.ReadFile("registry.sh")
		Expect(err).NotTo(HaveOccurred())
		Expect(string(sh)).To(ContainSubstring("KIND_REGISTRY_PORT:-5001"))
		for _, f := range []string{"koinon-values.yaml", "fakes.yaml", "../../../hack/dev/values-kind.yaml", "../../drill/topology/kind.yaml"} {
			b, err := os.ReadFile(f)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(b)).To(ContainSubstring("localhost:5001"), f)
			Expect(string(b)).NotTo(ContainSubstring("kind.local"), f)
		}
	})

	// A pushed image is not reloaded into the node the way `kind load` replaced it, so a reused tag (the e2e's
	// `e2e`, the Tiltfile's `dev` for the preflight probe pods) needs pullPolicy Always or the node keeps the old one.
	It("pulls the reused tags on every start, in the e2e values and in the Tilt kind profile", func() {
		for _, f := range []string{"koinon-values.yaml", "../../../hack/dev/values-kind.yaml"} {
			b, err := os.ReadFile(f)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(b)).To(MatchRegexp(`(?m)^image:\n(?:  .*\n|\s*#.*\n)*?  pullPolicy: Always$`), f)
		}
	})
})
