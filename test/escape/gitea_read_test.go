// SPDX-License-Identifier: Apache-2.0

package escape_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/reconciler"
	"github.com/stateracloud/koinon/templates"
)

// The repo.read grant on a Gitea connection (plan item CH5, ADR 0063) as the reconciler renders it,
// against a real gateway. With the read token's placeholder, push is denied and so are repositories
// outside the binding, whatever the server would say.
//
// Every request below is refused by OpenShell's L7 policy in the sandbox's proxy, so the suite needs
// no Gitea, only something to dial: OpenShell 0.1.5 admits the connect to an allowed host at L4 and
// dials (and TLS-handshakes) it before its L7 relay judges the request. KOINON_ESCAPE_GITEA_URL
// names the base URL the grant is rendered for; test/escape/kind.sh sets it to the in-cluster TLS
// stand-in (escape-gitea-standin, koinon-fakemodel -standin over TLS, with a sub-path, which
// exercises the prefix), with a throwaway CA that it appends to the trust bundle of a supervisor
// image the gateway is re-pointed at (OpenShell 0.1.5's supervisor verifies upstream TLS against
// the bundle in its own image, not the sandbox's; interim until #80), so nothing leaves the
// cluster (old issue 371). A request that reached the stand-in would get its 418, not the proxy's 403. The
// default, https://example.com/gitea, is for a cluster that can reach example.com:443 (IANA's
// reserved example domain, which resolves inside the sandbox: a name that fails DNS, such as one
// under .invalid, never reaches the proxy and the specs would see curl code 000, old issue 244).
// What a real Gitea does with the read token behind the policy (a push refused for the token's
// missing write scope, 404 for a repository the bot is not a collaborator on) is covered by
// the fake Gitea, which enforces both (internal/testing/fakegitea), and is confirmed on the home lab.
var _ = Describe("Gitea read grant", Label("escape", "openshell"), Ordered, func() {
	var (
		g         *gateway
		base      = strings.TrimRight(envOr("KOINON_ESCAPE_GITEA_URL", "https://example.com/gitea"), "/")
		repo      = envOr("KOINON_ESCAPE_GITEA_REPO", "koinon-demo/hello")
		other     = envOr("KOINON_ESCAPE_GITEA_OTHER_REPO", "koinon-demo/secrets")
		n         = suffix()
		sandbox   = fmt.Sprintf("kescg-%d", n)
		provider  = fmt.Sprintf("kescgp-%d", n)
		profileID = fmt.Sprintf("koinon-escape-gitea-%d", n)
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
	refusedByPolicy := func(code, body string) {
		GinkgoHelper()
		answered(code, body)
		Expect(code).To(Equal("403"))
		Expect(body).To(ContainSubstring("policy"))
	}

	BeforeAll(func(ctx SpecContext) {
		g = dialGateway(ctx)
		u, err := url.Parse(base)
		Expect(err).NotTo(HaveOccurred())
		t, ok := connectors.Get("gitea")
		Expect(ok).To(BeTrue())
		Expect(t.Spec.Auth.Type).To(Equal(connectors.AuthStaticToken))
		read, ok, err := reconciler.GitHubReadAccessAt(agentconfig.Config{
			Grants: []templates.Grant{{Connector: "gitea", Scopes: []string{"repo.read"}, Preset: templates.PresetStandard}},
			Bindings: []agentconfig.Binding{{
				Connector: "gitea", ConnectionID: "00000000-0000-0000-0000-000000000000", Targets: []string{repo},
			}},
		}, "https://"+u.Host+u.Path)
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

	It("denies push, however it is spelled", func(ctx SpecContext) {
		refusedByPolicy(curl(ctx, "GET", base+"/"+repo+".git/info/refs?service=git-receive-pack"))
		refusedByPolicy(curl(ctx, "POST", base+"/"+repo+".git/git-receive-pack"))
		refusedByPolicy(curl(ctx, "POST", base+"/"+repo+"/git-receive-pack"))

		By("git push itself fails")
		_, stderr, code := run(ctx, fmt.Sprintf(`set -e
cd /tmp && rm -rf pushg && git init -q pushg && cd pushg
git -c user.email=escape@koinon.invalid -c user.name=escape commit -q --allow-empty -m escape
git -c credential.helper= -c "http.extraHeader=Authorization: token $GITHUB_TOKEN" push %s/%s.git HEAD:refs/heads/koinon-escape`, base, repo))
		Expect(code).NotTo(Equal(0))
		Expect(stderr).To(ContainSubstring("403"))
	}, NodeTimeout(2*time.Minute))

	It("denies POST and other writes to the REST API", func(ctx SpecContext) {
		refusedByPolicy(curl(ctx, "POST", base+"/api/v1/repos/"+repo+"/pulls"))
		refusedByPolicy(curl(ctx, "POST", base+"/api/v1/repos/"+repo+"/issues"))
		refusedByPolicy(curl(ctx, "POST", base+"/api/v1/repos/"+repo+"/contents"))
		refusedByPolicy(curl(ctx, "PUT", base+"/api/v1/repos/"+repo+"/contents/escape.txt"))
		refusedByPolicy(curl(ctx, "DELETE", base+"/api/v1/repos/"+repo))
	}, NodeTimeout(2*time.Minute))

	It("denies repositories outside the binding, the user and admin APIs, and the host without its sub-path", func(ctx SpecContext) {
		refusedByPolicy(curl(ctx, "GET", base+"/"+other+".git/info/refs?service=git-upload-pack"))
		refusedByPolicy(curl(ctx, "POST", base+"/"+other+".git/git-upload-pack"))
		refusedByPolicy(curl(ctx, "GET", base+"/api/v1/repos/"+other))
		refusedByPolicy(curl(ctx, "GET", base+"/api/v1/user/repos"))
		refusedByPolicy(curl(ctx, "GET", base+"/api/v1/repos/search?q="+other))
		refusedByPolicy(curl(ctx, "GET", base+"/api/v1/admin/users"))
		u, err := url.Parse(base)
		Expect(err).NotTo(HaveOccurred())
		if u.Path != "" {
			refusedByPolicy(curl(ctx, "GET", "https://"+u.Host+"/api/v1/repos/"+repo))
		}
	}, NodeTimeout(2*time.Minute))
})
