// SPDX-License-Identifier: Apache-2.0

package openshell_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// The integration test runs against a real gateway only when KOINON_OPENSHELL_ENDPOINT is
// set (host:port, for example a port-forward), with:
//
//	KOINON_OPENSHELL_MTLS_DIR   directory with tls.crt, tls.key and ca.crt (required)
//	KOINON_OPENSHELL_IMAGE      sandbox image with sh and cat (required)
//	KOINON_OPENSHELL_WORKSPACE  workspace to use (default koinon-it)
//
// It creates one sandbox and deletes it again.
var _ = Describe("OpenShell gateway", Label("integration"), Ordered, func() {
	var (
		b         *openshell.Backend
		workspace string
		name      = fmt.Sprintf("kit-%d", time.Now().Unix()) // OpenShell 0.1.5: names are at most 19 characters
	)

	BeforeAll(func(ctx SpecContext) {
		testenv.Require(testenv.Var("openshell", "KOINON_OPENSHELL_ENDPOINT"))
		endpoint := os.Getenv("KOINON_OPENSHELL_ENDPOINT")
		dir := os.Getenv("KOINON_OPENSHELL_MTLS_DIR")
		image := os.Getenv("KOINON_OPENSHELL_IMAGE")
		Expect(dir).NotTo(BeEmpty(), "KOINON_OPENSHELL_MTLS_DIR")
		Expect(image).NotTo(BeEmpty(), "KOINON_OPENSHELL_IMAGE")
		workspace = os.Getenv("KOINON_OPENSHELL_WORKSPACE")
		if workspace == "" {
			workspace = "koinon-it"
		}

		var err error
		b, err = openshell.Dial(openshell.Config{
			Endpoint: endpoint,
			CertFile: filepath.Join(dir, "tls.crt"),
			KeyFile:  filepath.Join(dir, "tls.key"),
			CAFile:   filepath.Join(dir, "ca.crt"),
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(b.Close)

		_, err = b.EnsureWorkspace(ctx, workspace, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = b.CreateSandbox(ctx, workspace, execution.SandboxSpec{
			Name: name, Image: image, Command: []string{"sleep", "infinity"},
			Policy: execution.Policy{Filesystem: execution.FilesystemPolicy{
				IncludeWorkdir: true,
				ReadOnly:       []string{"/bin", "/usr", "/lib", "/etc"},
				ReadWrite:      []string{"/tmp", "/dev/null"},
			}},
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) error {
			_, err := b.DeleteSandbox(ctx, workspace, name)
			return err
		})

		Eventually(func(g Gomega) {
			sb, err := b.GetSandbox(ctx, workspace, name)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(sb.Phase).To(Equal(execution.PhaseReady))
		}).WithContext(ctx).WithTimeout(5 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
	}, NodeTimeout(6*time.Minute))

	It("streams stdin to stdout without a TTY", func(ctx SpecContext) {
		p, err := b.Exec(ctx, workspace, name, execution.ExecRequest{Command: []string{"cat"}})
		Expect(err).NotTo(HaveOccurred())
		out := bufio.NewReader(p.Stdout())
		for i := range 3 {
			msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"ping"}`+"\n", i)
			_, err := io.WriteString(p.Stdin(), msg)
			Expect(err).NotTo(HaveOccurred())
			Expect(out.ReadString('\n')).To(Equal(msg))
		}
		Expect(p.Stdin().Close()).To(Succeed())
		Expect(p.Wait()).To(Equal(0))
	}, NodeTimeout(time.Minute))

	It("returns the exit code", func(ctx SpecContext) {
		p, err := b.Exec(ctx, workspace, name, execution.ExecRequest{Command: []string{"sh", "-c", "exit 7"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Wait()).To(Equal(7))
	}, NodeTimeout(time.Minute))

	It("stops and starts the sandbox", func(ctx SpecContext) {
		_, err := b.StopSandbox(ctx, workspace, name)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			sb, err := b.GetSandbox(ctx, workspace, name)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(sb.Phase).To(Equal(execution.PhaseStopped))
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
		_, err = b.StartSandbox(ctx, workspace, name)
		Expect(err).NotTo(HaveOccurred())
	}, NodeTimeout(3*time.Minute))
})
