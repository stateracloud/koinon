// SPDX-License-Identifier: Apache-2.0

package templates_test

import (
	"regexp"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/sinkclass"
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

func boolPtr(b bool) *bool { return &b }

// knownTools are the tools the shipped templates name, each with the declaration a server record
// would carry for it (ADR 0091 decision 2): the platform's own questions (none), the forge tools
// (code-host derived: the class follows the repository, and the org's own repositories are internal,
// ask-tier) and the external-low web tools whose results an outsider controls. A tool that is not
// here comes from an admin-allowed server nobody classified: sinkclass.Default() on a server with
// mcpservers.DefaultDeclaration(), external-high with both data legs (ADR 0091 decision 4).
var knownTools = map[string]sinkclass.Declaration{
	"ask_user":                   {Class: sinkclass.None, Derivation: sinkclass.Fixed},
	"create_pull_request":        {Class: sinkclass.ExternalHigh, Derivation: sinkclass.CodeHost, AddressArgs: []string{"owner", "repo"}},
	"github_create_pull_request": {Class: sinkclass.ExternalHigh, Derivation: sinkclass.CodeHost, AddressArgs: []string{"owner", "repo"}},
	"gitea_create_pull_request":  {Class: sinkclass.ExternalHigh, Derivation: sinkclass.CodeHost, AddressArgs: []string{"owner", "repo"}},
	"comment_on_issue":           {Class: sinkclass.ExternalHigh, Derivation: sinkclass.CodeHost, AddressArgs: []string{"owner", "repo"}},
	"web_url_read":               {Class: sinkclass.ExternalLow, Derivation: sinkclass.Fetch, AddressArgs: []string{"url"}, ResultUntrustedInput: boolPtr(true), ResultPrivateData: boolPtr(false)},
	"searxng_web_search":         {Class: sinkclass.ExternalLow, Derivation: sinkclass.Fixed, ResultUntrustedInput: boolPtr(true), ResultPrivateData: boolPtr(false)},
}

// toolLegs returns the declaration of a named tool and the legs its results carry. An unlisted tool
// is the default of both.
func toolLegs(name string) (sinkclass.Declaration, []string) {
	if d, ok := knownTools[name]; ok {
		var legs []string
		if d.ResultUntrustedInput != nil && *d.ResultUntrustedInput {
			legs = append(legs, mcpservers.LegUntrustedInput)
		}
		if d.ResultPrivateData != nil && *d.ResultPrivateData {
			legs = append(legs, mcpservers.LegPrivateData)
		}
		return d, legs
	}
	return sinkclass.Default(), mcpservers.DefaultDeclaration().Legs
}

// autoExternalHigh reports whether a call of d would run without approval to a party outside the
// org: a fixed external-high tool. A code-host tool is derived per call and is internal for the
// org's own repositories.
func autoExternalHigh(d sinkclass.Declaration) bool {
	return d.Class == sinkclass.ExternalHigh && d.Derivation == sinkclass.Fixed
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
		d, legs := toolLegs(m[1])
		if slices.Contains(legs, mcpservers.LegUntrustedInput) {
			l.untrustedInput = true
		}
		if slices.Contains(legs, mcpservers.LegPrivateData) {
			l.privateData = true
		}
		if autoExternalHigh(d) {
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

	It("reports a template that names an unlisted tool and no web phrasing (negative fixture)", func() {
		t, err := templates.Parse(readFile("testdata/legs/unlisted-tool.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(webTools.MatchString(t.Spec.Instructions + t.Metadata.Description)).To(BeFalse())
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
