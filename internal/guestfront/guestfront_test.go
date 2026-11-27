// SPDX-License-Identifier: Apache-2.0

package guestfront_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/stateracloud/koinon/internal/guestauth"
	"github.com/stateracloud/koinon/internal/guestfront"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

var _ = Describe("KeysFromEnv", func() {
	It("reads the keys and removes the variable", func() {
		_, k, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		pub := k.Public().(ed25519.PublicKey)
		GinkgoT().Setenv(guestauth.VerifyKeysEnv, guestauth.EncodeKeys(pub))
		keys, err := guestfront.KeysFromEnv()
		Expect(err).NotTo(HaveOccurred())
		Expect(keys).To(Equal([]ed25519.PublicKey{pub}))
		_, set := os.LookupEnv(guestauth.VerifyKeysEnv)
		Expect(set).To(BeFalse())
	})

	It("fails closed without a key", func() {
		GinkgoT().Setenv(guestauth.VerifyKeysEnv, "")
		_, err := guestfront.KeysFromEnv()
		Expect(err).To(HaveOccurred())
		_, err = guestfront.New(guestfront.Config{})
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("Front", func() {
	const self, other = "org-1/sbx-a", "org-1/sbx-b"
	var (
		signer  *guestauth.Signer
		addr    string
		conn    *grpc.ClientConn
		process ateenvv1alpha.ProcessServiceClient
		ws      string
		noReap  bool
	)

	BeforeEach(func() { noReap = false })
	JustBeforeEach(func() {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		signer, err = guestauth.NewSigner(key)
		Expect(err).NotTo(HaveOccurred())
		ws = GinkgoT().TempDir()
		f, err := guestfront.New(guestfront.Config{
			Workspace: ws, LogDir: realTemp(),
			Keys: []ed25519.PublicKey{signer.PublicKey()}, NoReap: noReap,
		})
		Expect(err).NotTo(HaveOccurred())
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		addr = lis.Addr().String()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- f.Serve(ctx, lis) }()
		DeferCleanup(func() {
			cancel()
			Eventually(done, racescale.D(15*time.Second)).Should(Receive(BeNil()))
		})
		conn, err = grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(conn.Close)
		process = ateenvv1alpha.NewProcessServiceClient(conn)
	})

	with := func(actor, prev string) context.Context {
		tok, err := signer.Sign(actor, prev)
		Expect(err).NotTo(HaveOccurred())
		return metadata.AppendToOutgoingContext(context.Background(), guestauth.Header, tok)
	}
	start := func(ctx context.Context, marker string) error {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, err := process.StartProcess(cctx, &ateenvv1alpha.StartProcessRequest{
			Command: []string{"/bin/sh", "-c", "touch " + marker},
		})
		return err
	}

	It("serves /readyz without a token", func() {
		resp, err := http.Get("http://" + addr + "/readyz")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(string(body)).To(Equal("ok\n"))
	})

	It("refuses StartProcess without a token and starts no process", func() {
		marker := filepath.Join(ws, "no-token")
		Expect(status.Code(start(context.Background(), marker))).To(Equal(codes.Unauthenticated))
		bad := metadata.AppendToOutgoingContext(context.Background(), guestauth.Header, "koinon_test_not-a-real-token")
		Expect(status.Code(start(bad, marker))).To(Equal(codes.Unauthenticated))
		Consistently(func() bool { _, err := os.Stat(marker); return err == nil }, 500*time.Millisecond).Should(BeFalse())
	})

	It("starts a process with a valid token and then refuses another sandbox's token", func() {
		ok := filepath.Join(ws, "ok")
		Expect(start(with(self, ""), ok)).To(Succeed())
		Eventually(func() error { _, err := os.Stat(ok); return err }, racescale.D(5*time.Second)).Should(Succeed())
		denied := filepath.Join(ws, "other")
		Expect(status.Code(start(with(other, ""), denied))).To(Equal(codes.Unauthenticated))
	})

	It("moves the pin on a restore claim that names the pin", func() {
		Expect(start(with(self, ""), filepath.Join(ws, "1"))).To(Succeed())
		Expect(status.Code(start(with(other, "org-9/x"), filepath.Join(ws, "2")))).To(Equal(codes.Unauthenticated))
		Expect(start(with(other, self), filepath.Join(ws, "3"))).To(Succeed())
		Expect(status.Code(start(with(self, ""), filepath.Join(ws, "4")))).To(Equal(codes.Unauthenticated))
	})

	It("answers Bind only with a token", func() {
		Expect(status.Code(conn.Invoke(context.Background(), guestauth.BindMethod, &emptypb.Empty{}, &emptypb.Empty{}))).
			To(Equal(codes.Unauthenticated))
		Expect(conn.Invoke(with(self, ""), guestauth.BindMethod, &emptypb.Empty{}, &emptypb.Empty{})).To(Succeed())
		Expect(status.Code(start(with(other, ""), filepath.Join(ws, "x")))).To(Equal(codes.Unauthenticated))
	})

	It("needs a token for reflection", func() {
		stream, err := reflectionpb.NewServerReflectionClient(conn).ServerReflectionInfo(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(stream.Send(&reflectionpb.ServerReflectionRequest{
			MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{},
		})).To(Or(Succeed(), MatchError(io.EOF)))
		_, err = stream.Recv()
		Expect(status.Code(err)).To(Equal(codes.Unauthenticated))
	})

	It("answers a plain HTTP request without a token with 401", func() {
		resp, err := http.Post("http://"+addr+"/ateenv.v1alpha.ProcessService/StartProcess", "text/plain", strings.NewReader("x"))
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	// runJob starts a job and waits for its exit. It returns the exit code.
	runJob := func(script string) int32 {
		p, err := process.StartProcess(with(self, ""), &ateenvv1alpha.StartProcessRequest{
			Command: []string{"/bin/sh", "-c", script},
		})
		Expect(err).NotTo(HaveOccurred())
		var code int32 = -1
		Eventually(func() ateenvv1alpha.ProcessState {
			got, err := process.GetProcess(with(self, ""), &ateenvv1alpha.GetProcessRequest{ProcessId: p.GetProcessId()})
			Expect(err).NotTo(HaveOccurred())
			code = got.GetExitCode()
			return got.GetState()
		}, racescale.D(10*time.Second)).Should(Equal(ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED))
		return code
	}

	Context("reaper", func() {
		BeforeEach(func() {
			if runtime.GOOS != "linux" {
				Skip("the reaper is Linux only")
			}
		})
		// The shell starts a short child in the background and exits at once with code 7. The child
		// goes to the front (a subreaper here, PID 1 in the guest). It exits 300 ms later and is a zombie
		// until the front reaps it.
		const orphanJob = "(sleep 0.3 &) ; exit 7"

		Context("on", func() {
			BeforeEach(func() { DeferCleanup(func() { setSubreaper(false) }) })

			It("sets the subreaper flag, reaps the orphan and keeps the exit status of the job", func() {
				Eventually(subreaperFlag, racescale.D(5*time.Second)).Should(BeTrue())
				Expect(runJob(orphanJob)).To(Equal(int32(7)))
				Eventually(zombieChildren, racescale.D(10*time.Second)).Should(BeEmpty())
			})

			It("keeps the exit status of every direct child while orphans are reaped at the same time", func() {
				var wg sync.WaitGroup
				codes := make([]int32, 40)
				slots := make(chan struct{}, 6) // the tracker allows 10 running processes
				for i := range codes {
					wg.Add(1)
					go func() {
						defer GinkgoRecover()
						defer wg.Done()
						slots <- struct{}{}
						defer func() { <-slots }()
						// An orphan and a direct child that exits with a known code.
						codes[i] = runJob("(true &) ; exit 3")
					}()
				}
				wg.Wait()
				for _, c := range codes {
					Expect(c).To(Equal(int32(3)))
				}
				Eventually(zombieChildren, racescale.D(10*time.Second)).Should(BeEmpty())
			})
		})

		Context("off (the control case: without the reaper the orphan stays a zombie)", func() {
			BeforeEach(func() {
				noReap = true
				setSubreaper(true) // the test process takes the place of PID 1
				DeferCleanup(func() {
					setSubreaper(false)
					for _, z := range zombieChildren() { // take what the spec left
						pid, _ := strconv.Atoi(z)
						var ws syscall.WaitStatus
						_, _ = syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
					}
				})
			})

			It("leaves the orphan as a zombie", func() {
				Expect(runJob(orphanJob)).To(Equal(int32(7)))
				Eventually(zombieChildren, racescale.D(10*time.Second)).ShouldNot(BeEmpty())
				Consistently(zombieChildren, 500*time.Millisecond).ShouldNot(BeEmpty())
			})
		})
	})

	It("refuses a request with two token headers", func() {
		tok, err := signer.Sign(self, "")
		Expect(err).NotTo(HaveOccurred())
		ctx := metadata.AppendToOutgoingContext(context.Background(), guestauth.Header, tok, guestauth.Header, tok)
		Expect(status.Code(start(ctx, filepath.Join(ws, "two")))).To(Equal(codes.Unauthenticated))
	})
})

var _ = Describe("Gate", func() {
	It("logs one line per interval about refused calls, with the number it left out", func() {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		var buf strings.Builder
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		v, err := guestauth.NewVerifier([]ed25519.PublicKey{pub})
		Expect(err).NotTo(HaveOccurred())
		h := guestfront.Gate(v, http.NotFoundHandler(), logger)
		for range 50 {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ateenv.v1alpha.ProcessService/StartProcess", nil))
			Expect(rec.Code).To(Equal(http.StatusUnauthorized))
		}
		Expect(strings.Count(buf.String(), "guest API call refused")).To(Equal(1))
	})
})

var _ = Describe("PrepareLogDir", func() {
	It("makes the directory with mode 0750", func() {
		d := filepath.Join(realTemp(), "a", "jobs")
		Expect(guestfront.PrepareLogDir(d)).To(Succeed())
		st, err := os.Stat(d)
		Expect(err).NotTo(HaveOccurred())
		Expect(st.Mode().Perm()&0o027).To(BeZero(), "no group write, no access for others")
	})
	It("refuses a symlink as the directory", func() {
		base := realTemp()
		Expect(os.Mkdir(filepath.Join(base, "target"), 0o755)).To(Succeed())
		Expect(os.Symlink(filepath.Join(base, "target"), filepath.Join(base, "jobs"))).To(Succeed())
		Expect(guestfront.PrepareLogDir(filepath.Join(base, "jobs"))).To(MatchError(ContainSubstring("without following a symlink")))
	})
	It("refuses a symlink in a parent part", func() {
		base := realTemp()
		Expect(os.Mkdir(filepath.Join(base, "target"), 0o755)).To(Succeed())
		Expect(os.Symlink(filepath.Join(base, "target"), filepath.Join(base, "link"))).To(Succeed())
		Expect(guestfront.PrepareLogDir(filepath.Join(base, "link", "jobs"))).To(MatchError(ContainSubstring("without following a symlink")))
	})
	It("refuses a directory that others can write", func() {
		d := filepath.Join(realTemp(), "jobs")
		Expect(os.Mkdir(d, 0o755)).To(Succeed())
		Expect(os.Chmod(d, 0o777)).To(Succeed())
		Expect(guestfront.PrepareLogDir(d)).To(MatchError(ContainSubstring("lets group or others write")))
	})
	It("refuses a parent that others can write, without the sticky bit", func() {
		base := realTemp()
		Expect(os.Mkdir(filepath.Join(base, "open"), 0o755)).To(Succeed())
		Expect(os.Chmod(filepath.Join(base, "open"), 0o777)).To(Succeed())
		Expect(guestfront.PrepareLogDir(filepath.Join(base, "open", "jobs"))).To(MatchError(ContainSubstring("lets group or others write")))
	})
	// Issue #1516: a directory made before the front starts, with mode 0755, would let the payload
	// user read the job logs on the gVisor tiers.
	DescribeTable("refuses a last part that gives others access or the group write",
		func(mode os.FileMode) {
			d := filepath.Join(realTemp(), "jobs")
			Expect(os.Mkdir(d, 0o700)).To(Succeed())
			Expect(os.Chmod(d, mode)).To(Succeed())
			Expect(guestfront.PrepareLogDir(d)).To(HaveOccurred())
		},
		Entry("0755", os.FileMode(0o755)),
		Entry("0751", os.FileMode(0o751)),
		Entry("0770", os.FileMode(0o770)),
		Entry("0704", os.FileMode(0o704)),
	)
	DescribeTable("accepts a last part of 0750 or stricter",
		func(mode os.FileMode) {
			d := filepath.Join(realTemp(), "jobs")
			Expect(os.Mkdir(d, 0o700)).To(Succeed())
			// BSD and macOS give a new directory the group of its parent; the front wants its own.
			Expect(os.Chown(d, -1, os.Getegid())).To(Succeed())
			Expect(os.Chmod(d, mode)).To(Succeed())
			Expect(guestfront.PrepareLogDir(d)).To(Succeed())
		},
		Entry("0750", os.FileMode(0o750)),
		Entry("0700", os.FileMode(0o700)),
		Entry("0550", os.FileMode(0o550)),
	)
	// The platform decides the group of a new directory (BSD, macOS and a setgid parent give it the group
	// of the parent), so the front sets its own group on a directory that it makes.
	It("sets its own group on a directory that it makes below a parent of another group", func() {
		if os.Geteuid() != 0 {
			Skip("changing the group of the parent needs root")
		}
		base := realTemp()
		Expect(os.Chown(base, 0, 12345)).To(Succeed())
		Expect(os.Chmod(base, 0o755|os.ModeSetgid)).To(Succeed())
		d := filepath.Join(base, "jobs")
		Expect(guestfront.PrepareLogDir(d)).To(Succeed())
		st, err := os.Stat(d)
		Expect(err).NotTo(HaveOccurred())
		Expect(st.Sys().(*syscall.Stat_t).Gid).To(BeEquivalentTo(os.Getegid()))
	})
	It("refuses a last part of another group", func() {
		if os.Geteuid() != 0 {
			Skip("changing the group needs root")
		}
		d := filepath.Join(realTemp(), "jobs")
		Expect(os.Mkdir(d, 0o750)).To(Succeed())
		Expect(os.Chown(d, 0, 12345)).To(Succeed())
		Expect(guestfront.PrepareLogDir(d)).To(MatchError(ContainSubstring("has gid 12345")))
	})
	It("refuses the root directory with a clear message", func() {
		Expect(guestfront.PrepareLogDir("/")).To(MatchError(ContainSubstring("is the root directory")))
		Expect(guestfront.PrepareLogDir("//")).To(MatchError(ContainSubstring("is the root directory")))
	})
	It("refuses a file and a relative path", func() {
		f := filepath.Join(realTemp(), "file")
		Expect(os.WriteFile(f, nil, 0o600)).To(Succeed())
		Expect(guestfront.PrepareLogDir(f)).To(MatchError(ContainSubstring("not a directory")))
		Expect(guestfront.PrepareLogDir("jobs")).To(MatchError(ContainSubstring("not an absolute path")))
	})
})

// realTemp is a temporary directory whose path has no symlink.
func realTemp() string {
	d, err := filepath.EvalSymlinks(GinkgoT().TempDir())
	Expect(err).NotTo(HaveOccurred())
	return d
}

// zombieChildren lists the zombie children of the test process (the front is a subreaper here).
func zombieChildren() []string {
	ents, err := os.ReadDir("/proc")
	Expect(err).NotTo(HaveOccurred())
	var out []string
	for _, e := range ents {
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(b)
		f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(f) > 1 && f[0] == "Z" && f[1] == strconv.Itoa(os.Getpid()) {
			out = append(out, e.Name())
		}
	}
	return out
}
