// SPDX-License-Identifier: Apache-2.0

package guestlaunch_test

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
		for _, d := range []string{"tmp", "sandbox", "etc", "usr", "var", "bin"} {
			Expect(tw.WriteHeader(&tar.Header{Name: d + "/", Typeflag: tar.TypeDir, Mode: 0o1777})).To(Succeed())
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
  "landlock": {"mode": "require", "read_exec": ["/bin", "/etc", "/usr", "/var", "/proc"], "read_write": ["/sandbox", "/tmp"], "devices": ["/dev"]},
  "seccomp": {"mode": "require"}
}`)
		for name, doc := range map[string][]byte{
			"policy.json":           policy,
			"user-best-effort.json": []byte(`{"landlock":{"mode":"off"},"user":{"mode":"best_effort","uid":1000,"gid":1000}}`),
			"user-caps.json":        []byte(`{"capabilities":{"allow":["CAP_NET_BIND_SERVICE"]},"landlock":{"mode":"off"},"seccomp":{"mode":"off"},"user":{"mode":"require","uid":1000,"gid":1000}}`),
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
	run := func(argv ...string) (string, int) { return runWith(nil, argv...) }
	runWith = func(flags []string, argv ...string) (string, int) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		args := append([]string{"run", "--rm", "--network=none", "--user=0",
			"--cap-add=SYS_ADMIN", "--cap-add=SYS_PTRACE",
			"--security-opt=seccomp=unconfined", "--security-opt=apparmor=unconfined"}, flags...)
		args = append(append(args, image), argv...)
		cmd := exec.CommandContext(ctx, "docker", args...)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
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
	attempts := []string{"ptrace", "mount", "bpf", "keyctl", "perf_event_open", "unshare_newuser", "unshare_newns", "write:/etc/koinon-probe", "write:/usr/koinon-probe", "write:/var/koinon-probe"}

	It("shows every attempt open without the launcher (control)", func() {
		out, code := run("/bin/probe")
		Expect(code).To(Equal(0), out)
		v := verdicts(out)
		for _, a := range []string{"ptrace", "mount", "unshare_newns", "write:/etc/koinon-probe"} {
			Expect(v).To(HaveKeyWithValue(a, "open"), out)
		}
	})

	It("blocks the escape attempts and keeps the allowed paths writable", func() {
		// SYS_ADMIN and SYS_PTRACE stay in the allowlist so only seccomp and Landlock can be the cause.
		out, code := run("/launcher", "--policy", "/policy.json", "--", "/bin/probe")
		if code == 70 && strings.Contains(out, "landlock required but the kernel does not provide it") {
			Skip("the Docker host kernel has no Landlock")
		}
		Expect(code).To(Equal(0), out)
		v := verdicts(out)
		for _, a := range attempts {
			Expect(v).To(HaveKeyWithValue(a, "blocked"), a+"\n"+out)
		}
		Expect(v).To(HaveKeyWithValue("write:/sandbox/koinon-probe", "open"), out)
		Expect(v).To(HaveKeyWithValue("write:/tmp/koinon-probe", "open"), out)
		Expect(out).To(ContainSubstring("NoNewPrivs: 1"))
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
			out, code := runWith(noSetuid, "/launcher", "--policy", "/user-best-effort.json", "--", "/bin/probe")
			Expect(code).To(Equal(0), out)
			Expect(out).To(ContainSubstring("user drop failed, continuing as the current user"))
			Expect(out).To(ContainSubstring("uid 0"))
			Expect(verdicts(out)).To(HaveKeyWithValue("ptrace", "blocked"), "every other control still applies")
		})

		It("fails closed when the policy requires it", func() {
			out, code := runWith(noSetuid, "/launcher", "--policy", "/user-require.json", "--", "/bin/probe")
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
