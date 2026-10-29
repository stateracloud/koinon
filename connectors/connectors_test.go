// SPDX-License-Identifier: Apache-2.0

package connectors_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/templates"
)

func githubType() *connectors.Type {
	GinkgoHelper()
	t, ok := connectors.Get(connectors.GitHubID)
	Expect(ok).To(BeTrue())
	return t
}

func githubYAML() string {
	GinkgoHelper()
	b, err := os.ReadFile(filepath.Join("github", "connector.yaml"))
	Expect(err).NotTo(HaveOccurred())
	return string(b)
}

var _ = Describe("Schema", func() {
	It("declares the apiVersion derived from internal/project", func() {
		var schema struct {
			Properties struct {
				APIVersion struct {
					Const string `json:"const"`
				} `json:"apiVersion"`
			} `json:"properties"`
		}
		Expect(json.Unmarshal(connectors.Schema(), &schema)).To(Succeed())
		Expect(schema.Properties.APIVersion.Const).To(Equal(connectors.APIVersion))
	})
})

var _ = Describe("Embedded connector types", func() {
	It("embeds and validates every connectors/<id>/connector.yaml", func() {
		files, err := filepath.Glob("*/connector.yaml")
		Expect(err).NotTo(HaveOccurred())
		ts, err := connectors.All()
		Expect(err).NotTo(HaveOccurred())
		Expect(ts).To(HaveLen(len(files)))
	})

	It("defines the v0.1 GitHub scopes and tiers (ADR 0049)", func() {
		t := githubType()
		Expect(t.Spec.Auth.Type).To(Equal(connectors.AuthAppCredential))
		tiers := map[string]connectors.Tier{}
		for _, s := range t.Spec.Scopes {
			tiers[s.ID] = s.Tier
		}
		Expect(tiers).To(Equal(map[string]connectors.Tier{
			"repo.read":           connectors.TierRead,
			"pull_request.create": connectors.TierReversibleWrite,
			"issue.comment":       connectors.TierReversibleWrite,
		}))
		Expect(t.Spec.Auth.ForbiddenPermissions).To(ContainElement("workflows"))
	})

	It("maps presets to decisions", func() {
		t := githubType()
		Expect(t.Decide(templates.PresetReadOnly, "pull_request.create")).To(Equal(connectors.Deny))
		Expect(t.Decide(templates.PresetStandard, "pull_request.create")).To(Equal(connectors.Ask))
		Expect(t.Decide(templates.PresetTrusted, "pull_request.create")).To(Equal(connectors.Allow))
		Expect(t.Decide(templates.PresetStandard, "repo.read")).To(Equal(connectors.Allow))
		Expect(t.Decide(templates.PresetTrusted, "no.such")).To(Equal(connectors.Deny))
		Expect(t.Decide("unknown", "repo.read")).To(Equal(connectors.Deny))
	})

	It("derives the tool allowlist of a grant", func() {
		t := githubType()
		names := func(ts []connectors.ToolDecision) []string {
			var out []string
			for _, td := range ts {
				out = append(out, td.Tool)
			}
			return out
		}
		all := []string{"repo.read", "pull_request.create", "issue.comment"}
		Expect(names(t.Tools(templates.PresetStandard, all))).To(Equal(
			[]string{"comment_on_issue", "create_pull_request", "get_approval_status"}))
		Expect(t.Tools(templates.PresetReadOnly, all)).To(BeEmpty())
		Expect(names(t.Tools(templates.PresetStandard, []string{"issue.comment"}))).To(Equal(
			[]string{"comment_on_issue", "get_approval_status"}))
	})

	It("narrows permissions to the scopes", func() {
		t := githubType()
		Expect(t.Permissions("repo.read")).To(Equal(map[string]string{
			"contents": "read", "metadata": "read", "pull_requests": "read", "issues": "read",
		}))
		Expect(t.Permissions("repo.read", "pull_request.create")).To(Equal(
			map[string]string{"contents": "write", "metadata": "read", "pull_requests": "write", "issues": "read"}))
	})

	Describe("Network", func() {
		It("renders repo.read for one repository: clone and fetch, REST GET, push denied", func() {
			got, err := githubType().Network(templates.PresetStandard, []string{"repo.read", "pull_request.create"}, []string{"acme/widgets"})
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal([]connectors.HostAccess{
				{
					Host: "github.com", Port: 443,
					Allow: []connectors.HTTPRule{
						allow("GET", "/acme/widgets.git/info/refs", "service", "git-upload-pack"),
						allow("GET", "/acme/widgets/info/refs", "service", "git-upload-pack"),
						allow("POST", "/acme/widgets.git/git-upload-pack"),
						allow("POST", "/acme/widgets/git-upload-pack"),
					},
					Deny: []connectors.HTTPRule{
						allow("GET", "/**/info/refs", "service", "git-receive-pack"),
						allow("GET", "/**/git-receive-pack"),
						allow("HEAD", "/**/git-receive-pack"),
						allow("POST", "/**/git-receive-pack"),
						allow("PUT", "/**/git-receive-pack"),
						allow("PATCH", "/**/git-receive-pack"),
						allow("DELETE", "/**/git-receive-pack"),
					},
				},
				{
					Host: "api.github.com", Port: 443,
					Allow: []connectors.HTTPRule{
						allow("GET", "/repos/acme/widgets"),
						allow("GET", "/repos/acme/widgets/**"),
					},
					Deny: []connectors.HTTPRule{
						allow("POST", "/**"), allow("PUT", "/**"), allow("PATCH", "/**"), allow("DELETE", "/**"),
					},
				},
			}))
		})

		It("repeats the allow rules per repository and keeps deny rules once", func() {
			got, err := githubType().Network(templates.PresetReadOnly, []string{"repo.read"}, []string{"acme/widgets", "acme/gadgets"})
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(HaveLen(2))
			Expect(got[0].Allow).To(HaveLen(8))
			Expect(got[0].Deny).To(HaveLen(7))
			Expect(got[1].Allow).To(ContainElements(allow("GET", "/repos/acme/gadgets/**"), allow("GET", "/repos/acme/widgets")))
		})

		It("renders the rules at a connection's own host: git at the host, the API under /api/v3", func() {
			t := githubType()
			loc, err := t.Locate("https://ghes.example.test")
			Expect(err).NotTo(HaveOccurred())
			Expect(loc.Default).To(BeFalse())
			Expect(loc.BaseURL()).To(Equal("https://ghes.example.test"))
			Expect(loc.APIBaseURL()).To(Equal("https://ghes.example.test/api/v3"))
			got, err := t.NetworkAt(loc, templates.PresetStandard, []string{"repo.read"}, []string{"acme/widgets"})
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(HaveLen(1), "web and API share the host")
			h := got[0]
			Expect(h.Host).To(Equal("ghes.example.test"))
			Expect(h.Port).To(Equal(443))
			Expect(h.Allow).To(Equal([]connectors.HTTPRule{
				allow("GET", "/acme/widgets.git/info/refs", "service", "git-upload-pack"),
				allow("GET", "/acme/widgets/info/refs", "service", "git-upload-pack"),
				allow("POST", "/acme/widgets.git/git-upload-pack"),
				allow("POST", "/acme/widgets/git-upload-pack"),
				allow("GET", "/api/v3/repos/acme/widgets"),
				allow("GET", "/api/v3/repos/acme/widgets/**"),
			}))
			Expect(h.Deny).To(ContainElements(
				allow("GET", "/**/git-receive-pack"),
				allow("GET", "/**/info/refs", "service", "git-receive-pack"),
				allow("POST", "/api/v3/**"), allow("DELETE", "/api/v3/**"),
			))
			// Nothing names github.com any more.
			for _, r := range append(append([]connectors.HTTPRule{}, h.Allow...), h.Deny...) {
				Expect(r.Path).NotTo(ContainSubstring("github"))
			}
		})

		It("keeps a port, and the default location when the base URL is github.com's", func() {
			t := githubType()
			loc, err := t.Locate("https://GHES.example.test:8443/")
			Expect(err).NotTo(HaveOccurred())
			Expect(loc.Web).To(Equal(connectors.HostPort{Host: "ghes.example.test", Port: 8443}))
			Expect(loc.BaseURL()).To(Equal("https://ghes.example.test:8443"))
			got, err := t.NetworkAt(loc, templates.PresetStandard, []string{"repo.read"}, []string{"acme/widgets"})
			Expect(err).NotTo(HaveOccurred())
			Expect(got[0].Port).To(Equal(8443))

			for _, same := range []string{"", "https://github.com", "https://github.com/"} {
				loc, err := t.Locate(same)
				Expect(err).NotTo(HaveOccurred())
				Expect(loc.Default).To(BeTrue())
				def, err := t.NetworkAt(loc, templates.PresetStandard, []string{"repo.read"}, []string{"acme/widgets"})
				Expect(err).NotTo(HaveOccurred())
				Expect(def).To(Equal(mustNetwork(t)))
			}
		})

		It("refuses base URLs that are not a bare https host", func() {
			for _, bad := range []string{
				"http://ghes.example.test", "https://ghes.example.test/sub", "https://u:p@ghes.example.test",
				"https://ghes.example.test?x=1", "ghes.example.test", "https://", "https://ghes.example.test:99999", "https://-bad-",
				"https://*.example.test", "https://*", "https://ghes.*.test", "https://gh?s.example.test",
			} {
				_, err := githubType().Locate(bad)
				Expect(err).To(HaveOccurred(), bad)
			}
		})

		It("gives nothing without repo.read or without targets", func() {
			t := githubType()
			Expect(t.Network(templates.PresetStandard, []string{"pull_request.create"}, []string{"acme/widgets"})).To(BeEmpty())
			Expect(t.Network(templates.PresetStandard, []string{"repo.read"}, nil)).To(BeEmpty())
			Expect(t.HasNetwork(templates.PresetStandard, []string{"repo.read"})).To(BeTrue())
			Expect(t.HasNetwork(templates.PresetStandard, []string{"issue.comment"})).To(BeFalse())
		})

		It("rejects targets that would change a rendered path", func() {
			t := githubType()
			for _, bad := range []string{"acme/..", "acme/.", "acme/*", "acme", "acme/a/b", "acme/**"} {
				_, err := t.Network(templates.PresetStandard, []string{"repo.read"}, []string{bad})
				Expect(err).To(HaveOccurred(), bad)
			}
		})
	})

	It("validates targets", func() {
		t := githubType()
		Expect(t.ValidTarget("stateracloud/koinon")).To(BeTrue())
		Expect(t.ValidTarget("procoduck")).To(BeFalse())
		Expect(t.ValidTarget("a/b/c")).To(BeFalse())
		Expect(t.ValidTarget("-bad/repo")).To(BeFalse())
	})

	It("knows every scope the embedded templates grant", func() {
		ts, err := templates.All()
		Expect(err).NotTo(HaveOccurred())
		for _, tmpl := range ts {
			for _, g := range tmpl.Spec.Grants {
				ct, ok := connectors.Get(g.Connector)
				Expect(ok).To(BeTrue(), "template %s: unknown connector %s", tmpl.Metadata.ID, g.Connector)
				for _, s := range g.Scopes {
					_, ok := ct.Scope(s)
					Expect(ok).To(BeTrue(), "template %s: unknown scope %s", tmpl.Metadata.ID, s)
				}
			}
		}
	})

	It("marks the private key as the only secret field", func() {
		var secret []string
		for _, f := range githubType().Spec.SetupGuide.Fields {
			if f.Secret {
				secret = append(secret, f.ID)
			}
		}
		Expect(secret).To(Equal([]string{"privateKey"}))
	})
})

func mustNetwork(t *connectors.Type) []connectors.HostAccess {
	got, err := t.Network(templates.PresetStandard, []string{"repo.read"}, []string{"acme/widgets"})
	Expect(err).NotTo(HaveOccurred())
	return got
}

var _ = Describe("the code-host data of the GitHub type", func() {
	It("carries the capability descriptor, the CI paths and the default site", func() {
		t := githubType()
		Expect(t.Spec.Capabilities).To(Equal(&connectors.Capabilities{
			DisplayName: "GitHub", CredentialClass: "app-installation", NarrowsRepositories: true, NarrowsPermissions: true,
			MaxRepositories: 500, CanDenyCI: true, ExactModes: true,
		}))
		Expect(t.Spec.Auth.TokenLifetime).To(Equal("1h"))
		Expect(t.Spec.CIPaths).To(Equal([]string{".github/workflows/"}))
		Expect(t.Spec.CIPathsFoldCase).To(BeTrue())
		Expect(t.Spec.Site).To(Equal(&connectors.Site{BaseURL: "https://github.com", APIURL: "https://api.github.com", APIPath: "/api/v3"}))
	})
})

var _ = Describe("a connector that writes by push", func() {
	It("carries writes: push from its data", func() {
		doc := githubYAML()
		Expect(doc).To(ContainSubstring("    exactModes: true\n"))
		got, err := connectors.Parse([]byte(strings.Replace(doc, "    exactModes: true\n", "    exactModes: false\n    writes: push\n", 1)))
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Spec.Capabilities.Writes).To(Equal("push"))
	})
})

var _ = DescribeTable("Parse rejects",
	func(from, to, want string) {
		doc := githubYAML()
		Expect(doc).To(ContainSubstring(from))
		_, err := connectors.Parse([]byte(strings.Replace(doc, from, to, 1)))
		Expect(err).To(MatchError(ContainSubstring(want)))
	},
	Entry("an unknown way of writing", "    exactModes: true\n", "    exactModes: true\n    writes: sftp\n", "schema"),
	Entry("an unknown tier", "tier: reversible-write", "tier: sometimes", "schema"),
	Entry("a forbidden tier that is not denied", "      forbidden: deny\n    standard:", "      forbidden: allow\n    standard:", "schema"),
	Entry("network rules on a write scope", "      tools: [comment_on_issue, get_approval_status]",
		"      tools: [comment_on_issue, get_approval_status]\n      network:\n        - {host: api.github.com, methods: [POST], paths: ['/x']}",
		"only read-tier scopes may have network rules"),
	Entry("a network host outside spec.hosts", "        - host: api.github.com\n          methods: [GET]",
		"        - host: evil.example.com\n          methods: [GET]", "not in spec.hosts"),
	Entry("a wildcard network host", "        - host: api.github.com\n          methods: [GET]",
		"        - host: '*.github.com'\n          methods: [GET]", "schema"),
	Entry("a glob network host", "        - host: api.github.com\n          methods: [GET]",
		"        - host: '**'\n          methods: [GET]", "schema"),
	Entry("a scope permission the app lacks", "        issues: write\n      tools", "        actions: write\n      tools",
		"not among auth.permissions"),
	Entry("a forbidden permission on a scope", "        issues: write\n      tools", "        workflows: write\n      tools",
		"forbidden"),
	Entry("a deny rule naming a target", "          paths: ['/**/git-receive-pack']", "          paths: ['/{owner}/x']",
		"deny rules cannot use"),
	Entry("a site host outside spec.hosts", "    baseURL: https://github.com", "    baseURL: https://elsewhere.example.test", "not in spec.hosts"),
	Entry("a site that is not https", "    apiURL: https://api.github.com", "    apiURL: http://api.github.com", "schema"),
	Entry("an unknown credential class", "credentialClass: app-installation", "credentialClass: magic", "schema"),
	Entry("a CI path that climbs out", "    - .github/workflows/", "    - ../x/", "ciPaths"),
	Entry("a CI path that is absolute", "    - .github/workflows/", "    - /etc/", "schema"),
	Entry("an unknown capability", "    canDenyCI: true", "    canDenyCI: true\n    canFly: true", "schema"),
	Entry("a step naming an unknown field", "fields: [installationId]", "fields: [nope]", "unknown field"),
	Entry("an unknown property", "  hosts:\n", "  extra: 1\n  hosts:\n", "schema"),
	Entry("a wrong apiVersion", "apiVersion: "+connectors.APIVersion, "apiVersion: "+project.ConnectorsGroup+"/v1", "schema"),
)

func allow(method, path string, query ...string) connectors.HTTPRule {
	r := connectors.HTTPRule{Method: method, Path: path}
	if len(query) == 2 {
		r.Query = map[string]string{query[0]: query[1]}
	}
	return r
}

func giteaType() *connectors.Type {
	GinkgoHelper()
	t, ok := connectors.Get("gitea")
	Expect(ok).To(BeTrue())
	return t
}

var _ = Describe("the Gitea type (ADR 0062, 0063)", func() {
	It("is a static-token code host that writes by push, with the weaker-credential descriptor", func() {
		t := giteaType()
		Expect(t.Spec.Auth.Type).To(Equal(connectors.AuthStaticToken))
		Expect(t.Spec.Auth.TokenWarnAfter).To(Equal("2160h"), "90 days")
		Expect(t.Spec.Auth.TokenLifetime).To(BeEmpty(), "a Gitea token does not expire")
		Expect(t.Spec.Capabilities).To(Equal(&connectors.Capabilities{
			DisplayName: "Gitea", CredentialClass: "static-token", ExactModes: true, Writes: "push",
		}))
		Expect(t.Spec.CIPaths).To(ConsistOf(".gitea/workflows/", ".gitea/scoped_workflows/", ".github/workflows/"))
		Expect(t.Spec.CIPathsFoldCase).To(BeFalse())
	})

	It("keeps all three presets, with the same tiers as GitHub", func() {
		g, t := githubType(), giteaType()
		for _, p := range []templates.Preset{templates.PresetReadOnly, templates.PresetStandard, templates.PresetTrusted} {
			for _, s := range []string{"repo.read", "pull_request.create", "issue.comment"} {
				Expect(t.Decide(p, s)).To(Equal(g.Decide(p, s)), "%s %s", p, s)
			}
		}
	})

	It("has two secret fields, the read and the write token", func() {
		var secret []string
		for _, f := range giteaType().Spec.SetupGuide.Fields {
			if f.Secret {
				secret = append(secret, f.ID)
			}
		}
		Expect(secret).To(Equal([]string{"readToken", "writeToken"}))
	})

	It("needs a base URL: there is no default host", func() {
		_, err := giteaType().Locate("")
		Expect(err).To(MatchError(ContainSubstring("needs the URL of your server")))
	})

	DescribeTable("locates a server by its base URL",
		func(base, web, api string) {
			loc, err := giteaType().Locate(base)
			Expect(err).NotTo(HaveOccurred())
			Expect(loc.Default).To(BeFalse())
			Expect(loc.BaseURL()).To(Equal(web))
			Expect(loc.APIBaseURL()).To(Equal(api))
		},
		Entry("a host", "https://git.example.test", "https://git.example.test", "https://git.example.test/api/v1"),
		Entry("a port, upper case and a trailing slash", "https://GIT.example.test:8443/", "https://git.example.test:8443", "https://git.example.test:8443/api/v1"),
		Entry("a sub-path", "https://example.test/gitea", "https://example.test/gitea", "https://example.test/gitea/api/v1"),
		Entry("a deeper sub-path", "https://example.test/tools/git/", "https://example.test/tools/git", "https://example.test/tools/git/api/v1"),
	)

	DescribeTable("refuses",
		func(base string) {
			_, err := giteaType().Locate(base)
			Expect(err).To(HaveOccurred())
		},
		Entry("http", "http://git.example.test"),
		Entry("a query", "https://git.example.test/gitea?x=1"),
		Entry("a fragment", "https://git.example.test/gitea#x"),
		Entry("credentials", "https://u:p@git.example.test/gitea"),
		Entry("a path glob", "https://git.example.test/*"),
		Entry("a placeholder in the path", "https://git.example.test/{owner}"),
		Entry("a dot segment", "https://git.example.test/a/../b"),
		Entry("an empty segment", "https://git.example.test/a//b"),
		Entry("an escape", "https://git.example.test/a%2Fb"),
		Entry("a very long path", "https://git.example.test/"+strings.Repeat("a", 200)),
	)

	It("keeps GitHub's rule: no path, on any host", func() {
		_, err := githubType().Locate("https://ghes.example.test/sub")
		Expect(err).To(MatchError(ContainSubstring("only a host")))
	})

	It("renders the read grant at the server, under its sub-path, with push and writes denied", func() {
		t := giteaType()
		loc, err := t.Locate("https://example.test:8443/gitea")
		Expect(err).NotTo(HaveOccurred())
		got, err := t.NetworkAt(loc, templates.PresetStandard, []string{"repo.read"}, []string{"acme/widgets"})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		h := got[0]
		Expect(h.Host).To(Equal("example.test"))
		Expect(h.Port).To(Equal(8443))
		Expect(h.Allow).To(Equal([]connectors.HTTPRule{
			allow("GET", "/gitea/acme/widgets.git/info/refs", "service", "git-upload-pack"),
			allow("GET", "/gitea/acme/widgets/info/refs", "service", "git-upload-pack"),
			allow("POST", "/gitea/acme/widgets.git/git-upload-pack"),
			allow("POST", "/gitea/acme/widgets/git-upload-pack"),
			allow("GET", "/gitea/api/v1/repos/acme/widgets"),
			allow("GET", "/gitea/api/v1/repos/acme/widgets/**"),
		}))
		Expect(h.Deny).To(ContainElements(
			allow("GET", "/gitea/**/info/refs", "service", "git-receive-pack"),
			allow("POST", "/gitea/**/git-receive-pack"),
			allow("POST", "/gitea/api/v1/**"), allow("DELETE", "/gitea/api/v1/**"),
		))
		for _, r := range append(append([]connectors.HTTPRule{}, h.Allow...), h.Deny...) {
			Expect(r.Path).To(HavePrefix("/gitea/"), "every rule is under the sub-path")
			Expect(r.Path).NotTo(ContainSubstring("invalid"))
		}
	})

	It("renders the same rules at the root of a server without a sub-path", func() {
		t := giteaType()
		loc, err := t.Locate("https://git.example.test")
		Expect(err).NotTo(HaveOccurred())
		got, err := t.NetworkAt(loc, templates.PresetStandard, []string{"repo.read"}, []string{"acme/widgets", "acme/gadgets"})
		Expect(err).NotTo(HaveOccurred())
		Expect(got[0].Allow).To(ContainElements(allow("GET", "/api/v1/repos/acme/gadgets"), allow("POST", "/acme/widgets.git/git-upload-pack")))
		// Read only gives no write scope's rules: only repo.read has network rules.
		none, err := t.NetworkAt(loc, templates.PresetStandard, []string{"pull_request.create"}, []string{"acme/widgets"})
		Expect(err).NotTo(HaveOccurred())
		Expect(none).To(BeEmpty())
	})

	DescribeTable("validates targets", func(target string, ok bool) {
		Expect(giteaType().ValidTarget(target)).To(Equal(ok))
	},
		Entry("owner/name", "koinon-demo/hello.world_2", true),
		Entry("an organisation with a dot", "my.org/app", true),
		Entry("no owner", "app", false),
		Entry("a path", "a/b/c", false),
		Entry("a leading dot", ".a/b", false),
	)
})
