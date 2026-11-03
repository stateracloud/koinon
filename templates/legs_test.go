// SPDX-License-Identifier: Apache-2.0

package templates_test

import (
	"regexp"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/templates"
)

// A template does not name MCP servers: an admin allows them on the agent. What a template does
// fix is its grants (the org's repositories, private data) and the tools its instructions tell the
// agent to call. This test reads both, classifies the tools the instructions name with the same
// declaration a server record carries (ADR 0084 decision 8, ADR 0091 decision 4), and fails when
// one template holds all three legs of the lethal trifecta (ADR 0084 decision 9).

// webTools matches the instructions that send an agent to the public web: the RT1 search server's
// tools and the generic fetch and search tools an admin may allow.
var webTools = regexp.MustCompile(`(?i)web_url_read|searxng_web_search|web search|search and fetch|search or fetch|fetch tool|search tool`)

// webDeclaration is what such a tool declares: a fetch server whose results an outsider controls.
// Fetches and search queries are external-low sinks (ADR 0091 decisions 1, 4 and 5): they get a byte
// cap and never an approval, so they are not the outbound leg.
var webDeclaration = mcpservers.Declaration{Kind: mcpservers.KindFetch, Legs: []string{mcpservers.LegUntrustedInput}}

// toolCall matches a tool the instructions tell the agent to call, written in backticks.
var toolCall = regexp.MustCompile("`([a-z]+(?:_[a-z]+)+)`")

// knownTools are the tools the shipped templates name, with what they are under ADR 0091: the
// platform's own questions, the forge tools (ask-tier or internal) and the external-low web tools.
// A tool that is not here comes from an admin-allowed server nobody classified, which defaults to
// external-high (ADR 0091 decision 4). Instructions name only auto-tier tools to call; a tool that
// asks first is listed here so it does not count.
var knownTools = map[string]bool{
	"ask_user": true, "create_pull_request": true, "github_create_pull_request": true,
	"gitea_create_pull_request": true, "comment_on_issue": true,
	"web_url_read": true, "searxng_web_search": true,
}

type templateLegs struct {
	untrustedInput bool
	privateData    bool
	// autoOutbound is an auto-tier external-high tool (ADR 0091 line 146).
	autoOutbound bool
}

func (l templateLegs) all() bool { return l.untrustedInput && l.privateData && l.autoOutbound }

// bothInputLegs is untrusted input and private data: the pair that makes the byte cap apply.
func (l templateLegs) bothInputLegs() bool { return l.untrustedInput && l.privateData }

// legsOf reads the legs a template gives an agent made from it.
func legsOf(t *templates.Template) templateLegs {
	var l templateLegs
	// Every grant is on a connector to a system the org owns: repositories and what they hold.
	if len(t.Spec.Grants) > 0 {
		l.privateData = true
	}
	// MCP servers the template points the agent at without naming them are internal data sources
	// (billing, cost, metrics); no web tool is among them.
	if strings.Contains(t.Spec.Instructions, "MCP") {
		l.privateData = true
	}
	text := t.Metadata.Description + "\n" + t.Spec.Instructions
	if webTools.MatchString(text) {
		d := webDeclaration
		l.untrustedInput = slices.Contains(d.Legs, mcpservers.LegUntrustedInput)
		l.privateData = l.privateData || slices.Contains(d.Legs, mcpservers.LegPrivateData)
	}
	for _, m := range toolCall.FindAllStringSubmatch(t.Spec.Instructions, -1) {
		if !knownTools[m[1]] {
			l.autoOutbound = true
		}
	}
	return l
}

var _ = Describe("Template legs (ADR 0084 decision 9)", func() {
	It("splits private research from web research", func() {
		r, ok := templates.Get("researcher")
		Expect(ok).To(BeTrue())
		Expect(legsOf(r)).To(Equal(templateLegs{privateData: true}), "researcher is private-only")
		Expect(r.Spec.Grants).NotTo(BeEmpty())
		Expect(webTools.MatchString(r.Spec.Instructions+r.Metadata.Description)).To(BeFalse(), "researcher has no web-read tool")

		w, ok := templates.Get("web-researcher")
		Expect(ok).To(BeTrue())
		Expect(legsOf(w)).To(Equal(templateLegs{untrustedInput: true}), "web-researcher has no private data")
		Expect(w.Spec.Grants).To(BeEmpty(), "web-researcher holds no grant")
		Expect(w.Spec.Instructions).To(ContainSubstring("web_url_read"))
		Expect(w.Spec.Instructions).NotTo(ContainSubstring("MCP"), "an MCP server of private data is not named")
	})

	It("ships no template that holds untrusted input, private data and an auto-tier outbound tool", func() {
		all, err := templates.All()
		Expect(err).NotTo(HaveOccurred())
		for _, t := range all {
			Expect(legsOf(t).all()).To(BeFalse(), t.Metadata.ID+" holds untrusted input, private data and an auto-tier outbound tool: split it")
		}
	})

	It("reports a template that holds all three legs (negative fixture)", func() {
		t, err := templates.Parse(readFile("testdata/legs/all-three.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(legsOf(t)).To(Equal(templateLegs{untrustedInput: true, privateData: true, autoOutbound: true}))
		Expect(legsOf(t).all()).To(BeTrue())
	})

	It("reports researcher again when it gets a web read tool back", func() {
		r, ok := templates.Get("researcher")
		Expect(ok).To(BeTrue())
		raw := strings.Replace(string(readFile("researcher.yaml")),
			"Read long files in parts, not whole.", "Read pages with `web_url_read`.", 1)
		Expect(raw).NotTo(Equal(string(readFile("researcher.yaml"))))
		changed, err := templates.Parse([]byte(raw))
		Expect(err).NotTo(HaveOccurred())
		Expect(changed.Metadata.ID).To(Equal(r.Metadata.ID))
		Expect(legsOf(changed).bothInputLegs()).To(BeTrue())
	})

	It("reports web-researcher again when it gets a repository grant back", func() {
		raw := strings.Replace(string(readFile("web-researcher.yaml")), "  grants: []\n",
			"  grants:\n    - connector: gitea\n      scopes: [repo.read]\n      preset: read-only\n      targets: {min: 1, max: 1}\n", 1)
		changed, err := templates.Parse([]byte(raw))
		Expect(err).NotTo(HaveOccurred())
		Expect(legsOf(changed).bothInputLegs()).To(BeTrue())
	})
})
