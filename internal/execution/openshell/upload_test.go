// SPDX-License-Identifier: Apache-2.0

package openshell_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/execution/openshell"
	"github.com/stateracloud/koinon/internal/execution/openshell/fakegateway"
)

// slowReader makes the extractor slower than the sender, so that the window, not the network,
// is what holds the sender back.
type slowReader struct {
	r io.Reader
	d time.Duration
}

func (s slowReader) Read(p []byte) (int, error) {
	time.Sleep(s.d)
	return s.r.Read(p[:min(len(p), 16*1024)])
}

// extracted collects what the fake `tar xf -` saw.
type extracted struct {
	mu    sync.Mutex
	dest  string
	files map[string][]byte
}

func (e *extracted) handler(delay time.Duration) fakegateway.UploadHandler {
	return func(_ context.Context, dest string, archive io.Reader, stderr io.Writer) int {
		files := map[string][]byte{}
		tr := tar.NewReader(slowReader{archive, delay})
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "tar: %v\n", err)
				return 2
			}
			if h.Typeflag != tar.TypeReg {
				continue
			}
			b, err := io.ReadAll(tr)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "tar: %v\n", err)
				return 2
			}
			files[h.Name] = b
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		e.dest, e.files = dest, files
		return 0
	}
}

func buildTar(files map[string][]byte) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		Expect(tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})).To(Succeed())
		_, err := tw.Write(body)
		Expect(err).NotTo(HaveOccurred())
	}
	Expect(tw.Close()).To(Succeed())
	return buf.Bytes()
}

var _ = Describe("Upload", func() {
	var (
		b  *openshell.Backend
		gw *fakegateway.Gateway
		ex *extracted
	)

	BeforeEach(func(ctx SpecContext) {
		b, gw = newBackend()
		_, err := b.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: "sb", Image: "img"})
		Expect(err).NotTo(HaveOccurred())
		ex = &extracted{}
		gw.HandleUpload(ex.handler(100 * time.Microsecond))
	})

	noTokens := func(err error) {
		for _, t := range gw.SSHTokens() {
			Expect(err.Error()).NotTo(ContainSubstring(t), "the session token is never put into an error")
		}
	}

	It("uploads more than 20 MiB of random data through the flow-controlled channel", func(ctx SpecContext) {
		files := map[string][]byte{}
		for i := range 8 {
			body := make([]byte, 3<<20+i*1000)
			_, _ = rand.Read(body)
			files[fmt.Sprintf("dir%d/file.bin", i)] = body
		}
		files["small.txt"] = []byte("hello")
		archive := buildTar(files)
		Expect(len(archive)).To(BeNumerically(">", 20<<20))

		Expect(b.Upload(ctx, ws, "sb", "/sandbox", bytes.NewReader(archive))).To(Succeed())

		Expect(ex.dest).To(Equal("/sandbox"))
		Expect(ex.files).To(HaveLen(len(files)))
		for name, want := range files {
			Expect(bytes.Equal(ex.files[name], want)).To(BeTrue(), "%s differs", name)
		}
		st := gw.UploadStats()
		Expect(st.Uploads).To(Equal(1))
		Expect(st.Bytes).To(BeEquivalentTo(len(archive)))
		By("the client never ran more than the 1 MiB window ahead of the extractor")
		Expect(st.Violations).To(BeZero())
		Expect(st.MaxOutstanding).To(BeNumerically("<=", fakegateway.UploadWindow))
		Expect(st.MaxOutstanding).To(BeNumerically(">", fakegateway.UploadAckEvery), "the window was really used")
		By("the session token is revoked")
		Expect(gw.ActiveSSHSessions()).To(BeZero())
		Expect(gw.Calls("RevokeSshSession")).To(Equal(1))
	}, SpecTimeout(2*time.Minute))

	It("shows that the same stream over exec stdin is killed with 74 (the bug this replaces)", func(ctx SpecContext) {
		gw.Handle("stuck", func(ctx context.Context, _ []string, _ io.Reader, _, _ io.Writer) int {
			<-ctx.Done() // a command that reads no stdin
			return 143
		})
		p, err := b.Exec(ctx, ws, "sb", execution.ExecRequest{Command: []string{"stuck"}})
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = p.Close() }()
		go func() {
			_, _ = p.Stdin().Write(make([]byte, 20<<20))
		}()
		Expect(p.Wait()).To(Equal(74))
	}, SpecTimeout(30*time.Second))

	It("revokes the session and reports the extractor's exit status and output when extraction fails", func(ctx SpecContext) {
		gw.HandleUpload(func(_ context.Context, _ string, archive io.Reader, stderr io.Writer) int {
			_, _ = io.CopyN(io.Discard, archive, 1000)
			_, _ = io.WriteString(stderr, "tar: /sandbox/x: Cannot open: Permission denied\n")
			return 2
		})
		// The extractor stops reading after 1000 bytes and fails while the client still has
		// megabytes to send: the supervisor ends the channel with the exit status, and the client
		// learns of it while it waits for credit or in a write.
		body := make([]byte, 8<<20)
		_, _ = rand.Read(body)
		err := b.Upload(ctx, ws, "sb", "/sandbox", bytes.NewReader(buildTar(map[string][]byte{"x": body})))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("status 2"))
		Expect(err.Error()).To(ContainSubstring("Permission denied"))
		Expect(gw.ActiveSSHSessions()).To(BeZero())
		noTokens(err)
	}, SpecTimeout(30*time.Second))

	It("reports a gateway that ends the relay while the client is writing, without a second receiver", func(ctx SpecContext) {
		// The gateway refuses the token right after the init frame; the SSH client is already
		// writing its version line, so Write sees the ended stream while Read holds the status.
		for range 20 {
			gw.ExpireSSHSessions()
			err := b.Upload(ctx, ws, "sb", "/sandbox", bytes.NewReader(buildTar(map[string][]byte{"a": []byte("a")})))
			Expect(err).To(MatchError(execution.ErrPermissionDenied))
			Expect(err.Error()).To(ContainSubstring("expired"))
		}
	}, SpecTimeout(time.Minute))

	It("refuses a host key that does not match the gateway's fingerprint, before sending the plan", func(ctx SpecContext) {
		gw.SpoofHostKeyFingerprint("SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
		err := b.Upload(ctx, ws, "sb", "/sandbox", bytes.NewReader(buildTar(map[string][]byte{"a": []byte("a")})))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("host key"))
		Expect(err.Error()).To(ContainSubstring("does not match"))
		Expect(gw.UploadStats().Uploads).To(BeZero(), "nothing was sent")
		Expect(gw.ActiveSSHSessions()).To(BeZero())
		noTokens(err)
	})

	It("refuses a gateway that gives no fingerprint", func(ctx SpecContext) {
		gw.OmitHostKeyFingerprint()
		err := b.Upload(ctx, ws, "sb", "/sandbox", bytes.NewReader(buildTar(nil)))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("no host key fingerprint"))
		Expect(gw.UploadStats().Uploads).To(BeZero())
	})

	It("says what to upgrade when the supervisor has no upload subsystem", func(ctx SpecContext) {
		gw.DisableUploadSubsystem()
		err := b.Upload(ctx, ws, "sb", "/sandbox", bytes.NewReader(buildTar(map[string][]byte{"a": []byte("a")})))
		Expect(err).To(MatchError(execution.ErrUnsupported))
		Expect(err.Error()).To(ContainSubstring("does not support openshell-upload-v1"))
		Expect(err.Error()).To(ContainSubstring("OpenShell v0.1.4 or later"))
		Expect(gw.ActiveSSHSessions()).To(BeZero())
	})

	It("tells an expired session from a connection limit", func(ctx SpecContext) {
		gw.ExpireSSHSessions()
		err := b.Upload(ctx, ws, "sb", "/sandbox", bytes.NewReader(buildTar(nil)))
		Expect(err).To(MatchError(execution.ErrPermissionDenied))
		Expect(err.Error()).To(ContainSubstring("expired"))
		noTokens(err)

		b2, gw2 := newBackend()
		_, cerr := b2.CreateSandbox(ctx, ws, execution.SandboxSpec{Name: "sb", Image: "img"})
		Expect(cerr).NotTo(HaveOccurred())
		gw2.FailNext("ForwardTcp", status.Error(codes.ResourceExhausted, "sandbox SSH connection limit reached"))
		err = b2.Upload(ctx, ws, "sb", "/sandbox", bytes.NewReader(buildTar(nil)))
		Expect(err).To(MatchError(execution.ErrUnavailable))
		Expect(err.Error()).To(ContainSubstring("connection limit"))
		Expect(gw2.ActiveSSHSessions()).To(BeZero())
	})

	It("reports a missing sandbox", func(ctx SpecContext) {
		err := b.Upload(ctx, ws, "nope", "/sandbox", bytes.NewReader(nil))
		Expect(err).To(MatchError(execution.ErrNotFound))
	})

	It("returns the archive reader's error and still revokes the session", func(ctx SpecContext) {
		boom := errors.New("the snapshot is corrupt")
		r := io.MultiReader(bytes.NewReader(make([]byte, 300<<10)), &errReader{boom})
		err := b.Upload(ctx, ws, "sb", "/sandbox", r)
		Expect(err).To(MatchError(boom))
		Expect(gw.ActiveSSHSessions()).To(BeZero())
	}, SpecTimeout(30*time.Second))

	It("stops when the context ends", func(ctx SpecContext) {
		cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		pr, pw := io.Pipe()
		defer func() { _ = pw.Close() }()
		err := b.Upload(cctx, ws, "sb", "/sandbox", pr)
		Expect(err).To(MatchError(context.DeadlineExceeded))
		Expect(gw.ActiveSSHSessions()).To(BeZero())
	}, SpecTimeout(30*time.Second))

	It("rejects an empty destination", func(ctx SpecContext) {
		err := b.Upload(ctx, ws, "sb", "", strings.NewReader(""))
		Expect(err).To(MatchError(execution.ErrInvalid))
		Expect(gw.Calls("CreateSshSession")).To(BeZero())
	})
})

type errReader struct{ err error }

func (e *errReader) Read([]byte) (int, error) { return 0, e.err }
