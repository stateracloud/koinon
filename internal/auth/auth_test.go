// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/db/store"
)

var _ = Describe("passwords", func() {
	It("hashes with argon2id and a fresh salt", func() {
		h1, err := HashPassword("correct horse battery")
		Expect(err).NotTo(HaveOccurred())
		h2, err := HashPassword("correct horse battery")
		Expect(err).NotTo(HaveOccurred())
		Expect(h1).To(HavePrefix("$argon2id$v=19$m=65536,t=3,p=4$"))
		Expect(h1).NotTo(Equal(h2))

		Expect(VerifyPassword(h1, "correct horse battery")).To(BeTrue())
		Expect(VerifyPassword(h1, "correct horse batterY")).To(BeFalse())
	})

	It("rejects malformed and unsupported hashes", func() {
		for _, h := range []string{
			"", "plain", "$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$a2V5",
			"$argon2id$v=19$m=999999999,t=3,p=4$c2FsdA$a2V5",
			"$argon2id$v=19$m=65536,t=3,p=4$!!$a2V5",
		} {
			_, err := VerifyPassword(h, "x")
			Expect(err).To(HaveOccurred(), h)
		}
	})

	It("enforces the new-password policy", func() {
		Expect(ValidateNewPassword("short")).To(HaveOccurred())
		Expect(ValidateNewPassword(strings.Repeat("a", 2000))).To(HaveOccurred())
		Expect(ValidateNewPassword("twelve chars")).To(Succeed())
	})
})

var _ = Describe("group mappings", func() {
	It("parses group=role lists", func() {
		m, err := ParseGroupMappings(" admins=admin, staff = member ,,")
		Expect(err).NotTo(HaveOccurred())
		Expect(m).To(Equal([]GroupMapping{
			{Group: "admins", Role: store.OrgRoleAdmin},
			{Group: "staff", Role: store.OrgRoleMember},
		}))
		m, err = ParseGroupMappings("")
		Expect(err).NotTo(HaveOccurred())
		Expect(m).To(BeEmpty())
	})

	DescribeTable("rejects bad input", func(in string) {
		_, err := ParseGroupMappings(in)
		Expect(err).To(HaveOccurred())
	},
		Entry("no role", "admins"),
		Entry("unknown role", "admins=root"),
		Entry("no group", "=admin"),
		Entry("duplicate", "a=admin,a=member"),
	)
})

var _ = Describe("roles", func() {
	It("orders viewer < member < admin < owner", func() {
		Expect(RoleAtLeast(store.OrgRoleOwner, store.OrgRoleAdmin)).To(BeTrue())
		Expect(RoleAtLeast(store.OrgRoleMember, store.OrgRoleMember)).To(BeTrue())
		Expect(RoleAtLeast(store.OrgRoleViewer, store.OrgRoleMember)).To(BeFalse())
		Expect(RoleAtLeast(store.OrgRoleOwner, "")).To(BeFalse())
	})

	It("gives platform admins every role", func() {
		p := &Principal{PlatformAdmin: true}
		Expect(p.HasRole(p.UserID, store.OrgRoleOwner)).To(BeTrue())
		Expect((&Principal{}).HasRole(p.UserID, store.OrgRoleViewer)).To(BeFalse())
	})
})

var _ = DescribeTable("safeReturnTo", func(in, want string) {
	Expect(safeReturnTo(in)).To(Equal(want))
},
	Entry("empty", "", "/"),
	Entry("path", "/board?x=1#y", "/board?x=1#y"),
	Entry("relative", "board", "/"),
	Entry("protocol-relative", "//evil.test/", "/"),
	Entry("backslash", "/\\evil.test", "/"),
	Entry("absolute", "https://evil.test/", "/"),
	Entry("tab trick", "/\t/evil.test", "/"),
)
