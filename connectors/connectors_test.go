// SPDX-License-Identifier: Apache-2.0

package connectors_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/connectors"
	"github.com/procoduck/koinon/internal/project"
	"github.com/procoduck/koinon/templates"
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
		Expect(t.Permissions("repo.read")).To(Equal(map[string]string{"contents": "read", "metadata": "read"}))
		Expect(t.Permissions("repo.read", "pull_request.create")).To(Equal(
			map[string]string{"contents": "write", "metadata": "read", "pull_requests": "write"}))
	})

	It("validates targets", func() {
		t := githubType()
		Expect(t.ValidTarget("procoduck/koinon")).To(BeTrue())
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

var _ = DescribeTable("Parse rejects",
	func(from, to, want string) {
		doc := githubYAML()
		Expect(doc).To(ContainSubstring(from))
		_, err := connectors.Parse([]byte(strings.Replace(doc, from, to, 1)))
		Expect(err).To(MatchError(ContainSubstring(want)))
	},
	Entry("an unknown tier", "tier: reversible-write", "tier: sometimes", "schema"),
	Entry("a forbidden tier that is not denied", "      forbidden: deny\n    standard:", "      forbidden: allow\n    standard:", "schema"),
	Entry("network rules on a write scope", "      tools: [comment_on_issue, get_approval_status]",
		"      tools: [comment_on_issue, get_approval_status]\n      network:\n        - {host: api.github.com, methods: [POST], paths: ['/x']}",
		"only read-tier scopes may have network rules"),
	Entry("a network host outside spec.hosts", "        - host: api.github.com\n          methods: [GET]",
		"        - host: evil.example.com\n          methods: [GET]", "not in spec.hosts"),
	Entry("a scope permission the app lacks", "        issues: write\n      tools", "        actions: write\n      tools",
		"not among auth.permissions"),
	Entry("a forbidden permission on a scope", "        issues: write\n      tools", "        workflows: write\n      tools",
		"forbidden"),
	Entry("a step naming an unknown field", "fields: [installationId]", "fields: [nope]", "unknown field"),
	Entry("an unknown property", "  hosts:\n", "  extra: 1\n  hosts:\n", "schema"),
	Entry("a wrong apiVersion", "apiVersion: "+connectors.APIVersion, "apiVersion: "+project.ConnectorsGroup+"/v1", "schema"),
)
