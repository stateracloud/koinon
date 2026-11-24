// SPDX-License-Identifier: Apache-2.0

package egress_test

import (
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway/egress"
	"github.com/stateracloud/koinon/internal/gateway/route"
)

var _ = Describe("platform route file", func() {
	It("reads platform routes with no org and no credential", func() {
		t, err := egress.ParsePlatformRoutes([]byte(`{"routes":[
			{"name":"npm","class":"registry","host":"registry.npmjs.org","methods":["GET","HEAD"],"pathPrefixes":["/"]},
			{"name":"pypi","class":"registry","host":"pypi.org","port":443,"methods":["GET"],"pathPrefixes":["/simple"],"queryKeys":["format"]}
		]}`))
		Expect(err).NotTo(HaveOccurred())
		rs := t.Routes()
		Expect(rs).To(HaveLen(2))
		Expect(rs[0].Org).To(BeEmpty())
		Expect(rs[0].Credential).To(BeEmpty())
		Expect(rs[1].Class).To(Equal(route.ClassRegistry))
	})

	DescribeTable("refuses the whole file",
		func(body, want string) {
			_, err := egress.ParsePlatformRoutes([]byte(body))
			Expect(err).To(MatchError(ContainSubstring(want)))
		},
		Entry("an org field", `{"routes":[{"name":"a","class":"forge","org":"acme","host":"git.example.com","methods":["GET"],"pathPrefixes":["/"]}]}`, "unknown field"),
		Entry("a credential field", `{"routes":[{"name":"a","class":"forge","credential":"x","host":"git.example.com","methods":["GET"],"pathPrefixes":["/"]}]}`, "unknown field"),
		Entry("a wildcard host", `{"routes":[{"name":"a","class":"registry","host":"*.example.com","methods":["GET"],"pathPrefixes":["/"]}]}`, "exact name"),
		Entry("one bad route among good ones", `{"routes":[
			{"name":"ok","class":"registry","host":"pypi.org","methods":["GET"],"pathPrefixes":["/"]},
			{"name":"bad","class":"registry","host":"pypi.org","methods":[],"pathPrefixes":["/"]}]}`, "no method rule"),
		Entry("a name with a slash, which only org routes have", `{"routes":[{"name":"org/acme","class":"registry","host":"pypi.org","methods":["GET"],"pathPrefixes":["/"]}]}`, `has no "/"`),
		Entry("an agent field", `{"routes":[{"name":"a","class":"registry","agent":"x","host":"pypi.org","methods":["GET"],"pathPrefixes":["/"]}]}`, "unknown field"),
		Entry("a deny route with a credential field", `{"routes":[{"name":"a","class":"registry","deny":true,"credential":"x","host":"pypi.org","methods":["GET"],"paths":["/**"]}]}`, "unknown field"),
		Entry("no path rule", `{"routes":[{"name":"a","class":"registry","host":"pypi.org","methods":["GET"]}]}`, "path"),
		Entry("two JSON values", `{"routes":[]} {"routes":[]}`, "more than one"),
		Entry("not JSON", `routes: []`, "route file"),
	)

	It("reads patterns, query values, any-query and deny routes", func() {
		t, err := egress.ParsePlatformRoutes([]byte(`{"routes":[
			{"name":"pkg","class":"registry","host":"pkgs.example.test","methods":["GET"],"paths":["/simple/*/**"],"queryValues":{"format":"json"},"anyQuery":true},
			{"name":"no-upload","class":"registry","host":"pkgs.example.test","methods":["POST"],"paths":["/**"],"deny":true}
		]}`))
		Expect(err).NotTo(HaveOccurred())
		rs := t.Routes()
		Expect(rs).To(HaveLen(2))
		Expect(rs[0].Paths).To(Equal([]string{"/simple/*/**"}))
		Expect(rs[0].QueryValues).To(Equal(map[string]string{"format": "json"}))
		Expect(rs[0].AnyQuery).To(BeTrue())
		Expect(rs[1].Deny).To(BeTrue())
	})

	It("loads a file and refuses one over 1 MiB", func() {
		dir := GinkgoT().TempDir()
		ok := filepath.Join(dir, "routes.json")
		Expect(os.WriteFile(ok, []byte(`{"routes":[]}`), 0o600)).To(Succeed())
		t, err := egress.LoadPlatformRoutes(ok)
		Expect(err).NotTo(HaveOccurred())
		Expect(t.Routes()).To(BeEmpty())

		big := filepath.Join(dir, "big.json")
		Expect(os.WriteFile(big, []byte(`{"routes":[]}`+strings.Repeat(" ", 1<<20)), 0o600)).To(Succeed())
		_, err = egress.LoadPlatformRoutes(big)
		Expect(err).To(MatchError(ContainSubstring("larger than 1 MiB")))
	})
})
