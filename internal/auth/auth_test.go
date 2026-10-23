// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/db/store"
	authv1alpha1 "github.com/procoduck/koinon/internal/gen/koinon/auth/v1alpha1"
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

	It("maps every role to its wire form in the same order", func() {
		Expect(roleToProto(store.OrgRoleViewer)).To(Equal(authv1alpha1.OrgRole_ORG_ROLE_VIEWER))
		Expect(roleToProto(store.OrgRoleMember)).To(Equal(authv1alpha1.OrgRole_ORG_ROLE_MEMBER))
		Expect(roleToProto(store.OrgRoleAdmin)).To(Equal(authv1alpha1.OrgRole_ORG_ROLE_ADMIN))
		Expect(roleToProto(store.OrgRoleOwner)).To(Equal(authv1alpha1.OrgRole_ORG_ROLE_OWNER))
		Expect(roleToProto("root")).To(Equal(authv1alpha1.OrgRole_ORG_ROLE_UNSPECIFIED))
		for i := 1; i < len(orgRoles); i++ {
			Expect(orgRoles[i].proto).To(BeNumerically(">", orgRoles[i-1].proto))
		}
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

var _ = DescribeTable("ReadSecretFile strips trailing line breaks only", func(content, want string) {
	path := filepath.Join(GinkgoT().TempDir(), "secret")
	Expect(os.WriteFile(path, []byte(content), 0o600)).To(Succeed())
	Expect(ReadSecretFile(path)).To(Equal(want))
},
	Entry("bare", "s3cret", "s3cret"),
	Entry("LF", "s3cret\n", "s3cret"),
	Entry("CRLF", "s3cret\r\n", "s3cret"),
	Entry("inner and leading spaces are kept", " s3 cret \n", " s3 cret "),
	Entry("trailing tab is kept", "s3cret\t", "s3cret\t"),
)

var _ = It("ReadSecretFile returns an empty secret for an empty path", func() {
	Expect(ReadSecretFile("")).To(BeEmpty())
})
