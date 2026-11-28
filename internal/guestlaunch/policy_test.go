// SPDX-License-Identifier: Apache-2.0

package guestlaunch_test

import (
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/guestfront"
	"github.com/stateracloud/koinon/internal/guestlaunch"
)

var _ = Describe("ParsePolicy", func() {
	parse := func(s string) (guestlaunch.Policy, error) { return guestlaunch.ParsePolicy(strings.NewReader(s)) }

	It("takes the defaults for what the file leaves out", func() {
		p, err := parse(`{}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(p).To(Equal(guestlaunch.DefaultPolicy()))
		Expect(p.Capabilities.Allow).To(BeEmpty(), "every capability is dropped by default")
		Expect(p.User.Mode).To(Equal(guestlaunch.ModeRequire), "a payload that cannot drop to the non-root user does not start (issue #1387)")
		Expect(p.User.UID).To(BeEquivalentTo(guestlaunch.DefaultUID))
		Expect(p.User.GID).To(BeEquivalentTo(guestlaunch.DefaultUID))
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
		Entry("require root", `{"user":{"mode":"require","uid":0}}`, "non-zero uid"),
		Entry("require the root group", `{"user":{"mode":"require","uid":10001,"gid":0}}`, "non-zero gid"),
		Entry("not JSON", `nope`, "guest launcher policy"),
		Entry("relative hidden path", `{"landlock":{"read_exec":["/run"],"hidden":["run/koinon"]}}`, "not absolute"),
		Entry("hidden root", `{"landlock":{"read_exec":["/run"],"hidden":["/"]}}`, "is the root directory"),
		Entry("hidden path in a writable path", `{"landlock":{"read_exec":["/run"],"read_write":["/run"],"hidden":["/run/koinon"]}}`, "inside read_write path"),
		Entry("hidden path in the root writable path", `{"landlock":{"read_write":["/"],"hidden":["/run/koinon"]}}`, "inside read_write path"),
		Entry("read_exec entry inside a hidden path", `{"landlock":{"read_exec":["/run/koinon/ate-jobs"],"hidden":["/run/koinon"]}}`, "is inside hidden path"),
		Entry("read_exec entry equal to a hidden path", `{"landlock":{"read_exec":["/run/koinon"],"hidden":["/run/koinon"]}}`, "is inside hidden path"),
		Entry("unknown landlock scope", `{"landlock":{"scoped":["abstract_unix_socket"]}}`, "unknown landlock scope"),
	)

	// Issue #1496: a payload that can stay root never keeps a capability that reaches the Koinon
	// processes of the guest or the guest kernel.
	DescribeTable("refuses a dangerous capability when the payload can run as root",
		func(capability, mode string) {
			_, err := parse(`{"capabilities":{"allow":["` + capability + `"]},"user":{"mode":"` + mode + `","uid":10001,"gid":10001}}`)
			Expect(err).To(MatchError(ContainSubstring("refused for a payload that can run as root")))
		},
		func(capability, mode string) string { return capability + " with user mode " + mode },
		Entry(nil, "CAP_SYS_PTRACE", "best_effort"),
		Entry(nil, "CAP_SYS_ADMIN", "best_effort"),
		Entry(nil, "CAP_SYS_MODULE", "best_effort"),
		Entry(nil, "CAP_NET_RAW", "best_effort"),
		Entry(nil, "CAP_SYS_BOOT", "best_effort"),
		Entry(nil, "kill", "best_effort"),
		Entry(nil, "CAP_SYS_RAWIO", "best_effort"),
		Entry(nil, "CAP_MKNOD", "best_effort"),
		Entry(nil, "CAP_NET_ADMIN", "best_effort"),
		Entry(nil, "CAP_BPF", "best_effort"),
		Entry(nil, "CAP_PERFMON", "best_effort"),
		Entry(nil, "CAP_SETPCAP", "best_effort"),
		Entry(nil, "CAP_SYS_CHROOT", "best_effort"),
		Entry(nil, "CAP_SYS_ADMIN", "off"),
	)

	It("allows those capabilities when the drop to the non-root user is required", func() {
		_, err := parse(`{"capabilities":{"allow":["CAP_KILL","CAP_NET_RAW"]},"user":{"mode":"require","uid":10001,"gid":10001}}`)
		Expect(err).NotTo(HaveOccurred())
	})
})

// pathCovers is the check that Validate runs; "/" holds every path (a prefix test on dir+"/" would test "//", issue #1516).
var _ = DescribeTable("pathCovers",
	func(dir, path string, want bool) { Expect(guestlaunch.PathCovers(dir, path)).To(Equal(want)) },
	Entry("the root holds every path", "/", "/run/koinon/ate-jobs", true),
	Entry("a parent", "/run", "/run/koinon/ate-jobs", true),
	Entry("the same path", "/run/koinon/ate-jobs", "/run/koinon/ate-jobs", true),
	Entry("a sibling with the same prefix", "/run/koi", "/run/koinon/ate-jobs", false),
	Entry("a child", "/run/koinon/ate-jobs/x", "/run/koinon/ate-jobs", false),
)

// Issue #1516: a root payload on the vm tier must not read the job logs. Landlock has no deny rule, so
// the launcher grants the siblings of the hidden path.
var _ = Describe("ReadExecVisible", func() {
	// tree makes dir/{koinon/ate-jobs, koinon/x, other/file, lock -> other, sock} and returns dir.
	tree := func() string {
		d, err := filepath.EvalSymlinks(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		Expect(os.MkdirAll(filepath.Join(d, "koinon", "ate-jobs"), 0o750)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(d, "koinon", "x"), nil, 0o600)).To(Succeed())
		Expect(os.MkdirAll(filepath.Join(d, "other"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(d, "other", "file"), nil, 0o600)).To(Succeed())
		Expect(os.Symlink(filepath.Join(d, "other"), filepath.Join(d, "lock"))).To(Succeed())
		Expect(os.WriteFile(filepath.Join(d, "sock"), nil, 0o600)).To(Succeed())
		return d
	}

	It("keeps an entry that holds no hidden path", func() {
		l := guestlaunch.LandlockPolicy{ReadExec: []string{"/usr", "/etc"}, Hidden: []string{"/run/koinon"}}
		Expect(l.ReadExecVisible()).To(Equal([]string{"/usr", "/etc"}))
	})
	It("replaces the parent of a hidden path with its other entries", func() {
		d := tree()
		l := guestlaunch.LandlockPolicy{ReadExec: []string{d}, Hidden: []string{filepath.Join(d, "koinon")}}
		Expect(l.ReadExecVisible()).To(ConsistOf(filepath.Join(d, "other"), filepath.Join(d, "sock")), "no hidden path, no symlink")
	})
	It("goes down to a hidden path that is not a direct child", func() {
		d := tree()
		l := guestlaunch.LandlockPolicy{ReadExec: []string{d}, Hidden: []string{filepath.Join(d, "koinon", "ate-jobs")}}
		Expect(l.ReadExecVisible()).To(ConsistOf(filepath.Join(d, "koinon", "x"), filepath.Join(d, "other"), filepath.Join(d, "sock")))
	})
	It("drops an entry that is itself hidden, and skips a missing one", func() {
		d := tree()
		l := guestlaunch.LandlockPolicy{ReadExec: []string{d, filepath.Join(d, "none")}, Hidden: []string{d, filepath.Join(d, "none", "x")}}
		Expect(l.ReadExecVisible()).To(BeEmpty())
	})
	It("is set in both image policies and in the defaults", func() {
		Expect(guestlaunch.DefaultPolicy().Landlock.Hidden).To(ContainElement("/run/koinon"))
		for _, name := range []string{"policy-vm.json", "policy-gvisor.json"} {
			f, err := os.Open("../../cmd/koinon-guest-launcher/" + name)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.Close)
			p, err := guestlaunch.ParsePolicy(f)
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Landlock.Hidden).To(ContainElement(filepath.Dir(guestfront.DefaultLogDir)), name)
		}
	})
})

func jsonList(l []string) string { return `["` + strings.Join(l, `","`) + `"]` }

// The two image policies differ in the Landlock and user modes and the Landlock signal scope only
// (ADR 0120 section 4.2 and 4.3, amendments of 2026-11-26 and 2026-11-27): the vm tier fails closed
// without Landlock, keeps a best_effort user drop (a root payload is accepted in v0.1, issue #1496;
// the non-root drop is issue #1462) and blocks signals to processes outside the payload's Landlock
// domain; the gVisor tiers record the gap when the kernel has no Landlock (their root is writable by
// decision, issue #1150) and fail closed without the non-root user (issue #1387).
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
		func(name string, landlock, user guestlaunch.Mode) {
			p := load(name)
			Expect(p.Landlock.Mode).To(Equal(landlock))
			Expect(p.User.Mode).To(Equal(user))
			Expect(p.Seccomp.Mode).To(Equal(guestlaunch.ModeRequire))
			Expect(p.Capabilities.Allow).To(BeEmpty())
			Expect(p.User.UID).To(BeEquivalentTo(10001))
			Expect(p.User.GID).To(BeEquivalentTo(10001))
			Expect(p.Landlock.Devices).To(ConsistOf(guestlaunch.DefaultDevices), "single device nodes, never /dev as a whole")
			Expect(p.Landlock.Devices).NotTo(ContainElement("/dev"))
			Expect(p.Landlock.ListDirs).To(Equal([]string{"/dev"}))
			for _, rw := range p.Landlock.ReadWrite {
				Expect(rw).NotTo(HavePrefix("/dev"))
			}
		},
		Entry("vm tier", "policy-vm.json", guestlaunch.ModeRequire, guestlaunch.ModeBestEffort),
		Entry("gVisor tiers", "policy-gvisor.json", guestlaunch.ModeBestEffort, guestlaunch.ModeRequire),
	)

	// Issue #1503: the front, as root, writes the job logs in /run/koinon/ate-jobs (guestfront.DefaultLogDir). The payload must not write there, or it could plant a symlink.
	DescribeTable("keep the job log directory of the guest front out of the payload's write rules",
		func(name string) {
			logDir := guestfront.DefaultLogDir
			p := load(name)
			Expect(p.Landlock.ReadExec).To(ContainElement("/run"), "readable, never writable")
			for _, rw := range p.Landlock.ReadWrite {
				Expect(guestlaunch.PathCovers(rw, logDir)).To(BeFalse(), "read_write %s covers the job log directory", rw)
				Expect(rw).NotTo(HavePrefix(logDir))
			}
			for _, dev := range p.Landlock.Devices {
				Expect(dev).NotTo(HavePrefix("/run"), "a device entry becomes a write rule too")
			}
		},
		Entry("vm tier", "policy-vm.json"),
		Entry("gVisor tiers", "policy-gvisor.json"),
	)

	It("differ in nothing but the Landlock and user modes and the signal scope", func() {
		vm, gv := load("policy-vm.json"), load("policy-gvisor.json")
		Expect(vm.Landlock.Scoped).To(Equal([]string{guestlaunch.ScopeSignal}), "a root payload on the vm tier cannot signal the guest front (issue #1496)")
		Expect(gv.Landlock.Scoped).To(BeEmpty(), "the non-root payload of the gVisor tiers cannot signal the root front")
		vm.Landlock.Mode, vm.User.Mode, vm.Landlock.Scoped = gv.Landlock.Mode, gv.User.Mode, gv.Landlock.Scoped
		Expect(vm).To(Equal(gv))
	})
})
