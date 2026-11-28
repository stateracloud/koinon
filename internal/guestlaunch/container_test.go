// SPDX-License-Identifier: Apache-2.0

package guestlaunch_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// The launcher runs a payload in a real container: a scratch image made from the launcher, the
// escape probe (a port of hack/spike/probe) and a policy, with no network and no registry. The
// container gets SYS_ADMIN and SYS_PTRACE and runs without Docker's own seccomp and AppArmor, so
// that the control group (no launcher) shows each attempt open, and every block in the launcher
// run is the launcher's own.
var _ = Describe("the launcher in a container", Label("docker"), Ordered, func() {
	var image string // unique per run: parallel runs and leftovers never share a tag
	var dir string
	var runWith func(flags []string, argv ...string) (string, int)
	// broadCaps lets every attempt of the probe reach the point where only the launcher can stop it.
	broadCaps := []string{"--cap-add=SYS_ADMIN", "--cap-add=SYS_PTRACE"}

	build := func(pkg, out string) {
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, out), pkg)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
		cmd.Dir = repoRoot()
		out2, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out2))
	}

	BeforeAll(func() {
		testenv.Require(testenv.Docker, testenv.Binary("docker"), testenv.Binary("go"))
		image = fmt.Sprintf("koinon-guestlaunch-test:%d-%08x", os.Getpid(), rand.Uint32())
		dir = GinkgoT().TempDir()
		build("./cmd/koinon-guest-launcher", "launcher")
		build("./internal/guestlaunch/testdata/probe", "probe")

		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, d := range []string{"tmp", "sandbox", "etc", "usr", "var", "bin", "run"} {
			Expect(tw.WriteHeader(&tar.Header{Name: d + "/", Typeflag: tar.TypeDir, Mode: 0o1777})).To(Succeed())
		}
		// Issue #1516: the job log directory exists and is readable by anyone, so Landlock alone is the control.
		for _, d := range []string{"run/koinon", "run/koinon/ate-jobs"} {
			Expect(tw.WriteHeader(&tar.Header{Name: d + "/", Typeflag: tar.TypeDir, Mode: 0o755})).To(Succeed())
		}
		add := func(name, from string, mode int64) {
			b, err := os.ReadFile(from)
			Expect(err).NotTo(HaveOccurred())
			Expect(tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(b))})).To(Succeed())
			_, err = tw.Write(b)
			Expect(err).NotTo(HaveOccurred())
		}
		add("launcher", filepath.Join(dir, "launcher"), 0o755)
		add("bin/probe", filepath.Join(dir, "probe"), 0o755)
		policy := []byte(`{
  "capabilities": {"allow": ["CAP_SYS_ADMIN", "CAP_SYS_PTRACE"]},
  "landlock": {"mode": "require", "read_exec": ["/bin", "/etc", "/usr", "/var", "/proc"], "read_write": ["/sandbox", "/tmp"], "devices": ["/dev/null", "/dev/zero", "/dev/urandom", "/dev/tty", "/dev/ptmx", "/dev/pts", "/dev/shm"], "list_dirs": ["/dev"], "scoped": ["signal"]},
  "seccomp": {"mode": "require"},
  "user": {"mode": "require", "uid": 1000, "gid": 1000}
}`)
		// The image policies, as the overlay carries them (cmd/koinon-guest-launcher).
		images := map[string][]byte{}
		for _, name := range []string{"policy-vm.json", "policy-gvisor.json"} {
			b, err := os.ReadFile(filepath.Join(repoRoot(), "cmd", "koinon-guest-launcher", name))
			Expect(err).NotTo(HaveOccurred())
			images[name] = b
		}
		for name, doc := range images {
			Expect(tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(doc))})).To(Succeed())
			_, err := tw.Write(doc)
			Expect(err).NotTo(HaveOccurred())
		}
		for name, doc := range map[string][]byte{
			"policy.json":           policy,
			"user-best-effort.json": []byte(`{"landlock":{"mode":"off"},"user":{"mode":"best_effort","uid":1000,"gid":1000}}`),
			"user-caps.json":        []byte(`{"capabilities":{"allow":["CAP_NET_BIND_SERVICE"]},"landlock":{"mode":"off"},"seccomp":{"mode":"require"},"user":{"mode":"require","uid":1000,"gid":1000}}`),
			"user-require.json":     []byte(`{"landlock":{"mode":"off"},"user":{"mode":"require","uid":1000,"gid":1000}}`),
		} {
			Expect(tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(doc))})).To(Succeed())
			_, err := tw.Write(doc)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(tw.Close()).To(Succeed())
		imp := exec.Command("docker", "import", "-", image)
		imp.Stdin = &buf
		out, err := imp.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out))
	})

	AfterAll(func() { _ = exec.Command("docker", "rmi", "-f", image).Run() })

	// run starts the container with argv as its command and returns the combined output and exit code.
	run := func(argv ...string) (string, int) { return runWith(broadCaps, argv...) }
	// runWith starts a container as root with no network, no Docker seccomp (unless flags name a
	// profile) and no AppArmor, and only the capabilities that flags add to Docker's defaults.
	runWith = func(flags []string, argv ...string) (string, int) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		args := []string{
			"run", "--rm", "--network=none", "--user=0", "--security-opt=apparmor=unconfined",
			// A device node that no policy names, for the probe to open.
			"--device=/dev/zero:/dev/koinon-unlisted",
		}
		own := false
		for _, f := range flags {
			own = own || strings.HasPrefix(f, "--security-opt=seccomp=")
		}
		if !own {
			args = append(args, "--security-opt=seccomp=unconfined")
		}
		args = append(append(args, flags...), image)
		args = append(args, argv...)
		cmd := exec.CommandContext(ctx, "docker", args...)
		out, err := cmd.CombinedOutput()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			Fail(err.Error())
		}
		return string(out), code
	}
	verdicts := func(out string) map[string]string {
		m := map[string]string{}
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) >= 2 && (f[1] == "open" || f[1] == "blocked") {
				m[f[0]] = f[1]
			}
		}
		return m
	}
	// fdAttempts are the ways to a socket the payload did not create: a descriptor left open without
	// CLOEXEC before the launcher, and the descriptors of a process outside the launcher.
	fdAttempts := []string{
		"inherited_fd", "proc_reopen_own_socket", "proc_fd_open_other",
		"process_vm_readv_other", "ptrace_other", "pidfd_getfd_other",
		// Issue #1496: no signal reaches a process outside the payload, the guest front among them.
		"kill_other", "pidfd_signal_other",
	}
	// procReadAttempts read another process's /proc entries without taking a descriptor. A payload
	// that holds CAP_SYS_PTRACE (policy.json keeps it on purpose) passes the kernel's access check;
	// the image policies leave the payload no capability, and then they are blocked. The probe also
	// lists /proc/<pid>/fd (proc_fd_list_other): for the same uid that is a mode check only and shows
	// numbers, not descriptors, so it is reported and not asserted.
	procReadAttempts := []string{"proc_environ_other"}
	attempts := append([]string{
		"ptrace", "mount", "bpf", "keyctl", "perf_event_open", "unshare_newuser", "unshare_newns",
		"clone_newuser", "clone3", "io_uring_setup", "socket_vsock",
		// Issue #1496: loading a module, a reboot and a raw socket (CAP_SYS_MODULE, CAP_SYS_BOOT, CAP_NET_RAW).
		"finit_module", "reboot", "socket_raw",
		"write:/etc/koinon-probe", "write:/usr/koinon-probe", "write:/var/koinon-probe",
		// Issue #1503: the job log directory of the guest front is under /run, read-only for the payload.
		"write:/run/koinon-probe", "symlink:/run/koinon-link",
		// Issue #1516: nor may it read the job logs (the image policies list /run/koinon as hidden).
		"read:/run/koinon/ate-jobs",
		"dev_create:/dev/koinon-probe", "dev_mknod_block", "dev_open_unlisted:/dev/koinon-unlisted",
	}, fdAttempts...)
	// inherit runs argv after the probe has played the guest server: a socket at fd 7 without
	// CLOEXEC and an outside process that holds a socket.
	inherit := func(argv ...string) []string { return append([]string{"/bin/probe", "inherit", "--"}, argv...) }
	// enxio is the kernel's answer to open() of a socket through its /proc fd link.
	const enxio = "no such device or address"
	// skipWithoutLandlock turns the one outcome that means "this Docker host has no Landlock of the
	// ABI the policy needs" into a skip; every other failure stays a failure.
	skipWithoutLandlock := func(out string, code int) {
		if code == 70 && (strings.Contains(out, "landlock required but the kernel") ||
			strings.Contains(out, "landlock signal scope required but the kernel")) {
			Skip("the Docker host kernel has no Landlock of the ABI the policy needs")
		}
	}

	It("shows every attempt open without the launcher (control)", func() {
		out, code := run(inherit("/bin/probe")...)
		Expect(code).To(Equal(0), out)
		v := verdicts(out)
		for _, a := range []string{
			"ptrace", "mount", "unshare_newns", "write:/etc/koinon-probe", "dev_create:/dev/koinon-probe", "dev_mknod_block", "clone3",
			"inherited_fd", "proc_fd_list_other", "proc_environ_other", "ptrace_other", "pidfd_getfd_other",
			"kill_other", "pidfd_signal_other", "socket_raw", "write:/run/koinon-probe", "symlink:/run/koinon-link", "read:/run/koinon/ate-jobs",
		} {
			Expect(v).To(HaveKeyWithValue(a, "open"), out)
		}
		// Even unconfined, a socket cannot be opened through its /proc fd link: the kernel answers ENXIO.
		Expect(out).To(MatchRegexp(`proc_reopen_own_socket blocked .*`+enxio), out)
		Expect(out).To(MatchRegexp(`proc_fd_open_other blocked .*`+enxio), out)
	})

	It("blocks the escape attempts and keeps the allowed paths writable", func() {
		// SYS_ADMIN and SYS_PTRACE stay in the allowlist so only seccomp and Landlock can be the cause.
		// The drop to uid 1000 is required: a policy that can leave the payload root refuses them (issue #1496).
		out, code := run(inherit("/launcher", "--policy", "/policy.json", "--", "/bin/probe")...)
		skipWithoutLandlock(out, code)
		Expect(code).To(Equal(0), out)
		v := verdicts(out)
		for _, a := range attempts {
			Expect(v).To(HaveKeyWithValue(a, "blocked"), a+"\n"+out)
		}
		Expect(v).To(HaveKeyWithValue("write:/sandbox/koinon-probe", "open"), out)
		Expect(v).To(HaveKeyWithValue("write:/tmp/koinon-probe", "open"), out)
		Expect(v).To(HaveKeyWithValue("dev_allowed:/dev/null", "open"), "a listed device node works\n"+out)
		Expect(v).To(HaveKeyWithValue("dev_list", "open"), "/dev can be listed\n"+out)
		// A block device node in /dev, when the host gives the container one, cannot be written.
		if bv, ok := v["dev_write_blockdev"]; ok {
			Expect(bv).To(Equal("blocked"), out)
		}
		Expect(out).To(ContainSubstring("NoNewPrivs: 1"))
	})

	// The two image policies of ADR 0120 section 4.2 and 4.3.
	Describe("the image policies in the shape the Substrate guest gives a payload", func() {
		// vm tier: root with only CAP_KILL, CAP_NET_BIND_SERVICE and CAP_AUDIT_WRITE (phase 0 result,
		// issue #1087; the Substrate default set, until issue #1462).
		substrate := []string{"--cap-drop=ALL", "--cap-add=KILL", "--cap-add=NET_BIND_SERVICE", "--cap-add=AUDIT_WRITE"}
		// gVisor tiers: root with the set of the actor template (substrate.GVisorCapAdd, issue #1387).
		gvisor := []string{"--cap-drop=ALL", "--cap-add=KILL", "--cap-add=SETGID", "--cap-add=SETPCAP", "--cap-add=SETUID"}

		DescribeTable("leave the payload no capability and set no_new_privs",
			func(policy string, shape []string, uid string) {
				out, code := runWith(shape, inherit("/launcher", "--policy", "/"+policy, "--", "/bin/probe")...)
				skipWithoutLandlock(out, code)
				Expect(code).To(Equal(0), out)
				Expect(out).To(ContainSubstring("uid "+uid+"\n"), out)
				for _, f := range []string{"CapPrm", "CapEff", "CapAmb"} {
					Expect(out).To(MatchRegexp(`status `+f+`: 0+\b`), f+"\n"+out)
				}
				Expect(out).To(ContainSubstring("status NoNewPrivs: 1"))
				v := verdicts(out)
				for _, a := range slices.Concat(attempts, procReadAttempts) {
					Expect(v).To(HaveKeyWithValue(a, "blocked"), a+"\n"+out)
				}
				if bv, ok := v["dev_write_blockdev"]; ok {
					Expect(bv).To(Equal("blocked"), out)
				}
				Expect(v).To(HaveKeyWithValue("kill_own_child", "open"), "the payload signals its own processes\n"+out)
			},
			Entry("vm tier", "policy-vm.json", substrate, "0"),
			Entry("gVisor tiers", "policy-gvisor.json", gvisor, "10001"),
		)

		It("shrinks the bounding set to nothing on the gVisor tiers", func() {
			out, code := runWith(gvisor, inherit("/launcher", "--policy", "/policy-gvisor.json", "--", "/bin/probe")...)
			skipWithoutLandlock(out, code)
			Expect(code).To(Equal(0), out)
			Expect(out).To(MatchRegexp(`status CapBnd: 0+\b`), out)
			Expect(out).To(ContainSubstring("status NoNewPrivs: 1"))
		})

		// Issue #1387: a gVisor sandbox whose guest cannot drop to the non-root user does not start.
		// The vm shape (no CAP_SETUID or CAP_SETGID) is such a guest.
		It("refuses to start the payload on the gVisor tiers when the guest cannot drop to non-root", func() {
			out, code := runWith(substrate, "/launcher", "--policy", "/policy-gvisor.json", "--", "/bin/probe")
			Expect(code).To(Equal(70), out)
			Expect(out).To(ContainSubstring("user drop to 10001:10001 required but failed"))
			Expect(out).NotTo(ContainSubstring("ptrace"), "the payload never started")
		})

		// A host without a syscall is simulated by a Docker seccomp profile that answers it ENOSYS.
		hideWithIn := func(shape []string, syscall string, errno int) []string {
			prof := fmt.Sprintf(`{"defaultAction":"SCMP_ACT_ALLOW","syscalls":[{"names":[%q],"action":"SCMP_ACT_ERRNO","errnoRet":%d}]}`, syscall, errno)
			path := filepath.Join(dir, fmt.Sprintf("no-%s-%d.json", syscall, errno))
			Expect(os.WriteFile(path, []byte(prof), 0o600)).To(Succeed())
			return append([]string{"--security-opt=seccomp=" + path}, shape...)
		}
		hideWith := func(syscall string, errno int) []string { return hideWithIn(substrate, syscall, errno) }
		hide := func(syscall string) []string { return hideWith(syscall, 38) }
		noLandlock := func() []string { return hide("landlock_create_ruleset") }
		noLandlockGVisor := func() []string { return hideWithIn(gvisor, "landlock_create_ruleset", 38) }

		It("closes inherited descriptors through /proc/self/fd when close_range is missing", func() {
			out, code := runWith(hide("close_range"), inherit("/launcher", "--policy", "/policy-vm.json", "--", "/bin/probe")...)
			skipWithoutLandlock(out, code)
			Expect(code).To(Equal(0), out)
			Expect(verdicts(out)).To(HaveKeyWithValue("inherited_fd", "blocked"), out)
		})

		It("falls back to /proc/self/fd when an outer seccomp profile answers close_range with EPERM", func() {
			out, code := runWith(hideWith("close_range", 1), inherit("/launcher", "--policy", "/policy-vm.json", "--", "/bin/probe")...)
			skipWithoutLandlock(out, code)
			Expect(code).To(Equal(0), out)
			Expect(verdicts(out)).To(HaveKeyWithValue("inherited_fd", "blocked"), out)
		})

		It("keeps the outside process's descriptors closed to the payload without Landlock", func() {
			// No Landlock domain. The launcher closes the inherited descriptors before it starts the
			// payload. The kernel's ptrace access check refuses the /proc reads: the payload has another
			// uid than the outside process and no CAP_SYS_PTRACE.
			out, code := runWith(noLandlockGVisor(), inherit("/launcher", "--policy", "/policy-gvisor.json", "--", "/bin/probe")...)
			Expect(code).To(Equal(0), out)
			Expect(out).To(ContainSubstring("uid 10001\n"))
			v := verdicts(out)
			for _, a := range slices.Concat(fdAttempts, procReadAttempts) {
				Expect(v).To(HaveKeyWithValue(a, "blocked"), a+"\n"+out)
			}
		})

		It("fails closed on the vm tier when Landlock is unavailable", func() {
			out, code := runWith(noLandlock(), "/launcher", "--policy", "/policy-vm.json", "--", "/bin/probe")
			Expect(code).To(Equal(70), out)
			Expect(out).To(ContainSubstring("landlock required but the kernel does not provide it"))
			Expect(out).NotTo(ContainSubstring("ptrace"), "the payload never started")
		})

		It("starts on the gVisor tiers when Landlock is unavailable and says what the control is", func() {
			out, code := runWith(noLandlockGVisor(), "/launcher", "--policy", "/policy-gvisor.json", "--", "/bin/probe")
			Expect(code).To(Equal(0), out)
			Expect(out).To(ContainSubstring("filesystem rules NOT applied"))
			Expect(out).To(ContainSubstring("writable root by decision"))
			Expect(verdicts(out)).To(HaveKeyWithValue("ptrace", "blocked"), "every other control still applies")
		})
	})

	// Phase 0 (issue #1087): the Substrate guest gives payloads no CAP_SETUID or CAP_SETGID.
	// --cap-drop=SETUID --cap-drop=SETGID reproduces that.
	Describe("the non-root user drop", func() {
		noSetuid := []string{"--cap-drop=SETUID", "--cap-drop=SETGID"}

		It("drops to the user when the guest allows it", func() {
			out, code := run("/launcher", "--policy", "/user-best-effort.json", "--", "/bin/probe")
			Expect(code).To(Equal(0), out)
			Expect(out).To(ContainSubstring("uid 1000"))
		})

		It("keeps exactly the allowlist in the bounding, permitted, effective and ambient sets after the drop", func() {
			out, code := run("/launcher", "--policy", "/user-caps.json", "--", "/bin/probe")
			Expect(code).To(Equal(0), out)
			Expect(out).To(ContainSubstring("uid 1000"))
			// CAP_NET_BIND_SERVICE is capability 10: 0x400.
			for _, f := range []string{"CapBnd", "CapPrm", "CapEff", "CapAmb"} {
				Expect(out).To(MatchRegexp(`status `+f+`: 0+400\b`), f+"\n"+out)
			}
			Expect(out).NotTo(ContainSubstring("could not be raised"))
		})

		It("continues as root with a clear log line when the guest lacks CAP_SETUID", func() {
			out, code := runWith(append(append([]string{}, broadCaps...), noSetuid...), "/launcher", "--policy", "/user-best-effort.json", "--", "/bin/probe")
			Expect(code).To(Equal(0), out)
			Expect(out).To(ContainSubstring("user drop failed, continuing as the current user"))
			Expect(out).To(ContainSubstring("uid 0"))
			Expect(verdicts(out)).To(HaveKeyWithValue("ptrace", "blocked"), "every other control still applies")
		})

		It("fails closed when the policy requires it", func() {
			out, code := runWith(append(append([]string{}, broadCaps...), noSetuid...), "/launcher", "--policy", "/user-require.json", "--", "/bin/probe")
			Expect(code).To(Equal(70), out)
			Expect(out).To(ContainSubstring("user drop to 1000:1000 required but failed"))
			Expect(out).NotTo(ContainSubstring("ptrace"), "the payload never started")
		})
	})
})

func repoRoot() string {
	wd, err := os.Getwd()
	Expect(err).NotTo(HaveOccurred())
	for d := wd; d != "/"; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	Fail("go.mod not found above " + wd)
	return ""
}
