// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"io"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/cli"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/execution/substrate/fakeate"
)

var _ = Describe("Execution", func() {
	It("defaults to OpenShell, which needs its address", func() {
		e := cli.Execution{}
		Expect(e.Validate("")).To(MatchError(ContainSubstring("--openshell-address")))
		Expect(e.Validate("gw:8080")).To(Succeed())
		Expect(e.IsSubstrate()).To(BeFalse())
	})
	It("needs both Substrate addresses and no OpenShell address for substrate", func() {
		e := cli.Execution{Driver: cli.DriverSubstrate}
		Expect(e.Validate("")).To(MatchError(ContainSubstring("--substrate-control-address")))
		e.Substrate = cli.Substrate{ControlAddress: "api:443", RouterAddress: "router:80"}
		Expect(e.Validate("")).To(MatchError(ContainSubstring("--substrate-allow-unhardened")))
		e.Substrate.AllowUnhardened = true
		Expect(e.Validate("")).To(MatchError(ContainSubstring("--substrate-default-tier")))
		e.Substrate.DefaultTier = "dev"
		Expect(e.Validate("")).To(Succeed())
	})
	It("refuses a tier it does not know, a malformed entry and a class listed twice", func() {
		ok := cli.Substrate{ControlAddress: "api:443", RouterAddress: "router:80", AllowUnhardened: true}
		for _, c := range []struct {
			s    func(*cli.Substrate)
			want string
		}{
			{func(s *cli.Substrate) { s.DefaultTier = "root" }, `tier "root"`},
			{func(s *cli.Substrate) { s.RuntimeClassTiers = []string{"kata"} }, "is not class=tier"},
			{func(s *cli.Substrate) { s.RuntimeClassTiers = []string{"kata=fast"} }, `class "kata"`},
			{func(s *cli.Substrate) { s.RuntimeClassTiers = []string{"kata=vm", "kata=dev"} }, "listed twice"},
		} {
			s := ok
			c.s(&s)
			e := cli.Execution{Driver: cli.DriverSubstrate, Substrate: s}
			Expect(e.Validate("")).To(MatchError(ContainSubstring(c.want)))
		}
	})
	It("gives a substrate Exec the tier of the sandbox's runtime class", func(ctx SpecContext) {
		cfg, err := cli.Substrate{
			RuntimeClassTiers: []string{"kata=vm", "gvisor=hardened"}, DefaultTier: "dev",
		}.DriverConfig(nil)
		Expect(err).NotTo(HaveOccurred())
		cfg.PollInterval, cfg.GuestReadyTimeout = 5*time.Millisecond, 2*time.Second
		cl := fakeate.New()
		control, router, err := cl.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(cl.Stop)
		DeferCleanup(control.Close)
		DeferCleanup(router.Close)
		d := substrate.New(control, router, cfg)
		_, err = d.EnsureWorkspace(ctx, "ws", nil)
		Expect(err).NotTo(HaveOccurred())
		for class, policy := range map[string]string{"kata": "policy-vm.json", "gvisor": "policy-gvisor.json", "": "policy-gvisor.json"} {
			name := "sb-" + class
			_, err = d.CreateSandbox(ctx, "ws", execution.SandboxSpec{Name: name, Image: "img", RuntimeClassName: class})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() execution.Phase {
				sb, _ := d.GetSandbox(ctx, "ws", name)
				return sb.Phase
			}).Should(Equal(execution.PhaseReady))
			p, err := d.Exec(ctx, "ws", name, execution.ExecRequest{Command: []string{"cat"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Stdin().Close()).To(Succeed())
			_, _ = io.ReadAll(p.Stdout())
			Expect(p.Close()).To(Succeed())
			Expect(cl.Started("ws", name)[0][:4]).To(Equal([]string{
				"/usr/local/bin/koinon-guest-launcher", "--policy", "/etc/koinon/guest-launcher/" + policy, "--"}))
		}
	})
	It("refuses an unknown driver", func() {
		Expect((&cli.Execution{Driver: "nomad"}).Validate("x")).To(MatchError(ContainSubstring("nomad")))
	})
	It("dials substrate lazily and exposes its collectors", func() {
		c, err := cli.Execution{Driver: cli.DriverSubstrate, Substrate: cli.Substrate{ControlAddress: "127.0.0.1:1", RouterAddress: "127.0.0.1:2", AllowUnhardened: true, DefaultTier: "dev"}}.Dial()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(c.Close)
		Expect(c.Backend).NotTo(BeNil())
		Expect(c.Collectors).NotTo(BeEmpty())
	})
})
