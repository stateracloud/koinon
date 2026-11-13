// SPDX-License-Identifier: Apache-2.0

package runevidence_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/runevidence"
)

var _ = Describe("HostMasker", func() {
	m := runevidence.NewHostMaskerWithKey([]byte("koinon_test_not-a-real-key-000000"))

	It("replaces the host of a URL, an address and a dotted name, the same way each time", func() {
		in := `curl https://lab-db.corp.example.com:8443/x?a=1 and 10.1.2.3 and svc.ns.svc.cluster.local`
		out := m.Mask(in)
		Expect(out).NotTo(ContainSubstring("example.com"))
		Expect(out).NotTo(ContainSubstring("10.1.2.3"))
		Expect(out).NotTo(ContainSubstring("cluster.local"))
		Expect(out).To(ContainSubstring(".invalid"))
		Expect(m.Mask(in)).To(Equal(out))
	})

	It("maps one name to one pseudonym and two names to two", func() {
		Expect(m.Name("a.example.com")).To(Equal(m.Name("a.example.com")))
		Expect(m.Name("a.example.com")).NotTo(Equal(m.Name("b.example.com")))
	})

	It("uses a different pseudonym under another key", func() {
		other := runevidence.NewHostMaskerWithKey([]byte("koinon_test_not-a-real-key-111111"))
		Expect(other.Name("a.example.com")).NotTo(Equal(m.Name("a.example.com")))
	})

	It("leaves file names and code alone", func() {
		in := "edit main.go and values.yaml, call fmt.Println"
		Expect(m.Mask(in)).To(Equal(in))
	})

	It("masks names under other public suffixes and IPv6 addresses outside a URL", func() {
		in := "db.prod.gov x.edu svc.acme.me box.tv fd00:1234:5678::1 2001:db8:0:0:0:0:0:1"
		out := m.Mask(in)
		for _, s := range []string{"prod.gov", "x.edu", "acme.me", "box.tv", "fd00:", "2001:db8"} {
			Expect(out).NotTo(ContainSubstring(s))
		}
		Expect(m.Mask("run.sh and main.go and 12:34:56")).To(Equal("run.sh and main.go and 12:34:56"))
	})
})
