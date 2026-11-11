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
