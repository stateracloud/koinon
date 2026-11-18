// SPDX-License-Identifier: Apache-2.0

package guestlaunch_test

import (
	"encoding/binary"

	seccomp "github.com/elastic/go-seccomp-bpf"
	"github.com/elastic/go-seccomp-bpf/arch"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/net/bpf"

	"github.com/stateracloud/koinon/internal/guestlaunch"
)

const (
	eperm  = uint32(seccomp.ActionErrno) | 1
	enosys = uint32(seccomp.ActionErrno) | 38
	allow  = uint32(seccomp.ActionAllow)
)

// run feeds one syscall to the filter in the BPF VM. The VM reads packet words big endian, and the
// filter reads seccomp_data in native order, so each word is stored as the value the filter should
// see (low half of an argument at its offset, high half 4 bytes later, as on little endian hosts).
func run(insts []bpf.Instruction, info *arch.Info, nr int, arg0 uint64) uint32 {
	vm, err := bpf.NewVM(insts)
	Expect(err).NotTo(HaveOccurred())
	pkt := make([]byte, 64)
	binary.BigEndian.PutUint32(pkt[0:], uint32(nr))
	binary.BigEndian.PutUint32(pkt[4:], uint32(info.ID))
	binary.BigEndian.PutUint32(pkt[16:], uint32(arg0))
	binary.BigEndian.PutUint32(pkt[20:], uint32(arg0>>32))
	ret, err := vm.Run(pkt)
	Expect(err).NotTo(HaveOccurred())
	return uint32(ret)
}

var _ = Describe("BuildSeccomp", func() {
	{
		Describe("running architecture", func() {
			var (
				insts   []bpf.Instruction
				info    *arch.Info
				skipped []string
			)
			BeforeEach(func() {
				var err error
				info, err = arch.GetInfo("")
				Expect(err).NotTo(HaveOccurred())
				insts, skipped, err = guestlaunch.BuildSeccomp(guestlaunch.SeccompPolicy{})
				Expect(err).NotTo(HaveOccurred())
			})

			It("denies the escape syscalls with EPERM", func() {
				for _, n := range []string{"ptrace", "mount", "bpf", "keyctl", "kexec_load", "init_module", "finit_module", "pivot_root", "perf_event_open", "unshare", "setns", "umount2"} {
					Expect(info.SyscallNames).To(HaveKey(n))
					Expect(run(insts, info, info.SyscallNames[n], 0)).To(Equal(eperm), n)
				}
			})

			It("denies io_uring with EPERM", func() {
				for _, n := range []string{"io_uring_setup", "io_uring_enter", "io_uring_register"} {
					Expect(info.SyscallNames).To(HaveKey(n))
					Expect(run(insts, info, info.SyscallNames[n], 0)).To(Equal(eperm), n)
				}
			})

			It("denies socket(AF_VSOCK) and allows the other families", func() {
				socket := info.SyscallNames["socket"]
				Expect(run(insts, info, socket, 40)).To(Equal(eperm), "AF_VSOCK")
				// The domain is an int: whatever sits in the high word of the register does not change it.
				Expect(run(insts, info, socket, 0xdeadbeef00000028)).To(Equal(eperm), "AF_VSOCK with a dirty high word")
				for _, family := range []uint64{1, 2, 10, 16, 17} { // UNIX, INET, INET6, NETLINK, PACKET
					Expect(run(insts, info, socket, family)).To(Equal(allow), "family %d", family)
				}
			})

			It("answers a call of the x32 ABI with ENOSYS", func() {
				if info.Name != "x86_64" {
					Skip("x32 exists on x86_64 only")
				}
				for _, nr := range []int{0x40000000, 0x40000000 + info.SyscallNames["ptrace"], 0x40000000 + 41, 0x7fffffff} {
					Expect(run(insts, info, nr, 0)).To(Equal(enosys), "nr %#x", nr)
				}
			})

			It("allows ordinary syscalls", func() {
				for _, n := range []string{"read", "write", "openat", "execve", "futex", "socket", "connect", "mmap"} {
					Expect(run(insts, info, info.SyscallNames[n], 0)).To(Equal(allow), n)
				}
			})

			It("denies clone with a namespace flag and allows a thread clone", func() {
				clone := info.SyscallNames["clone"]
				Expect(run(insts, info, clone, 0x10000000)).To(Equal(eperm), "CLONE_NEWUSER")
				Expect(run(insts, info, clone, 0x40000000)).To(Equal(eperm), "CLONE_NEWNET")
				Expect(run(insts, info, clone, 0x00010f00)).To(Equal(allow), "CLONE_VM|FS|FILES|SIGHAND|THREAD")
			})

			It("answers clone3 with ENOSYS so glibc falls back to clone", func() {
				Expect(run(insts, info, info.SyscallNames["clone3"], 0)).To(Equal(enosys))
			})

			It("refuses a call from a foreign architecture (i386 compat ptrace)", func() {
				vm, err := bpf.NewVM(insts)
				Expect(err).NotTo(HaveOccurred())
				pkt := make([]byte, 64)
				binary.BigEndian.PutUint32(pkt[0:], 26) // ptrace in the i386 numbering
				binary.BigEndian.PutUint32(pkt[4:], 0x40000003)
				ret, err := vm.Run(pkt)
				Expect(err).NotTo(HaveOccurred())
				Expect(uint32(ret)).To(Equal(enosys))
			})

			It("names only calls the architecture knows", func() {
				for _, n := range skipped {
					Expect(info.SyscallNames).NotTo(HaveKey(n))
				}
				Expect(skipped).NotTo(ContainElement("ptrace"))
			})
		})
	}

	It("lets a policy replace and extend the list", func() {
		insts, _, err := guestlaunch.BuildSeccomp(guestlaunch.SeccompPolicy{Deny: []string{"ptrace"}, DenyExtra: []string{"syslog"}})
		Expect(err).NotTo(HaveOccurred())
		info, _ := arch.GetInfo("")
		Expect(run(insts, info, info.SyscallNames["ptrace"], 0)).To(Equal(eperm))
		Expect(run(insts, info, info.SyscallNames["syslog"], 0)).To(Equal(eperm))
		Expect(run(insts, info, info.SyscallNames["mount"], 0)).To(Equal(allow), "the replaced list no longer names mount")
	})

	It("keeps the process-access calls denied when a policy replaces the list", func() {
		insts, _, err := guestlaunch.BuildSeccomp(guestlaunch.SeccompPolicy{Deny: []string{"syslog"}})
		Expect(err).NotTo(HaveOccurred())
		info, _ := arch.GetInfo("")
		for _, n := range []string{"ptrace", "process_vm_readv", "process_vm_writev", "pidfd_getfd", "kcmp"} {
			Expect(info.SyscallNames).To(HaveKey(n))
			Expect(run(insts, info, info.SyscallNames[n], 0)).To(Equal(eperm), n)
		}
		Expect(run(insts, info, info.SyscallNames["mount"], 0)).To(Equal(allow), "the replaced list no longer names mount")
	})

	It("skips a name the running architecture does not have", func() {
		info, _ := arch.GetInfo("")
		other := "arm_fadvise64_64" // arm only
		if _, ok := info.SyscallNames[other]; ok {
			other = "_sysctl" // gone from arm64
			if _, ok := info.SyscallNames[other]; ok {
				Skip("no name the running architecture lacks")
			}
		}
		_, skipped, err := guestlaunch.BuildSeccomp(guestlaunch.SeccompPolicy{DenyExtra: []string{other}})
		Expect(err).NotTo(HaveOccurred())
		Expect(skipped).To(ContainElement(other))
	})
})
