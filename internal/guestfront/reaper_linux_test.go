// SPDX-License-Identifier: Apache-2.0

package guestfront

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("parseZombie", func() {
	const stat = "123 (my (odd) name) Z 77 123 123 0 -1 4194308 0 0 0 0 0 0 0 0 20 0 1 0 4242 0 0 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0"

	It("reads a zombie child, with a name that holds brackets", func() {
		z, ok := parseZombie(123, stat, 77)
		Expect(ok).To(BeTrue())
		Expect(z).To(Equal(zombie{PID: 123}))
	})
	It("skips a zombie of another parent", func() {
		_, ok := parseZombie(123, stat, 1)
		Expect(ok).To(BeFalse())
	})
	It("skips a live process", func() {
		_, ok := parseZombie(123, "123 (sh) S 77 123 123 0 -1 4194304 0 0 0 0 0 0 0 0 20 0 1 0 4242 0 0 0", 77)
		Expect(ok).To(BeFalse())
	})
})
