// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/agentconfig"
	"github.com/procoduck/koinon/internal/execution"
	"github.com/procoduck/koinon/internal/execution/openshell"
	"github.com/procoduck/koinon/internal/reconciler"
	"github.com/procoduck/koinon/templates"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// The repo.read grant (plan item C2) as the reconciler renders it, against a real gateway: push,
// writes through the REST API and repositories outside the binding are refused by policy.
var _ = Describe("GitHub read grant", Label("escape", "openshell"), Ordered, func() {
	var (
		b         *openshell.Backend
		workspace string
		repo      = envOr("KOINON_ESCAPE_REPO", "octocat/Hello-World")
		other     = envOr("KOINON_ESCAPE_OTHER_REPO", "octocat/Spoon-Knife")
		suffix    = time.Now().Unix() % 100_000_000
		sandbox   = fmt.Sprintf("kesc-%d", suffix) // OpenShell 0.1.5: at most 19 characters
		provider  = fmt.Sprintf("kescp-%d", suffix)
		profileID = fmt.Sprintf("koinon-escape-github-%d", suffix)
	)

	// run executes script with sh in the sandbox and returns stdout, stderr and the exit code.
	run := func(ctx context.Context, script string) (string, string, int) {
		GinkgoHelper()
		var stderr bytes.Buffer
		p, err := b.Exec(ctx, workspace, sandbox, execution.ExecRequest{
			Command: []string{"sh", "-c", script}, Timeout: time.Minute, Stderr: &stderr,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Stdin().Close()).To(Succeed())
		out, err := io.ReadAll(p.Stdout())
		Expect(err).NotTo(HaveOccurred())
		code, err := p.Wait()
		Expect(err).NotTo(HaveOccurred())
		return string(out), stderr.String(), code
	}
	// curl makes one request with curl and returns the status code and body. The placeholder
	// goes in the Authorization header, as a client of the grant would send it.
	curl := func(ctx context.Context, method, url string) (string, string) {
		GinkgoHelper()
		out, stderr, _ := run(ctx, fmt.Sprintf(
			`curl -sS -X %s -H "Authorization: Bearer $GITHUB_TOKEN" -o /tmp/body -w '%%{http_code}' '%s'; echo; cat /tmp/body`, method, url))
		code, body, _ := strings.Cut(out, "\n")
		GinkgoWriter.Printf("%s %s -> %s %s %s\n", method, url, code, body, stderr)
		return code, body
	}
	// refusedByPolicy is OpenShell's answer to a request its L7 policy denies (spike 3.2).
	refusedByPolicy := func(code, body string) {
		GinkgoHelper()
		Expect(code).To(Equal("403"))
		Expect(body).To(ContainSubstring("policy"))
	}

	BeforeAll(func(ctx SpecContext) {
		endpoint := os.Getenv("KOINON_OPENSHELL_ENDPOINT")
		if endpoint == "" {
			Skip("KOINON_OPENSHELL_ENDPOINT is not set")
		}
		dir := os.Getenv("KOINON_OPENSHELL_MTLS_DIR")
		image := os.Getenv("KOINON_OPENSHELL_IMAGE")
		Expect(dir).NotTo(BeEmpty(), "KOINON_OPENSHELL_MTLS_DIR")
		Expect(image).NotTo(BeEmpty(), "KOINON_OPENSHELL_IMAGE")
		workspace = envOr("KOINON_OPENSHELL_WORKSPACE", "koinon-escape")

		var err error
		b, err = openshell.Dial(openshell.Config{
			Endpoint: endpoint,
			CertFile: filepath.Join(dir, "tls.crt"),
			KeyFile:  filepath.Join(dir, "tls.key"),
			CAFile:   filepath.Join(dir, "ca.crt"),
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(b.Close)
		_, err = b.EnsureWorkspace(ctx, workspace, nil)
		Expect(err).NotTo(HaveOccurred())

		read, ok, err := reconciler.GitHubReadAccess(agentconfig.Config{
			Grants: []templates.Grant{{Connector: "github", Scopes: []string{"repo.read"}, Preset: templates.PresetStandard}},
			Bindings: []agentconfig.Binding{{
				Connector: "github", ConnectionID: "00000000-0000-0000-0000-000000000000", Targets: []string{repo},
			}},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		binaries := []string{"/usr/bin/git", "/usr/bin/curl"}
		creds := map[string]string{}
		for _, env := range reconciler.GitHubTokenEnvVars {
			creds[env] = "koinon-escape-dummy-value"
		}
		Expect(b.EnsureProvider(ctx, workspace, execution.Provider{
			Name: provider, Profile: reconciler.GitHubProfile(profileID, read, binaries), Credentials: creds,
		})).To(Succeed())
		DeferCleanup(func(ctx context.Context) error {
			if err := b.DeleteProvider(ctx, workspace, provider); err != nil {
				return err
			}
			return b.DeleteProviderProfile(ctx, workspace, profileID)
		})

		_, err = b.CreateSandbox(ctx, workspace, execution.SandboxSpec{
			Name: sandbox, Image: image, Command: []string{"sleep", "infinity"},
			Providers: []string{provider},
			Policy:    reconciler.SandboxPolicy(map[string]execution.NetworkRule{"github_read": reconciler.GitHubRule(read, binaries)}),
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) error { return b.DeleteSandbox(ctx, workspace, sandbox) })
		Eventually(func(g Gomega) {
			sb, err := b.GetSandbox(ctx, workspace, sandbox)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(sb.Phase).To(Equal(execution.PhaseReady))
		}).WithContext(ctx).WithTimeout(5 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
	}, NodeTimeout(6*time.Minute))

	It("holds only the placeholder in the sandbox environment", func(ctx SpecContext) {
		out, _, code := run(ctx, `printf %s "$GITHUB_TOKEN"`)
		Expect(code).To(Equal(0))
		Expect(out).To(HavePrefix("openshell:resolve:env:"))
		Expect(out).NotTo(ContainSubstring("koinon-escape-dummy-value"))
	}, NodeTimeout(2*time.Minute))

	It("denies push", func(ctx SpecContext) {
		refusedByPolicy(curl(ctx, "GET", "https://github.com/"+repo+".git/info/refs?service=git-receive-pack"))
		refusedByPolicy(curl(ctx, "POST", "https://github.com/"+repo+".git/git-receive-pack"))
		refusedByPolicy(curl(ctx, "POST", "https://github.com/"+repo+"/git-receive-pack"))

		By("git push itself fails")
		_, stderr, code := run(ctx, fmt.Sprintf(`set -e
cd /tmp && rm -rf push && git init -q push && cd push
git -c user.email=escape@koinon.invalid -c user.name=escape commit -q --allow-empty -m escape
git -c credential.helper= -c "http.extraHeader=Authorization: Bearer $GITHUB_TOKEN" push https://github.com/%s.git HEAD:refs/heads/koinon-escape`, repo))
		Expect(code).NotTo(Equal(0))
		Expect(stderr).To(ContainSubstring("403"))
	}, NodeTimeout(2*time.Minute))

	It("denies POST and other writes to the REST API", func(ctx SpecContext) {
		refusedByPolicy(curl(ctx, "POST", "https://api.github.com/repos/"+repo+"/issues"))
		refusedByPolicy(curl(ctx, "POST", "https://api.github.com/repos/"+repo+"/pulls"))
		refusedByPolicy(curl(ctx, "PUT", "https://api.github.com/repos/"+repo+"/contents/escape.txt"))
		refusedByPolicy(curl(ctx, "DELETE", "https://api.github.com/repos/"+repo))
		refusedByPolicy(curl(ctx, "POST", "https://api.github.com/graphql"))
	}, NodeTimeout(2*time.Minute))

	It("denies repositories outside the binding", func(ctx SpecContext) {
		refusedByPolicy(curl(ctx, "GET", "https://github.com/"+other+".git/info/refs?service=git-upload-pack"))
		refusedByPolicy(curl(ctx, "POST", "https://github.com/"+other+".git/git-upload-pack"))
		refusedByPolicy(curl(ctx, "GET", "https://api.github.com/repos/"+other))
		refusedByPolicy(curl(ctx, "GET", "https://api.github.com/search/code?q=repo:"+other))
	}, NodeTimeout(2*time.Minute))
})
