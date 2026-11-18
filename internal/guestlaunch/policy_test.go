// SPDX-License-Identifier: Apache-2.0

package guestlaunch_test

import (
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/guestlaunch"
)

var _ = Describe("ParsePolicy", func() {
	parse := func(s string) (guestlaunch.Policy, error) { return guestlaunch.ParsePolicy(strings.NewReader(s)) }

	It("takes the defaults for what the file leaves out", func() {
		p, err := parse(`{}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(p).To(Equal(guestlaunch.DefaultPolicy()))
		Expect(p.Capabilities.Allow).To(BeEmpty(), "every capability is dropped by default")
		Expect(p.User.Mode).To(Equal(guestlaunch.ModeBestEffort), "the user drop continues as root when the guest cannot do it")
		Expect(p.Seccomp.Mode).To(Equal(guestlaunch.ModeRequire))
	})

	It("replaces the Landlock path lists as a whole", func() {
		p, err := parse(`{"landlock":{"read_exec":["/usr"],"read_write":["/work"]}}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Landlock.ReadExec).To(Equal([]string{"/usr"}))
		Expect(p.Landlock.ReadWrite).To(Equal([]string{"/work"}))
		Expect(p.Landlock.Devices).To(BeEmpty())
		Expect(p.Landlock.Mode).To(Equal(guestlaunch.ModeBestEffort))
	})

	It("accepts capability names with or without the prefix", func() {
		p, err := parse(`{"capabilities":{"allow":["CAP_KILL","net_bind_service"]}}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Capabilities.Allow).To(HaveLen(2))
	})

	DescribeTable("refuses a policy that would silently weaken a control",
		func(doc, msg string) {
			_, err := parse(doc)
			Expect(err).To(MatchError(ContainSubstring(msg)))
		},
		Entry("unknown field", `{"seccomp":{"mod":"off"}}`, "unknown field"),
		Entry("unknown mode", `{"seccomp":{"mode":"warn"}}`, "must be require, best_effort or off"),
		Entry("unknown capability", `{"capabilities":{"allow":["CAP_NOPE"]}}`, "unknown capability"),
		Entry("relative path", `{"landlock":{"read_write":["tmp"]}}`, "not absolute"),
		Entry("require root", `{"user":{"mode":"require"}}`, "non-zero uid"),
		Entry("not JSON", `nope`, "guest launcher policy"),
	)
})

var _ = Describe("the image policy", func() {
	It("parses and keeps the guarantees the image contract promises", func() {
		f, err := os.Open("../../cmd/koinon-guest-launcher/policy.json")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(f.Close)
		p, err := guestlaunch.ParsePolicy(f)
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Seccomp.Mode).To(Equal(guestlaunch.ModeRequire))
		Expect(p.Capabilities.Allow).To(BeEmpty())
		Expect(p.User.UID).To(BeEquivalentTo(10001))
		Expect(p.User.Mode).NotTo(Equal(guestlaunch.ModeRequire), "the guest lacks CAP_SETUID until Substrate offers it (issue #1087)")
	})
})
