// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// answered fails a spec whose request got no HTTP answer from the sandbox proxy: curl's code 000
// means DNS or the connection failed before the proxy decided anything, so the spec observed no
// policy decision (old issue 244). It is a test error, never an expected code. Specs that expect no
// answer (direct egress, which the sandbox refuses at connect) do not call it.
func answered(code, body string) {
	GinkgoHelper()
	Expect(code).NotTo(Equal("000"), "no HTTP answer from the sandbox proxy (DNS or connect failure, not a policy decision): "+body)
}

// gateway is a real OpenShell gateway from the KOINON_OPENSHELL_* variables.
type gateway struct {
	b         *openshell.Backend
	workspace string
	image     string
}

// dialGateway connects to the gateway and ensures the workspace, or skips the calling
// container without KOINON_OPENSHELL_ENDPOINT. Call it from a BeforeAll.
func dialGateway(ctx context.Context) *gateway {
	GinkgoHelper()
	testenv.Require(testenv.Var("openshell", "KOINON_OPENSHELL_ENDPOINT"))
	endpoint := os.Getenv("KOINON_OPENSHELL_ENDPOINT")
	dir := os.Getenv("KOINON_OPENSHELL_MTLS_DIR")
	image := os.Getenv("KOINON_OPENSHELL_IMAGE")
	Expect(dir).NotTo(BeEmpty(), "KOINON_OPENSHELL_MTLS_DIR")
	Expect(image).NotTo(BeEmpty(), "KOINON_OPENSHELL_IMAGE")
	g := &gateway{workspace: envOr("KOINON_OPENSHELL_WORKSPACE", "koinon-escape"), image: image}

	var err error
	g.b, err = openshell.Dial(openshell.Config{
		Endpoint: endpoint,
		CertFile: filepath.Join(dir, "tls.crt"),
		KeyFile:  filepath.Join(dir, "tls.key"),
		CAFile:   filepath.Join(dir, "ca.crt"),
	})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(g.b.Close)
	_, err = g.b.EnsureWorkspace(ctx, g.workspace, nil)
	Expect(err).NotTo(HaveOccurred())
	return g
}

// suffix makes the names of one run unique; OpenShell 0.1.5 allows at most 19 characters in a
// sandbox name.
func suffix() int64 { return time.Now().UnixNano() / int64(time.Millisecond) % 100_000_000 }

// provider imports p with its profile and deletes both when the container ends.
func (g *gateway) provider(ctx context.Context, p execution.Provider) {
	GinkgoHelper()
	Expect(g.b.EnsureProvider(ctx, g.workspace, p)).To(Succeed())
	DeferCleanup(func(ctx context.Context) error {
		if err := g.b.DeleteProvider(ctx, g.workspace, p.Name); err != nil {
			return err
		}
		return g.b.DeleteProviderProfile(ctx, g.workspace, p.Profile.ID)
	})
}

// sandbox creates spec, deletes it when the container ends, and waits until it is ready.
func (g *gateway) sandbox(ctx context.Context, spec execution.SandboxSpec) {
	GinkgoHelper()
	if spec.Image == "" {
		spec.Image = g.image
	}
	if spec.Command == nil {
		spec.Command = []string{"sleep", "infinity"}
	}
	_, err := g.b.CreateSandbox(ctx, g.workspace, spec)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func(ctx context.Context) error { return g.remove(ctx, spec.Name) })
	Eventually(func(gm Gomega) {
		sb, err := g.b.GetSandbox(ctx, g.workspace, spec.Name)
		gm.Expect(err).NotTo(HaveOccurred())
		gm.Expect(sb.Phase).To(Equal(execution.PhaseReady))
	}).WithContext(ctx).WithTimeout(5 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
}

// remove deletes a sandbox so that it leaves no draft policy chunk behind (old issue 640). The
// gateway refuses to delete a workspace that holds a draft chunk. A running sandbox submits
// chunks, and a submit that races the delete leaves an orphan chunk that no call can reach, so:
// stop it and wait for Stopped (its supervisor is gone, nothing submits any more; this is what
// prevents the orphan), then clear the pending chunks and wait until none is pending (extra
// safety: the gateway deletes all chunks of a sandbox when it deletes the sandbox).
//
// Stop, wait and clear are best effort: a sandbox stuck in Provisioning cannot be stopped, and
// that must not keep it from being deleted. DeleteSandbox is always called, with plain polling
// that returns errors (no Gomega assertion, which would panic out of the cleanup), and every
// error is reported.
func (g *gateway) remove(ctx context.Context, name string) error {
	var stopErr, clearErr error
	if _, stopErr = g.b.StopSandbox(ctx, g.workspace, name); stopErr == nil {
		stopErr = pollUntil(ctx, 2*time.Minute, func() error {
			sb, err := g.b.GetSandbox(ctx, g.workspace, name)
			if err != nil {
				return err
			}
			if sb.Phase != execution.PhaseStopped {
				return fmt.Errorf("sandbox %s is %s, not Stopped", name, sb.Phase)
			}
			return nil
		})
	}
	clearErr = pollUntil(ctx, time.Minute, func() error {
		if _, err := g.b.ClearDraftChunks(ctx, g.workspace, name); err != nil {
			return err
		}
		n, err := g.b.PendingDraftChunks(ctx, g.workspace, name)
		if err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("%d pending draft policy chunks of sandbox %s", n, name)
		}
		return nil
	})
	_, deleteErr := g.b.DeleteSandbox(ctx, g.workspace, name)
	return errors.Join(stopErr, clearErr, deleteErr)
}

// pollUntil calls check every two seconds until it returns nil, the timeout passes or ctx ends,
// and returns the last error of check. It never panics.
func pollUntil(ctx context.Context, timeout time.Duration, check func() error) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		err := check()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-deadline.C:
			return err
		case <-tick.C:
		}
	}
}

// exec runs command in the sandbox with stdin as its standard input and returns stdout,
// stderr and the exit code.
func (g *gateway) exec(ctx context.Context, sandbox, stdin string, command ...string) (string, string, int) {
	GinkgoHelper()
	var stderr bytes.Buffer
	p, err := g.b.Exec(ctx, g.workspace, sandbox, execution.ExecRequest{
		Command: command, Timeout: 2 * time.Minute, Stderr: &stderr,
	})
	Expect(err).NotTo(HaveOccurred())
	go func() {
		defer GinkgoRecover()
		_, _ = io.Copy(p.Stdin(), strings.NewReader(stdin))
		_ = p.Stdin().Close()
	}()
	out, err := io.ReadAll(p.Stdout())
	Expect(err).NotTo(HaveOccurred())
	code, err := p.Wait()
	Expect(err).NotTo(HaveOccurred())
	return string(out), stderr.String(), code
}

// sh runs script with sh in the sandbox.
func (g *gateway) sh(ctx context.Context, sandbox, script string) (string, string, int) {
	GinkgoHelper()
	return g.exec(ctx, sandbox, "", "sh", "-c", script)
}

// curl makes one request with curl from the sandbox and returns the status code ("000" when
// no response arrived), the body and curl's exit code. The placeholder goes in the
// Authorization header, as a client of the grant would send it.
func (g *gateway) curl(ctx context.Context, sandbox, method, url string) (string, string, int) {
	GinkgoHelper()
	out, stderr, exit := g.sh(ctx, sandbox, fmt.Sprintf(
		`curl -sS -m 20 -X %s -H "Authorization: Bearer $GITHUB_TOKEN" -o /tmp/body -w '%%{http_code}' '%s'; rc=$?; echo; cat /tmp/body 2>/dev/null; rm -f /tmp/body; exit $rc`, method, url))
	code, body, _ := strings.Cut(out, "\n")
	GinkgoWriter.Printf("%s %s -> %s (curl %d) %s %s\n", method, url, code, exit, body, stderr)
	return code, body, exit
}

// standinMarker is the header line koinon-fakemodel -standin adds to every answer.
const standinMarker = "X-Koinon-Standin: 1"

// curlGateway is curl with the gateway key's placeholder (reconciler.GatewayKeyEnvVar) in the
// Authorization header, as the agent's harness sends it.
func (g *gateway) curlGateway(ctx context.Context, sandbox, method, url string) (string, string) {
	GinkgoHelper()
	code, body, _ := g.curlGatewayHeaders(ctx, sandbox, method, url)
	return code, body
}

// curlGatewayHeaders is curlGateway that also returns the response headers, one per line.
func (g *gateway) curlGatewayHeaders(ctx context.Context, sandbox, method, url string) (code, body, header string) {
	GinkgoHelper()
	out, stderr, exit := g.sh(ctx, sandbox, fmt.Sprintf(
		`curl -sS -m 20 -X %s -H "Authorization: Bearer $%s" -D /tmp/header -o /tmp/body -w '%%{http_code}' '%s'; rc=$?; echo; cat /tmp/body 2>/dev/null; echo; echo %s; cat /tmp/header 2>/dev/null; rm -f /tmp/body /tmp/header; exit $rc`,
		method, reconciler.GatewayKeyEnvVar, url, headerMark))
	code, rest, _ := strings.Cut(out, "\n")
	body, header, _ = strings.Cut(rest, "\n"+headerMark+"\n")
	GinkgoWriter.Printf("%s %s -> %s (curl %d) %s %s\n", method, url, code, exit, body, stderr)
	return code, body, strings.ReplaceAll(header, "\r", "")
}

// headerMark separates the body from the headers in curlGatewayHeaders' output.
const headerMark = "--koinon-headers--"

// deleteWorkspace removes the run's workspace record through the gateway, for
// KOINON_ESCAPE_DELETE_WORKSPACE=1 (the kind e2e, test/e2e/escape-openshell.sh): deleting only the
// namespace leaves the record, and a rerun then finds a workspace without a namespace. Run it after
// the last spec, which needs the namespace.
//
// The gateway refuses to delete a workspace that still holds a draft policy chunk (old issue 640);
// gateway.remove clears them per sandbox while it still exists. Here wait until the workspace lists
// no sandbox (deletion is asynchronous), then delete it.
func deleteWorkspace(ctx context.Context) {
	GinkgoHelper()
	if os.Getenv("KOINON_ESCAPE_DELETE_WORKSPACE") != "1" || os.Getenv("KOINON_OPENSHELL_ENDPOINT") == "" {
		return
	}
	dir := os.Getenv("KOINON_OPENSHELL_MTLS_DIR")
	b, err := openshell.Dial(openshell.Config{
		Endpoint: os.Getenv("KOINON_OPENSHELL_ENDPOINT"),
		CertFile: filepath.Join(dir, "tls.crt"),
		KeyFile:  filepath.Join(dir, "tls.key"),
		CAFile:   filepath.Join(dir, "ca.crt"),
	})
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = b.Close() }()
	workspace := envOr("KOINON_OPENSHELL_WORKSPACE", "koinon-escape")
	Eventually(func(g Gomega) {
		names, err := b.ListSandboxNames(ctx, workspace)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(names).To(BeEmpty(), "sandboxes still in the workspace")
	}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
	Expect(b.DeleteWorkspace(ctx, workspace)).To(Succeed())
}
