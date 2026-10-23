// SPDX-License-Identifier: Apache-2.0

package openshell_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
	openshellv1 "github.com/stateracloud/koinon/internal/gen/openshell/v1"
)

var _ = Describe("Exec", func() {
	var (
		b  *openshell.Backend
		gw *fakegateway.Gateway
	)

	BeforeEach(func(ctx SpecContext) {
		b, gw = newBackend()
		gw.Handle("cat", fakegateway.Cat)
		gw.Handle("fail", func(_ context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
			_, _ = io.WriteString(stdout, "partial\n")
			_, _ = io.WriteString(stderr, "error: "+strings.Join(args, " ")+"\n")
			return 3
		})
		gw.Handle("sleep", func(ctx context.Context, _ []string, _ io.Reader, _, _ io.Writer) int {
			<-ctx.Done()
			return 143
		})
		gw.Handle("flood", func(ctx context.Context, _ []string, _ io.Reader, stdout, _ io.Writer) int {
			line := bytes.Repeat([]byte("x"), 1024)
			for ctx.Err() == nil {
				if _, err := stdout.Write(line); err != nil {
					return 1
				}
			}
			return 143
		})
		_, err := b.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: "sb", Image: "img"})
		Expect(err).NotTo(HaveOccurred())
	})

	It("sends a non-TTY start request without a login shell", func(ctx SpecContext) {
		p, err := b.Exec(ctx, ws, "sb", execution.ExecRequest{
			Command: []string{"cat"}, Workdir: "/sandbox/work", Timeout: 30 * time.Minute,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Stdin().Close()).To(Succeed())
		Expect(p.Wait()).To(Equal(0))

		execs := gw.Execs()
		Expect(execs).To(HaveLen(1))
		Expect(execs[0].GetTty()).To(BeFalse())
		Expect(execs[0].GetNoLoginShell()).To(BeTrue())
		Expect(execs[0].GetWorkspaceScope().GetWorkspace()).To(Equal(ws))
		Expect(execs[0].GetSandbox()).To(Equal("sb"))
		Expect(execs[0].GetWorkdir()).To(Equal("/sandbox/work"))
		Expect(execs[0].GetExecutionTimeout().AsDuration()).To(Equal(30 * time.Minute))
		Expect(execs[0].GetEnvironment()).To(BeEmpty())
	})

	It("streams newline-delimited JSON-RPC both ways, one message at a time", func(ctx SpecContext) {
		p, err := b.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"cat"}})
		Expect(err).NotTo(HaveOccurred())
		out := bufio.NewReader(p.Stdout())

		// Each reply is read before the next request is written, so the stream cannot be
		// buffering stdin until end-of-file.
		for i := range 5 {
			msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/prompt","params":{"text":"héllo\tworld \\n"}}`+"\n", i)
			_, err := io.WriteString(p.Stdin(), msg)
			Expect(err).NotTo(HaveOccurred())
			line, err := out.ReadString('\n')
			Expect(err).NotTo(HaveOccurred())
			Expect(line).To(Equal(msg))
		}
		Expect(p.Stdin().Close()).To(Succeed())
		rest, err := io.ReadAll(out)
		Expect(err).NotTo(HaveOccurred())
		Expect(rest).To(BeEmpty())
		Expect(p.Wait()).To(Equal(0))
	})

	It("passes arbitrary bytes unchanged, across chunk boundaries", func(ctx SpecContext) {
		payload := make([]byte, 1<<20+7)
		_, _ = rand.Read(payload)
		copy(payload, "line one\r\nline two\n\x00")

		p, err := b.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"cat"}})
		Expect(err).NotTo(HaveOccurred())
		go func() {
			defer GinkgoRecover()
			_, err := p.Stdin().Write(payload)
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Stdin().Close()).To(Succeed())
		}()
		got, err := io.ReadAll(p.Stdout())
		Expect(err).NotTo(HaveOccurred())
		Expect(bytes.Equal(got, payload)).To(BeTrue(), "stdout differs from stdin (%d vs %d bytes)", len(got), len(payload))
		Expect(p.Wait()).To(Equal(0))
	})

	It("returns the exit code and stderr separately from stdout", func(ctx SpecContext) {
		var stderr bytes.Buffer
		p, err := b.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"fail", "a", "b"}, Stderr: &stderr})
		Expect(err).NotTo(HaveOccurred())
		out, err := io.ReadAll(p.Stdout())
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(Equal("partial\n"))
		Expect(p.Wait()).To(Equal(3))
		Expect(stderr.String()).To(Equal("error: a b\n"))
	})

	It("reports a missing or stopped sandbox from Exec itself", func(ctx SpecContext) {
		_, err := b.Exec(ctx, ws, "nope", execution.ExecRequest{Command: []string{"cat"}})
		Expect(err).To(MatchError(execution.ErrNotFound))

		gw.SetPhase(ws, "sb", openshellv1.SandboxPhase_SANDBOX_PHASE_STOPPED)
		_, err = b.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"cat"}})
		Expect(err).To(MatchError(execution.ErrFailedPrecondition))

		_, err = b.Exec(ctx, ws, "sb", execution.ExecRequest{})
		Expect(err).To(MatchError(execution.ErrInvalid))
	})

	It("maps a gateway rejection of the stream", func(ctx SpecContext) {
		gw.FailNext("ExecSandboxInteractive", status.Error(codes.Unavailable, "supervisor relay failed"))
		_, err := b.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"cat"}})
		Expect(err).To(MatchError(execution.ErrUnavailable))
		Expect(err.Error()).To(ContainSubstring("supervisor relay failed"))
	})

	It("closes the streams and ends the command when the context is cancelled", func(ctx SpecContext) {
		cctx, cancel := context.WithCancel(ctx)
		p, err := b.Exec(cctx, ws, "sb", execution.ExecRequest{Command: []string{"sleep"}})
		Expect(err).NotTo(HaveOccurred())

		read := make(chan error, 1)
		go func() {
			_, err := io.ReadAll(p.Stdout())
			read <- err
		}()
		Consistently(read, 100*time.Millisecond).ShouldNot(Receive())
		cancel()

		Eventually(read).WithTimeout(5 * time.Second).Should(Receive(MatchError(context.Canceled)))
		_, err = p.Wait()
		Expect(err).To(MatchError(context.Canceled))
		_, err = p.Stdin().Write([]byte("late\n"))
		Expect(err).To(HaveOccurred())
	}, SpecTimeout(10*time.Second))

	It("ends even when nobody reads stdout", func(ctx SpecContext) {
		cctx, cancel := context.WithCancel(ctx)
		p, err := b.Exec(cctx, ws, "sb", execution.ExecRequest{Command: []string{"flood"}})
		Expect(err).NotTo(HaveOccurred())
		time.Sleep(50 * time.Millisecond)
		cancel()

		done := make(chan error, 1)
		go func() { _, err := p.Wait(); done <- err }()
		Eventually(done).WithTimeout(5 * time.Second).Should(Receive(MatchError(context.Canceled)))
	}, SpecTimeout(10*time.Second))

	It("ends the command on Close", func(ctx SpecContext) {
		p, err := b.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"sleep"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Close()).To(Succeed())
		Expect(p.Close()).To(Succeed())
		_, err = p.Wait()
		Expect(err).To(MatchError(context.Canceled))
	}, SpecTimeout(10*time.Second))

	It("reports a gateway that goes away mid-stream", func(ctx SpecContext) {
		p, err := b.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"sleep"}})
		Expect(err).NotTo(HaveOccurred())
		gw.Stop()
		_, err = p.Wait()
		Expect(err).To(MatchError(execution.ErrUnavailable))
		_, err = io.ReadAll(p.Stdout())
		Expect(err).To(MatchError(execution.ErrUnavailable))
	}, SpecTimeout(10*time.Second))
})
