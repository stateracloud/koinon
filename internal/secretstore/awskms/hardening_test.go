// SPDX-License-Identifier: Apache-2.0

package awskms_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/awskms"
)

// refreshWait is how long a spec waits for a background credential refresh. The clock is the
// test's, so the refresh is due at once; what the spec waits for is a goroutine and one HTTP round
// trip, which a loaded ginkgo -p runner can delay past Gomega's 1 s default. It is a failure
// ceiling: a spec returns as soon as the refresh is done.
const refreshWait = 30 * time.Second

const fakeKey = "arn:aws:kms:us-east-1:111122223333:key/0b1d2e3f-4a5b-4c6d-8e9f-a0b1c2d3e4f5"

// creds are obviously fake temporary credentials named n.
func creds(n string) awskms.Credentials {
	return awskms.Credentials{
		AccessKeyID:     "koinon_test_not-a-real-key-id-" + n,
		SecretAccessKey: "koinon_test_not-a-real-secret-" + n,
		SessionToken:    "koinon_test_not-a-real-session-token-" + n,
	}
}

// fakeKMS is a KMS that checks rights by access key ID, as IAM would for each role (LocalStack
// does not enforce IAM), and the shape of every request. Its ciphertexts are handles. Every role's
// policy carries the condition kms:EncryptionContext:koinon = kek-wrap/v1.
//
// It answers a plain Decrypt of something that is no ciphertext of it with InvalidCiphertextException
// before it checks the right, as AWS KMS may: only a dry run proves the right. A dry run with
// IGNORE_CIPHERTEXT is answered as AWS documents it: the authorization is checked, the ciphertext
// is not, and an allowed call is DryRunOperationException.
type fakeKMS struct {
	mu     sync.Mutex
	rights map[string][]string // access key ID: the operations it may call
	stale  map[string]bool     // access key IDs answered ExpiredTokenException
	blobs  map[string][]byte
	calls  atomic.Int64
	// dryRun is how it treats DryRun: "" as AWS, "ignore" as if absent (an emulator), "reject"
	// with a ValidationException naming it.
	dryRun string
	// bodies are the Decrypt request bodies it received.
	bodies []map[string]any
}

var credentialPattern = regexp.MustCompile(`^AWS4-HMAC-SHA256 Credential=([^/]+)/[0-9]{8}/us-east-1/kms/aws4_request, SignedHeaders=content-type;host;x-amz-date;x-amz-security-token;x-amz-target, Signature=[0-9a-f]{64}$`)

func (f *fakeKMS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	fail := func(status int, typ, msg string) {
		w.Header().Set("X-Amzn-ErrorType", typ)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"__type": typ, "message": msg})
	}
	m := credentialPattern.FindStringSubmatch(r.Header.Get("Authorization"))
	if m == nil || r.Header.Get("X-Amz-Security-Token") == "" || r.Header.Get("Content-Type") != "application/x-amz-json-1.1" {
		fail(http.StatusBadRequest, "IncompleteSignatureException", "bad request")
		return
	}
	var in struct {
		KeyID             string            `json:"KeyId"`
		Plaintext         []byte            `json:"Plaintext"`
		CiphertextBlob    []byte            `json:"CiphertextBlob"`
		EncryptionContext map[string]string `json:"EncryptionContext"`
		DryRun            bool              `json:"DryRun"`
		DryRunModifiers   []string          `json:"DryRunModifiers"`
	}
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &in)
	op := r.Header.Get("X-Amz-Target")
	f.mu.Lock()
	defer f.mu.Unlock()
	if op == "TrentService.Decrypt" {
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.bodies = append(f.bodies, body)
	}
	dryRun := in.DryRun && f.dryRun == ""
	_, known := f.blobs[string(in.CiphertextBlob)]
	switch {
	case f.dryRun == "reject" && (in.DryRun || len(in.DryRunModifiers) > 0):
		fail(http.StatusBadRequest, "ValidationException", "1 validation error detected: unknown parameter DryRun")
	case f.stale[m[1]]:
		fail(http.StatusBadRequest, "ExpiredTokenException", "The security token included in the request is expired")
	case op == "TrentService.Decrypt" && !dryRun && !known:
		fail(http.StatusBadRequest, "InvalidCiphertextException", "")
	case !slices.Contains(f.rights[m[1]], op) || (dryRun && in.EncryptionContext["koinon"] != "kek-wrap/v1"):
		fail(http.StatusBadRequest, "AccessDeniedException", fmt.Sprintf("User: arn:aws:sts::111122223333:assumed-role/koinon/koinon is not authorized to perform: kms:%s on resource: %s", op, fakeKey))
	case in.KeyID != fakeKey:
		fail(http.StatusBadRequest, "NotFoundException", "Key '"+in.KeyID+"' does not exist")
	case dryRun && slices.Contains(in.DryRunModifiers, "IGNORE_CIPHERTEXT"):
		fail(http.StatusBadRequest, "DryRunOperationException", "The request would have succeeded, but the DryRun option is set.")
	case in.EncryptionContext["koinon"] != "kek-wrap/v1":
		fail(http.StatusBadRequest, "InvalidCiphertextException", "")
	case op == "TrentService.Encrypt":
		h := fmt.Sprintf("koinon-test-blob-%d", len(f.blobs))
		f.blobs[h] = in.Plaintext
		_ = json.NewEncoder(w).Encode(map[string]any{"CiphertextBlob": []byte(h), "KeyId": fakeKey})
	case op == "TrentService.Decrypt":
		p, ok := f.blobs[string(in.CiphertextBlob)]
		if !ok {
			fail(http.StatusBadRequest, "InvalidCiphertextException", "")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Plaintext": p, "KeyId": fakeKey})
	default:
		fail(http.StatusBadRequest, "UnknownOperationException", "")
	}
}

func newFakeKMS() (*fakeKMS, string) {
	GinkgoHelper()
	f := &fakeKMS{
		rights: map[string][]string{
			creds("admin").AccessKeyID:   {"TrentService.Encrypt", "TrentService.Decrypt"},
			creds("decrypt").AccessKeyID: {"TrentService.Decrypt"},
		},
		stale: map[string]bool{},
		blobs: map[string][]byte{},
	}
	s := httptest.NewServer(f)
	DeferCleanup(s.Close)
	return f, s.URL
}

// lockedBuffer is a log destination that the adapter's goroutines may write while a spec reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// fixed fetches c, counting the fetches.
func fixed(c awskms.Credentials, n *atomic.Int64) func(context.Context) (awskms.Credentials, error) {
	return func(context.Context) (awskms.Credentials, error) {
		if n != nil {
			n.Add(1)
		}
		return c, nil
	}
}

func onFake(endpoint string, fetch func(context.Context) (awskms.Credentials, error)) *awskms.Wrapper {
	GinkgoHelper()
	w, err := awskms.NewWithCredentials(awskms.Config{Region: "us-east-1", KeyID: fakeKey, Endpoint: endpoint}, fetch)
	Expect(err).NotTo(HaveOccurred())
	return w
}

// answering is a KMS stand-in that answers every request with status, headers and body.
func answering(status int, header map[string]string, body string) string {
	GinkgoHelper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	DeferCleanup(s.Close)
	return s.URL
}

// noLeak fails when err quotes anything of the credentials c.
func noLeak(err error, c awskms.Credentials) {
	GinkgoHelper()
	for _, s := range []string{c.AccessKeyID, c.SecretAccessKey, c.SessionToken, "not-a-real"} {
		Expect(err.Error()).NotTo(ContainSubstring(s))
	}
}

var _ = Describe("rights", func() {
	kek := bytes.Repeat([]byte("k"), secretstore.KeySize) // obviously fake
	aad := []byte("koinon-test-aad")

	It("lets a decrypt-only identity unwrap and probe, and refuses its wrap as unreachable", func(ctx SpecContext) {
		_, url := newFakeKMS()
		wrapped, err := onFake(url, fixed(creds("admin"), nil)).Wrap(ctx, kek, aad)
		Expect(err).NotTo(HaveOccurred())
		ro := onFake(url, fixed(creds("decrypt"), nil))
		got, err := ro.Unwrap(ctx, wrapped, aad)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(kek))
		Expect(ro.Probe(ctx)).To(Succeed())
		_, err = ro.Wrap(ctx, kek, aad)
		Expect(err).To(MatchError(secretstore.ErrUnreachable))
		Expect(err.Error()).To(ContainSubstring("AccessDeniedException"))
		noLeak(err, creds("decrypt"))
	})

	It("reports an identity without any right as unreachable, also on probe", func(ctx SpecContext) {
		_, url := newFakeKMS()
		wrapped, err := onFake(url, fixed(creds("admin"), nil)).Wrap(ctx, kek, aad)
		Expect(err).NotTo(HaveOccurred())
		w := onFake(url, fixed(creds("nobody"), nil))
		err = w.Probe(ctx)
		Expect(err).To(MatchError(secretstore.ErrUnreachable))
		Expect(err.Error()).To(ContainSubstring("AccessDeniedException"))
		_, err = w.Unwrap(ctx, wrapped, aad)
		Expect(err).To(MatchError(secretstore.ErrUnreachable))
		Expect(err).NotTo(MatchError(secretstore.ErrOpen))
	})

	It("probes with a dry run that ignores the ciphertext, with the key and encryption context", func(ctx SpecContext) {
		f, url := newFakeKMS()
		Expect(onFake(url, fixed(creds("decrypt"), nil)).Probe(ctx)).To(Succeed())
		Expect(f.bodies).To(HaveLen(1))
		Expect(f.bodies[0]).To(HaveKeyWithValue("DryRun", true))
		Expect(f.bodies[0]).To(HaveKeyWithValue("DryRunModifiers", []any{"IGNORE_CIPHERTEXT"}))
		Expect(f.bodies[0]).To(HaveKeyWithValue("KeyId", fakeKey))
		Expect(f.bodies[0]).To(HaveKeyWithValue("EncryptionContext", map[string]any{"koinon": "kek-wrap/v1"}))
	})

	// KMS may refuse a junk ciphertext before it checks the right: a plain Decrypt of one would
	// call a role without kms:Decrypt healthy.
	It("calls a role without kms:Decrypt unhealthy although KMS refuses junk before the right", func(ctx SpecContext) {
		f, url := newFakeKMS()
		f.rights[creds("encrypt-only").AccessKeyID] = []string{"TrentService.Encrypt"}
		for _, who := range []string{"nobody", "encrypt-only"} {
			_, err := onFake(url, fixed(creds(who), nil)).Unwrap(ctx, []byte("koinon-test-junk"), aad)
			Expect(err).To(MatchError(secretstore.ErrOpen), "the fake refuses junk first, as KMS may")
			err = onFake(url, fixed(creds(who), nil)).Probe(ctx)
			Expect(err).To(MatchError(secretstore.ErrUnreachable), who)
			Expect(err.Error()).To(ContainSubstring("AccessDeniedException"), who)
		}
		for _, b := range f.bodies {
			if b["DryRun"] == nil {
				Expect(b["CiphertextBlob"]).NotTo(Equal("a29pbm9uLXByb2Jl"), "the probe never sends a plain Decrypt")
			}
		}
	})

	// An endpoint without the dry run cannot prove the decrypt right: the probe calls it unreachable
	// and says why, with no weaker fallback, so a role without kms:Decrypt is never healthy there.
	Describe("an endpoint without the dry run", func() {
		It("is unreachable, the right unverified, when it ignores DryRun", func(ctx SpecContext) {
			f, url := newFakeKMS()
			f.dryRun = "ignore"
			for _, who := range []string{"decrypt", "nobody"} {
				err := onFake(url, fixed(creds(who), nil)).Probe(ctx)
				Expect(err).To(MatchError(secretstore.ErrUnreachable), who)
				Expect(err).To(MatchError(awskms.ErrDryRunUnverified), who)
				Expect(err.Error()).To(ContainSubstring("the endpoint ignored or rejected DryRun; the decrypt right is unverified"), who)
				Expect(err.Error()).To(ContainSubstring("InvalidCiphertextException"), who)
			}
			Expect(f.bodies).To(HaveLen(2), "one dry run per probe, never a plain Decrypt after it")
			for _, b := range f.bodies {
				Expect(b).To(HaveKeyWithValue("DryRun", true))
			}
		})

		It("is unreachable, the right unverified, when it rejects DryRun", func(ctx SpecContext) {
			f, url := newFakeKMS()
			f.dryRun = "reject"
			err := onFake(url, fixed(creds("decrypt"), nil)).Probe(ctx)
			Expect(err).To(MatchError(secretstore.ErrUnreachable))
			Expect(err).To(MatchError(awskms.ErrDryRunUnverified))
			Expect(err.Error()).To(ContainSubstring("ValidationException"))
			Expect(f.bodies).To(HaveLen(1), "no retry without DryRun")
		})

		It("reports an access denial as such, not as an unverified right", func(ctx SpecContext) {
			f, url := newFakeKMS()
			err := onFake(url, fixed(creds("nobody"), nil)).Probe(ctx)
			Expect(err).To(MatchError(secretstore.ErrUnreachable))
			Expect(err).NotTo(MatchError(awskms.ErrDryRunUnverified))
			Expect(err.Error()).To(ContainSubstring("AccessDeniedException"))
			Expect(f.bodies).To(HaveLen(1))
		})
	})

	It("refreshes credentials KMS calls expired once, and only once", func(ctx SpecContext) {
		f, url := newFakeKMS()
		f.stale[creds("admin").AccessKeyID] = true
		f.rights[creds("fresh").AccessKeyID] = []string{"TrentService.Decrypt"}
		var n atomic.Int64
		w := onFake(url, func(context.Context) (awskms.Credentials, error) {
			if n.Add(1) == 1 {
				return creds("admin"), nil
			}
			return creds("fresh"), nil
		})
		Expect(w.Probe(ctx)).To(Succeed())
		Expect(n.Load()).To(BeEquivalentTo(2))

		f.stale[creds("fresh").AccessKeyID] = true
		n.Store(0)
		w = onFake(url, fixed(creds("fresh"), &n))
		err := w.Probe(ctx)
		Expect(err).To(MatchError(secretstore.ErrUnreachable))
		Expect(err.Error()).To(ContainSubstring("ExpiredTokenException"))
		Expect(n.Load()).To(BeEquivalentTo(2), "one refresh, then it gives up")
	})
})

var _ = Describe("what an answer means", func() {
	aad := []byte("koinon-test-aad")
	kmsJSON := map[string]string{"Content-Type": "application/x-amz-json-1.1"}
	kmsErr := func(typ string) string { return `{"__type":"` + typ + `","message":"koinon test"}` }

	// Only KMS's own refusal of the ciphertext is ErrOpen, which callers may treat as final; a
	// proxy's 400 page or another KMS error is not. A healthy probe is exactly the dry run's
	// DryRunOperationException; InvalidCiphertextException to the dry run is an endpoint that
	// ignored it, which leaves the decrypt right unverified.
	DescribeTable("an answer to decrypt", func(status int, header map[string]string, body string, open, healthy bool) {
		ctx := context.Background()
		w := onFake(answering(status, header, body), fixed(creds("admin"), nil))
		_, err := w.Unwrap(ctx, []byte("koinon-test-blob"), aad)
		if open {
			Expect(err).To(MatchError(secretstore.ErrOpen))
		} else {
			Expect(err).To(MatchError(secretstore.ErrUnreachable))
			Expect(err).NotTo(MatchError(secretstore.ErrOpen))
		}
		if healthy {
			Expect(w.Probe(ctx)).To(Succeed())
		} else {
			Expect(w.Probe(ctx)).To(MatchError(secretstore.ErrUnreachable))
		}
	},
		Entry("the dry run's yes", 400, kmsJSON, kmsErr("DryRunOperationException"), false, true),
		Entry("a modified ciphertext (an endpoint that ignores the dry run)", 400, kmsJSON, kmsErr("InvalidCiphertextException"), true, false),
		Entry("a namespaced error type", 400, kmsJSON, kmsErr("com.amazonaws.kms#InvalidCiphertextException"), true, false),
		Entry("another key's ciphertext", 400, kmsJSON, kmsErr("IncorrectKeyException"), true, false),
		Entry("a proxy's HTML page", 400, nil, `<html><body>400 Bad Request</body></html>`, false, false),
		Entry("an empty body", 400, nil, ``, false, false),
		Entry("only the error header", 400, map[string]string{"X-Amzn-ErrorType": "InvalidCiphertextException"}, `oops`, false, false),
		Entry("a refused right", 400, kmsJSON, kmsErr("AccessDeniedException"), false, false),
		Entry("the missing key", 400, kmsJSON, kmsErr("NotFoundException"), false, false),
		Entry("a disabled key", 400, kmsJSON, kmsErr("DisabledException"), false, false),
		Entry("a key pending deletion", 400, kmsJSON, kmsErr("KMSInvalidStateException"), false, false),
		Entry("a key not for encryption", 400, kmsJSON, kmsErr("InvalidKeyUsageException"), false, false),
		Entry("throttling", 400, kmsJSON, kmsErr("ThrottlingException"), false, false),
		Entry("a validation error", 400, kmsJSON, kmsErr("ValidationException"), false, false),
		Entry("a validation error naming DryRun, twice", 400, kmsJSON, `{"__type":"ValidationException","message":"unknown parameter DryRun"}`, false, false),
		Entry("a refused right naming DryRun", 400, kmsJSON, `{"__type":"AccessDeniedException","message":"not authorized to perform kms:Decrypt with DryRun"}`, false, false),
		Entry("an unknown client", 400, kmsJSON, kmsErr("UnrecognizedClientException"), false, false),
		Entry("the ciphertext refusal with a 500", 500, kmsJSON, kmsErr("InvalidCiphertextException"), false, false),
		Entry("an internal error", 500, kmsJSON, kmsErr("KMSInternalException"), false, false),
		Entry("a dependency timeout", 503, kmsJSON, kmsErr("DependencyTimeoutException"), false, false),
		Entry("a 403 page", 403, nil, `Forbidden`, false, false),
		Entry("a 200 that is not KMS's", 200, nil, `<html>welcome</html>`, false, false),
	)

	It("never quotes a credential that KMS's message holds", func(ctx SpecContext) {
		c := creds("admin")
		for _, body := range []string{
			`{"__type":"InvalidSignatureException","message":"The Canonical String for this request should have been 'x-amz-security-token:` + c.SessionToken + `'"}`,
			`{"__type":"AccessDeniedException","message":"key ` + c.AccessKeyID + ` may not"}`,
			`{"__type":"ThrottlingException","message":"token ` + c.SessionToken + `"}`,
		} {
			w := onFake(answering(400, kmsJSON, body), fixed(c, nil))
			err := w.Probe(ctx)
			Expect(err).To(MatchError(secretstore.ErrUnreachable))
			noLeak(err, c)
		}
	})

	Describe("a redirect", func() {
		// elsewhere is where a redirect points: it must never be called.
		var elsewhere *httptest.Server
		var called atomic.Int64
		BeforeEach(func() {
			called.Store(0)
			elsewhere = httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Add(1) }))
			DeferCleanup(elsewhere.Close)
		})
		redirecting := func() string {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
			}))
			DeferCleanup(s.Close)
			return s.URL
		}

		It("is not followed on decrypt, which would re-send a signed request", func(ctx SpecContext) {
			w := onFake(redirecting(), fixed(creds("admin"), nil))
			_, err := w.Unwrap(ctx, []byte("koinon-test-blob"), aad)
			Expect(err).To(MatchError(secretstore.ErrUnreachable))
			Expect(err.Error()).To(ContainSubstring("redirect refused"))
			Expect(w.Probe(ctx)).To(MatchError(ContainSubstring("redirect refused")))
			Expect(called.Load()).To(BeZero())
		})

		It("is not followed on the STS sign-in, which would re-send the web identity token", func(ctx SpecContext) {
			p := filepath.Join(GinkgoT().TempDir(), "token")
			Expect(os.WriteFile(p, []byte("koinon_test_not-a-real-jwt"), 0o600)).To(Succeed())
			w, err := awskms.NewAnyHTTP(awskms.Config{
				Region: "us-east-1", KeyID: fakeKey, Endpoint: redirecting(), STSEndpoint: redirecting(),
				Identity: awskms.Identity{RoleARN: "arn:aws:iam::111122223333:role/koinon-api", WebIdentityTokenFile: p},
			})
			Expect(err).NotTo(HaveOccurred())
			err = w.Probe(ctx)
			Expect(err).To(MatchError(secretstore.ErrUnreachable))
			Expect(err.Error()).To(ContainSubstring("redirect refused"))
			Expect(called.Load()).To(BeZero())
		})

		It("is not followed by the Pod Identity request, which would re-send the pod's token", func(ctx SpecContext) {
			p := filepath.Join(GinkgoT().TempDir(), "token")
			Expect(os.WriteFile(p, []byte("koinon_test_not-a-real-jwt"), 0o600)).To(Succeed())
			w, err := awskms.NewAnyHTTP(awskms.Config{
				Region: "us-east-1", KeyID: fakeKey, Endpoint: redirecting(),
				Identity: awskms.Identity{CredentialsURI: redirecting() + "/v1/credentials", CredentialsTokenFile: p},
			})
			Expect(err).NotTo(HaveOccurred())
			err = w.Probe(ctx)
			Expect(err).To(MatchError(secretstore.ErrUnreachable))
			Expect(err.Error()).To(ContainSubstring("redirect refused"))
			Expect(called.Load()).To(BeZero())
		})
	})
})

var _ = Describe("credentials under concurrency", func() {
	kek := bytes.Repeat([]byte("k"), secretstore.KeySize) // obviously fake
	aad := []byte("koinon-test-aad")

	It("fetches once for many concurrent Wrap, Unwrap and Probe calls", func(ctx SpecContext) {
		_, url := newFakeKMS()
		wrapped, err := onFake(url, fixed(creds("admin"), nil)).Wrap(ctx, kek, aad)
		Expect(err).NotTo(HaveOccurred())
		var n atomic.Int64
		w := onFake(url, func(context.Context) (awskms.Credentials, error) {
			n.Add(1)
			time.Sleep(50 * time.Millisecond) // a fetch that takes a while: the others wait for it
			return creds("admin"), nil
		})
		var wg sync.WaitGroup
		errs := make(chan error, 30)
		for i := range 30 {
			wg.Go(func() {
				var err error
				switch i % 3 {
				case 0:
					_, err = w.Wrap(ctx, kek, aad)
				case 1:
					var got []byte
					got, err = w.Unwrap(ctx, wrapped, aad)
					if err == nil && !bytes.Equal(got, kek) {
						err = secretstore.ErrOpen
					}
				default:
					err = w.Probe(ctx)
				}
				errs <- err
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(n.Load()).To(BeEquivalentTo(1))
	})

	It("lets each caller stop waiting for a fetch on its own context, without failing the others", func(ctx SpecContext) {
		_, url := newFakeKMS()
		release := make(chan struct{})
		var n atomic.Int64
		w := onFake(url, func(context.Context) (awskms.Credentials, error) {
			n.Add(1)
			<-release
			return creds("decrypt"), nil
		})
		short, cancel := context.WithCancel(ctx)
		first := make(chan error, 1)
		go func() { first <- w.Probe(short) }()
		Eventually(n.Load).Should(BeEquivalentTo(1))
		second := make(chan error, 1)
		go func() { second <- w.Probe(ctx) }()
		cancel()
		Eventually(first).Should(Receive(MatchError(context.Canceled)))
		Consistently(second, 100*time.Millisecond).ShouldNot(Receive())
		close(release)
		Eventually(second).Should(Receive(BeNil()))
		Expect(n.Load()).To(BeEquivalentTo(1))
	})
})

var _ = Describe("pod identity", func() {
	// clock is a settable time, so expiry needs no sleeping.
	type clock struct {
		mu  sync.Mutex
		now time.Time
	}
	newClock := func() *clock { return &clock{now: time.Now()} }
	read := func(c *clock) func() time.Time {
		return func() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
	}
	advance := func(c *clock, d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

	tokenFile := func(jwt string) string {
		GinkgoHelper()
		p := filepath.Join(GinkgoT().TempDir(), "token")
		Expect(os.WriteFile(p, []byte(jwt+"\n"), 0o600)).To(Succeed())
		return p
	}

	Describe("IRSA", func() {
		// fakeSTS answers AssumeRoleWithWebIdentity for one role with credentials valid for an hour
		// of clk, recording the tokens it was given.
		fakeSTS := func(clk *clock, tokens *[]string, mu *sync.Mutex) string {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				mu.Lock()
				*tokens = append(*tokens, r.PostForm.Get("WebIdentityToken"))
				mu.Unlock()
				if r.PostForm.Get("Action") != "AssumeRoleWithWebIdentity" || r.PostForm.Get("RoleArn") != "arn:aws:iam::111122223333:role/koinon-api" ||
					r.PostForm.Get("RoleSessionName") != "koinon" || r.Header.Get("Authorization") != "" {
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`<ErrorResponse><Error><Type>Sender</Type><Code>AccessDenied</Code><Message>Not authorized to perform sts:AssumeRoleWithWebIdentity</Message></Error></ErrorResponse>`))
					return
				}
				c := creds("decrypt")
				_, _ = fmt.Fprintf(w, `<AssumeRoleWithWebIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleWithWebIdentityResult><Credentials><AccessKeyId>%s</AccessKeyId><SecretAccessKey>%s</SecretAccessKey><SessionToken>%s</SessionToken><Expiration>%s</Expiration></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`,
					c.AccessKeyID, c.SecretAccessKey, c.SessionToken, read(clk)().Add(time.Hour).UTC().Format(time.RFC3339))
			}))
			DeferCleanup(s.Close)
			return s.URL
		}

		It("trades the token for credentials, reuses them, and reads the rotated token on refresh", func(ctx SpecContext) {
			f, kmsURL := newFakeKMS()
			clk := newClock()
			var tokens []string
			var mu sync.Mutex
			p := tokenFile("koinon_test_not-a-real-jwt-1")
			w, err := awskms.NewAnyHTTP(awskms.Config{
				Region: "us-east-1", KeyID: fakeKey, Endpoint: kmsURL, STSEndpoint: fakeSTS(clk, &tokens, &mu),
				Identity: awskms.Identity{RoleARN: "arn:aws:iam::111122223333:role/koinon-api", WebIdentityTokenFile: p},
			})
			Expect(err).NotTo(HaveOccurred())
			w.SetClock(read(clk))
			Expect(w.Probe(ctx)).To(Succeed())
			Expect(w.Probe(ctx)).To(Succeed())
			Expect(os.WriteFile(p, []byte("koinon_test_not-a-real-jwt-2"), 0o600)).To(Succeed())
			advance(clk, 40*time.Minute)
			Expect(w.Probe(ctx)).To(Succeed())
			advance(clk, 10*time.Minute) // past 4/5 of the hour: refreshed in the background
			Expect(w.Probe(ctx)).To(Succeed())
			Eventually(func() []string {
				mu.Lock()
				defer mu.Unlock()
				return slices.Clone(tokens)
			}).WithTimeout(refreshWait).Should(Equal([]string{"koinon_test_not-a-real-jwt-1", "koinon_test_not-a-real-jwt-2"}))
			Expect(f.calls.Load()).To(BeEquivalentTo(4))
		})

		It("reports a refused role and a missing token file as unreachable, without the token", func(ctx SpecContext) {
			_, kmsURL := newFakeKMS()
			var tokens []string
			var mu sync.Mutex
			sts := fakeSTS(newClock(), &tokens, &mu)
			w, err := awskms.NewAnyHTTP(awskms.Config{
				Region: "us-east-1", KeyID: fakeKey, Endpoint: kmsURL, STSEndpoint: sts,
				Identity: awskms.Identity{RoleARN: "arn:aws:iam::111122223333:role/koinon-other", WebIdentityTokenFile: tokenFile("koinon_test_not-a-real-jwt")},
			})
			Expect(err).NotTo(HaveOccurred())
			err = w.Probe(ctx)
			Expect(err).To(MatchError(secretstore.ErrUnreachable))
			Expect(err.Error()).To(ContainSubstring("AccessDenied"))
			Expect(err.Error()).NotTo(ContainSubstring("not-a-real-jwt"))

			w, err = awskms.NewAnyHTTP(awskms.Config{
				Region: "us-east-1", KeyID: fakeKey, Endpoint: kmsURL, STSEndpoint: sts,
				Identity: awskms.Identity{RoleARN: "arn:aws:iam::111122223333:role/koinon-api", WebIdentityTokenFile: filepath.Join(GinkgoT().TempDir(), "absent")},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(w.Probe(ctx)).To(MatchError(secretstore.ErrUnreachable))
		})
	})

	Describe("EKS Pod Identity", func() {
		// fakeAgent serves credentials to the pod that presents the current token.
		fakeAgent := func(clk *clock, want *atomic.Value, seen *atomic.Int64) string {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Counted when the handler returns, not when it starts: the response is only sent then, so
				// a test that waits for seen has seen the token checked against the current want, and
				// cannot change want or the clock under a request that is still being served.
				defer seen.Add(1)
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != want.Load().(string) {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"code":"InvalidTokenException","message":"Service account token cannot be empty or invalid"}`))
					return
				}
				c := creds("decrypt")
				_ = json.NewEncoder(w).Encode(map[string]string{
					"AccessKeyId": c.AccessKeyID, "SecretAccessKey": c.SecretAccessKey, "Token": c.SessionToken,
					"AccountId": "111122223333", "Expiration": read(clk)().Add(6 * time.Hour).UTC().Format(time.RFC3339),
				})
			}))
			DeferCleanup(s.Close)
			return s.URL + "/v1/credentials"
		}

		It("presents the token file, and reads it again on refresh", func(ctx SpecContext) {
			_, kmsURL := newFakeKMS()
			clk := newClock()
			var want atomic.Value
			var seen atomic.Int64
			want.Store("koinon_test_not-a-real-jwt-1")
			p := tokenFile("koinon_test_not-a-real-jwt-1")
			w, err := awskms.NewAnyHTTP(awskms.Config{
				Region: "us-east-1", KeyID: fakeKey, Endpoint: kmsURL,
				Identity: awskms.Identity{CredentialsURI: fakeAgent(clk, &want, &seen), CredentialsTokenFile: p},
			})
			Expect(err).NotTo(HaveOccurred())
			w.SetClock(read(clk))
			Expect(w.Probe(ctx)).To(Succeed())
			Expect(w.Probe(ctx)).To(Succeed())
			Expect(seen.Load()).To(BeEquivalentTo(1))

			want.Store("koinon_test_not-a-real-jwt-2")
			Expect(os.WriteFile(p, []byte("koinon_test_not-a-real-jwt-2"), 0o600)).To(Succeed())
			advance(clk, 5*time.Hour) // past 4/5 of six hours: refreshed in the background
			Expect(w.Probe(ctx)).To(Succeed())
			Eventually(seen.Load).WithTimeout(refreshWait).Should(BeEquivalentTo(2))
			Expect(w.Probe(ctx)).To(Succeed())

			// The agent now refuses the token: the call after the 4/5 mark fails only once the
			// credentials have expired (less a minute).
			logged := &lockedBuffer{}
			w.SetLogger(slog.New(slog.NewTextHandler(logged, nil)))
			want.Store("koinon_test_not-a-real-jwt-3")
			advance(clk, 5*time.Hour) // 10 h: past 4/5 of the credentials of 5 h, which expire at 11 h
			Expect(w.Probe(ctx)).To(Succeed(), "a failed refresh leaves the current credentials in use")
			Eventually(logged.String).WithTimeout(refreshWait).Should(ContainSubstring("credential refresh failed"))
			Expect(logged.String()).To(ContainSubstring("InvalidTokenException"))
			Expect(logged.String()).NotTo(ContainSubstring("not-a-real"))
			Expect(w.Probe(ctx)).To(Succeed())
			advance(clk, 59*time.Minute+30*time.Second) // within a minute of their expiry
			err = w.Probe(ctx)
			Expect(err).To(MatchError(secretstore.ErrUnreachable))
			Expect(err.Error()).To(ContainSubstring("InvalidTokenException"))
			Expect(err.Error()).NotTo(ContainSubstring("not-a-real-jwt"))
		})

		It("tries a failed refresh again at most once per 10 s while the credentials are usable", func(ctx SpecContext) {
			_, kmsURL := newFakeKMS()
			clk := newClock()
			var want atomic.Value
			var seen atomic.Int64
			want.Store("koinon_test_not-a-real-jwt-1")
			w, err := awskms.NewAnyHTTP(awskms.Config{
				Region: "us-east-1", KeyID: fakeKey, Endpoint: kmsURL,
				Identity: awskms.Identity{CredentialsURI: fakeAgent(clk, &want, &seen), CredentialsTokenFile: tokenFile("koinon_test_not-a-real-jwt-1")},
			})
			Expect(err).NotTo(HaveOccurred())
			w.SetClock(read(clk))
			logged := &lockedBuffer{}
			w.SetLogger(slog.New(slog.NewTextHandler(logged, nil)))
			Expect(w.Probe(ctx)).To(Succeed())
			Expect(seen.Load()).To(BeEquivalentTo(1))

			// The agent is down for this pod from here on.
			want.Store("koinon_test_not-a-real-jwt-other")
			failed := func() int { return strings.Count(logged.String(), "credential refresh failed") }
			advance(clk, 5*time.Hour) // past 4/5 of six hours
			Expect(w.Probe(ctx)).To(Succeed())
			Eventually(failed).WithTimeout(refreshWait).Should(Equal(1))
			for range 20 {
				Expect(w.Probe(ctx)).To(Succeed())
			}
			advance(clk, 9*time.Second)
			Expect(w.Probe(ctx)).To(Succeed())
			Consistently(seen.Load, 200*time.Millisecond).Should(BeEquivalentTo(2), "no new attempt within 10 s of the failed one")
			Expect(failed()).To(Equal(1))

			advance(clk, time.Second)
			Expect(w.Probe(ctx)).To(Succeed())
			Eventually(failed).WithTimeout(refreshWait).Should(Equal(2))
			Expect(seen.Load()).To(BeEquivalentTo(3))
			Expect(w.Probe(ctx)).To(Succeed())
			Consistently(seen.Load, 200*time.Millisecond).Should(BeEquivalentTo(3))
		})
	})
})

var _ = Describe("configuration", func() {
	irsa := awskms.Identity{RoleARN: "arn:aws:iam::111122223333:role/koinon-api", WebIdentityTokenFile: "/var/run/secrets/eks.amazonaws.com/serviceaccount/token"}
	pod := awskms.Identity{CredentialsURI: "http://169.254.170.23/v1/credentials", CredentialsTokenFile: "/var/run/secrets/pods.eks.amazonaws.com/serviceaccount/eks-pod-identity-token"}
	cfg := func(region, key string, mod ...func(*awskms.Config)) awskms.Config {
		c := awskms.Config{Region: region, KeyID: key, Identity: irsa}
		for _, m := range mod {
			m(&c)
		}
		return c
	}
	endpoint := func(e string) func(*awskms.Config) { return func(c *awskms.Config) { c.Endpoint = e } }
	identity := func(id awskms.Identity) func(*awskms.Config) { return func(c *awskms.Config) { c.Identity = id } }

	DescribeTable("refuses", func(c awskms.Config) {
		_, err := awskms.New(c)
		Expect(err).To(HaveOccurred())
	},
		Entry("no region", cfg("", fakeKey)),
		Entry("a region that is not one", cfg("Europe", fakeKey)),
		Entry("no key", cfg("us-east-1", "")),
		Entry("a key name that is no alias", cfg("us-east-1", "koinon-kek")),
		Entry("an AWS managed key", cfg("us-east-1", "alias/aws/secretsmanager")),
		Entry("a key ARN in another region", cfg("eu-central-1", fakeKey)),
		Entry("a key ARN of another service", cfg("us-east-1", "arn:aws:s3:us-east-1:111122223333:key/0b1d2e3f-4a5b-4c6d-8e9f-a0b1c2d3e4f5")),
		Entry("a key ARN without a key ID", cfg("us-east-1", "arn:aws:kms:us-east-1:111122223333:key/koinon")),
		Entry("plain http off the loopback", cfg("us-east-1", fakeKey, endpoint("http://kms.example:4566"))),
		Entry("an endpoint with a path", cfg("us-east-1", fakeKey, endpoint("https://kms.example/kms"))),
		Entry("an endpoint with user info", cfg("us-east-1", fakeKey, endpoint("https://user:pw@kms.example"))),
		Entry("another scheme", cfg("us-east-1", fakeKey, endpoint("ftp://kms.example"))),
		Entry("no pod identity", cfg("us-east-1", fakeKey, identity(awskms.Identity{}))),
		Entry("IRSA without a token file", cfg("us-east-1", fakeKey, identity(awskms.Identity{RoleARN: irsa.RoleARN}))),
		Entry("IRSA with a role that is not one", cfg("us-east-1", fakeKey, identity(awskms.Identity{RoleARN: "koinon-api", WebIdentityTokenFile: "/t"}))),
		Entry("Pod Identity over plain http elsewhere", cfg("us-east-1", fakeKey, identity(awskms.Identity{CredentialsURI: "http://agent.example/v1/credentials"}))),
		Entry("a missing CA bundle", cfg("us-east-1", fakeKey, func(c *awskms.Config) { c.CACertFile = "/nonexistent/ca.pem" })),
	)

	DescribeTable("accepts", func(c awskms.Config) {
		_, err := awskms.New(c)
		Expect(err).NotTo(HaveOccurred())
	},
		Entry("a key ARN", cfg("us-east-1", fakeKey)),
		Entry("a key ID", cfg("us-east-1", "0b1d2e3f-4a5b-4c6d-8e9f-a0b1c2d3e4f5")),
		Entry("a multi-Region key ID", cfg("us-east-1", "mrk-0123456789abcdef0123456789abcdef")),
		Entry("an alias", cfg("us-east-1", "alias/koinon-kek")),
		Entry("an alias ARN", cfg("us-east-1", "arn:aws:kms:us-east-1:111122223333:alias/koinon-kek")),
		Entry("a GovCloud key ARN", cfg("us-gov-west-1", "arn:aws-us-gov:kms:us-gov-west-1:111122223333:key/0b1d2e3f-4a5b-4c6d-8e9f-a0b1c2d3e4f5")),
		Entry("a China key ARN", cfg("cn-north-1", "arn:aws-cn:kms:cn-north-1:111122223333:key/0b1d2e3f-4a5b-4c6d-8e9f-a0b1c2d3e4f5")),
		Entry("a VPC endpoint", cfg("us-east-1", fakeKey, endpoint("https://vpce-0123-abcd.kms.us-east-1.vpce.amazonaws.com/"))),
		Entry("http on the loopback", cfg("us-east-1", fakeKey, endpoint("http://127.0.0.1:4566"))),
		Entry("EKS Pod Identity", cfg("us-east-1", fakeKey, identity(pod))),
	)

	It("reads the pod identity from the environment and never the static keys", func() {
		GinkgoT().Setenv(awskms.EnvRoleARN, irsa.RoleARN)
		GinkgoT().Setenv(awskms.EnvWebIdentityTokenFile, irsa.WebIdentityTokenFile)
		GinkgoT().Setenv("AWS_ACCESS_KEY_ID", "koinon_test_not-a-real-key-id")
		GinkgoT().Setenv("AWS_SECRET_ACCESS_KEY", "koinon_test_not-a-real-secret")
		id, ignored := awskms.IdentityFromEnv()
		Expect(id).To(Equal(irsa))
		Expect(ignored).To(ConsistOf("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"))

		GinkgoT().Setenv(awskms.EnvRoleARN, "")
		GinkgoT().Setenv(awskms.EnvWebIdentityTokenFile, "")
		id, _ = awskms.IdentityFromEnv()
		_, err := awskms.New(awskms.Config{Region: "us-east-1", KeyID: fakeKey, Identity: id})
		Expect(err).To(MatchError(ContainSubstring("no pod identity")), "static keys alone are not an identity")
	})
})
