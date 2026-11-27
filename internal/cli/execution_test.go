// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"io"
	"os"
	"strings"
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
		e.Substrate = cli.Substrate{ControlAddress: "api:443", RouterAddress: "router:443"}
		Expect(e.Validate("")).To(MatchError(ContainSubstring("--substrate-guest-key-file is required")))
		e.Substrate.GuestKeyFile = guestKeyFile()
		Expect(e.Validate("")).To(MatchError(ContainSubstring("--substrate-allow-unhardened")))
		e.Substrate.AllowUnhardened = true
		Expect(e.Validate("")).To(MatchError(ContainSubstring("--substrate-default-tier")))
		e.Substrate.DefaultTier = "dev"
		Expect(e.Validate("")).To(Succeed())
		// Validate does not read the key: the startup check runs where the Secret is not mounted.
		// Dial reads it (the next spec).
		e.Substrate.GuestKeyFile = GinkgoT().TempDir() + "/missing"
		Expect(e.Validate("")).To(Succeed())
	})
	It("refuses a tier it does not know, a malformed entry and a class listed twice", func() {
		ok := cli.Substrate{ControlAddress: "api:443", RouterAddress: "router:443", AllowUnhardened: true, GuestKeyFile: guestKeyFile()}
		for _, c := range []struct {
			s    func(*cli.Substrate)
			want string
		}{
			{func(s *cli.Substrate) { s.DefaultTier = "root" }, `tier "root"`},
			{func(s *cli.Substrate) { s.RuntimeClassTiers = []string{"kata"} }, "is not class=tier"},
			{func(s *cli.Substrate) { s.RuntimeClassTiers = []string{"kata=fast"} }, `class "kata"`},
			{func(s *cli.Substrate) { s.RuntimeClassTiers = []string{"kata=vm", "kata=dev"} }, "listed twice"},
			{func(s *cli.Substrate) { s.ImageMirrors = []string{"ghcr.io"} }, "is not host=target"},
			{func(s *cli.Substrate) { s.ImageMirrors = []string{"ghcr.io=a", "GHCR.io=b"} }, "listed twice"},
		} {
			s := ok
			c.s(&s)
			e := cli.Execution{Driver: cli.DriverSubstrate, Substrate: s}
			Expect(e.Validate("")).To(MatchError(ContainSubstring(c.want)))
		}
	})
	It("reads the image source rewrite into the driver config", func() {
		cfg, err := cli.Substrate{
			DefaultTier: "dev", ImageRegistry: " mirror.example.com/all ",
			ImageMirrors: []string{"GHCR.io=cache.koinon.svc/ghcr.io", "docker.io = cache.koinon.svc/docker.io"},
		}.DriverConfig(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.ImageRegistry).To(Equal("mirror.example.com/all"))
		Expect(cfg.ImageMirrors).To(Equal(map[string]string{"ghcr.io": "cache.koinon.svc/ghcr.io", "docker.io": "cache.koinon.svc/docker.io"}))
	})
	It("leaves golden snapshots off unless the setting is on (#1492)", func() {
		cfg, err := cli.Substrate{DefaultTier: "dev"}.DriverConfig(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.GoldenSnapshots).To(BeFalse())
		cfg, err = cli.Substrate{DefaultTier: "dev", GoldenSnapshots: true}.DriverConfig(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.GoldenSnapshots).To(BeTrue())
	})
	It("builds the content digest from the snapshot store flags, or fails closed without them", func() {
		cfg, err := cli.Substrate{DefaultTier: "dev"}.DriverConfig(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.ContentDigest).To(BeNil()) // snapshots fail closed in the driver
		store := cli.SnapshotStore{Endpoint: "http://s3:9000", Bucket: "koinon-test"}
		_, err = cli.Substrate{SnapshotStore: store}.DriverConfig(nil)
		Expect(err).To(MatchError(ContainSubstring("--substrate-snapshot-access-key-file")))
		dir := GinkgoT().TempDir()
		store.AccessKeyFile, store.SecretKeyFile = dir+"/access", dir+"/secret"
		Expect(os.WriteFile(store.AccessKeyFile, []byte("koinon_test_not-a-real-key\n"), 0o600)).To(Succeed())
		_, err = cli.Substrate{SnapshotStore: store}.DriverConfig(nil)
		Expect(err).To(MatchError(ContainSubstring("--substrate-snapshot-secret-key-file")))
		Expect(os.WriteFile(store.SecretKeyFile, []byte("koinon_test_not-a-real-secret\n"), 0o600)).To(Succeed())
		cfg, err = cli.Substrate{SnapshotStore: store}.DriverConfig(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.ContentDigest).NotTo(BeNil())
	})
	It("copies valid gateway routes into the driver config, and refuses invalid ones (#1444)", func() {
		cfg, err := cli.Substrate{}.DriverConfig(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.GatewayRoutes).To(BeEmpty(), "agentd sets none: it creates no sandbox")
		routes := []substrate.GatewayRoute{substrate.ModelRoute("koinon-gateway.koinon.svc", 8080)}
		cfg, err = cli.Substrate{GatewayRoutes: routes}.DriverConfig(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.GatewayRoutes).To(Equal(routes))
		_, err = cli.Substrate{GatewayRoutes: []substrate.GatewayRoute{substrate.ModelRoute("10.0.0.1", 8080)}}.DriverConfig(nil)
		Expect(err).To(MatchError(ContainSubstring("IP address")))
	})
	It("reads the guest API signing key, or refuses a malformed one", func() {
		cfg, err := cli.Substrate{GuestKeyFile: guestKeyFile()}.DriverConfig(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.GuestSigner).NotTo(BeNil())
		Expect(cfg.GuestSigner.PublicKey()).To(Equal(fakeate.Signer().PublicKey()))
		bad := GinkgoT().TempDir() + "/key"
		Expect(os.WriteFile(bad, []byte("koinon_test_not-a-real-key\n"), 0o600)).To(Succeed())
		_, err = cli.Substrate{GuestKeyFile: bad}.DriverConfig(nil)
		Expect(err).To(MatchError(ContainSubstring("--substrate-guest-key-file")))
		_, err = cli.Substrate{GuestKeyFile: GinkgoT().TempDir() + "/missing"}.DriverConfig(nil)
		Expect(err).To(MatchError(ContainSubstring("--substrate-guest-key-file")))
	})
	It("gives a substrate Exec the tier of the sandbox's runtime class", func(ctx SpecContext) {
		cfg, err := cli.Substrate{
			GuestKeyFile:      guestKeyFile(),
			RuntimeClassTiers: []string{"kata=vm", "gvisor=hardened"}, DefaultTier: "dev",
			GatewayRoutes: []substrate.GatewayRoute{substrate.ModelRoute(fakeate.GatewayHost, fakeate.GatewayPort)},
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
			if class == "" {
				name = "sb-default" // the fake control plane needs a DNS-1123 label
			}
			_, err = d.CreateSandbox(ctx, "ws", execution.SandboxSpec{
				Name: name, Image: "img@sha256:" + strings.Repeat("0", 64), RuntimeClassName: class,
				CPULimit: "500m", MemoryLimit: "512Mi",
			})
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
				"/usr/local/bin/koinon-guest-launcher", "--policy", "/etc/koinon/guest-launcher/" + policy, "--",
			}))
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

// guestKeyFile writes the fake guest API key of fakeate.Signer (an obviously fake seed) to a file.
func guestKeyFile() string {
	p := GinkgoT().TempDir() + "/guest-key"
	Expect(os.WriteFile(p, []byte(strings.Repeat("11", 32)+"\n"), 0o600)).To(Succeed())
	return p
}
