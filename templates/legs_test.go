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
// Fetches and search queries are external-low sinks, tools in the auto tier (ADR 0091 decision 1),
// so the same tools are the outbound leg.
var webDeclaration = mcpservers.Declaration{Kind: mcpservers.KindFetch, Legs: []string{mcpservers.LegUntrustedInput}}

type templateLegs struct {
	untrustedInput bool
	privateData    bool
	autoOutbound   bool
}

func (l templateLegs) all() bool { return l.untrustedInput && l.privateData && l.autoOutbound }

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
		l.autoOutbound = d.Kind == mcpservers.KindFetch || d.Kind == mcpservers.KindSearch
	}
	return l
}

// knownAllThree lists the shipped templates that still hold all three legs, each with the reason.
// The test below fails for a template that is not here, and for an entry that no longer holds all
// three, so the list can only shrink. Do not add to it: split the template instead.
var knownAllThree = map[string]string{
	"lcm-upgrader": "reads the Gitea repository and release notes through the allowed fetch tools; " +
		"the pull request it opens is an ask-tier call, but the fetch is not",
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
		Expect(legsOf(w)).To(Equal(templateLegs{untrustedInput: true, autoOutbound: true}), "web-researcher has no private data")
		Expect(w.Spec.Grants).To(BeEmpty(), "web-researcher holds no grant")
		Expect(w.Spec.Instructions).To(ContainSubstring("web_url_read"))
		Expect(w.Spec.Instructions).NotTo(ContainSubstring("MCP"), "an MCP server of private data is not named")
	})

	It("ships no template that holds untrusted input, private data and an auto-tier outbound tool", func() {
		all, err := templates.All()
		Expect(err).NotTo(HaveOccurred())
		for _, t := range all {
			if reason, known := knownAllThree[t.Metadata.ID]; known {
				Expect(legsOf(t).all()).To(BeTrue(), t.Metadata.ID+" is listed in knownAllThree ("+reason+") but no longer holds all three: remove it from the list")
				continue
			}
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
		Expect(legsOf(changed).all()).To(BeTrue())
	})

	It("reports web-researcher again when it gets a repository grant back", func() {
		raw := strings.Replace(string(readFile("web-researcher.yaml")), "  grants: []\n",
			"  grants:\n    - connector: gitea\n      scopes: [repo.read]\n      preset: read-only\n      targets: {min: 1, max: 1}\n", 1)
		changed, err := templates.Parse([]byte(raw))
		Expect(err).NotTo(HaveOccurred())
		Expect(legsOf(changed).all()).To(BeTrue())
	})
})
