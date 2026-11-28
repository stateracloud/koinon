// SPDX-License-Identifier: Apache-2.0

package substrate_test

import (
	"archive/tar"
	"bytes"
	"io"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/execution/substrate/fakeate"
	"github.com/stateracloud/koinon/internal/harness"
)

const launcher = "/usr/local/bin/koinon-guest-launcher"

var _ = Describe("the launcher prefix", func() {
	var (
		d  *substrate.Driver
		cl *fakeate.Cluster
	)
	BeforeEach(func() {
		noTier = false
		DeferCleanup(func() { noTier = false })
		d, cl = newDriver()
	})

	// run starts a command, reads it to its end and returns its output.
	run := func(ctx SpecContext, sandbox string, req execution.ExecRequest) (string, int) {
		p, err := d.Exec(ctx, ws, sandbox, req)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(p.Close)
		Expect(p.Stdin().Close()).To(Succeed()) // cat ends at end of input
		out, err := io.ReadAll(p.Stdout())
		Expect(err).NotTo(HaveOccurred())
		code, err := p.Wait()
		Expect(err).NotTo(HaveOccurred())
		return string(out), code
	}

	createIn := func(ctx SpecContext, name, class string) {
		sp := spec(name)
		sp.RuntimeClassName = class
		_, err := d.CreateSandbox(ctx, ws, sp)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() execution.Phase {
			sb, _ := d.GetSandbox(ctx, ws, name)
			return sb.Phase
		}).Should(Equal(execution.PhaseReady))
	}

	// The fake guest strips the launcher and runs the rest, so cat ends at end of input.
	It("puts the launcher and the policy of the tier before every command", func(ctx SpecContext) {
		for class, policy := range map[string]string{"kata": "policy-vm.json", "gvisor": "policy-gvisor.json"} {
			name := "sb-" + class
			createIn(ctx, name, class)
			_, _ = run(ctx, name, execution.ExecRequest{Command: []string{"cat"}})
			Expect(cl.Started(ws, name)).To(Equal([][]string{
				{launcher, "--policy", "/etc/koinon/guest-launcher/" + policy, "--", "cat"},
			}))
		}
	})

	It("uses the default tier for a sandbox with no runtime class", func(ctx SpecContext) {
		ready(ctx, d, "sb")
		_, _ = run(ctx, "sb", execution.ExecRequest{Command: []string{"cat"}})
		Expect(cl.Started(ws, "sb")[0][:4]).To(Equal(
			[]string{launcher, "--policy", "/etc/koinon/guest-launcher/policy-gvisor.json", "--"}))
	})

	It("wraps the login shell too, so the launcher is the outermost process", func(ctx SpecContext) {
		ready(ctx, d, "sb")
		_, _ = run(ctx, "sb", execution.ExecRequest{Command: []string{"cat"}, LoginShell: true})
		Expect(cl.Started(ws, "sb")[0]).To(Equal([]string{
			launcher, "--policy", "/etc/koinon/guest-launcher/policy-gvisor.json", "--",
			"bash", "-lc", `exec "$@"`, "bash", "cat",
		}))
	})

	It("does not change the caller's command", func(ctx SpecContext) {
		ready(ctx, d, "sb")
		cmd := []string{"true"}
		_, _ = run(ctx, "sb", execution.ExecRequest{Command: cmd})
		Expect(cmd).To(Equal([]string{"true"}))
	})

	It("refuses a runtime class that has no tier, and starts no process", func(ctx SpecContext) {
		createIn(ctx, "sb", "firecracker")
		_, err := d.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"true"}})
		Expect(err).To(MatchError(execution.ErrFailedPrecondition))
		Expect(cl.Started(ws, "sb")).To(BeEmpty())
	})

	It("refuses every command when the driver has no tier at all", func(ctx SpecContext) {
		noTier = true
		d, cl = newDriver()
		ready(ctx, d, "sb")
		_, err := d.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"true"}})
		Expect(err).To(MatchError(execution.ErrFailedPrecondition))
		Expect(cl.Started(ws, "sb")).To(BeEmpty())
	})

	It("refuses a tier that has no launcher policy", func(ctx SpecContext) {
		d, cl = newDriver(substrate.Config{DefaultTier: harness.Tier("bogus")})
		ready(ctx, d, "sb")
		_, err := d.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"true"}})
		Expect(err).To(MatchError(execution.ErrFailedPrecondition))
		Expect(cl.Started(ws, "sb")).To(BeEmpty())
	})

	It("wraps the commands of Upload too", func(ctx SpecContext) {
		ready(ctx, d, "sb")
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		Expect(tw.Close()).To(Succeed())
		Expect(d.Upload(ctx, ws, "sb", "/sandbox", &buf)).To(Succeed())
		started := cl.Started(ws, "sb")
		Expect(started).To(HaveLen(2)) // mkdir, then tar with the archive on stdin
		for _, argv := range started {
			Expect(argv[:4]).To(Equal([]string{launcher, "--policy", "/etc/koinon/guest-launcher/policy-gvisor.json", "--"}))
		}
	})
})
