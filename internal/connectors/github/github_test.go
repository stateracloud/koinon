// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
)

func TestGitHub(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "GitHub Suite")
}

// clock is a settable time source shared by the client and the fake.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var _ = Describe("ParsePrivateKey", func() {
	It("accepts GitHub's PKCS #1 .pem file and hides the key", func() {
		fake := githubtest.New()
		DeferCleanup(fake.Server.Close)
		key, err := github.ParsePrivateKey(fake.PEM)
		Expect(err).NotTo(HaveOccurred())
		Expect(key.Fingerprint()).To(HavePrefix("SHA256:"))
		for _, s := range []string{fmt.Sprint(key), fmt.Sprintf("%v %+v %#v %s", key, key, key, key)} {
			Expect(s).NotTo(ContainSubstring("PRIVATE"))
			Expect(s).To(ContainSubstring("[redacted]"))
		}
		b, err := json.Marshal(struct{ K *github.PrivateKey }{key})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(b)).To(Equal(`{"K":"[redacted]"}`))

		var buf bytes.Buffer
		slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "key", key)
		Expect(buf.String()).To(ContainSubstring(`"key":"[redacted]"`))
	})

	It("rejects other input without quoting it", func() {
		ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		der, err := x509.MarshalPKCS8PrivateKey(ec)
		Expect(err).NotTo(HaveOccurred())
		ecPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		for _, in := range [][]byte{[]byte("not a key at all SECRET"), ecPEM, []byte("-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n")} {
			_, err := github.ParsePrivateKey(in)
			Expect(err).To(MatchError(github.ErrInvalidPrivateKey))
			Expect(err.Error()).NotTo(ContainSubstring("SECRET"))
		}
	})
})

var _ = Describe("Client", func() {
	var (
		fake   *githubtest.Fake
		client *github.Client
		app    github.App
		clk    *clock
		ctx    context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		clk = &clock{t: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
		fake = githubtest.New()
		fake.Now = clk.Now
		DeferCleanup(fake.Server.Close)
		key, err := github.ParsePrivateKey(fake.PEM)
		Expect(err).NotTo(HaveOccurred())
		app = github.App{AppID: fake.AppID, InstallationID: fake.InstallationID, Key: key}
		client = github.NewClient(github.Options{BaseURL: fake.URL(), Now: clk.Now, RefreshMargin: 10 * time.Minute})
	})

	It("authenticates as the App", func() {
		info, err := client.GetApp(ctx, app)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.ID).To(Equal(fake.AppID))
		inst, err := client.GetInstallation(ctx, app)
		Expect(err).NotTo(HaveOccurred())
		Expect(inst.Account.Login).To(Equal("acme"))
		Expect(inst.Permissions).To(HaveKeyWithValue("contents", "write"))
	})

	It("reports a wrong app ID or installation as an API error", func() {
		_, err := client.GetApp(ctx, github.App{AppID: 1, InstallationID: fake.InstallationID, Key: app.Key})
		var apiErr *github.APIError
		Expect(err).To(BeAssignableToTypeOf(apiErr))
		Expect(err.Error()).To(ContainSubstring("401"))
		_, err = client.GetInstallation(ctx, github.App{AppID: fake.AppID, InstallationID: 9, Key: app.Key})
		Expect(err).To(MatchError(ContainSubstring("404")))
	})

	It("narrows the token to the requested repositories and permissions", func() {
		tok, err := client.InstallationToken(ctx, app, github.TokenRequest{
			Repositories: []string{"acme/widgets"},
			Permissions:  map[string]string{"contents": "read", "metadata": "read"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(tok.Value()).To(HavePrefix("ghs_"))
		Expect(tok.Repositories).To(Equal([]string{"acme/widgets"}))
		Expect(tok.ExpiresAt).To(Equal(clk.Now().Add(time.Hour)))
		Expect(fake.Requests()).To(Equal([]githubtest.Request{{
			Repositories: []string{"widgets"},
			Permissions:  map[string]string{"contents": "read", "metadata": "read"},
		}}))
		Expect(fmt.Sprintf("%v %+v %#v", tok, tok, tok)).NotTo(ContainSubstring(tok.Value()))
		var buf bytes.Buffer
		slog.New(slog.NewJSONHandler(&buf, nil)).Info("minted", "token", tok)
		Expect(buf.String()).NotTo(ContainSubstring(tok.Value()))
	})

	It("pages through the repositories the installation can access", func() {
		fake.Repositories = nil
		for i := range 5 {
			fake.Repositories = append(fake.Repositories, fmt.Sprintf("repo-%d", i))
		}
		fake.Private = map[string]bool{"repo-1": true}
		tok, err := client.InstallationToken(ctx, app, github.TokenRequest{Permissions: map[string]string{"metadata": "read"}})
		Expect(err).NotTo(HaveOccurred())

		var names []string
		for page := 1; page <= 3; page++ {
			res, err := client.ListInstallationRepositories(ctx, tok, page, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.TotalCount).To(BeEquivalentTo(5))
			for _, r := range res.Repositories {
				names = append(names, r.FullName)
				Expect(r.DefaultBranch).To(Equal("main"))
				Expect(r.Private).To(Equal(r.FullName == "acme/repo-1"))
			}
		}
		Expect(names).To(Equal([]string{"acme/repo-0", "acme/repo-1", "acme/repo-2", "acme/repo-3", "acme/repo-4"}))

		res, err := client.ListInstallationRepositories(ctx, tok, 4, 2)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Repositories).To(BeEmpty())

		_, err = client.ListInstallationRepositories(ctx, tok, 1, 101)
		Expect(err).To(MatchError(ContainSubstring("per_page")))
	})

	It("caches a token until the refresh margin before expiry", func() {
		req := github.TokenRequest{Repositories: []string{"acme/widgets"}, Permissions: map[string]string{"contents": "read"}}
		first, err := client.InstallationToken(ctx, app, req)
		Expect(err).NotTo(HaveOccurred())

		clk.Advance(49 * time.Minute) // 11 minutes left: still cached
		again, err := client.InstallationToken(ctx, app, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(again.Value()).To(Equal(first.Value()))
		Expect(fake.Mints()).To(Equal(1))

		clk.Advance(2 * time.Minute) // 9 minutes left: inside the margin
		fresh, err := client.InstallationToken(ctx, app, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(fresh.Value()).NotTo(Equal(first.Value()))
		Expect(fake.Mints()).To(Equal(2))
	})

	It("keys the cache by repositories and permissions, not their order", func() {
		_, err := client.InstallationToken(ctx, app, github.TokenRequest{
			Repositories: []string{"acme/widgets", "acme/gadgets"}, Permissions: map[string]string{"contents": "read", "issues": "write"},
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = client.InstallationToken(ctx, app, github.TokenRequest{
			Repositories: []string{"ACME/gadgets", "acme/widgets"}, Permissions: map[string]string{"issues": "write", "contents": "read"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.Mints()).To(Equal(1))

		_, err = client.InstallationToken(ctx, app, github.TokenRequest{
			Repositories: []string{"acme/widgets"}, Permissions: map[string]string{"contents": "read", "issues": "write"},
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = client.InstallationToken(ctx, app, github.TokenRequest{
			Repositories: []string{"acme/widgets"}, Permissions: map[string]string{"contents": "write"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.Mints()).To(Equal(3))
	})

	It("mints once for concurrent callers", func() {
		req := github.TokenRequest{Repositories: []string{"acme/widgets"}, Permissions: map[string]string{"contents": "read"}}
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				defer GinkgoRecover()
				_, err := client.InstallationToken(ctx, app, req)
				Expect(err).NotTo(HaveOccurred())
			})
		}
		wg.Wait()
		Expect(fake.Mints()).To(Equal(1))
	})

	It("refuses a repository outside the installation's account", func() {
		// GitHub takes names without owners, so other/widgets would silently become
		// acme/widgets; the client compares full names.
		_, err := client.InstallationToken(ctx, app, github.TokenRequest{
			Repositories: []string{"other/widgets"}, Permissions: map[string]string{"contents": "read"},
		})
		Expect(err).To(MatchError(ContainSubstring("covers repositories [acme/widgets], requested [other/widgets]")))
	})

	It("refuses a token broader than requested", func() {
		fake.LieAboutPermissions = true
		_, err := client.InstallationToken(ctx, app, github.TokenRequest{
			Repositories: []string{"acme/widgets"}, Permissions: map[string]string{"contents": "read"},
		})
		Expect(err).To(MatchError(ContainSubstring("was not requested")))
	})

	It("passes GitHub's refusal through", func() {
		_, err := client.InstallationToken(ctx, app, github.TokenRequest{
			Repositories: []string{"acme/widgets"}, Permissions: map[string]string{"workflows": "write"},
		})
		Expect(err).To(MatchError(ContainSubstring("422")))
		_, err = client.InstallationToken(ctx, app, github.TokenRequest{
			Repositories: []string{"acme/nope"}, Permissions: map[string]string{"contents": "read"},
		})
		Expect(err).To(MatchError(ContainSubstring("not accessible to the parent installation")))
	})

	It("validates requests before calling GitHub", func() {
		_, err := client.InstallationToken(ctx, app, github.TokenRequest{Repositories: []string{"acme/widgets"}})
		Expect(err).To(MatchError(ContainSubstring("must name its permissions")))
		_, err = client.InstallationToken(ctx, app, github.TokenRequest{Repositories: []string{"widgets"}, Permissions: map[string]string{"contents": "read"}})
		Expect(err).To(MatchError(ContainSubstring("owner/name")))
		_, err = client.InstallationToken(ctx, app, github.TokenRequest{Permissions: map[string]string{"contents": "admin"}})
		Expect(err).To(MatchError(ContainSubstring("read or write")))
		Expect(fake.Mints()).To(BeZero())
	})

	It("signs JWTs GitHub accepts at the clock edges", func() {
		// The fake rejects exp more than ten minutes ahead and iat in the future.
		jwt, err := client.AppJWT(app)
		Expect(err).NotTo(HaveOccurred())
		Expect(jwt).To(MatchRegexp(`^[\w-]+\.[\w-]+\.[\w-]+$`))
		clk.Advance(8 * time.Minute)
		_, err = client.GetApp(ctx, app)
		Expect(err).NotTo(HaveOccurred())
	})
})
