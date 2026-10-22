// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/agentconfig"
	"github.com/procoduck/koinon/internal/execution"
	"github.com/procoduck/koinon/internal/reconciler"
	"github.com/procoduck/koinon/templates"
)

// The repo.read grant (plan item C2) as the reconciler renders it, against a real gateway: push,
// writes through the REST API and repositories outside the binding are refused by policy.
var _ = Describe("GitHub read grant", Label("escape", "openshell"), Ordered, func() {
	var (
		g         *gateway
		repo      = envOr("KOINON_ESCAPE_REPO", "octocat/Hello-World")
		other     = envOr("KOINON_ESCAPE_OTHER_REPO", "octocat/Spoon-Knife")
		n         = suffix()
		sandbox   = fmt.Sprintf("kesc-%d", n)
		provider  = fmt.Sprintf("kescp-%d", n)
		profileID = fmt.Sprintf("koinon-escape-github-%d", n)
	)

	run := func(ctx context.Context, script string) (string, string, int) {
		GinkgoHelper()
		return g.sh(ctx, sandbox, script)
	}
	curl := func(ctx context.Context, method, url string) (string, string) {
		GinkgoHelper()
		code, body, _ := g.curl(ctx, sandbox, method, url)
		return code, body
	}
	// refusedByPolicy is OpenShell's answer to a request its L7 policy denies (spike 3.2).
	refusedByPolicy := func(code, body string) {
		GinkgoHelper()
		Expect(code).To(Equal("403"))
		Expect(body).To(ContainSubstring("policy"))
	}

	BeforeAll(func(ctx SpecContext) {
		g = dialGateway(ctx)
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
		g.provider(ctx, execution.Provider{
			Name: provider, Profile: reconciler.GitHubProfile(profileID, read, binaries), Credentials: creds,
		})
		g.sandbox(ctx, execution.SandboxSpec{
			Name: sandbox, Providers: []string{provider},
			Policy: reconciler.SandboxPolicy(map[string]execution.NetworkRule{"github_read": reconciler.GitHubRule(read, binaries)}),
		})
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
