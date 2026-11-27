// SPDX-License-Identifier: Apache-2.0

package codehost_test

import (
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/testing/gitenv"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

const helperPath = "../../harnesses/common/koinon-git-credential"

// pkt is a git pkt-line.
func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

var _ = Describe("git in a sandbox on a connection's host", func() {
	runHelper := func(args []string, token string, stdin string) string {
		GinkgoHelper()
		c := exec.Command("sh", append([]string{helperPath}, args...)...)
		c.Env = []string{"PATH=" + os.Getenv("PATH"), "GITHUB_TOKEN=" + token}
		c.Stdin = strings.NewReader(stdin)
		out, err := c.Output()
		Expect(err).NotTo(HaveOccurred())
		return string(out)
	}

	Describe("koinon-git-credential", func() {
		const token = "koinon_test_not-a-real-token"
		It("answers for github.com by default and for no other host", func() {
			Expect(runHelper([]string{"get"}, token, "protocol=https\nhost=github.com\n\n")).
				To(Equal("username=x-access-token\npassword=" + token + "\n"))
			Expect(runHelper([]string{"get"}, token, "protocol=https\nhost=ghes.example.test\n\n")).To(BeEmpty())
		})
		It("answers only for the host it is told, port included", func() {
			Expect(runHelper([]string{"--host", "ghes.example.test", "get"}, token, "protocol=https\nhost=ghes.example.test\n\n")).
				To(Equal("username=x-access-token\npassword=" + token + "\n"))
			Expect(runHelper([]string{"--host", "ghes.example.test", "get"}, token, "protocol=https\nhost=github.com\n\n")).To(BeEmpty())
			Expect(runHelper([]string{"--host", "ghes.example.test:8443", "get"}, token, "protocol=https\nhost=ghes.example.test\n\n")).To(BeEmpty())
			Expect(runHelper([]string{"--host", "ghes.example.test", "get"}, token, "protocol=http\nhost=ghes.example.test\n\n")).To(BeEmpty())
		})
		It("stores and erases nothing", func() {
			Expect(runHelper([]string{"--host", "ghes.example.test", "store"}, token, "protocol=https\nhost=ghes.example.test\npassword=x\n\n")).To(BeEmpty())
		})
	})

	It("trusts the connection's CA for git, and sends the helper's placeholder only to its host", func() {
		testenv.Require(testenv.Binary("git"))
		var mu sync.Mutex
		var auths []string
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			auths = append(auths, r.Header.Get("Authorization"))
			mu.Unlock()
			if r.Header.Get("Authorization") == "" {
				w.Header().Set("WWW-Authenticate", `Basic realm="koinon"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			// An empty repository, advertised the smart-HTTP way.
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_, _ = fmt.Fprint(w, pkt("# service=git-upload-pack\n")+"0000"+
				pkt("0000000000000000000000000000000000000000 capabilities^{}\x00agent=koinon-test\n")+"0000")
		}))
		DeferCleanup(srv.Close)

		loc, err := githubType().Locate(srv.URL)
		Expect(err).NotTo(HaveOccurred())
		dir := GinkgoT().TempDir()
		caFile := filepath.Join(dir, "ca.pem")
		Expect(os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)).To(Succeed())
		rendered := string(codehost.GitConfig(loc, true))
		rendered = strings.ReplaceAll(rendered, codehost.SandboxGitCA, caFile)
		rendered = strings.ReplaceAll(rendered, codehost.CredentialHelper, "!sh "+mustAbs(helperPath))
		cfgFile := filepath.Join(dir, "config")
		Expect(os.WriteFile(cfgFile, []byte(rendered), 0o600)).To(Succeed())

		ls := func(config string) ([]byte, error) {
			c := gitenv.Command(context.Background(), dir, "", "ls-remote", srv.URL+"/acme/widgets.git")
			c.Env = gitenv.Env(dir, "GIT_CONFIG_GLOBAL="+config, "GITHUB_TOKEN=koinon_test_not-a-real-token")
			return c.CombinedOutput()
		}

		// Without the rendered config git does not trust the server's certificate.
		empty := filepath.Join(dir, "empty")
		Expect(os.WriteFile(empty, nil, 0o600)).To(Succeed())
		out, err := ls(empty)
		Expect(err).To(HaveOccurred())
		Expect(string(out)).To(MatchRegexp(`(?i)certificate`))

		// With it git verifies the CA and answers the 401 with the helper's credential.
		out, err = ls(cfgFile)
		Expect(err).NotTo(HaveOccurred(), string(out))
		mu.Lock()
		defer mu.Unlock()
		Expect(auths).To(ContainElement(HavePrefix("Basic ")))
	})
})

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	Expect(err).NotTo(HaveOccurred())
	return a
}

var _ = Describe("git in a sandbox through the gateway's egress prefix", func() {
	// A compact JWS shape; the gateway is a stand-in here, so the signature is not checked.
	const token = "koinon_test.not-a-real.token"

	type seen struct{ path, actor, auth string }
	serve := func() (*httptest.Server, func() []seen) {
		var mu sync.Mutex
		var got []seen
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			got = append(got, seen{r.URL.Path, r.Header.Get(codehost.ActorTokenHeader), r.Header.Get("Authorization")})
			mu.Unlock()
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_, _ = fmt.Fprint(w, pkt("# service=git-upload-pack\n")+"0000"+
				pkt("0000000000000000000000000000000000000000 capabilities^{}\x00agent=koinon-test\n")+"0000")
		}))
		DeferCleanup(srv.Close)
		return srv, func() []seen { mu.Lock(); defer mu.Unlock(); return append([]seen(nil), got...) }
	}

	setup := func(gw string, withToken bool) (dir, cfgFile string) {
		GinkgoHelper()
		loc, err := githubType().Locate("https://ghes.example.test")
		Expect(err).NotTo(HaveOccurred())
		dir = GinkgoT().TempDir()
		tokFile := filepath.Join(dir, "token.gitconfig")
		if withToken {
			b, ok := codehost.EgressTokenGitConfig(gw, token)
			Expect(ok).To(BeTrue())
			Expect(os.WriteFile(tokFile, b, 0o600)).To(Succeed())
		}
		rendered := string(codehost.GitConfig(loc, false)) + string(codehost.EgressGitConfig(gw, loc))
		rendered = strings.ReplaceAll(rendered, codehost.SandboxEgressToken, tokFile)
		rendered = strings.ReplaceAll(rendered, codehost.CredentialHelper, "!sh "+mustAbs(helperPath))
		cfgFile = filepath.Join(dir, "config")
		Expect(os.WriteFile(cfgFile, []byte(rendered), 0o600)).To(Succeed())
		return dir, cfgFile
	}
	ls := func(dir, cfgFile, url string) ([]byte, error) {
		c := gitenv.Command(context.Background(), dir, "", "ls-remote", url)
		c.Env = gitenv.Env(dir, "GIT_CONFIG_GLOBAL="+cfgFile, "GITHUB_TOKEN=koinon_test_not-a-real-token")
		return c.CombinedOutput()
	}

	It("sends the code host's URL to the gateway prefix with the actor token and no credential", func() {
		testenv.Require(testenv.Binary("git"))
		gw, got := serve()
		dir, cfg := setup(gw.URL, true)
		out, err := ls(dir, cfg, "https://ghes.example.test/acme/widgets.git")
		Expect(err).NotTo(HaveOccurred(), string(out))
		Expect(got()).To(ContainElement(seen{path: "/egress/ghes.example.test/acme/widgets.git/info/refs", actor: token}))
	})

	It("sends the token to the gateway prefix only", func() {
		testenv.Require(testenv.Binary("git"))
		gw, _ := serve()
		other, got := serve()
		dir, cfg := setup(gw.URL, true)
		out, err := ls(dir, cfg, other.URL+"/acme/widgets.git")
		Expect(err).NotTo(HaveOccurred(), string(out))
		Expect(got()).NotTo(BeEmpty())
		for _, s := range got() {
			Expect(s.actor).To(BeEmpty())
		}
	})

	It("still runs git without the token file, and then sends no token", func() {
		testenv.Require(testenv.Binary("git"))
		gw, got := serve()
		dir, cfg := setup(gw.URL, false)
		out, err := ls(dir, cfg, "https://ghes.example.test/acme/widgets.git")
		Expect(err).NotTo(HaveOccurred(), string(out))
		Expect(got()).To(ContainElement(seen{path: "/egress/ghes.example.test/acme/widgets.git/info/refs"}))
	})

	DescribeTable("refuses a token that could add a line or a header",
		func(tok string) {
			_, ok := codehost.EgressTokenGitConfig("http://gw:8081", tok)
			Expect(ok).To(BeFalse())
		},
		Entry("empty", ""),
		Entry("two parts", "a.b"),
		Entry("a newline", "a.b.c\n[http]"),
		Entry("a space", "a.b.c d"),
		Entry("an empty part", "a..c"),
	)

	// Agent Substrate (issue #1386): the egress layer outside the guest adds the actor token, so the
	// guest sends only the public placeholder and includes no token file.
	Describe("with the actor identity added outside the guest", func() {
		placeholderSetup := func(gw string) (dir, cfgFile string) {
			GinkgoHelper()
			loc, err := githubType().Locate("https://ghes.example.test")
			Expect(err).NotTo(HaveOccurred())
			dir = GinkgoT().TempDir()
			rendered := string(codehost.GitConfig(loc, false)) + string(codehost.EgressPlaceholderGitConfig(gw, loc))
			rendered = strings.ReplaceAll(rendered, codehost.CredentialHelper, "!sh "+mustAbs(helperPath))
			cfgFile = filepath.Join(dir, "config")
			Expect(os.WriteFile(cfgFile, []byte(rendered), 0o600)).To(Succeed())
			return dir, cfgFile
		}

		It("includes no token file", func() {
			loc, err := githubType().Locate("https://ghes.example.test")
			Expect(err).NotTo(HaveOccurred())
			cfg := string(codehost.EgressPlaceholderGitConfig("http://gw:8081", loc))
			Expect(cfg).NotTo(ContainSubstring("[include]"))
			Expect(cfg).NotTo(ContainSubstring(codehost.SandboxEgressToken))
			Expect(cfg).To(ContainSubstring("extraHeader = " + codehost.ActorTokenHeader + ": " + actortoken.Placeholder + "\n"))
		})

		It("sends the code host's URL to the gateway prefix with the placeholder and no credential", func() {
			testenv.Require(testenv.Binary("git"))
			gw, got := serve()
			dir, cfg := placeholderSetup(gw.URL)
			out, err := ls(dir, cfg, "https://ghes.example.test/acme/widgets.git")
			Expect(err).NotTo(HaveOccurred(), string(out))
			Expect(got()).To(ContainElement(seen{path: "/egress/ghes.example.test/acme/widgets.git/info/refs", actor: actortoken.Placeholder}))
		})

		It("sends the placeholder to the gateway prefix only", func() {
			testenv.Require(testenv.Binary("git"))
			gw, _ := serve()
			other, got := serve()
			dir, cfg := placeholderSetup(gw.URL)
			out, err := ls(dir, cfg, other.URL+"/acme/widgets.git")
			Expect(err).NotTo(HaveOccurred(), string(out))
			Expect(got()).NotTo(BeEmpty())
			for _, s := range got() {
				Expect(s.actor).To(BeEmpty())
			}
		})
	})
})
