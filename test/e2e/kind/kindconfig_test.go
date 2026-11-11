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

	It("matches the Service IPs the escape suite defaults to", func() {
		// kube-dns is the 10th and the API server the 1st address of the Service range.
		api := svc.Masked().Addr().Next()
		dns := api
		for range 9 {
			dns = dns.Next()
		}
		src, err := os.ReadFile("../../escape/sandbox_test.go")
		Expect(err).NotTo(HaveOccurred())
		Expect(string(src)).To(ContainSubstring(`"`+dns.String()+`"`), "cluster DNS default")
		Expect(string(src)).To(ContainSubstring(api.String() + ":443"))
		Expect(string(src)).To(ContainSubstring(api.String() + ":6443"))
		Expect(strings.Contains(string(src), "10.96.")).To(BeFalse(), "a hard-coded default-kind Service IP")
	})
})
