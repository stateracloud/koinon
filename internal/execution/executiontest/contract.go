// SPDX-License-Identifier: Apache-2.0

// Package executiontest is the contract every execution driver must pass (epic 1082, issue 1085)
// and an in-memory driver, [Fake], that passes it. A driver's tests call [ComputeContract] with a
// function that builds a fresh driver; the specs then run in that package's Ginkgo suite.
//
// The contract covers [execution.Compute] and, only when the driver implements them, the optional
// capabilities [execution.Suspender] and [execution.Snapshotter]. It says nothing about egress:
// provider records and L7 rules are not part of what a new driver provides (ADR 0097).
package executiontest

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // the Ginkgo DSL is dot-imported by convention
	. "github.com/onsi/gomega"    //nolint:staticcheck // the Gomega DSL is dot-imported by convention

	"github.com/stateracloud/koinon/internal/execution"
)

// Harness is one fresh driver under test, with the few things the contract cannot know.
type Harness struct {
	// Compute is the driver. It starts with no workspace and no sandbox.
	Compute execution.Compute
	// Workspace is the workspace name the specs use.
	Workspace string
	// Image is the sandbox image. The specs give no other sandbox setting.
	Image string
	// Exec is a command that copies its stdin to its stdout and exits with 0 at end of input,
	// like cat(1). The driver (or its test double) must run it.
	Exec []string
	// Uploaded returns the content of the file "name" (the archive entry name) that the last
	// Upload to the sandbox put into destDir. Optional: when nil, the specs check only that the
	// upload succeeds.
	Uploaded func(sandbox, destDir, name string) ([]byte, bool)
}

const settle = 10 * time.Second

// ComputeContract registers the contract specs for the driver that newHarness builds. It is
// called at the top level of a test file, like Describe. newHarness runs in a BeforeEach, so it may
// call DeferCleanup. It returns the Describe result, so that `var _ = ComputeContract(...)` works.
func ComputeContract(driver string, newHarness func() Harness) bool {
	return Describe("the execution.Compute contract: "+driver, func() {
		var h Harness
		BeforeEach(func(ctx SpecContext) {
			h = newHarness()
			_, err := h.Compute.EnsureWorkspace(ctx, h.Workspace, map[string]string{"org": "acme"})
			Expect(err).NotTo(HaveOccurred())
		})

		spec := func(name string) execution.SandboxSpec {
			return execution.SandboxSpec{Name: name, Image: h.Image, CPULimit: "500m", MemoryLimit: "512Mi"}
		}
		phaseOf := func(ctx SpecContext, name string) func() (execution.Phase, error) {
			return func() (execution.Phase, error) {
				sb, err := h.Compute.GetSandbox(ctx, h.Workspace, name)
				return sb.Phase, err
			}
		}
		// stopped is what a stopped sandbox reports. A driver whose only way to stop is to suspend
		// (Agent Substrate: a stopped actor is a suspended one) reports PhaseSuspended; both mean the
		// sandbox keeps its state and is not ready.
		stopped := BeElementOf(execution.PhaseStopped, execution.PhaseSuspended)
		create := func(ctx SpecContext, name string) execution.Sandbox {
			sb, err := h.Compute.CreateSandbox(ctx, h.Workspace, spec(name))
			Expect(err).NotTo(HaveOccurred())
			Eventually(phaseOf(ctx, name)).WithTimeout(settle).Should(Equal(execution.PhaseReady))
			return sb
		}

		Describe("workspaces", func() {
			It("ensures a workspace idempotently", func(ctx SpecContext) {
				a, err := h.Compute.EnsureWorkspace(ctx, h.Workspace, nil)
				Expect(err).NotTo(HaveOccurred())
				b, err := h.Compute.EnsureWorkspace(ctx, h.Workspace, nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(a.Name).To(Equal(h.Workspace))
				Expect(b.ID).To(Equal(a.ID))
			})
		})

		Describe("sandbox lifecycle", func() {
			It("creates a sandbox that becomes ready", func(ctx SpecContext) {
				sb := create(ctx, "sb")
				Expect(sb.Name).To(Equal("sb"))
				Expect(sb.ID).NotTo(BeEmpty())
			})

			It("returns the existing sandbox when it creates one with the same name", func(ctx SpecContext) {
				first := create(ctx, "sb")
				again, err := h.Compute.CreateSandbox(ctx, h.Workspace, spec("sb"))
				Expect(err).NotTo(HaveOccurred())
				Expect(again.ID).To(Equal(first.ID))
			})

			It("refuses to create a sandbox in a missing workspace with ErrNotFound", func(ctx SpecContext) {
				_, err := h.Compute.CreateSandbox(ctx, "no-such-workspace", spec("sb"))
				Expect(errors.Is(err, execution.ErrNotFound)).To(BeTrue(), "got %v", err)
			})

			It("reports a missing sandbox as ErrNotFound", func(ctx SpecContext) {
				_, err := h.Compute.GetSandbox(ctx, h.Workspace, "missing")
				Expect(errors.Is(err, execution.ErrNotFound)).To(BeTrue(), "got %v", err)
			})

			It("stops and starts a sandbox, and both are no-ops when repeated", func(ctx SpecContext) {
				create(ctx, "sb")
				_, err := h.Compute.StartSandbox(ctx, h.Workspace, "sb")
				Expect(err).NotTo(HaveOccurred(), "start of a running sandbox")

				_, err = h.Compute.StopSandbox(ctx, h.Workspace, "sb")
				Expect(err).NotTo(HaveOccurred())
				Eventually(phaseOf(ctx, "sb")).WithTimeout(settle).Should(stopped)
				_, err = h.Compute.StopSandbox(ctx, h.Workspace, "sb")
				Expect(err).NotTo(HaveOccurred(), "stop of a stopped sandbox")

				_, err = h.Compute.StartSandbox(ctx, h.Workspace, "sb")
				Expect(err).NotTo(HaveOccurred())
				Eventually(phaseOf(ctx, "sb")).WithTimeout(settle).Should(Equal(execution.PhaseReady))
			})

			It("deletes a sandbox, and a missing one is not an error", func(ctx SpecContext) {
				create(ctx, "sb")
				out, err := h.Compute.DeleteSandbox(ctx, h.Workspace, "sb")
				Expect(err).NotTo(HaveOccurred())
				Expect(out).To(BeElementOf(execution.DeleteCompleted, execution.DeleteAccepted))
				Eventually(func() error {
					_, err := h.Compute.GetSandbox(ctx, h.Workspace, "sb")
					return err
				}).WithTimeout(settle).Should(MatchError(execution.ErrNotFound))

				out, err = h.Compute.DeleteSandbox(ctx, h.Workspace, "sb")
				Expect(err).NotTo(HaveOccurred())
				Expect(out).To(Equal(execution.DeleteAbsent))
			})
		})

		Describe("exec", func() {
			It("streams stdin to the command and its output back, without a TTY", func(ctx SpecContext) {
				create(ctx, "sb")
				p, err := h.Compute.Exec(ctx, h.Workspace, "sb", execution.ExecRequest{Command: h.Exec})
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(p.Close)

				// Newline-delimited lines, as ACP sends them, plus a body larger than any pipe buffer.
				want := strings.Repeat("{\"jsonrpc\":\"2.0\"}\n", 3) + strings.Repeat("x", 512<<10) + "\n"
				werr := make(chan error, 1)
				go func() {
					_, err := io.WriteString(p.Stdin(), want)
					if cerr := p.Stdin().Close(); err == nil {
						err = cerr
					}
					werr <- err
				}()
				got, err := io.ReadAll(p.Stdout())
				Expect(err).NotTo(HaveOccurred())
				Expect(<-werr).To(Succeed())
				Expect(string(got)).To(Equal(want))

				code, err := p.Wait()
				Expect(err).NotTo(HaveOccurred())
				Expect(code).To(BeZero())

				By("a Close after the exit keeps the result")
				Expect(p.Close()).To(Succeed())
				code, err = p.Wait()
				Expect(err).NotTo(HaveOccurred())
				Expect(code).To(BeZero())
			}, SpecTimeout(30*time.Second))

			It("ends the command when its context is cancelled", func(ctx SpecContext) {
				create(ctx, "sb")
				cctx, cancel := context.WithCancel(ctx)
				p, err := h.Compute.Exec(cctx, h.Workspace, "sb", execution.ExecRequest{Command: h.Exec})
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(p.Close)
				cancel()
				done := make(chan struct{})
				go func() { defer close(done); _, _ = p.Wait() }()
				Eventually(done).WithTimeout(settle).Should(BeClosed())
			}, SpecTimeout(30*time.Second))

			It("reports a missing sandbox as ErrNotFound", func(ctx SpecContext) {
				_, err := h.Compute.Exec(ctx, h.Workspace, "missing", execution.ExecRequest{Command: h.Exec})
				Expect(errors.Is(err, execution.ErrNotFound)).To(BeTrue(), "got %v", err)
			})

			It("ends the command when the process is closed, and Close can repeat", func(ctx SpecContext) {
				create(ctx, "sb")
				p, err := h.Compute.Exec(ctx, h.Workspace, "sb", execution.ExecRequest{Command: h.Exec})
				Expect(err).NotTo(HaveOccurred())
				Expect(p.Close()).To(Succeed())
				Expect(p.Close()).To(Succeed())
				done := make(chan struct{})
				go func() { defer close(done); _, _ = p.Wait() }()
				Eventually(done).WithTimeout(settle).Should(BeClosed())
			})

			It("refuses a sandbox that is not ready with ErrFailedPrecondition", func(ctx SpecContext) {
				create(ctx, "sb")
				_, err := h.Compute.StopSandbox(ctx, h.Workspace, "sb")
				Expect(err).NotTo(HaveOccurred())
				Eventually(phaseOf(ctx, "sb")).WithTimeout(settle).Should(stopped)
				_, err = h.Compute.Exec(ctx, h.Workspace, "sb", execution.ExecRequest{Command: h.Exec})
				Expect(errors.Is(err, execution.ErrFailedPrecondition)).To(BeTrue(), "got %v", err)
			})
		})

		Describe("upload", func() {
			It("extracts the archive into the destination", func(ctx SpecContext) {
				create(ctx, "sb")
				var buf bytes.Buffer
				tw := tar.NewWriter(&buf)
				body := []byte("hello from the contract suite")
				Expect(tw.WriteHeader(&tar.Header{Name: "dir/a.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})).To(Succeed())
				_, err := tw.Write(body)
				Expect(err).NotTo(HaveOccurred())
				Expect(tw.Close()).To(Succeed())

				err = h.Compute.Upload(ctx, h.Workspace, "sb", "/sandbox", &buf)
				if errors.Is(err, execution.ErrUnsupported) {
					Skip(driver + " does not support uploads")
				}
				Expect(err).NotTo(HaveOccurred())
				if h.Uploaded != nil {
					got, ok := h.Uploaded("sb", "/sandbox", "dir/a.txt")
					Expect(ok).To(BeTrue(), "the file is in the sandbox")
					Expect(got).To(Equal(body))
				}
			})

			It("reports a missing sandbox as ErrNotFound", func(ctx SpecContext) {
				err := h.Compute.Upload(ctx, h.Workspace, "missing", "/sandbox", bytes.NewReader(nil))
				if errors.Is(err, execution.ErrUnsupported) {
					Skip(driver + " does not support uploads")
				}
				Expect(errors.Is(err, execution.ErrNotFound)).To(BeTrue(), "got %v", err)
			})
		})

		Describe("suspend and resume", func() {
			var s execution.Suspender
			BeforeEach(func() {
				var ok bool
				if s, ok = h.Compute.(execution.Suspender); !ok {
					Skip(driver + " does not implement execution.Suspender")
				}
			})

			It("suspends a running sandbox and resumes it, and both are no-ops when repeated", func(ctx SpecContext) {
				create(ctx, "sb")
				_, err := s.ResumeSandbox(ctx, h.Workspace, "sb")
				Expect(err).NotTo(HaveOccurred(), "resume of a running sandbox")

				_, err = s.SuspendSandbox(ctx, h.Workspace, "sb")
				Expect(err).NotTo(HaveOccurred())
				Eventually(phaseOf(ctx, "sb")).WithTimeout(settle).Should(Equal(execution.PhaseSuspended))
				_, err = s.SuspendSandbox(ctx, h.Workspace, "sb")
				Expect(err).NotTo(HaveOccurred(), "suspend of a suspended sandbox")

				_, err = s.ResumeSandbox(ctx, h.Workspace, "sb")
				Expect(err).NotTo(HaveOccurred())
				Eventually(phaseOf(ctx, "sb")).WithTimeout(settle).Should(Equal(execution.PhaseReady))
			})
		})

		Describe("snapshot and restore", func() {
			var s execution.Snapshotter
			BeforeEach(func() {
				var ok bool
				if s, ok = h.Compute.(execution.Snapshotter); !ok {
					Skip(driver + " does not implement execution.Snapshotter")
				}
			})

			It("saves a sandbox with a digest and restores it into a new sandbox", func(ctx SpecContext) {
				src := create(ctx, "src")
				snap, err := s.SnapshotSandbox(ctx, h.Workspace, "src")
				Expect(err).NotTo(HaveOccurred())
				Expect(snap.ID).NotTo(BeEmpty())
				Expect(snap.Digest).To(MatchRegexp(`^[a-z0-9]+:[0-9a-f]+$`), "the digest is <algorithm>:<hex>")
				Expect(snap.Kind).To(BeElementOf(execution.SnapshotDisk, execution.SnapshotDiskMemory))
				Expect(snap.Sandbox).To(Equal("src"))

				sb, err := s.RestoreSandbox(ctx, h.Workspace, snap.SnapshotRef, spec("copy"))
				Expect(err).NotTo(HaveOccurred())
				Expect(sb.Name).To(Equal("copy"))
				Expect(sb.ID).NotTo(Equal(src.ID))
				Eventually(phaseOf(ctx, "copy")).WithTimeout(settle).Should(Equal(execution.PhaseReady))
			})

			It("refuses a digest that does not match", func(ctx SpecContext) {
				create(ctx, "src")
				snap, err := s.SnapshotSandbox(ctx, h.Workspace, "src")
				Expect(err).NotTo(HaveOccurred())
				bad := snap.SnapshotRef
				bad.Digest = fmt.Sprintf("%s:%s", strings.SplitN(snap.Digest, ":", 2)[0], strings.Repeat("0", 64))
				_, err = s.RestoreSandbox(ctx, h.Workspace, bad, spec("copy"))
				Expect(errors.Is(err, execution.ErrInvalid)).To(BeTrue(), "got %v", err)
			})

			It("does not restore a snapshot in another workspace", func(ctx SpecContext) {
				create(ctx, "src")
				snap, err := s.SnapshotSandbox(ctx, h.Workspace, "src")
				Expect(err).NotTo(HaveOccurred())
				_, err = h.Compute.EnsureWorkspace(ctx, h.Workspace+"-other", nil)
				Expect(err).NotTo(HaveOccurred())
				_, err = s.RestoreSandbox(ctx, h.Workspace+"-other", snap.SnapshotRef, spec("copy"))
				Expect(errors.Is(err, execution.ErrNotFound)).To(BeTrue(), "got %v", err)
			})

			It("refuses to restore onto an existing sandbox name with ErrAlreadyExists", func(ctx SpecContext) {
				create(ctx, "src")
				snap, err := s.SnapshotSandbox(ctx, h.Workspace, "src")
				Expect(err).NotTo(HaveOccurred())
				_, err = s.RestoreSandbox(ctx, h.Workspace, snap.SnapshotRef, spec("src"))
				Expect(errors.Is(err, execution.ErrAlreadyExists)).To(BeTrue(), "got %v", err)
			})

			It("reports an unknown snapshot as ErrNotFound", func(ctx SpecContext) {
				_, err := s.RestoreSandbox(ctx, h.Workspace, execution.SnapshotRef{ID: "missing", Digest: "sha256:00"}, spec("copy"))
				Expect(errors.Is(err, execution.ErrNotFound)).To(BeTrue(), "got %v", err)
			})
		})
	})
}
