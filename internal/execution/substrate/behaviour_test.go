// SPDX-License-Identifier: Apache-2.0

package substrate_test

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/substrate"
	"github.com/stateracloud/koinon/internal/execution/substrate/fakeate"
	ateapiv1 "github.com/stateracloud/koinon/internal/gen/substrate/ateapi/v1"
)

// The behaviour the contract suite cannot know: what Substrate does that the driver must handle.
var _ = Describe("the Substrate driver", func() {
	var (
		d  *substrate.Driver
		cl *fakeate.Cluster
	)
	BeforeEach(func() {
		dd, c := newDriver()
		d, cl = dd, c
	})

	Describe("exec across a suspend", func() {
		It("loses and doubles no line when the sandbox is suspended and resumed mid-stream", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			p, err := d.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"cat"}})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(p.Close)

			const lines = 60
			var (
				mu  sync.Mutex
				got strings.Builder
			)
			readDone := make(chan struct{})
			go func() {
				defer close(readDone)
				b, _ := io.ReadAll(p.Stdout())
				mu.Lock()
				got.Write(b)
				mu.Unlock()
			}()
			for i := range lines {
				_, err := fmt.Fprintf(p.Stdin(), "line %03d\n", i)
				Expect(err).NotTo(HaveOccurred())
				if i%20 == 10 { // three suspend and resume cycles, with the stream open
					// A request already on the wire before the pause may wake the actor late; that is allowed.
					sb, err := d.SuspendSandbox(ctx, ws, "sb")
					Expect(err).NotTo(HaveOccurred())
					Expect(sb.Phase).To(Equal(execution.PhaseSuspended))
					_, err = d.ResumeSandbox(ctx, ws, "sb")
					Expect(err).NotTo(HaveOccurred())
				}
			}
			Expect(p.Stdin().Close()).To(Succeed())
			Eventually(readDone).WithTimeout(10 * time.Second).Should(BeClosed())
			code, err := p.Wait()
			Expect(err).NotTo(HaveOccurred())
			Expect(code).To(BeZero())

			var want strings.Builder
			for i := range lines {
				fmt.Fprintf(&want, "line %03d\n", i)
			}
			mu.Lock()
			Expect(got.String()).To(Equal(want.String()))
			mu.Unlock()
		}, SpecTimeout(30*time.Second))

		It("reads on from its offset after a dropped connection", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			p, err := d.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"cat"}})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(p.Close)
			body := strings.Repeat("0123456789abcdef", 64<<10/16*8) // 512 KiB, many output messages
			c := cl
			stop := make(chan struct{})
			go func() { // drop the guest streams over and over while the output flows
				for {
					select {
					case <-stop:
						return
					case <-time.After(time.Millisecond):
						c.DropStreams(ws, "sb")
					}
				}
			}()
			defer close(stop)
			go func() {
				_, _ = io.WriteString(p.Stdin(), body)
				_ = p.Stdin().Close()
			}()
			got, err := io.ReadAll(p.Stdout())
			Expect(err).NotTo(HaveOccurred())
			Expect(len(got)).To(Equal(len(body)))
			Expect(bytes.Equal(got, []byte(body))).To(BeTrue(), "the bytes are the same, in order")
		}, SpecTimeout(30*time.Second))

		It("keeps an idle stream alive across a long suspend and one failed reopen", func(ctx SpecContext) {
			dd, c := newDriver(substrate.Config{
				PollInterval: 5 * time.Millisecond, GuestReadyTimeout: 2 * time.Second, StreamStall: 100 * time.Millisecond,
			})
			ready(ctx, dd, "sb")
			p, err := dd.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"cat"}})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(p.Close)

			time.Sleep(50 * time.Millisecond) // idle: the stream is open and delivers nothing
			_, err = dd.SuspendSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			time.Sleep(300 * time.Millisecond) // longer than StreamStall
			c.FailNextOutputStreams(ws, "sb", 1)
			_, err = dd.ResumeSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())

			_, err = io.WriteString(p.Stdin(), "after\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Stdin().Close()).To(Succeed())
			got, err := io.ReadAll(p.Stdout())
			Expect(err).NotTo(HaveOccurred())
			Expect(string(got)).To(Equal("after\n"))
			code, err := p.Wait()
			Expect(err).NotTo(HaveOccurred())
			Expect(code).To(BeZero())
		}, SpecTimeout(15*time.Second))

		It("ends Wait when ctx is cancelled and nobody reads stdout", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			pctx, cancel := context.WithCancel(ctx)
			defer cancel()
			p, err := d.Exec(pctx, ws, "sb", execution.ExecRequest{Command: []string{"cat"}})
			Expect(err).NotTo(HaveOccurred())
			_, err = io.WriteString(p.Stdin(), "nobody reads this\n")
			Expect(err).NotTo(HaveOccurred())
			time.Sleep(100 * time.Millisecond) // the pump now waits in its stdout write
			cancel()
			waited := make(chan error, 1)
			go func() { _, err := p.Wait(); waited <- err }()
			var werr error
			Eventually(waited).WithTimeout(5 * time.Second).Should(Receive(&werr))
			Expect(errors.Is(werr, context.Canceled)).To(BeTrue(), "got %v", werr)
			Expect(p.Stdin().Close()).To(Succeed(), "the command ended, so closing stdin is no error")
		}, SpecTimeout(15*time.Second))

		It("closes stdin without error while the command exits", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			for range 50 {
				p, err := d.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"mkdir", "x"}})
				Expect(err).NotTo(HaveOccurred())
				Expect(p.Stdin().Close()).To(Succeed())
				_, err = p.Wait()
				Expect(err).NotTo(HaveOccurred())
			}
		}, SpecTimeout(30*time.Second))

		It("does not resume a suspended sandbox to run a command", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			_, err := d.StopSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			_, err = d.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"cat"}})
			Expect(errors.Is(err, execution.ErrFailedPrecondition)).To(BeTrue(), "got %v", err)
			err = d.Upload(ctx, ws, "sb", "/sandbox", bytes.NewReader(nil))
			Expect(errors.Is(err, execution.ErrFailedPrecondition)).To(BeTrue(), "got %v", err)
			Expect(cl.RouterResumes(ws, "sb")).To(BeZero())
		})

		It("reports suspended only when the sandbox is, while a suspend overlaps an upload or a stdin write", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			// An upload whose reader stalls after the first chunk: the stream is open during the suspend,
			// so the drain waits DrainTimeout for it and then cancels it.
			release := make(chan struct{})
			started := make(chan struct{})
			upErr := make(chan error, 1)
			go func() {
				upErr <- d.Upload(ctx, ws, "sb", "/sandbox", &stallReader{first: bytes.Repeat([]byte("x"), 1024), started: started, release: release})
			}()
			Eventually(started).WithTimeout(5 * time.Second).Should(BeClosed())
			sb, err := d.SuspendSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			Expect(sb.Phase).To(Equal(execution.PhaseSuspended))
			close(release)
			Eventually(upErr).WithTimeout(5 * time.Second).Should(Receive(HaveOccurred()))
			_, err = d.ResumeSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			_, err = d.WaitPhase(ctx, ws, "sb", execution.PhaseReady)
			Expect(err).NotTo(HaveOccurred())

			// A writer that never stops, over repeated suspends and resumes. A request that left
			// before a pause may still land after SuspendActor (timing); what the driver guarantees is
			// that it reports suspended only after it read the actor as suspended.
			p, err := d.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"cat"}})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(p.Close)
			go func() { _, _ = io.Copy(io.Discard, p.Stdout()) }()
			stop := make(chan struct{})
			wrote := make(chan struct{})
			go func() {
				defer close(wrote)
				for {
					select {
					case <-stop:
						return
					default:
						_, _ = p.Stdin().Write([]byte("line\n"))
					}
				}
			}()
			for range 5 {
				sb, err := d.SuspendSandbox(ctx, ws, "sb")
				Expect(err).NotTo(HaveOccurred())
				Expect(sb.Phase).To(Equal(execution.PhaseSuspended))
				_, err = d.ResumeSandbox(ctx, ws, "sb")
				Expect(err).NotTo(HaveOccurred())
				_, err = d.WaitPhase(ctx, ws, "sb", execution.PhaseReady)
				Expect(err).NotTo(HaveOccurred())
			}
			close(stop)
			Eventually(wrote).WithTimeout(10 * time.Second).Should(BeClosed())
		}, SpecTimeout(30*time.Second))

		It("starts no router call once a suspend has closed the gate", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			p, err := d.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"cat"}})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(p.Close)
			go func() { _, _ = io.Copy(io.Discard, p.Stdout()) }()
			stop := make(chan struct{})
			wrote := make(chan struct{})
			go func() {
				defer close(wrote)
				for {
					select {
					case <-stop:
						return
					default:
						_, _ = p.Stdin().Write([]byte("line\n"))
					}
				}
			}()
			DeferCleanup(func() { close(stop); Eventually(wrote).WithTimeout(10 * time.Second).Should(BeClosed()) })
			Eventually(func() int { return cl.Calls("WriteProcessInput") }).WithTimeout(5 * time.Second).Should(BeNumerically(">", 3))

			unpause := d.PauseGate(ws, "sb")
			admitted := d.RouterCallsStarted(ws, "sb") // stable from here: no begin succeeds on a paused gate
			_, err = d.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"cat"}})
			Expect(errors.Is(err, execution.ErrFailedPrecondition)).To(BeTrue(), "got %v", err)
			err = d.Upload(ctx, ws, "sb", "/sandbox", bytes.NewReader(nil))
			Expect(errors.Is(err, execution.ErrFailedPrecondition)).To(BeTrue(), "got %v", err)
			Consistently(func() uint64 { return d.RouterCallsStarted(ws, "sb") }).
				WithTimeout(200*time.Millisecond).Should(Equal(admitted), "the writer and the output stream wait")
			unpause()

			before := cl.Calls("WriteProcessInput")
			Eventually(func() int { return cl.Calls("WriteProcessInput") }).WithTimeout(5*time.Second).
				Should(BeNumerically(">", before+1), "the writer goes on once the gate opens")
		}, SpecTimeout(30*time.Second))

		It("suspends again when a request sent before the pause lands after SuspendActor", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			// The output stream's first request stops on the wire; the pause cancels the client side,
			// and the request lands only once SuspendActor has suspended the actor, and resumes it.
			held := cl.DelayNextRouterRequest(ws, "sb", "StreamProcessOutput")
			p, err := d.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"cat"}})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(p.Close)
			Eventually(held).WithTimeout(5 * time.Second).Should(BeClosed())

			sb, err := d.SuspendSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			Expect(sb.Phase).To(Equal(execution.PhaseSuspended))
			Expect(cl.State(ws, "sb")).To(Equal(ateapiv1.ActorState_ACTOR_STATE_SUSPENDED))
			Expect(cl.RouterResumes(ws, "sb")).To(Equal(1), "the late request landed and resumed the actor")
			Expect(cl.Calls("SuspendActor")).To(Equal(2))
			Expect(d.Resuspends("retried")).To(Equal(1.0))
			Expect(d.Resuspends("failed")).To(BeZero())

			// The process outlives it all: resume and send one line through cat.
			_, err = d.ResumeSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			_, err = d.WaitPhase(ctx, ws, "sb", execution.PhaseReady)
			Expect(err).NotTo(HaveOccurred())
			_, err = io.WriteString(p.Stdin(), "after\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(bufio.NewReader(p.Stdout()).ReadString('\n')).To(Equal("after\n"))
		}, SpecTimeout(30*time.Second))

		It("suspends again a bounded number of times, then fails rather than report suspended", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			cl.ResumeAfterSuspends(ws, "sb", 2)
			sb, err := d.SuspendSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			Expect(sb.Phase).To(Equal(execution.PhaseSuspended))
			Expect(cl.Calls("SuspendActor")).To(Equal(3))
			_, err = d.ResumeSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			_, err = d.WaitPhase(ctx, ws, "sb", execution.PhaseReady)
			Expect(err).NotTo(HaveOccurred())

			cl.ResumeAfterSuspends(ws, "sb", 1000) // a router request that never stops
			_, err = d.SuspendSandbox(ctx, ws, "sb")
			Expect(errors.Is(err, execution.ErrUnavailable)).To(BeTrue(), "got %v", err)
			Expect(cl.Calls("SuspendActor")).To(Equal(3+4), "one suspend and three more")
			Expect(cl.State(ws, "sb")).To(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			Expect(d.Resuspends("retried")).To(Equal(2.0 + 3.0))
			Expect(d.Resuspends("failed")).To(Equal(1.0))
		})

		It("fails with ErrUnavailable when the state read after SuspendActor outlasts DrainTimeout", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			// The read in settled is held past its bound: the error is the one of a state that did
			// not settle, whether the bound ends during a read or between two.
			_, release := cl.HoldNextGetActor(ws, "sb")
			DeferCleanup(release)
			_, err := d.SuspendSandbox(ctx, ws, "sb")
			Expect(errors.Is(err, execution.ErrUnavailable)).To(BeTrue(), "got %v", err)
			Expect(err.Error()).To(ContainSubstring("did not settle"))
		}, SpecTimeout(10*time.Second))

		It("sends no stdin write on a RUNNING state that a whole suspend made stale", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			p, err := d.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"cat"}})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(p.Close)
			// One line through cat: the output stream is open, so the next GetActor is the write's.
			_, err = io.WriteString(p.Stdin(), "first\n")
			Expect(err).NotTo(HaveOccurred())
			out := bufio.NewReader(p.Stdout())
			Expect(out.ReadString('\n')).To(Equal("first\n"))
			go func() { _, _ = io.Copy(io.Discard, out) }()

			// The write reads RUNNING, and the reply arrives only after a whole suspend: pause,
			// SuspendActor and unpause. The open gate must not let the write through on that read.
			held, release := cl.HoldNextGetActor(ws, "sb")
			DeferCleanup(release)
			wrote := make(chan error, 1)
			go func() { _, err := io.WriteString(p.Stdin(), "x\n"); wrote <- err }()
			Eventually(held).WithTimeout(5 * time.Second).Should(BeClosed())
			_, err = d.SuspendSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			release()
			Consistently(wrote).WithTimeout(100*time.Millisecond).ShouldNot(Receive(), "the write waits for the resume")
			Expect(cl.RouterResumes(ws, "sb")).To(BeZero())

			_, err = d.ResumeSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			Eventually(wrote).WithTimeout(5 * time.Second).Should(Receive(BeNil()))
			Expect(cl.RouterResumes(ws, "sb")).To(BeZero())
		}, SpecTimeout(30*time.Second))

		It("reports the exit code and stderr of a command", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			var stderr bytes.Buffer
			p, err := d.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"nope"}, Stderr: &stderr})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(p.Close)
			_, _ = io.Copy(io.Discard, p.Stdout())
			code, err := p.Wait()
			Expect(err).NotTo(HaveOccurred())
			Expect(code).To(Equal(127))
			Expect(stderr.String()).To(ContainSubstring("not found"))
		})
	})

	Describe("upload", func() {
		It("stops without a complete archive when the reader fails, and returns its error", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			boom := errors.New("tar source broke")
			err := d.Upload(ctx, ws, "sb", "/sandbox", io.MultiReader(strings.NewReader("partial"), failing{boom}))
			Expect(errors.Is(err, boom)).To(BeTrue(), "got %v", err)
		})

		It("reports a corrupt archive as ErrInvalid", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			err := d.Upload(ctx, ws, "sb", "/sandbox", strings.NewReader(strings.Repeat("not a tar file ", 100)))
			Expect(errors.Is(err, execution.ErrInvalid)).To(BeTrue(), "got %v", err)
		})

		It("retries the whole write on Unavailable that arrives when the stream closes", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			Expect(tw.WriteHeader(&tar.Header{Name: "a.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})).To(Succeed())
			_, _ = tw.Write([]byte("hi"))
			Expect(tw.Close()).To(Succeed())
			cl.FailNextWriteFiles(ws, "sb", 2)
			Expect(d.Upload(ctx, ws, "sb", "/work", &buf)).To(Succeed())
			Expect(cl.Calls("WriteFile")).To(Equal(3), "two failed streams and one that worked")
		})

		It("gives up after the ready timeout when every write ends Unavailable", func(ctx SpecContext) {
			dd, c := newDriver(substrate.Config{PollInterval: time.Millisecond, GuestReadyTimeout: 50 * time.Millisecond})
			ready(ctx, dd, "sb")
			c.FailNextWriteFiles(ws, "sb", 1<<20)
			err := dd.Upload(ctx, ws, "sb", "/work", strings.NewReader("x"))
			Expect(errors.Is(err, execution.ErrUnavailable)).To(BeTrue(), "got %v", err)
		})

		It("uploads a large archive and leaves no temporary file", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			body := bytes.Repeat([]byte("x"), 3<<20)
			Expect(tw.WriteHeader(&tar.Header{Name: "big.bin", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})).To(Succeed())
			_, _ = tw.Write(body)
			Expect(tw.Close()).To(Succeed())
			Expect(d.Upload(ctx, ws, "sb", "/work", &buf)).To(Succeed())
		})
	})

	Describe("the template", func() {
		It("shares a template between equal specs and carries no environment variable", func(ctx SpecContext) {
			dd, c := newDriver()
			for _, n := range []string{"a", "b"} {
				_, err := dd.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: n, Image: img, CPULimit: "500m", MemoryLimit: "1Gi"})
				Expect(err).NotTo(HaveOccurred())
			}
			_, err := dd.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: "c", Image: "img2" + digest})
			Expect(err).NotTo(HaveOccurred())
			Expect(c.Templates(ws)).To(HaveLen(2))
			Expect(c.TemplateOf(ws, "a")).To(Equal(c.TemplateOf(ws, "b")))
			Expect(c.TemplateOf(ws, "a")).NotTo(Equal(c.TemplateOf(ws, "c")))
			tmpl, ok := c.Template(ws, c.TemplateOf(ws, "a"))
			Expect(ok).To(BeTrue())
			Expect(tmpl.Containers).To(HaveLen(1))
			Expect(tmpl.Containers[0].Env).To(BeEmpty(), "the guest holds no value Koinon could keep secret")
			Expect(tmpl.Resources.Limits.CpuMillicores).To(BeEquivalentTo(500))
			Expect(tmpl.Resources.Limits.MemoryBytes).To(BeEquivalentTo(1 << 30))
			Expect(tmpl.SnapshotConfig.PreferredFidelity).To(Equal(ateapiv1.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY))
		})

		It("refuses egress it cannot enforce", func(ctx SpecContext) {
			_, err := d.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: "x", Image: img, Providers: []string{"p"}})
			Expect(errors.Is(err, execution.ErrUnsupported)).To(BeTrue(), "got %v", err)
			_, err = d.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: "x", Image: img, Policy: execution.Policy{Network: map[string]execution.NetworkRule{"r": {}}}})
			Expect(errors.Is(err, execution.ErrUnsupported)).To(BeTrue(), "got %v", err)
		})

		It("refuses a filesystem policy it cannot carry, on create and on restore", func(ctx SpecContext) {
			for _, fs := range []execution.FilesystemPolicy{
				{IncludeWorkdir: true}, {ReadOnly: []string{"/usr"}}, {ReadWrite: []string{"/sandbox"}},
			} {
				s := spec("x")
				s.Policy.Filesystem = fs
				_, err := d.CreateSandbox(ctx, ws, s)
				Expect(errors.Is(err, execution.ErrUnsupported)).To(BeTrue(), "got %v", err)
			}
			ready(ctx, d, "src")
			snap, err := d.SnapshotSandbox(ctx, ws, "src")
			Expect(err).NotTo(HaveOccurred())
			s := spec("copy")
			s.Policy.Filesystem.ReadWrite = []string{"/sandbox"}
			_, err = d.RestoreSandbox(ctx, ws, snap.SnapshotRef, s)
			Expect(errors.Is(err, execution.ErrUnsupported)).To(BeTrue(), "got %v", err)
			s = spec("copy")
			s.Providers = []string{"p"}
			_, err = d.RestoreSandbox(ctx, ws, snap.SnapshotRef, s)
			Expect(errors.Is(err, execution.ErrUnsupported)).To(BeTrue(), "got %v", err)
			_, err = d.GetSandbox(ctx, ws, "copy")
			Expect(errors.Is(err, execution.ErrNotFound)).To(BeTrue(), "nothing was created: %v", err)
		})

		It("rejects a quantity that is not one", func(ctx SpecContext) {
			_, err := d.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: "x", Image: img, MemoryLimit: "lots"})
			Expect(errors.Is(err, execution.ErrInvalid)).To(BeTrue(), "got %v", err)
		})

		DescribeTable("maps limits and requests to the one limit Substrate has",
			func(s execution.SandboxSpec, cpu, mem int64) {
				c, m, err := substrate.ResourceLimits(s)
				Expect(err).NotTo(HaveOccurred())
				Expect(c).To(Equal(cpu))
				Expect(m).To(Equal(mem))
			},
			Entry("limits win over requests", execution.SandboxSpec{CPURequest: "250m", CPULimit: "2", MemoryRequest: "1Gi", MemoryLimit: "2Gi"}, int64(2000), int64(2<<30)),
			Entry("a request with no limit is the limit", execution.SandboxSpec{CPURequest: "250m", MemoryRequest: "512Mi"}, int64(250), int64(512<<20)),
			Entry("nothing is nothing", execution.SandboxSpec{}, int64(0), int64(0)),
		)
	})

	Describe("snapshots", func() {
		It("takes a snapshot of a running sandbox and leaves it running, with no router request", func(ctx SpecContext) {
			ready(ctx, d, "src")
			snap, err := d.SnapshotSandbox(ctx, ws, "src")
			Expect(err).NotTo(HaveOccurred())
			Expect(snap.Kind).To(Equal(execution.SnapshotDiskMemory))
			Expect(snap.Digest).To(HavePrefix("sha256:"))
			sb, err := d.WaitPhase(ctx, ws, "src", execution.PhaseReady)
			Expect(err).NotTo(HaveOccurred())
			Expect(sb.Phase).To(Equal(execution.PhaseReady))
			Expect(cl.RouterResumes(ws, "src")).To(BeZero())
		})

		It("resumes on a retry a sandbox that an earlier snapshot could not resume", func(ctx SpecContext) {
			ready(ctx, d, "src")
			cl.FailNextResumes(ws, "src", 1)
			_, err := d.SnapshotSandbox(ctx, ws, "src")
			Expect(errors.Is(err, execution.ErrUnavailable)).To(BeTrue(), "got %v", err)
			sb, err := d.GetSandbox(ctx, ws, "src")
			Expect(err).NotTo(HaveOccurred())
			Expect(sb.Phase).To(Equal(execution.PhaseSuspended))

			_, err = d.SnapshotSandbox(ctx, ws, "src")
			Expect(err).NotTo(HaveOccurred())
			sb, err = d.WaitPhase(ctx, ws, "src", execution.PhaseReady)
			Expect(err).NotTo(HaveOccurred())
			Expect(sb.Phase).To(Equal(execution.PhaseReady))
		})

		It("leaves the sandbox running when the suspend fails after SuspendActor", func(ctx SpecContext) {
			ready(ctx, d, "src")
			cl.FailGetActorsAfterSuspend(ws, "src", 1) // settled fails: the actor is suspended
			_, err := d.SnapshotSandbox(ctx, ws, "src")
			Expect(err).To(HaveOccurred())
			Expect(cl.Calls("SuspendActor")).To(Equal(1))
			sb, err := d.WaitPhase(ctx, ws, "src", execution.PhaseReady)
			Expect(err).NotTo(HaveOccurred(), "the snapshot resumed the sandbox it suspended")
			Expect(sb.Phase).To(Equal(execution.PhaseReady))
			Expect(cl.RouterResumes(ws, "src")).To(BeZero())
		}, SpecTimeout(10*time.Second))

		It("resumes on the next snapshot when the suspend and the resume after it both failed", func(ctx SpecContext) {
			ready(ctx, d, "src")
			cl.FailGetActorsAfterSuspend(ws, "src", 1)
			cl.FailNextResumes(ws, "src", 1)
			_, err := d.SnapshotSandbox(ctx, ws, "src")
			Expect(err).To(HaveOccurred())
			Expect(cl.State(ws, "src")).To(Equal(ateapiv1.ActorState_ACTOR_STATE_SUSPENDED))

			_, err = d.SnapshotSandbox(ctx, ws, "src")
			Expect(err).NotTo(HaveOccurred())
			sb, err := d.WaitPhase(ctx, ws, "src", execution.PhaseReady)
			Expect(err).NotTo(HaveOccurred())
			Expect(sb.Phase).To(Equal(execution.PhaseReady))
		}, SpecTimeout(10*time.Second))

		It("keeps the gate closed from the suspend until the resume", func(ctx SpecContext) {
			ready(ctx, d, "src")
			held, release := cl.HoldNextCreateTag(ws, "src")
			DeferCleanup(release)
			done := make(chan error, 1)
			go func() { _, err := d.SnapshotSandbox(ctx, ws, "src"); done <- err }()
			Eventually(held).WithTimeout(5 * time.Second).Should(BeClosed())
			// The actor is suspended and not yet tagged: no router call may start now.
			_, err := d.Exec(ctx, ws, "src", execution.ExecRequest{Command: []string{"true"}})
			Expect(errors.Is(err, execution.ErrFailedPrecondition)).To(BeTrue(), "got %v", err)
			Expect(err.Error()).To(ContainSubstring("being suspended"), "the gate, not the state read, refused it")
			release()
			Eventually(done).WithTimeout(5 * time.Second).Should(Receive(BeNil()))
			_, err = d.WaitPhase(ctx, ws, "src", execution.PhaseReady)
			Expect(err).NotTo(HaveOccurred())
			p, err := d.Exec(ctx, ws, "src", execution.ExecRequest{Command: []string{"true"}})
			Expect(err).NotTo(HaveOccurred(), "the gate opens once the sandbox runs again")
			Expect(p.Close()).To(Succeed())
		}, SpecTimeout(30*time.Second))

		It("leaves a sandbox suspended that was suspended before the snapshot", func(ctx SpecContext) {
			ready(ctx, d, "src")
			cl.FailNextResumes(ws, "src", 1)
			_, err := d.SnapshotSandbox(ctx, ws, "src")
			Expect(err).To(HaveOccurred())
			_, err = d.SuspendSandbox(ctx, ws, "src") // the caller now wants it suspended
			Expect(err).NotTo(HaveOccurred())
			_, err = d.SnapshotSandbox(ctx, ws, "src")
			Expect(err).NotTo(HaveOccurred())
			sb, err := d.GetSandbox(ctx, ws, "src")
			Expect(err).NotTo(HaveOccurred())
			Expect(sb.Phase).To(Equal(execution.PhaseSuspended))
		})

		It("restores into a template of its source and a different identity", func(ctx SpecContext) {
			dd, c := newDriver()
			ready(ctx, dd, "src")
			snap, err := dd.SnapshotSandbox(ctx, ws, "src")
			Expect(err).NotTo(HaveOccurred())
			cp, err := dd.RestoreSandbox(ctx, ws, snap.SnapshotRef, spec("copy"))
			Expect(err).NotTo(HaveOccurred())
			src, _ := dd.GetSandbox(ctx, ws, "src")
			Expect(cp.ID).NotTo(Equal(src.ID))
			Expect(c.TemplateOf(ws, "copy")).To(Equal(c.TemplateOf(ws, "src")))
		})

		It("refuses a restore after the stored snapshot changed", func(ctx SpecContext) {
			dd, c := newDriver()
			ready(ctx, dd, "src")
			snap, err := dd.SnapshotSandbox(ctx, ws, "src")
			Expect(err).NotTo(HaveOccurred())
			c.TamperTag(ws, snap.ID)
			_, err = dd.RestoreSandbox(ctx, ws, snap.SnapshotRef, spec("copy"))
			Expect(errors.Is(err, execution.ErrInvalid)).To(BeTrue(), "got %v", err)
			_, err = dd.GetSandbox(ctx, ws, "copy")
			Expect(errors.Is(err, execution.ErrNotFound)).To(BeTrue(), "nothing was created: %v", err)
		})

		It("fails closed without a content digest, and does not suspend the sandbox first", func(ctx SpecContext) {
			noContentDigest = true
			DeferCleanup(func() { noContentDigest = false })
			dd, c := newDriver()
			ready(ctx, dd, "src")
			_, err := dd.SnapshotSandbox(ctx, ws, "src")
			Expect(errors.Is(err, execution.ErrUnsupported)).To(BeTrue(), "got %v", err)
			Expect(c.State(ws, "src")).To(Equal(ateapiv1.ActorState_ACTOR_STATE_RUNNING))
			_, err = dd.RestoreSandbox(ctx, ws, execution.SnapshotRef{ID: "t", Digest: "sha256:00"}, spec("copy"))
			Expect(errors.Is(err, execution.ErrUnsupported)).To(BeTrue(), "got %v", err)
		})

		It("adds the content digest to the snapshot digest when one is configured", func(ctx SpecContext) {
			var calls int
			content := "aaa"
			dd, _ := newDriver(substrate.Config{
				PollInterval: 5 * time.Millisecond,
				ContentDigest: func(context.Context, string) (string, error) {
					calls++
					return content, nil
				},
			})
			ready(ctx, dd, "src")
			snap, err := dd.SnapshotSandbox(ctx, ws, "src")
			Expect(err).NotTo(HaveOccurred())
			content = "bbb" // the object's bytes changed behind the same address
			_, err = dd.RestoreSandbox(ctx, ws, snap.SnapshotRef, spec("copy"))
			Expect(errors.Is(err, execution.ErrInvalid)).To(BeTrue(), "got %v", err)
			Expect(calls).To(BeNumerically(">=", 2))
		})
	})

	Describe("the actor-state observer", func() {
		It("names the cause of a crashed actor from its message, without a router request", func(ctx SpecContext) {
			dd, c := newDriver()
			ready(ctx, dd, "sb")
			c.Crash(ws, "sb", `failed to pull image "img": manifest unknown`)
			o, err := dd.Observe(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			Expect(o.Sandbox.Phase).To(Equal(execution.PhaseError))
			Expect(o.Cause).To(Equal(execution.WaitCauseImagePull))
			Expect(o.Sandbox.Message).To(ContainSubstring("manifest unknown"))
			Expect(c.RouterResumes(ws, "sb")).To(BeZero())
		})

		It("has no cause for a ready sandbox", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			o, err := d.Observe(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			Expect(o.Cause).To(BeEmpty())
			Expect(o.Worker).NotTo(BeEmpty())
		})
	})

	Describe("delete", func() {
		It("leaves no gate behind when a sandbox with a running process is deleted", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			p, err := d.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"cat"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(d.GateCount()).To(Equal(1))
			out, err := d.DeleteSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal(execution.DeleteCompleted))
			Expect(p.Close()).To(Succeed())
			Eventually(d.GateCount).Should(BeZero())
		})

		It("forgets the gate of a sandbox that ran a command to its end", func(ctx SpecContext) {
			ready(ctx, d, "sb")
			p, err := d.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"echo", "hi"}})
			Expect(err).NotTo(HaveOccurred())
			_, err = io.ReadAll(p.Stdout())
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Close()).To(Succeed())
			Eventually(d.GateCount).Should(BeZero())
		})

		It("reports an actor that is still going away as accepted", func(ctx SpecContext) {
			dd, c := newDriver()
			c.AsyncDelete = true
			ready(ctx, dd, "sb")
			out, err := dd.DeleteSandbox(ctx, ws, "sb")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal(execution.DeleteAccepted))
			sb, err := dd.GetSandbox(ctx, ws, "sb")
			if err == nil {
				Expect(sb.Phase).To(Equal(execution.PhaseDeleting))
			}
		})
	})
})

type failing struct{ err error }

func (f failing) Read([]byte) (int, error) { return 0, f.err }

// stallReader gives one chunk, then blocks until released and ends the stream.
type stallReader struct {
	first   []byte
	started chan struct{}
	release chan struct{}
	done    bool
}

func (r *stallReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		close(r.started)
		return copy(p, r.first), nil
	}
	<-r.release
	return 0, io.EOF
}
