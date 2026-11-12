// SPDX-License-Identifier: Apache-2.0

package codehost_test

import (
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
			c := exec.Command("git", "ls-remote", srv.URL+"/acme/widgets.git")
			c.Env = []string{
				"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + config,
				"GIT_TERMINAL_PROMPT=0", "GITHUB_TOKEN=koinon_test_not-a-real-token",
			}
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
