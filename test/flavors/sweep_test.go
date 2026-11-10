// SPDX-License-Identifier: Apache-2.0

package flavors_test

import (
	"context"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// koinon-agentd's sweep script (internal/agentd/sweep.sh) on three PID 1s: the OpenShell
// supervisor, whose first child is the sandbox's main process; the contract suite's stand-in,
// whose PID 1 is; and one it does not know, where it must end nothing.
var _ = Describe("koinon-agentd's sandbox sweep", func() {
	var script string

	BeforeEach(func() {
		testenv.Require(testenv.Docker)
		ensureImage(opencode)
		b, err := os.ReadFile("../../internal/agentd/sweep.sh")
		Expect(err).NotTo(HaveOccurred())
		script = string(b)
	})

	// container starts the OpenCode image as the sandbox user with init as PID 1 and env, and
	// leaves a background job in it, as an earlier run could. It returns the container and the
	// job's PID.
	container := func(ctx context.Context, init string, env ...string) (string, string) {
		name := "koinon-sweep-" + randomSuffix()
		args := []string{
			"run", "--detach", "--name", name, "--user", sandboxUID, "--read-only",
			"--tmpfs", "/tmp:rw,exec,mode=1777", "--entrypoint", "/bin/sh",
		}
		for _, e := range env {
			args = append(args, "--env", e)
		}
		docker(ctx, append(args, opencode.image(), "-c", init)...)
		DeferCleanup(func(ctx context.Context) { docker(ctx, "rm", "--force", name) })
		Eventually(func() string {
			out, _, _ := tryExec(ctx, name, "cat", "/proc/1/cmdline")
			return out
		}).Should(Not(BeEmpty()))
		job := strings.TrimSpace(docker(ctx, "exec", name, "sh", "-c",
			`nohup sh -c "while :; do sleep 0.1; done" >/dev/null 2>&1 & echo $!`))
		return name, job
	}
	running := func(ctx context.Context, name, pid string) bool {
		_, _, code := tryExec(ctx, name, "grep", "-qE", `^State:\s+[^Z]`, "/proc/"+pid+"/status")
		return code == 0
	}

	It("keeps the OpenShell supervisor's main process and ends the rest", func(ctx SpecContext) {
		// PID 1 named openshell-sandbox, with the main process (sleep) as its first child.
		name, job := container(ctx, `cp /bin/dash /tmp/openshell-sandbox && exec /tmp/openshell-sandbox -c "sleep 100000 & wait"`)
		main := strings.TrimSpace(docker(ctx, "exec", name, "sh", "-c",
			`for d in /proc/[0-9]*; do grep -q '^PPid:\s*1$' $d/status && grep -q sleep $d/comm && echo ${d#/proc/}; done; true`))
		stdout, stderr, code := tryExec(ctx, name, "/bin/sh", "-c", script, "sweep")
		Expect(code).To(BeZero(), stderr)
		Expect(stdout).To(ContainSubstring("main: " + main + "\n"))
		Expect(stdout).To(MatchRegexp(`ended: .*\b` + job + `\b`))
		Expect(running(ctx, name, main)).To(BeTrue(), "the main process runs")
		Expect(running(ctx, name, job)).To(BeFalse(), "the job is ended")
	})

	It("keeps the stand-in's PID 1 and ends the rest", func(ctx SpecContext) {
		name, job := container(ctx, "exec sleep infinity", "KOINON_SANDBOX_STANDIN=1")
		stdout, stderr, code := tryExec(ctx, name, "/bin/sh", "-c", script, "sweep")
		Expect(code).To(BeZero(), stderr)
		Expect(stdout).To(ContainSubstring("main: 1\n"))
		Expect(running(ctx, name, job)).To(BeFalse(), "the job is ended")
	})

	It("reports a process that survives its signals, and procinfo.sh then reads its state and wait channel", func(ctx SpecContext) {
		// An unkillable process cannot be made without a stuck kernel: `kill` is replaced by a
		// no-op, so the job survives every round, as one in uninterruptible sleep would.
		name, job := container(ctx, "exec sleep infinity", "KOINON_SANDBOX_STANDIN=1")
		stdout, stderr, code := tryExec(ctx, name, "/bin/sh", "-c", "kill() { :; }\n"+script, "sweep")
		Expect(code).To(Equal(1), stderr)
		Expect(stderr).To(ContainSubstring("still running: " + job))
		Expect(stdout).To(MatchRegexp(`(?m)^survivors: .*\b` + job + `:[0-9]+\b`))
		var survivors []string
		for _, l := range strings.Split(stdout, "\n") {
			if v, ok := strings.CutPrefix(l, "survivors:"); ok {
				survivors = strings.Fields(v)
			}
		}

		procinfo, err := os.ReadFile("../../internal/agentd/procinfo.sh")
		Expect(err).NotTo(HaveOccurred())
		out, stderr, code := tryExec(ctx, name, append([]string{"/bin/sh", "-c", string(procinfo), "procinfo"}, survivors...)...)
		Expect(code).To(BeZero(), stderr)
		// State, comm, wchan of the real /proc entry. The job loops on `sleep`, so it forks every
		// 100 ms; a shell that forks with vfork waits killable (D) while the child starts, and
		// procinfo.sh reads state and wchan in separate reads, so D can pair with a later do_wait.
		// A sampling race, not a hang: S, R and D are all valid here.
		Expect(out).To(MatchRegexp(`(?m)^` + job + `\|[SRD]\|sh\|`))

		By("a reused PID (another start time) is not reported")
		out, _, _ = tryExec(ctx, name, "/bin/sh", "-c", string(procinfo), "procinfo", job+":1")
		Expect(out).To(BeEmpty())
	})

	It("refuses, ending nothing, under a PID 1 it does not know", func(ctx SpecContext) {
		name, job := container(ctx, "exec sleep infinity")
		stdout, stderr, code := tryExec(ctx, name, "/bin/sh", "-c", script, "sweep")
		Expect(code).To(Equal(2))
		Expect(stderr).To(ContainSubstring("unknown PID 1 (sleep)"))
		Expect(stdout).NotTo(ContainSubstring("ended:"))
		Expect(running(ctx, name, "1")).To(BeTrue())
		Expect(running(ctx, name, job)).To(BeTrue(), "the job still runs")
	})
})

// tryExec runs cmd in the container name; see sandbox.try.
func tryExec(ctx context.Context, name string, cmd ...string) (stdout, stderr string, code int) {
	return (&sandbox{name: name}).try(ctx, cmd...)
}
