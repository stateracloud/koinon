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

	It("accepts a name that only another architecture has", func() {
		// _sysctl and nfsservctl are gone from arm64; both stay valid on every architecture.
		_, err := parse(`{"seccomp":{"deny_extra":["_sysctl","nfsservctl","arm_fadvise64_64"]}}`)
		Expect(err).NotTo(HaveOccurred())
	})

	It("knows every name of the built-in deny list", func() {
		_, err := parse(`{"seccomp":{"deny_extra":` + jsonList(guestlaunch.DefaultSeccompDeny) + `}}`)
		Expect(err).NotTo(HaveOccurred())
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
		Entry("seccomp off", `{"seccomp":{"mode":"off"}}`, "seccomp mode \"off\" is refused"),
		Entry("seccomp best_effort", `{"seccomp":{"mode":"best_effort"}}`, "seccomp mode \"best_effort\" is refused"),
		Entry("unknown capability", `{"capabilities":{"allow":["CAP_NOPE"]}}`, "unknown capability"),
		Entry("relative path", `{"landlock":{"read_write":["tmp"]}}`, "not absolute"),
		Entry("unknown deny name", `{"seccomp":{"deny":["ptrace","not_a_syscall"]}}`, "unknown syscall"),
		Entry("unknown deny_extra name", `{"seccomp":{"deny_extra":["ptrcae"]}}`, "unknown syscall"),
		Entry("relative list dir", `{"landlock":{"list_dirs":["dev"]}}`, "not absolute"),
		Entry("require root", `{"user":{"mode":"require"}}`, "non-zero uid"),
		Entry("not JSON", `nope`, "guest launcher policy"),
	)
})

func jsonList(l []string) string { return `["` + strings.Join(l, `","`) + `"]` }

// The two image policies differ in the Landlock mode only (ADR 0120 section 4.2 and 4.3): the vm
// tier fails closed, the gVisor tiers rely on the read-only root when the kernel has no Landlock.
var _ = Describe("the image policies", func() {
	load := func(name string) guestlaunch.Policy {
		f, err := os.Open("../../cmd/koinon-guest-launcher/" + name)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(f.Close)
		p, err := guestlaunch.ParsePolicy(f)
		Expect(err).NotTo(HaveOccurred())
		return p
	}

	DescribeTable("parse and keep the guarantees the image contract promises",
		func(name string, landlock guestlaunch.Mode) {
			p := load(name)
			Expect(p.Landlock.Mode).To(Equal(landlock))
			Expect(p.Seccomp.Mode).To(Equal(guestlaunch.ModeRequire))
			Expect(p.Capabilities.Allow).To(BeEmpty())
			Expect(p.User.UID).To(BeEquivalentTo(10001))
			Expect(p.User.Mode).NotTo(Equal(guestlaunch.ModeRequire), "the guest lacks CAP_SETUID until Substrate offers it (issue #1087)")
			Expect(p.Landlock.Devices).To(ConsistOf(guestlaunch.DefaultDevices), "single device nodes, never /dev as a whole")
			Expect(p.Landlock.Devices).NotTo(ContainElement("/dev"))
			Expect(p.Landlock.ListDirs).To(Equal([]string{"/dev"}))
			for _, rw := range p.Landlock.ReadWrite {
				Expect(rw).NotTo(HavePrefix("/dev"))
			}
		},
		Entry("vm tier", "policy-vm.json", guestlaunch.ModeRequire),
		Entry("gVisor tiers", "policy-gvisor.json", guestlaunch.ModeBestEffort),
	)

	It("differ in nothing but the Landlock mode", func() {
		vm, gv := load("policy-vm.json"), load("policy-gvisor.json")
		vm.Landlock.Mode = gv.Landlock.Mode
		Expect(vm).To(Equal(gv))
	})
})
